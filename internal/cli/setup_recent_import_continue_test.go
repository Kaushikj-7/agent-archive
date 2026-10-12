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
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// bucketContains reports whether any object in the bucket, gunzipped when
// it is compressed, contains text.
func bucketContains(t *testing.T, bucket storage.ObjectStore, text string) bool {
	t.Helper()
	objects, err := bucket.List(context.Background(), "")
	must(t, err)
	for _, object := range objects {
		body, err := bucket.Get(context.Background(), object.Key)
		must(t, err)
		if strings.HasSuffix(object.Key, ".gz") {
			reader, err := gzip.NewReader(bytes.NewReader(body))
			must(t, err)
			body, err = io.ReadAll(reader)
			must(t, err)
			must(t, reader.Close())
		}
		if strings.Contains(string(body), text) {
			return true
		}
	}
	return false
}

// A session that started before setup and keeps writing after it is
// imported by setup, and its later turns reach the archive on later
// collector passes, without a hook or another import.
func TestSetupImportedSessionKeepsUpdating(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	f.pastSession(t, "open", "src/web-app", screenNow.Add(-2*time.Hour))
	out := f.runSetup(t, "", setupYesArgs...)
	if !strings.Contains(out, "Imported 1 session from the last 7 days (Claude Code 1).") {
		t.Fatalf("output:\n%s", out)
	}
	now := screenNow.Add(time.Minute)
	f.env.Now = func() time.Time { return now }
	for range 2 {
		if result, err := runOnePass(f.env, true); err != nil || len(result.Errors) > 0 {
			t.Fatalf("first pass: %+v %v", result, err)
		}
	}
	reg := theRegistration(t, f.home)
	if reg.Origin != archive.SessionOriginImport || reg.NativeSessionID != "open" {
		t.Fatalf("registration %+v", reg)
	}
	if !bucketContains(t, f.bucket, "please check it") {
		t.Fatal("imported session was not uploaded")
	}
	const marker = "a later turn written after setup"
	if bucketContains(t, f.bucket, marker) {
		t.Fatal("marker uploaded before it was written")
	}
	// The session keeps going after setup.
	file, err := os.OpenFile(reg.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = fmt.Fprintf(file, `{"type":"user","uuid":"c","parentUuid":"b","sessionId":"open","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", now.Add(time.Minute).Format(time.RFC3339), marker)
	must(t, err)
	must(t, file.Close())
	written := now.Add(time.Minute)
	must(t, os.Chtimes(reg.TranscriptPath, written, written))
	now = now.Add(10 * time.Minute)
	for range 2 {
		if result, err := runOnePass(f.env, true); err != nil || len(result.Errors) > 0 {
			t.Fatalf("later pass: %+v %v", result, err)
		}
	}
	if !bucketContains(t, f.bucket, marker) {
		t.Fatal("the imported session's later turn never reached the archive")
	}
	if after := theRegistration(t, f.home); after.ArchiveSessionID != reg.ArchiveSessionID || after.Origin != archive.SessionOriginImport {
		t.Fatalf("identity changed: %+v", after)
	}
}

// The setup case that motivated the import: a Codex session that started
// before discovery was enabled is refused by discovery's start floor, but
// once backfill --since 7d (setup's import) registers it, discovery leaves
// it alone and the collector publishes its later turns.
func TestImportedCodexSessionBeforeDiscoveryFloorKeepsUpdating(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	physicalTemp := func() string {
		path, err := filepath.EvalSymlinks(t.TempDir())
		must(t, err)
		return path
	}
	home, userHome, project := physicalTemp(), physicalTemp(), physicalTemp()
	must(t, os.Chmod(home, 0o700))
	source := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(source, 0o700))
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{source}}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	db, err := sql.Open("sqlite", filepath.Join(source, "state_5.sqlite"))
	must(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT)")
	must(t, err)
	// Started an hour before discovery was enabled.
	created := at.Add(-time.Hour)
	id := "00000000-0000-0000-0000-000000000001"
	path := filepath.Join(source, "sessions", "rollout-2026-10-02T11-00-00-"+id+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	meta, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": 0, "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"id": id, "timestamp": created.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex-tui", "cli_version": "0.159.3", "history_mode": "paginated"}})
	must(t, err)
	task, err := json.Marshal(map[string]any{"type": "event_msg", "ordinal": 1, "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": created.Format(time.RFC3339Nano)}})
	must(t, err)
	prompt := `{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt before setup"}]}}`
	must(t, os.WriteFile(path, []byte(string(meta)+"\n"+string(task)+"\n"+prompt+"\n"), 0o600))
	must(t, os.Chtimes(path, created.Add(time.Minute), created.Add(time.Minute)))
	_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", id, path)
	must(t, err)

	remote := storagetest.NewMemoryStore()
	now := at.Add(2 * time.Minute)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.Now = func() time.Time { return now }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	pass := func(stage string) {
		t.Helper()
		for range 2 {
			if result, err := runOnePass(env, true); err != nil || len(result.Errors) > 0 {
				t.Fatalf("%s: %+v %v", stage, result, err)
			}
		}
	}
	pass("before import")
	if regs, err := state.OpenReadOnly(home).LoadRegistrations(); err != nil || len(regs) != 0 {
		t.Fatalf("discovery admitted a session that started before it was enabled: %+v %v", regs, err)
	}
	health, found, err := discovery.ReadHealth(home)
	must(t, err)
	if !found || health.Outcomes["start_not_authorized"] == 0 {
		t.Fatalf("discovery did not refuse the start: %+v", health)
	}

	var out bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--since", "7d"}, strings.NewReader(""), &out, &out, env); code != 0 {
		t.Fatalf("backfill exit %d\n%s", code, &out)
	}
	pass("after import")
	reg := theRegistration(t, home)
	if reg.Origin != archive.SessionOriginImport || reg.NativeSessionID != id {
		t.Fatalf("registration %+v", reg)
	}
	if !bucketContains(t, remote, "first prompt before setup") {
		t.Fatal("imported session was not uploaded")
	}

	const marker = "second prompt after setup"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = fmt.Fprintf(file, `{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`+"\n", marker)
	must(t, err)
	must(t, file.Close())
	now = now.Add(10 * time.Minute)
	must(t, os.Chtimes(path, now.Add(-time.Minute), now.Add(-time.Minute)))
	pass("after append")
	if !bucketContains(t, remote, marker) {
		t.Fatal("the imported session's later turn never reached the archive")
	}
	if after := theRegistration(t, home); after.ArchiveSessionID != reg.ArchiveSessionID || after.Origin != archive.SessionOriginImport {
		t.Fatalf("identity changed: %+v", after)
	}
	health, _, err = discovery.ReadHealth(home)
	must(t, err)
	if health.Outcomes["admission_retry"] != 0 {
		t.Fatalf("discovery kept retrying the imported session: %+v", health.Outcomes)
	}
}
