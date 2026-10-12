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

func TestBackfillForkRejectsInheritedFirstOwnTask(t *testing.T) {
	for _, first := range []forkFirstTaskCase{forkTaskExternal, forkTaskPredates, forkTaskMissingTime, forkTaskMissingTask, forkTaskInvalidRoot} {
		t.Run(string(first), func(t *testing.T) {
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
			own := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic fork own prompt"}]}}`+"\n", fork, at.Format(time.RFC3339Nano))
			invalid := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"external-import-turn","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano))
			if first == forkTaskPredates {
				invalid = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n", fork, at.Add(-time.Second-time.Millisecond).Format(time.RFC3339Nano))
			}
			if first == forkTaskInvalidRoot {
				invalid = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"root_turn_id":%q,"started_at":%q}}`+"\n", fork, "invalid-root", at.Format(time.RFC3339Nano))
			}
			if first == forkTaskMissingTime {
				invalid = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`+"\n", fork)
			}
			if first == forkTaskMissingTask {
				invalid = ""
				own = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic fork prompt without own task"}]}}` + "\n"
			}
			must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+fork+".jsonl"), append(append(header, '\n'), []byte(inherited+invalid+own)...), 0600))
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
			must(t, config.Save(home, cfg))
			cloud := storagetest.NewMemoryStore()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			beforePreview := snapshotTree(t, home)
			var preview, previewErr bytes.Buffer
			if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex", "--include-temp", "--project", project}, nil, &preview, &previewErr, env); code != 0 {
				t.Fatal(code, previewErr.String())
			}
			var plan struct {
				Diagnostics []backfill.DiagnosticSummary `json:"diagnostics"`
			}
			must(t, json.Unmarshal(preview.Bytes(), &plan))
			want := backfill.DetailOwnTaskTurnIDInvalid
			if first == forkTaskInvalidRoot {
				want = backfill.DetailOwnTaskRootTurnInvalid
			}
			if first == forkTaskMissingTime {
				want = backfill.DetailOwnTaskStartedAtMissing
			}
			if first == forkTaskMissingTask {
				want = backfill.DetailOwnTaskMissing
			}
			if first == forkTaskPredates {
				want = backfill.DetailOwnTaskBeforeCreation
			}
			if len(plan.Diagnostics) != 1 || plan.Diagnostics[0].Detail != want || plan.Diagnostics[0].Count != 1 {
				t.Fatalf("missing own-task refusal detail: %s", preview.String())
			}
			if snapshotTree(t, home) != beforePreview {
				t.Fatal("fork preview wrote protection or state")
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("fork import failed", code, out.String(), errOut.String())
			}
			local, e := state.Open(home)
			must(t, e)
			regs, e := local.LoadRegistrations()
			must(t, e)
			if len(regs) != 0 {
				t.Fatal("inherited first task admitted fork", regs)
			}
			if mustLoadConfig(t, home).CodexHistoryProtection {
				t.Fatal("declined fork import installed protection")
			}
		})
	}
}

type forkFirstTaskCase string

const (
	forkTaskInvalidRoot forkFirstTaskCase = "invalid_root"
	forkTaskExternal    forkFirstTaskCase = "external"
	forkTaskPredates    forkFirstTaskCase = "predates"
	forkTaskMissingTime forkFirstTaskCase = "missing_time"
	forkTaskMissingTask forkFirstTaskCase = "missing_task"
)
