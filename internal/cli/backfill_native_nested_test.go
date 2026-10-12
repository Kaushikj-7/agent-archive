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
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Both generations are still running: no task-ended or stop-hook event exists.
func TestBackfillNativeNestedChildrenUseNewBatchAndUndoKeepsOldParent(t *testing.T) {
	canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
	home, userHome, project := canonical(), canonical(), canonical()
	native := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const grandchild = "33333333-3333-4333-8333-333333333333"
	write := func(id, ancestor string, depth int) {
		meta := map[string]any{"id": id, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}
		if ancestor != "" {
			meta["parent_thread_id"] = ancestor
			meta["session_id"] = ancestor
			meta["subagent_history_start_ordinal"] = 0
			meta["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": ancestor, "depth": depth}}}
		}
		header, e := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
		must(t, e)
		body := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"root_turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic independent own prompt"}]}}`+"\n", id, parent, at.Format(time.RFC3339Nano))
		must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+id+".jsonl"), append(append(header, '\n'), []byte(body)...), 0600))
	}
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
	must(t, config.Save(home, cfg))
	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	importNow := func() {
		var out, errOut bytes.Buffer
		if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
			t.Fatal("actual import failed", code, out.String(), errOut.String())
		}
	}
	write(parent, "", 0)
	importNow()
	local, e := state.Open(home)
	must(t, e)
	regs, e := local.LoadRegistrations()
	must(t, e)
	if len(regs) != 1 {
		t.Fatal("old parent not imported", regs)
	}
	oldParent := regs[0]
	for range 6 {
		result, e := runOnePass(env, true)
		must(t, e)
		if len(result.Published) > 0 {
			break
		}
	}
	env.Now = func() time.Time { return at.Add(3 * time.Hour) }
	write(child, parent, 1)
	write(grandchild, child, 2)
	importNow()
	for range 8 {
		_, e := runOnePass(env, true)
		must(t, e)
	}
	regs, e = local.LoadRegistrations()
	must(t, e)
	byNative := map[string]archive.SessionRegistration{}
	for _, reg := range regs {
		byNative[reg.NativeSessionID] = reg
	}
	c, g := byNative[child], byNative[grandchild]
	if len(regs) != 3 || c.ParentSessionID != oldParent.ArchiveSessionID || g.ParentSessionID != c.ArchiveSessionID || c.ImportBatch.IsZero() || c.ImportBatch.Recorded() == oldParent.ImportBatch.Recorded() || c.ImportBatch.Recorded() != g.ImportBatch.Recorded() || !c.HookObservedAt.IsZero() || !g.HookObservedAt.IsZero() {
		t.Fatal("nested ownership/batch/provenance lost", regs)
	}
	for _, reg := range []archive.SessionRegistration{c, g} {
		evidence, e := readVerification(home, reg.ArchiveSessionID)
		must(t, e)
		if evidence.VerifiedAt.IsZero() {
			t.Fatal("running child not uploaded/read back", reg.NativeSessionID)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "undo", c.ImportBatch.Recorded(), "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal("nested undo failed", code, out.String(), errOut.String())
	}
	reopened, e := state.Open(home)
	must(t, e)
	regs, e = reopened.LoadRegistrations()
	must(t, e)
	if len(regs) != 1 || regs[0].ArchiveSessionID != oldParent.ArchiveSessionID || regs[0].ImportBatch.Recorded() != oldParent.ImportBatch.Recorded() {
		t.Fatal("new batch undo removed old parent", regs)
	}
	for _, id := range []string{child, grandchild} {
		removed, found, e := reopened.Removal("codex", id)
		must(t, e)
		if !found || removed.Reason != state.RemovalReasonUndo {
			t.Fatal("nested tombstone missing", id, removed)
		}
	}
	importNow()
	regs, e = reopened.LoadRegistrations()
	must(t, e)
	if len(regs) != 1 {
		t.Fatal("nested children resurrected", regs)
	}
}
