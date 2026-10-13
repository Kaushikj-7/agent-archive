package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// lifecycleStore observes the real retention deletion order without changing
// the in-memory store's bounded-read and conditional-write capabilities.
type lifecycleStore struct {
	*storagetest.MemoryStore
	mu      sync.Mutex
	deleted []string
}

func (s *lifecycleStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deleted = append(s.deleted, key)
	s.mu.Unlock()
	return s.MemoryStore.Delete(ctx, key)
}

// This control connects the real supported native discovery, collector,
// verification, reader, CLI cache and retention paths. It uses only private
// synthetic files and an injected store; it does not establish live acceptance.
func TestCodexCaptureReadAndRetentionLifecycle(t *testing.T) {
	t.Parallel()
	physical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
	home, userHome, project, native := physical(), physical(), physical(), physical()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), RetentionDays: 1, Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{native}}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	must(t, os.WriteFile(filepath.Join(project, ".env"), []byte("PRIVATE_FILE_NEVER_READ"), 0600))
	db, err := sql.Open("sqlite", filepath.Join(native, "state_5.sqlite"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT)")
	must(t, err)
	// The fixture is the supported 0.155 legacy producer shape. Children have
	// their own task boundary, including one whose parent is unavailable.
	write := func(n int, parent, root, marker string, started time.Time) (string, string) {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
		payload := map[string]any{"id": id, "session_id": id, "timestamp": started.Format(time.RFC3339Nano), "cwd": root, "source": "cli", "originator": "codex-tui", "cli_version": "0.155.0", "history_mode": "legacy", "thread_source": "user"}
		if parent != "" {
			payload["session_id"], payload["parent_thread_id"] = parent, parent
			payload["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
			payload["subagent_history_start_ordinal"] = 1
		}
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		must(t, enc.Encode(map[string]any{"type": "session_meta", "timestamp": started.Format(time.RFC3339Nano), "payload": payload}))
		must(t, enc.Encode(map[string]any{"type": "event_msg", "timestamp": started.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "started_at": started.Format(time.RFC3339Nano)}}))
		must(t, enc.Encode(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": marker + " password=synthetic-private-password"}}, "analysis": "PRIVATE_ANALYSIS_NEVER_UPLOAD"}}))
		path := filepath.Join(native, "sessions", "rollout-"+id+".jsonl")
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		must(t, os.WriteFile(path, body.Bytes(), 0600))
		_, err := db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", id, path)
		must(t, err)
		return id, path
	}
	rootID, rootPath := write(1, "", project, "SYNTHETIC_ROOT", at.Add(time.Minute))
	linkedID, _ := write(2, rootID, project, "SYNTHETIC_CHILD", at.Add(time.Minute))
	orphanID, _ := write(3, "00000000-0000-0000-0000-000000000999", project, "SYNTHETIC_ORPHAN", at.Add(time.Minute))
	write(4, "", physical(), "EXCLUDED_SCOPE_NEVER_UPLOAD", at.Add(time.Minute))
	remote := &lifecycleStore{MemoryStore: storagetest.NewMemoryStore()}
	now := at.Add(2 * time.Minute)
	env := testEnv(t, home, now)
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.Now = func() time.Time { return now }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	storageClockFollows(t, &env, func() time.Time { return now })
	run := func(args ...string) string {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if code := Run(args, nil, &out, &diagnostics, env); code != 0 {
			t.Fatalf("%v exit=%d diagnostics=%s", args, code, diagnostics.String())
		}
		return out.String()
	}
	checkPrivacy := func() {
		t.Helper()
		objects, err := remote.List(t.Context(), "")
		must(t, err)
		for _, object := range objects {
			body, err := remote.Get(t.Context(), object.Key)
			must(t, err)
			if strings.Contains(object.Key, "/source.") {
				stream, err := gzip.NewReader(bytes.NewReader(body))
				must(t, err)
				body, err = io.ReadAll(stream)
				must(t, err)
				must(t, stream.Close())
			}
			for _, forbidden := range []string{"synthetic-private-password", "PRIVATE_ANALYSIS_NEVER_UPLOAD", "PRIVATE_FILE_NEVER_READ", "EXCLUDED_SCOPE_NEVER_UPLOAD", "DISABLED_SCOPE_NEVER_UPLOAD"} {
				if bytes.Contains(body, []byte(forbidden)) {
					t.Fatalf("forbidden synthetic content uploaded: %s", forbidden)
				}
			}
		}
	}
	for range 2 {
		run("sync")
	}
	checkPrivacy()
	local := state.OpenReadOnly(home)
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 3 {
		t.Fatalf("admission: got %d registrations, want 3", len(regs))
	}
	byNative := map[string]archive.SessionRegistration{}
	for _, reg := range regs {
		byNative[reg.NativeSessionID] = reg
		verified, err := readVerification(home, reg.ArchiveSessionID)
		must(t, err)
		if verified.VerifiedAt.IsZero() {
			t.Fatal("publication lacks read-back verification")
		}
	}
	root, linked, orphan := byNative[rootID], byNative[linkedID], byNative[orphanID]
	if root.NativeChild || !linked.NativeChild || linked.ParentSessionID != root.ArchiveSessionID || !orphan.NativeChild || orphan.ParentSessionID != "" {
		t.Fatalf("native relationships: root=%+v linked=%+v orphan=%+v", root, linked, orphan)
	}
	list := func(want int) []archive.Metadata {
		t.Helper()
		var doc listDocument
		must(t, json.Unmarshal([]byte(run("list", "--all-projects", "--json")), &doc))
		if doc.Returned != want || len(doc.Sessions) != want || !doc.TotalMatchedKnown || doc.TotalMatched == nil || *doc.TotalMatched != want {
			t.Fatalf("listing: %+v", doc)
		}
		return doc.Sessions
	}
	list(3) // Warm the actual disposable metadata cache before republishing.
	for _, reg := range regs {
		var metadata archive.Metadata
		must(t, json.Unmarshal([]byte(run("show", reg.ArchiveSessionID, "--json")), &metadata))
		bundle, err := reader.LoadSource(t.Context(), remote, metadata, reader.Limits{})
		must(t, err)
		if bundle.NativeSessionID != reg.NativeSessionID || bundle.ArchiveSessionID != reg.ArchiveSessionID {
			t.Fatal("verified source identity differs from selected session")
		}
		if text := run("show", reg.ArchiveSessionID, "--transcript", "--no-pager"); !strings.Contains(text, "SYNTHETIC_") {
			t.Fatal("native transcript was not rendered")
		}
	}
	var statistics statsDocument
	must(t, json.Unmarshal([]byte(run("stats", "--days", "1", "--all", "--json")), &statistics))
	if statistics.Coverage.Sessions != 2 || statistics.Coverage.SubagentSessions != 1 || statistics.Coverage.OrphanSubagents != 1 {
		t.Fatalf("native accounting: %+v", statistics.Coverage)
	}
	// Disabled discovery cannot admit another owner, but existing admitted
	// sessions still catch up through the actual hook and collector paths.
	cfg = mustLoadConfig(t, home)
	previous := cfg
	discoveryPolicy := *cfg.Discovery
	discoveryPolicy.Enabled = false
	cfg.Discovery = &discoveryPolicy
	must(t, config.ReconcileDiscovery(&cfg, previous, now))
	must(t, config.Save(home, cfg))
	write(5, "", project, "DISABLED_SCOPE_NEVER_UPLOAD", at.Add(3*time.Minute))
	now = at.Add(10 * time.Minute)
	file, err := os.OpenFile(rootPath, os.O_APPEND|os.O_WRONLY, 0600)
	must(t, err)
	_, err = file.WriteString(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SYNTHETIC_REFRESH"}]}}` + "\n")
	must(t, err)
	must(t, file.Close())
	must(t, handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": rootID, "cwd": project, "transcript_path": rootPath}, now))
	run("sync")
	checkPrivacy()
	fresh := false
	for _, m := range list(3) {
		if m.SessionID == root.ArchiveSessionID {
			fresh = m.CapturedAt.Equal(now)
		}
	}
	if !fresh || !strings.Contains(run("show", root.ArchiveSessionID, "--transcript", "--no-pager"), "SYNTHETIC_REFRESH") {
		t.Fatal("warm read path retained pre-publication metadata or source")
	}
	// A newer admitted session survives the same sweep that expires all three
	// original owners. Native files remain present to challenge tombstones.
	now = at.Add(36 * time.Hour)
	newID, newPath := write(6, "", project, "SYNTHETIC_SURVIVOR", now)
	must(t, handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": newID, "cwd": project, "transcript_path": newPath}, now))
	run("sync")
	now = at.Add(48 * time.Hour)
	run("sync")
	for _, reg := range regs {
		removal, found, err := local.Removal("codex", reg.NativeSessionID)
		must(t, err)
		if !found || removal.Reason != state.RemovalReasonRetention {
			t.Fatal("expired native owner lacks its durable retention tombstone")
		}
	}
	remaining := list(1)
	if remaining[0].NativeSessionID != newID {
		t.Fatal("retention deleted the newer session")
	}
	// Re-enable scanning over the still-present expired native files. Expiry
	// must remove discovery ownership as well as both publication objects.
	cfg = mustLoadConfig(t, home)
	previous = cfg
	discoveryPolicy = *cfg.Discovery
	discoveryPolicy.Enabled = true
	cfg.Discovery = &discoveryPolicy
	must(t, config.ReconcileDiscovery(&cfg, previous, now))
	must(t, config.Save(home, cfg))
	for range 2 {
		run("sync")
	}
	list(1)
	objects, err := remote.List(t.Context(), "")
	must(t, err)
	for _, object := range objects {
		for _, reg := range regs {
			if strings.HasPrefix(object.Key, "sessions/codex/"+reg.ArchiveSessionID+"/") {
				t.Fatal("expired publication survived or resurrected")
			}
		}
	}
	checkPrivacy()
	remote.mu.Lock()
	deletes := append([]string(nil), remote.deleted...)
	remote.mu.Unlock()
	for _, reg := range regs {
		prefix := "sessions/codex/" + reg.ArchiveSessionID + "/"
		first := ""
		for _, key := range deletes {
			if strings.HasPrefix(key, prefix) {
				first = key
				break
			}
		}
		if first != prefix+"metadata.json" {
			t.Fatalf("canonical pointer was not deleted first for %s: %q", reg.ArchiveSessionID, first)
		}
	}
}
