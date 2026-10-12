package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestBackfillStagedAdmissionCrashBackgroundRecoversDeletedNative(t *testing.T) {
	f, _ := newImportFixture(t)
	f.env.backfillCheckpoint = func(step string) error {
		if step == "staged" {
			return errors.New("pre-admission crash")
		}
		return nil
	}
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude"); code != 1 {
		t.Fatalf("crash exit %d", code)
	}
	batch, _ := loadBatch(t, f.data)
	if len(batch.StagedSessions) != 1 || len(batch.Sessions) != 0 {
		t.Fatalf("stage prematurely admitted %+v", batch)
	}
	if err := os.RemoveAll(filepath.Join(f.userHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	f.env.backfillCheckpoint = nil
	result, err := runOnePass(f.env, true)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("background recovery %+v %v", result, err)
	}
	st, err := state.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	reg, found, err := st.LoadRegistration(batch.StagedSessions[0])
	if err != nil || !found {
		t.Fatalf("not admitted %v", err)
	}
	pub, err := st.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil || !pub.Summary().Published {
		t.Fatalf("not verified %v", err)
	}
	if _, found, err := st.LoadImportStage(t.Context(), reg); err != nil || found {
		t.Fatalf("ack stage remains %v", err)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil || !cfg.ImportStagingProtection {
		t.Fatalf("missing compatibility fence %v", err)
	}
}

func TestBackfillDryRunNeverCreatesImportStaging(t *testing.T) {
	f, bucket := newImportFixture(t)
	before := snapshotTree(t, f.data)
	remote := bucketSnapshot(t, bucket)
	if _, errout, code := f.importRun(t, nil, false, "--dry-run"); code != 0 {
		t.Fatalf("dry run %d %s", code, errout)
	}
	if snapshotTree(t, f.data) != before || bucketSnapshot(t, bucket) != remote {
		t.Fatal("dry run wrote import evidence or configuration")
	}
}

func TestBackfillStagedCodexForkSurvivesNativeDeletion(t *testing.T) {
	canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
	home, userHome, project := canonical(), canonical(), canonical()
	native := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const fork = "11111111-1111-4111-8111-111111111111"
	const ancestor = "22222222-2222-4222-8222-222222222222"
	header, e := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": fork, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev", "forked_from_id": ancestor, "forked_from_ordinal_exclusive": 3}})
	must(t, e)
	inherited := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic inherited fork context"}]}}`+"\n", ancestor, at.Add(-time.Hour).Format(time.RFC3339Nano))
	own := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic fork own prompt"}]}}`+"\n", fork, at.Add(-time.Second).Format(time.RFC3339Nano))
	must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+fork+".jsonl"), append(append(header, '\n'), []byte(inherited+own)...), 0600))
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
	must(t, config.Save(home, cfg))
	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }

	var out, errout bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errout, env); code != 0 {
		t.Fatalf("admission %d %s", code, errout.String())
	}
	must(t, os.RemoveAll(filepath.Join(native, "sessions")))
	for range 4 {
		result, err := runOnePass(env, true)
		if err != nil || len(result.Errors) > 0 {
			t.Fatalf("deleted fork recovery %+v %v", result, err)
		}
	}
	objects, err := cloud.List(t.Context(), "")
	must(t, err)
	found := false
	for _, object := range objects {
		if strings.HasSuffix(object.Key, "metadata.json") {
			metadata, err := reader.ReadMetadata(t.Context(), cloud, object.Key)
			must(t, err)
			bundle, err := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
			must(t, err)
			if bundle.History == nil || bundle.History.OwnStart == nil || *bundle.History.OwnStart != 3 {
				t.Fatal("frozen fork boundary lost")
			}
			encoded, err := json.Marshal(bundle)
			must(t, err)
			if !bytes.Contains(encoded, []byte("Synthetic inherited fork context")) || !bytes.Contains(encoded, []byte("Synthetic fork own prompt")) {
				t.Fatal("staged history lost selected evidence")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("deleted native history never verified")
	}
}

// This opt-in check runs an actual predecessor build, rather than approximating
// downgrade behavior with the current decoder. Release validation provides it.
func TestStagedEvidenceRefusesRetentionOnlyWriterBeforeMutation(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_RETENTION_CLI")
	if binary == "" {
		t.Skip("set AGENT_ARCHIVE_RETENTION_CLI to a retention-only schema8 CLI")
	}
	f, _ := newImportFixture(t)
	f.env.backfillCheckpoint = func(step string) error {
		if step == "staged" {
			return errors.New("pre-admission crash")
		}
		return nil
	}
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude"); code != 1 {
		t.Fatalf("crash exit %d", code)
	}
	if err := os.RemoveAll(filepath.Join(f.userHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, f.data)
	for _, args := range [][]string{{"status"}, {"sync"}, {"_collect"}, {"setup", "--refresh"}} {
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Env = append(os.Environ(), "HOME="+f.userHome, "AGENT_ARCHIVE_HOME="+f.data)
		out, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(out, []byte("newer")) {
			t.Fatalf("retention-only %v did not refuse writer9: %v %s", args, err, out)
		}
		if snapshotTree(t, f.data) != before {
			t.Fatalf("retention-only %v mutated staged evidence or ownership", args)
		}
	}
}

func TestSetupRollbackPreservesImportStagingCapability(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("original", "us-east-1", "profile", true, false, false, t.TempDir()))
	cfg := mustLoadConfig(t, home)
	cfg.ImportStagingProtection = true
	must(t, config.Save(home, cfg))
	cfg = mustLoadConfig(t, home)
	sched := recordLaunchd(t, &env, "loaded")
	sched.beforeLoad = func(scheduler.Ref) error { return errors.New("synthetic restart failure after config apply") }
	next := cfg
	// A resumable settings draft may predate the staging capability.
	next.ImportStagingProtection = false
	next.RetentionDays = 30
	next.NoSkills = !cfg.NoSkills
	if err := applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env); err == nil || !strings.Contains(err.Error(), "synthetic restart failure") {
		t.Fatalf("rollback injection did not run: %v", err)
	}
	loaded := mustLoadConfig(t, home)
	if loaded.SchemaVersion != 9 || !loaded.ImportStagingProtection || loaded.RetentionDays != cfg.RetentionDays {
		t.Fatalf("setup rollback weakened staging ownership fence %+v", loaded)
	}
}

func TestBackfillStagesPreservedRevisionBeforeNativeDeletion(t *testing.T) {
	canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
	home, userHome, project := canonical(), canonical(), canonical()
	nativeHome := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const thread = "11111111-1111-4111-8111-111111111111"
	const physical = "22222222-2222-4222-8222-222222222222"
	meta := func(base map[string]any, ordinal int) []byte {
		payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
		if base != nil {
			payload["history_base"] = base
		}
		raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "timestamp": at.Format(time.RFC3339Nano), "payload": payload})
		must(t, err)
		return append(raw, '\n')
	}
	task := []byte(fmt.Sprintf(`{"type":"event_msg","ordinal":1,"timestamp":%q,"payload":{"type":"task_started","turn_id":"synthetic-task","root_turn_id":"synthetic-task","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)))
	seed := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+thread+".jsonl")
	prefix := append(meta(nil, 0), task...)
	must(t, os.WriteFile(seed, append(prefix, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic outgoing-only"}]}}`+"\n")...), 0600))
	current := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+physical+".jsonl")
	raw := meta(map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
	must(t, os.WriteFile(current, append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic current-only"}]}}`+"\n")...), 0600))
	db, err := sql.Open("sqlite", filepath.Join(nativeHome, "state_5.sqlite"))
	must(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
	must(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, current)
	must(t, err)
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	must(t, config.Save(home, cfg))
	local, err := state.Open(home)
	must(t, err)

	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("selected revision admission %d %s %s", code, errOut.String(), out.String())
	}
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 1 {
		t.Fatalf("unexpected owners: %+v", regs)
	}
	stage, found, err := local.LoadImportStage(t.Context(), regs[0])
	must(t, err)
	if !found || len(stage.Preserved) == 0 {
		t.Fatal("selected alternative not staged before admission")
	}
	must(t, db.Close())
	must(t, os.RemoveAll(nativeHome))
	for range 4 {
		result, err := runOnePass(env, true)
		must(t, err)
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatalf("selected revision recovery: %+v", result)
			}
		}
	}
	key, err := archive.MetadataObjectKey(regs[0].Harness.Name, regs[0].ArchiveSessionID)
	must(t, err)
	metadata, err := reader.ReadMetadata(t.Context(), cloud, key)
	must(t, err)
	active, err := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
	must(t, err)
	encoded, err := json.Marshal(active)
	must(t, err)
	if !bytes.Contains(encoded, []byte("synthetic current-only")) || bytes.Contains(encoded, []byte("synthetic outgoing-only")) || metadata.History == nil || len(metadata.History.Preserved) == 0 {
		t.Fatal("selected source set lost active boundary or preserved alternative")
	}
	recovered := false
	for _, revision := range metadata.History.Preserved {
		prior, err := reader.LoadRevision(t.Context(), cloud, metadata, revision.RevisionID, reader.Limits{})
		must(t, err)
		raw, err := json.Marshal(prior)
		must(t, err)
		recovered = recovered || bytes.Contains(raw, []byte("synthetic outgoing-only"))
	}
	if !recovered {
		t.Fatal("staged preserved alternative unreadable without native files")
	}
	if _, found, err := local.LoadImportStage(t.Context(), regs[0]); err != nil || found {
		t.Fatalf("verified source set staging not released: %t %v", found, err)
	}
}

type incompleteBackfillJSONWriter struct{}

func (incompleteBackfillJSONWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBackfillDryRunJSONShortWriteFailsWithoutMutation(t *testing.T) {
	f, bucket := newImportFixture(t)
	before, remote := snapshotTree(t, f.data), bucketSnapshot(t, bucket)
	var stderr bytes.Buffer
	code := Run([]string{"backfill", "--dry-run", "--json"}, nil, incompleteBackfillJSONWriter{}, &stderr, f.env)
	if code != 1 || !strings.Contains(stderr.String(), io.ErrShortWrite.Error()) {
		t.Fatalf("incomplete JSON reported success: %d %s", code, stderr.String())
	}
	if snapshotTree(t, f.data) != before || bucketSnapshot(t, bucket) != remote {
		t.Fatal("failed JSON dry run changed local or remote state")
	}
}
