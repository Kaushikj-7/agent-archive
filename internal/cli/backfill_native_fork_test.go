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
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestBackfillNativeForkRetainsIndependentIdentityAndOwnEvidence(t *testing.T) {
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
	own := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"root_turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic fork own prompt"}]}}`+"\n", fork, ancestor, at.Add(-time.Second).Format(time.RFC3339Nano))
	must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+fork+".jsonl"), append(append(header, '\n'), []byte(inherited+own)...), 0600))
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
	must(t, config.Save(home, cfg))
	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	beforePreview := snapshotTree(t, home)
	var preview, previewErr bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run", "--harness", "codex", "--include-temp", "--project", project}, nil, &preview, &previewErr, env); code != 0 {
		t.Fatal(code, previewErr.String())
	}
	if snapshotTree(t, home) != beforePreview {
		t.Fatal("fork preview wrote protection or state")
	}
	declinedEnv := env
	declinedEnv.IsTerminal = func(any) bool { return true }
	var declined, declinedErr bytes.Buffer
	if code := Run([]string{"backfill", "--background", "--harness", "codex", "--include-temp", "--project", project}, strings.NewReader("n\n"), &declined, &declinedErr, declinedEnv); code != 0 {
		t.Fatal("declined fork import failed", code, declined.String(), declinedErr.String())
	}
	if snapshotTree(t, home) != beforePreview || mustLoadConfig(t, home).CodexHistoryProtection {
		t.Fatal("declined fork import wrote state or writer protection")
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
		t.Fatal("fork import failed", code, out.String(), errOut.String())
	}
	local, e := state.Open(home)
	must(t, e)
	regs, e := local.LoadRegistrations()
	must(t, e)
	if len(regs) != 1 {
		t.Fatal("fork identity not imported", regs, out.String())
	}
	reg := regs[0]
	protected := mustLoadConfig(t, home)
	if !protected.CodexHistoryProtection {
		t.Fatal("imported fork binding lacks permanent history protection")
	}
	// Saving an older setup snapshot cannot remove an established fence.
	must(t, config.Save(home, cfg))
	if !mustLoadConfig(t, home).CodexHistoryProtection {
		t.Fatal("older configuration snapshot cleared history protection")
	}
	rawConfig, err := os.ReadFile(filepath.Join(home, "config.json"))
	must(t, err)
	var oldWriter struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(rawConfig, &oldWriter) == nil {
		t.Fatal("old scalar writer accepted imported fork configuration")
	}
	if reg.NativeChild || reg.NativeSessionID != fork || reg.ParentSessionID != "" || reg.ParentNativeSessionID != "" || reg.NativeRootSessionID != "" || reg.CodexBinding == nil || reg.CodexBinding.OwnStart == nil || *reg.CodexBinding.OwnStart != 3 {
		t.Fatal("fork became child or invented a lineage root", reg)
	}
	for range 8 {
		_, e := runOnePass(env, true)
		must(t, e)
	}
	objects, e := cloud.List(t.Context(), "")
	must(t, e)
	readback := false
	for _, object := range objects {
		if strings.HasSuffix(object.Key, "metadata.json") {
			metadata, e := reader.ReadMetadata(t.Context(), cloud, object.Key)
			must(t, e)
			bundle, e := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
			must(t, e)
			encoded, e := json.Marshal(bundle)
			must(t, e)
			if !strings.Contains(string(encoded), "Synthetic inherited fork context") || !strings.Contains(string(encoded), "Synthetic fork own prompt") || bundle.History == nil || bundle.History.OwnStart == nil || *bundle.History.OwnStart != 3 {
				t.Fatal("fork retained/own source lost", bundle.History)
			}
			readback = true
		}
	}
	if !readback {
		t.Fatal("fork not published/read back")
	}
	// Older fork proofs must revalidate admission once without creating activity.
	signature, found, e := local.LoadScanSignature(reg.ArchiveSessionID)
	must(t, e)
	if !found {
		t.Fatal("fork proof missing")
	}
	signature.SourceSetVersion = 2
	must(t, local.SaveScanSignature(reg.ArchiveSessionID, signature))
	loads := state.PublishedStateLoads()
	rechecked, e := runOnePass(env, true)
	must(t, e)
	if state.PublishedStateLoads() <= loads || len(rechecked.Errors) != 0 || len(rechecked.Published) != 0 {
		t.Fatal("old fork proof did not revalidate without publication", rechecked)
	}
	proof, found, e := local.LoadScanSignature(reg.ArchiveSessionID)
	must(t, e)
	if !found || proof.SourceSetVersion != 3 {
		t.Fatal("related fork proof not upgraded", proof)
	}
	loads = state.PublishedStateLoads()
	settled, e := runOnePass(env, true)
	must(t, e)
	if state.PublishedStateLoads() != loads || len(settled.Errors) != 0 || len(settled.Published) != 0 {
		t.Fatal("fork proof did not settle", settled)
	}
	path := filepath.Join(native, "sessions", "rollout-"+fork+".jsonl")
	original, e := os.ReadFile(path)
	must(t, e)
	changed := strings.Replace(string(original), `"turn_id":"`+fork+`"`, `"turn_id":"external-import-turn"`, 1)
	before := bucketSnapshot(t, cloud)
	must(t, os.WriteFile(path, []byte(changed), 0600))
	must(t, local.SaveRequest(reg.ArchiveSessionID, "synthetic-fork-source-change", at.Add(3*time.Hour)))
	rejected, e := runOnePass(env, true)
	must(t, e)
	if len(rejected.Published) != 0 || len(rejected.Errors) == 0 || bucketSnapshot(t, cloud) != before {
		t.Fatal("ongoing fork accepted invalid first own task", rejected)
	}
	if _, pending, e := local.LoadRequest(reg.ArchiveSessionID); e != nil || !pending {
		t.Fatal("invalid fork acknowledged request", pending, e)
	}
}
