package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestBackfillNativeMissingTaskResiduals(t *testing.T) {
	t.Parallel()
	const ownPrompt = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic own prompt without task"}]}}`
	for _, test := range []struct {
		name     string
		own      string
		valid    bool
		residual string
		text     string
	}{
		{name: "valid_own_task", valid: true},
		{name: "inherited_only", residual: "no_own_records_in_snapshot", text: "No own records in the validated source snapshot."},
		{name: "own_prompt", own: ownPrompt + "\n", residual: "own_records_without_task", text: "Own records remain without first own-task evidence; independent activity is unqualified."},
		{name: "own_control", own: "{\"type\":\"turn_context\",\"payload\":{\"model\":\"synthetic\"}}\n", residual: "own_records_without_task", text: "Own records remain without first own-task evidence; independent activity is unqualified."},
		{name: "unknown_own_record", own: "{\"type\":\"synthetic_future_record\",\"payload\":{}}\n", residual: "own_records_without_task", text: "Own records remain without first own-task evidence; independent activity is unqualified."},
		{name: "uncommitted_own_tail", own: ownPrompt},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			native := filepath.Join(userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			const fork = "11111111-1111-4111-8111-111111111111"
			const ancestor = "22222222-2222-4222-8222-222222222222"
			header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": fork, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev", "forked_from_id": ancestor, "forked_from_ordinal_exclusive": 3}})
			must(t, err)
			inherited := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic inherited prompt"}]}}`+"\n", ancestor, at.Add(-time.Hour).Format(time.RFC3339Nano))
			own := test.own
			if test.valid {
				own = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+ownPrompt+"\n", fork, at.Format(time.RFC3339Nano))
			}
			must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+fork+".jsonl"), append(append(header, '\n'), []byte(inherited+own)...), 0600))
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
			must(t, config.Save(home, cfg))
			cloud := storagetest.NewMemoryStore()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			beforeHome, beforeNative := snapshotTree(t, home), snapshotTree(t, native)
			var preview, previewErr bytes.Buffer
			args := []string{"backfill", "--dry-run", "--harness", "codex", "--include-temp", "--project", project}
			if code := Run(append(args, "--json"), nil, &preview, &previewErr, env); code != 0 {
				t.Fatal("preview failed", code, previewErr.String())
			}
			// Decode the additive field independently of the production result type,
			// so this regression also executes against the original implementation.
			var plan struct {
				Diagnostics []struct {
					Skip     backfill.SkipReason       `json:"skip"`
					Detail   backfill.DiagnosticDetail `json:"detail"`
					Count    int                       `json:"count"`
					Residual string                    `json:"residual"`
				} `json:"diagnostics"`
				Projects []struct {
					Sessions map[string]int `json:"sessions"`
				} `json:"projects"`
			}
			must(t, json.Unmarshal(preview.Bytes(), &plan))
			planned := 0
			for _, p := range plan.Projects {
				planned += p.Sessions["codex"]
			}
			if test.valid {
				if planned != 1 || len(plan.Diagnostics) != 0 {
					t.Fatal("supported own-task control was not eligible", preview.String())
				}
				t.Log("qualified supported native own-task positive control")
			} else {
				if planned != 0 || len(plan.Diagnostics) != 1 || plan.Diagnostics[0].Skip != backfill.SkipRelatedHistory || plan.Diagnostics[0].Detail != backfill.DetailOwnTaskMissing || plan.Diagnostics[0].Count != 1 {
					t.Fatal("fixture did not reach the existing missing-own-task decision", preview.String())
				}
				t.Log("qualified native missing-own-task primary decision")
				if plan.Diagnostics[0].Residual != test.residual {
					t.Errorf("native residual = %q, want %q", plan.Diagnostics[0].Residual, test.residual)
				}
			}
			var text, textErr bytes.Buffer
			if code := Run(args, nil, &text, &textErr, env); code != 0 {
				t.Fatal("text preview failed", code, textErr.String())
			}
			if test.text != "" && !strings.Contains(text.String(), test.text) {
				t.Errorf("text preview omitted snapshot residual: %s", text.String())
			}
			for _, output := range []string{preview.String(), text.String()} {
				for _, private := range []string{fork, ancestor, "Synthetic inherited prompt", "Synthetic own prompt without task", native} {
					if strings.Contains(output, private) {
						t.Fatal("preview exposed native identity, locator or transcript content")
					}
				}
			}
			if snapshotTree(t, home) != beforeHome || snapshotTree(t, native) != beforeNative || bucketSnapshot(t, cloud) != "" {
				t.Fatal("preview changed archive, native source or bucket")
			}
			t.Log("preview preserved archive, native source and bucket")
			if test.valid {
				return
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("refused import failed", code, errOut.String())
			}
			local, err := state.Open(home)
			must(t, err)
			registrations, err := local.LoadRegistrations()
			must(t, err)
			stages, next, err := local.ImportStages(t.Context(), "", 50)
			must(t, err)
			if len(registrations) != 0 || len(stages) != 0 || next != "" || mustLoadConfig(t, home).CodexHistoryProtection || bucketSnapshot(t, cloud) != "" {
				t.Fatal("missing own-task evidence acquired durable authority")
			}
			t.Log("refused native source has no stage, registration, protection or bucket objects")
		})
	}
}
