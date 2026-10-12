package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestHistoricalRevisedHistoryKeepsAllSourcesWithoutNativeCheckout(t *testing.T) {
	canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
	home, userHome := canonical(), canonical()
	project := filepath.Join(userHome, "vanished-project")
	nativeHome := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const thread = "11111111-1111-4111-8111-111111111111"
	const physical = "22222222-2222-4222-8222-222222222222"
	meta := func(base map[string]any, ordinal int) []byte {
		payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
		if base != nil {
			payload["history_base"] = base
			payload["cwd"] = "/gone/revision-checkout"
			payload["cli_version"] = "0.170.0"
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
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: nil}}
	must(t, config.Save(home, cfg))
	local, err := state.Open(home)
	must(t, err)

	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	mapping := writeHistoricalMapping(t, project)
	env.repoKey = func(string) string { t.Fatal("historical source invoked Git"); return "" }
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", mapping}, nil, &out, &errOut, env); code != 0 {
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
	cfg = mustLoadConfig(t, home)
	if len(cfg.Archive.Projects) != 0 || regs[0].HistoricalProject == nil || len(regs[0].HistoricalProject.SourceCwds) < 2 {
		t.Fatal("history lacks independent source ownership", regs[0])
	}
	var renameOut, renameErr bytes.Buffer
	if code := Run([]string{"historical-project", "rename", regs[0].ProjectID, "Renamed historical revision"}, nil, &renameOut, &renameErr, env); code != 0 {
		t.Fatalf("rename %d %s", code, renameErr.String())
	}
	for range 5 {
		result, err := runOnePass(env, true)
		must(t, err)
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatalf("rename retained revision: %+v", result)
			}
		}
	}
	renamed, err := reader.ReadMetadata(t.Context(), cloud, key)
	must(t, err)
	if renamed.ProjectName != "Renamed historical revision" || renamed.ProjectID != metadata.ProjectID || !renamed.CapturedAt.Equal(metadata.CapturedAt) || renamed.SourceBundle != metadata.SourceBundle || !reflect.DeepEqual(renamed.History, metadata.History) {
		t.Fatal("rename changed history ownership or failed to update label")
	}
	for range 2 {
		result, err := runOnePass(env, true)
		must(t, err)
		if len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("renamed retained history did not settle: %+v", result)
		}
	}
	signature, found, err := local.LoadScanSignature(regs[0].ArchiveSessionID)
	must(t, err)
	if !found || !signature.Frozen || signature.PublishedProjectName != renamed.ProjectName {
		t.Fatal("renamed history lacks a label-bound idle signature")
	}
	cfg = mustLoadConfig(t, home)
	loads := state.PublishedStateLoads()
	idle, err := collector.Run(t.Context(), local, cloud, collector.Options{Sources: env.agentRegistry(), Parsers: parsersFor(env), MachineID: cfg.MachineID, Now: env.Now, AcceptSession: cfg.AcceptSession, HistoricalProjectLabel: cfg.HistoricalProjectLabel, SkillEvidence: cfg.EffectiveSkillEvidence()})
	must(t, err)
	if len(idle.Errors) != 0 || len(idle.Published) != 0 || state.PublishedStateLoads() != loads {
		t.Fatal("settled renamed history decoded retained sources or republished")
	}
	metadata = renamed
	published, err := local.LoadPublishedState(regs[0].ArchiveSessionID)
	must(t, err)
	if _, err := verifyPublication(t.Context(), cfg, cloud, regs[0], published); err != nil {
		t.Fatal(err)
	}
	downgraded := metadata
	downgraded.History = nil
	downgraded.SchemaVersion = archive.MetadataSchemaVersion
	downgradedRaw, err := json.Marshal(downgraded)
	must(t, err)
	must(t, cloud.Put(t.Context(), key, downgradedRaw))
	if _, err := verifyPublication(t.Context(), cfg, cloud, regs[0], published); err == nil {
		t.Fatal("remote history omission verified")
	}
	exact, err := json.Marshal(metadata)
	must(t, err)
	must(t, cloud.Put(t.Context(), key, exact))
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: "/gone/revision-checkout", Included: false})
	must(t, config.Save(home, cfg))
	if cfg.AcceptSession(regs[0]) {
		t.Fatal("new dependency exclusion was lost after source deletion")
	}
	before := bucketSnapshot(t, cloud)
	result, err := runOnePass(env, true)
	must(t, err)
	if len(result.Published) != 0 || bucketSnapshot(t, cloud) != before {
		t.Fatal("excluded retained dependencies were republished")
	}

}
