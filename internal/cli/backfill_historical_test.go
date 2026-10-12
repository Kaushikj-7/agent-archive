package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestHistoricalImportSurvivesNativeDeletionWithoutLiveProject(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	cfg := config.Config{MachineID: "historical-machine", Storage: credentialsTestConfig(), RetentionDays: 90, Harnesses: []string{"claude"}, Archive: archive.Config{Enabled: true}}
	must(t, config.Save(home, cfg))
	const projectID = "11111111111111111111111111111111"
	cwd := filepath.Join(userHome, "vanished-service")
	source := filepath.Join(userHome, ".claude", "projects", "synthetic", "historical-session.jsonl")
	must(t, os.MkdirAll(filepath.Dir(source), 0700))
	must(t, os.WriteFile(source, []byte(fmt.Sprintf(`{"type":"user","sessionId":"historical-session","cwd":%q,"timestamp":"2026-10-11T12:00:00Z","message":{"role":"user","content":"Review synthetic retired service"}}`+"\n", cwd)), 0600))
	mapping := filepath.Join(t.TempDir(), "mappings.json")
	must(t, os.WriteFile(mapping, []byte(fmt.Sprintf(`{"schema_version":1,"historical_projects":[{"id":%q,"label":"Retired service"}],"mappings":[{"cwd":%q,"historical_project_id":%q}]}`, projectID, cwd, projectID)), 0600))
	bucket := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	env.repoKey = func(string) string { t.Fatal("historical import invoked Git"); return "" }
	run := func(args ...string) {
		t.Helper()
		var out, errout bytes.Buffer
		if code := Run(args, nil, &out, &errout, env); code != 0 {
			t.Fatalf("%v: exit %d %s %s", args, code, out.String(), errout.String())
		}
	}
	before := snapshotTree(t, home)
	run("backfill", "--dry-run", "--mapping-file", mapping)
	if snapshotTree(t, home) != before {
		t.Fatal("dry-run wrote historical permission")
	}
	run("backfill", "--yes", "--background", "--mapping-file", mapping)
	cfg, _, err := config.Load(home)
	must(t, err)
	if len(cfg.Archive.Projects) != 0 || len(cfg.HistoricalProjects) != 1 || !cfg.HistoricalProjectProtection {
		t.Fatalf("historical authority leaked into live config: %+v", cfg)
	}
	must(t, os.RemoveAll(filepath.Join(userHome, ".claude")))
	result, err := runOnePass(env, true)
	must(t, err)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("publication after deletion: %+v", result)
	}
	store, err := state.Open(home)
	must(t, err)
	regs, err := store.LoadRegistrations()
	must(t, err)
	if len(regs) != 1 || regs[0].ProjectID != projectID || regs[0].ProjectRoot != "" || regs[0].HistoricalProject == nil {
		t.Fatalf("ownership %+v", regs)
	}
	published, err := store.LoadPublishedState(regs[0].ArchiveSessionID)
	must(t, err)
	var prior archive.Metadata
	must(t, json.Unmarshal(published.Metadata(), &prior))
	if prior.ProjectName != "Retired service" {
		t.Fatalf("label %q", prior.ProjectName)
	}
	run("historical-project", "rename", projectID, "Renamed service")
	result, err = runOnePass(env, true)
	must(t, err)
	if len(result.Errors) > 0 {
		t.Fatalf("rename %+v", result)
	}
	published, err = store.LoadPublishedState(regs[0].ArchiveSessionID)
	must(t, err)
	var renamed archive.Metadata
	must(t, json.Unmarshal(published.Metadata(), &renamed))
	if renamed.ProjectName != "Renamed service" || !renamed.CapturedAt.Equal(prior.CapturedAt) {
		t.Fatalf("rename changed evidence: %s %v %v", renamed.ProjectName, renamed.CapturedAt, prior.CapturedAt)
	}
	run("historical-project", "disable", projectID)
	cfg, _, err = config.Load(home)
	must(t, err)
	if cfg.AcceptSession(regs[0]) {
		t.Fatal("disabled import still accepted")
	}
	run("historical-project", "enable", projectID)
	batches, err := backfill.LoadBatches(home)
	must(t, err)
	if len(batches) != 1 {
		t.Fatal(batches)
	}
	firstBatch := batches[0]
	secondSource := filepath.Join(filepath.Dir(source), "independent-session.jsonl")
	must(t, os.MkdirAll(filepath.Dir(secondSource), 0700))
	must(t, os.WriteFile(secondSource, []byte(fmt.Sprintf(`{"type":"user","sessionId":"independent-session","cwd":%q,"timestamp":"2026-10-11T13:00:00Z","message":{"role":"user","content":"Independent historical import"}}`+"\n", cwd)), 0600))
	run("backfill", "--yes", "--mapping-file", mapping)
	run("backfill", "undo", firstBatch.ID, "--yes")
	remaining, err := store.LoadRegistrations()
	must(t, err)
	if len(remaining) != 1 || remaining[0].NativeSessionID != "independent-session" {
		t.Fatal("undo claimed independent empty-root owner", remaining)
	}
	if len(sessionKeys(t, bucket, regs[0])) != 0 {
		t.Fatal("undo leaked historical source objects")
	}
	cfg, _, err = config.Load(home)
	must(t, err)
	if len(cfg.HistoricalProjects) != 1 || !cfg.AcceptSession(remaining[0]) {
		t.Fatal("undo changed explicit shared historical policy")
	}
	run("backfill", "undo", "--yes")
	regs, err = store.LoadRegistrations()
	must(t, err)
	if len(regs) != 0 {
		t.Fatalf("undo retained registrations %+v", regs)
	}
}

func writeHistoricalMapping(t *testing.T, cwd string) string {
	t.Helper()
	mapping := filepath.Join(t.TempDir(), "mappings.json")
	must(t, os.WriteFile(mapping, []byte(fmt.Sprintf(`{"schema_version":1,"historical_projects":[{"id":"11111111111111111111111111111111","label":"Retired service"}],"mappings":[{"cwd":%q,"historical_project_id":"11111111111111111111111111111111"}]}`, cwd)), 0600))
	return mapping
}
