package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type namePublicationStore struct {
	*storagetest.MemoryStore
	puts int
}

func (s *namePublicationStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts++
	return s.MemoryStore.Put(ctx, key, data)
}

// This is synthetic settled-file integration, not native UI acceptance. It uses
// the production Codex label provider, writer, listing index and CLI readers.
func TestSettledCodexNamePublicationAndArchiveOnlyRead(t *testing.T) {
	t.Parallel()
	home, nativeHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	must(t, err)
	local, err := state.Open(home)
	must(t, err)
	remote := &namePublicationStore{MemoryStore: storagetest.NewMemoryStore()}
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	env.LookPath = func(string) (string, error) {
		t.Fatal("files/reader started native executable discovery")
		return "", nil
	}
	env.CodexLabelHost = func(context.Context, string) (agentapi.LabelTransport, error) {
		t.Fatal("files/reader started native host")
		return nil, nil
	}
	must(t, os.Mkdir(filepath.Join(nativeHome, "sessions"), 0700))
	ids := []string{"01900000-0000-7000-8000-000000000001", "01900000-0000-7000-8000-000000000002"}
	prompts := []string{"Invented first prompt", "Invented second prompt"}
	paths := make([]string, len(ids))
	for i, id := range ids {
		paths[i] = filepath.Join(nativeHome, "sessions", id+".jsonl")
		raw := fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-10-06T23:00:00Z","payload":{"id":%q,"cli_version":"0.159.2","history_mode":"paginated","cwd":%q,"source":"cli"}}
{"type":"response_item","timestamp":"2026-10-07T00:01:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}
`, id, project, prompts[i])
		must(t, os.WriteFile(paths[i], []byte(raw), 0600))
		must(t, handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": id, "cwd": project, "transcript_path": paths[i]}, now.Add(time.Duration(i)*time.Minute)))
	}
	// Codex's name index: one record per thread, the last nonempty name wins.
	// unfinished appends a record Codex is still writing, which proves nothing.
	writeIndex := func(first, unfinished string) {
		t.Helper()
		var data []byte
		for i, id := range ids {
			name := "Shared synthetic name"
			if i == 0 {
				name = first
			}
			line, marshalErr := json.Marshal(map[string]string{"id": id, "thread_name": name, "updated_at": "2026-10-07T00:00:00Z"})
			must(t, marshalErr)
			data = append(append(data, line...), '\n')
		}
		if unfinished != "" {
			data = append(data, fmt.Sprintf(`{"id":%q,"thread_name":%q`, ids[0], unfinished)...)
		}
		must(t, os.WriteFile(filepath.Join(nativeHome, "session_index.jsonl"), data, 0600))
	}
	writeNames := func(first string) {
		t.Helper()
		writeIndex(first, "")
	}
	writeNames("Shared synthetic name")
	opts := collector.Options{Sources: productionAgents, Parsers: productionAgents, Labels: env.labelProviders(cfg), LabelEnvironment: agentapi.LabelEnvironment{Homes: []string{nativeHome}}, MachineID: cfg.MachineID, Now: func() time.Time { return now }, AcceptSession: cfg.AcceptSession}
	collect := func() collector.Result {
		t.Helper()
		result, runErr := collector.Run(t.Context(), local, remote, opts)
		if runErr != nil || len(result.Errors) != 0 {
			t.Fatalf("collector: %+v %v", result, runErr)
		}
		return result
	}
	collect() // Retained ordinary source must exist before external lookup.
	now = now.Add(time.Hour)
	collect()
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 2 {
		t.Fatalf("registrations: %d", len(regs))
	}
	var target archive.SessionRegistration
	for _, reg := range regs {
		metadataKey, keyErr := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
		must(t, keyErr)
		metadata, _, readErr := reader.RefreshAndLoad(t.Context(), remote, metadataKey, reader.Limits{})
		must(t, readErr)
		if metadata.NativeSessionID != reg.NativeSessionID || metadata.Name != "Shared synthetic name" {
			t.Fatal("same-title baseline lost UUID identity or published name")
		}
		if reg.NativeSessionID == ids[0] {
			target = reg
		}
	}
	if target.ArchiveSessionID == "" {
		t.Fatal("missing UUID-matched registration")
	}
	// Exercise bounded indexed and uncapped CLI reads while both names are
	// still equal. A fresh reader home cannot hide a coalesced index.
	readerHome := t.TempDir()
	must(t, config.Save(readerHome, cfg))
	env.Home = func() (string, error) { return readerHome, nil }
	var commandStderr string
	run := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := Run(args, nil, &out, &stderr, env); code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, &stderr)
		}
		commandStderr = stderr.String()
		return out.String()
	}
	for _, limit := range []string{"2", "0"} {
		indexedBodies := 0
		env.observeListBody = func(string, bool) { indexedBodies++ }
		var baseline listDocument
		must(t, json.Unmarshal([]byte(run("list", "--harness", "codex", "--all-projects", "--json", "--limit", limit)), &baseline))
		env.observeListBody = nil
		if limit == "2" && (commandStderr != "" || indexedBodies != len(ids)) {
			t.Fatalf("same-name indexed list did not verify both bodies: bodies=%d stderr=%s", indexedBodies, commandStderr)
		}
		if len(baseline.Sessions) != len(ids) || baseline.Returned != len(ids) || !baseline.TotalMatchedKnown || baseline.TotalMatched == nil || *baseline.TotalMatched != len(ids) || baseline.Truncated {
			t.Fatalf("same-name list lost a session: %+v", baseline)
		}
		seen := make(map[string]bool)
		for _, row := range baseline.Sessions {
			matched := false
			for i, id := range ids {
				for _, reg := range regs {
					if reg.NativeSessionID == id && row.SessionID == reg.ArchiveSessionID {
						matched = row.NativeSessionID == id && row.Name == "Shared synthetic name" && row.Title == prompts[i]
					}
				}
			}
			if !matched || seen[row.SessionID] {
				t.Fatalf("same-name list mismatched or repeated identity: %+v", row)
			}
			seen[row.SessionID] = true
		}
	}
	for i, id := range ids {
		for _, reg := range regs {
			if reg.NativeSessionID != id {
				continue
			}
			var shown archive.Metadata
			must(t, json.Unmarshal([]byte(run("show", reg.ArchiveSessionID, "--harness", "codex", "--json")), &shown))
			if shown.SessionID != reg.ArchiveSessionID || shown.NativeSessionID != id || shown.Name != "Shared synthetic name" || shown.Title != prompts[i] {
				t.Fatalf("same-name show mismatched identity: %+v", shown)
			}
			out := run("show", reg.ArchiveSessionID, "--harness", "codex", "--no-pager")
			if !strings.Contains(out, "Shared synthetic name") || !strings.Contains(out, prompts[i]) || strings.Contains(out, prompts[1-i]) {
				t.Fatalf("same-name text show mismatched prompt: %s", out)
			}
		}
	}
	key, err := archive.MetadataObjectKey("codex", target.ArchiveSessionID)
	must(t, err)
	read := func() (archive.Metadata, archive.SourceBundle) {
		t.Helper()
		m, b, readErr := reader.RefreshAndLoad(t.Context(), remote, key, reader.Limits{})
		must(t, readErr)
		return m, b
	}
	before, beforeBundle := read()
	if before.Name != "Shared synthetic name" || before.Title != prompts[0] || before.NativeSessionID != ids[0] {
		t.Fatalf("baseline: %+v", before)
	}
	transcript, err := os.ReadFile(paths[0])
	must(t, err)
	writeNames("Renamed without a prompt")
	now = now.Add(time.Hour)
	if result := collect(); len(result.Published) != 1 {
		t.Fatalf("rename publication: %+v", result)
	}
	after, afterBundle := read()
	if after.Name != "Renamed without a prompt" || after.Title != before.Title || after.SourceBundle == before.SourceBundle || after.NativeSessionID != before.NativeSessionID || after.SessionID != before.SessionID || !after.CapturedAt.Equal(before.CapturedAt) || !after.StartedAt.Equal(before.StartedAt) || !reflect.DeepEqual(after.EndedAt, before.EndedAt) || !reflect.DeepEqual(after.Counts, before.Counts) || !reflect.DeepEqual(afterBundle.NativeRecords, beforeBundle.NativeRecords) {
		t.Fatal("rename lost retained evidence or changed conversation activity")
	}
	unchanged, err := os.ReadFile(paths[0])
	must(t, err)
	if !bytes.Equal(transcript, unchanged) {
		t.Fatal("rename changed transcript")
	}
	puts := remote.puts
	now = now.Add(time.Hour)
	local, err = state.Open(home) // Restart the archive writer from durable state.
	must(t, err)
	if result := collect(); len(result.Published) != 0 || remote.puts != puts {
		t.Fatalf("unchanged restart wrote objects: %+v puts=%d/%d", result, remote.puts, puts)
	}
	persisted, _ := read()
	if !reflect.DeepEqual(persisted, after) {
		t.Fatal("unchanged pass changed metadata")
	}
	// An unfinished index record leaves the index unavailable. Refusal must
	// keep the already published name; it cannot turn the newer name into proof.
	writeIndex("Renamed without a prompt", "Unavailable newer name")
	now = now.Add(time.Hour)
	if result := collect(); len(result.Published) != 0 || remote.puts != puts {
		t.Fatalf("live sidecar republished: %+v", result)
	}
	retained, _ := read()
	if !reflect.DeepEqual(retained, after) {
		t.Fatal("unavailable files cleared prior name")
	}
	must(t, os.RemoveAll(nativeHome)) // Reader has no native home at all.
	archiveReaderHome := t.TempDir()
	must(t, config.Save(archiveReaderHome, cfg))
	env.Home = func() (string, error) { return archiveReaderHome, nil }
	for _, limit := range []string{"1", "0"} {
		out := run("list", "--harness", "codex", "--all-projects", "--json", "--limit", limit)
		var listing listDocument
		must(t, json.Unmarshal([]byte(out), &listing))
		rows := listing.Sessions
		want := 2
		if limit == "1" {
			want = 1
		}
		if len(rows) != want || listing.Returned != want || !listing.TotalMatchedKnown || listing.TotalMatched == nil || *listing.TotalMatched != 2 || listing.Truncated != (limit == "1") {
			t.Fatalf("limit %s returned unexpected envelope: %+v", limit, listing)
		}
		if limit == "0" {
			matched := false
			for _, row := range rows {
				if row.SessionID == target.ArchiveSessionID {
					matched = row.NativeSessionID == ids[0] && row.Name == after.Name
				}
			}
			if !matched {
				t.Fatal("uncapped list lost UUID-matched renamed row")
			}
		}
	}
	for _, args := range [][]string{{"list", "--harness", "codex", "--all-projects", "--no-pager"}, {"show", target.ArchiveSessionID, "--harness", "codex", "--no-pager"}, {"show", target.ArchiveSessionID, "--harness", "codex", "--json"}} {
		if out := run(args...); !strings.Contains(out, after.Name) || strings.Contains(out, "Unavailable newer name") {
			t.Fatalf("archive-only name missing: %s", out)
		}
	}
	if remote.puts != puts {
		t.Fatal("reader wrote remote objects")
	}
}
