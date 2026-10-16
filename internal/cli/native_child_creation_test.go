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
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The command and collector must apply the same original-creation constraint as
// automatic discovery, including when the first UUID-shaped task was inherited.
func TestNativeChildCreationAcrossDiscoveryImportAndCapture(t *testing.T) {
	for _, initial := range []string{"discovery", "import"} {
		for _, boundary := range []int{-1, 0, 3} {
			for _, offset := range []time.Duration{-time.Hour, -time.Second - time.Millisecond, -time.Second, 0, time.Second} {
				t.Run(fmt.Sprintf("%s/boundary_%d/task_%s", initial, boundary, offset), func(t *testing.T) {
					canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
					home, userHome, project := canonical(), canonical(), canonical()
					native := filepath.Join(userHome, ".codex")
					must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
					at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
					const child = "11111111-1111-4111-8111-111111111111"
					const parent = "22222222-2222-4222-8222-222222222222"
					payload := map[string]any{"id": child, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev", "parent_thread_id": parent}
					if boundary >= 0 {
						payload["subagent_history_start_ordinal"] = boundary
					}
					header, e := json.Marshal(map[string]any{"type": "session_meta", "payload": payload})
					must(t, e)
					task := func(when time.Time) string {
						return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"root_turn_id":%q,"started_at":%q}}`+"\n", child, child, when.Format(time.RFC3339Nano))
					}
					write := func(first time.Time) {
						prefix := ""
						if boundary > 0 {
							prefix = task(at.Add(-time.Hour)) + "{\"type\":\"turn_context\",\"payload\":{\"model\":\"inherited\"}}\n"
						}
						body := prefix + task(first) + task(at.Add(time.Minute)) + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic child creation prompt"}]}}` + "\n"
						must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+child+".jsonl"), append(append([]byte(nil), header...), []byte("\n"+body)...), 0600))
					}
					write(at.Add(offset))
					cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Hour)}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{native}}}
					must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at.Add(-time.Hour)))
					if initial == "import" {
						cfg.Discovery.Enabled = false
					}
					must(t, config.Save(home, cfg))
					cloud := storagetest.NewMemoryStore()
					env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
					env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
					local, e := state.Open(home)
					must(t, e)
					valid := offset >= -time.Second
					if initial == "import" {
						var out, errOut bytes.Buffer
						if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
							t.Fatal("historical creation import", code, out.String(), errOut.String())
						}
					}
					for range 5 {
						_, e := runOnePass(env, true)
						must(t, e)
					}
					regs, e := local.LoadRegistrations()
					must(t, e)
					if valid && len(regs) != 1 || !valid && len(regs) != 0 {
						t.Fatalf("initial child creation constraint: valid=%v registrations=%+v", valid, regs)
					}
					if !valid {
						// A later native-looking task must not rescue the invalid first event.
						cfg = mustLoadConfig(t, home)
						cfg.Discovery.Enabled = false
						must(t, config.Save(home, cfg))
						var out, errOut bytes.Buffer
						code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env)
						if code != 0 {
							t.Fatal("import failed", code, out.String(), errOut.String())
						}
						regs, e = local.LoadRegistrations()
						must(t, e)
						if len(regs) != 0 {
							t.Fatal("copied task before child creation licensed historical registration", regs)
						}
						objects, e := cloud.List(t.Context(), "sessions/")
						must(t, e)
						if len(objects) != 0 {
							t.Fatal("invalid child published", objects)
						}
						return
					}
					// Ongoing capture cannot replace a valid first task with an inherited one,
					// even when a retained binding already records a valid native task.
					reg := regs[0]
					key, e := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
					must(t, e)
					metadata, e := reader.ReadMetadata(t.Context(), cloud, key)
					must(t, e)
					_, e = reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
					must(t, e)
					if initial == "discovery" && boundary == 0 && offset == 0 {
						signature, found, e := local.LoadScanSignature(reg.ArchiveSessionID)
						must(t, e)
						if !found {
							t.Fatal("settled child proof missing")
						}
						signature.SourceSetVersion = 2 // Previously accepted child validation.
						must(t, local.SaveScanSignature(reg.ArchiveSessionID, signature))
						loads := state.PublishedStateLoads()
						rechecked, e := runOnePass(env, true)
						must(t, e)
						if state.PublishedStateLoads() <= loads || len(rechecked.Errors) != 0 || len(rechecked.Published) != 0 {
							t.Fatal("old child proof bypassed new creation validation", rechecked)
						}
						proof, found, e := local.LoadScanSignature(reg.ArchiveSessionID)
						must(t, e)
						if !found || proof.SourceSetVersion != 3 {
							t.Fatal("new child proof was not recorded", proof)
						}
						loads = state.PublishedStateLoads()
						settled, e := runOnePass(env, true)
						must(t, e)
						// The next CLI invocation has a fresh Store: its cold summary
						// requires one closed validation, while native work stays skipped.
						if decoded := state.PublishedStateLoads() - loads; decoded != 1 || len(settled.Skipped) != 1 || len(settled.Errors) != 0 || len(settled.Published) != 0 {
							t.Fatalf("settled child cold validation decoded %d states: %+v", decoded, settled)
						}
					}
					before := bucketSnapshot(t, cloud)
					write(at.Add(-time.Hour))
					must(t, local.SaveRequest(reg.ArchiveSessionID, "synthetic-source-change", at.Add(2*time.Minute)))
					result, e := runOnePass(env, true)
					must(t, e)
					if len(result.Published) != 0 || len(result.Errors) == 0 || bucketSnapshot(t, cloud) != before {
						t.Fatal("ongoing capture accepted task before original creation", result)
					}
					if _, pending, e := local.LoadRequest(reg.ArchiveSessionID); e != nil || !pending {
						t.Fatal("invalid source acknowledged request", e)
					}
				})
			}
		}
	}
}
