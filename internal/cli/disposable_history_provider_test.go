package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

// Preserve the concrete provider's bounded/versioned/conditional capabilities.
// Only an acknowledgement after a successful real metadata PUT is lost.
type uncertainHistoryCommit struct {
	*providertest.DisposableS3
	key  string
	lost bool
}

func (s *uncertainHistoryCommit) Put(ctx context.Context, key string, body []byte) error {
	if err := s.DisposableS3.Put(ctx, key, body); err != nil {
		return err
	}
	if key == s.key && !s.lost {
		s.lost = true
		return errors.New("injected lost history metadata acknowledgement")
	}
	return nil
}

func TestDisposableProviderHistoryRecoversAfterNativeAndLocalLoss(t *testing.T) {
	// Check opt-in before preparing any fixtures.
	cloud := providertest.NewDisposableS3(t)
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

	uncertain := &uncertainHistoryCommit{DisposableS3: cloud}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return uncertain, nil }
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

	key, err := archive.MetadataObjectKey(regs[0].Harness.Name, regs[0].ArchiveSessionID)
	must(t, err)
	uncertain.key = key
	// A single pass must actually commit metadata before the injected loss.
	for range 4 {
		result, runErr := runOnePass(env, true)
		must(t, runErr)
		result.CloseStagedResults()
		if uncertain.lost {
			break
		}
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatal(issue)
			}
		}
	}
	if !uncertain.lost {
		t.Fatal("no real history metadata commit before acknowledgement loss")
	}
	pending, found, err := local.LoadPublicationPending(regs[0].ArchiveSessionID)
	must(t, err)
	if !found || pending.JournalVersion != 5 || pending.Commit == nil || pending.Commit.Version != 2 || len(pending.Sources) < 2 {
		t.Fatalf("lost acknowledgement lacks exact sourceSet5 history authority: found=%t journal=%d commit=%+v", found, pending.JournalVersion, pending.Commit)
	}
	exact := bytes.Clone(pending.MetadataBytes)
	proof := *pending.Commit
	raw, err = cloud.Get(t.Context(), key)
	must(t, err)
	if !bytes.Equal(raw, exact) {
		t.Fatal("lost acknowledgement preceded actual metadata commit")
	}
	local, err = state.Open(home)
	must(t, err)
	restarted, found, err := local.LoadPublicationPending(regs[0].ArchiveSessionID)
	must(t, err)
	if !found || restarted.Commit == nil || *restarted.Commit != proof || !bytes.Equal(restarted.MetadataBytes, exact) {
		t.Fatal("restart changed immutable history selection")
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	for range 4 {
		result, runErr := runOnePass(env, true)
		must(t, runErr)
		result.CloseStagedResults()
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatal(issue)
			}
		}
	}
	if found, err := local.HasPending(regs[0].ArchiveSessionID); err != nil || found {
		t.Fatal("verified history pending retained", found, err)
	}
	if _, found, err := local.LoadImportStage(t.Context(), regs[0]); err != nil || found {
		t.Fatal("verified history stage retained", found, err)
	}
	if _, found, err := local.LoadRequest(regs[0].ArchiveSessionID); err != nil || found {
		t.Fatal("covered history request retained", found, err)
	}
	raw, err = cloud.Get(t.Context(), key)
	must(t, err)
	if !bytes.Equal(raw, exact) {
		t.Fatal("exact replay replaced selected history metadata")
	}
	// Remove every local publication, stage and registration. The independent
	// provider credential must recover the complete selected history alone.
	must(t, os.RemoveAll(home))
	peer := cloud.IndependentOwner(t)
	metadata, err := reader.ReadMetadata(t.Context(), peer, key)
	must(t, err)
	if metadata.History == nil || len(metadata.History.Preserved) == 0 {
		t.Fatal("provider lost preserved history")
	}
	active, err := reader.LoadSource(t.Context(), peer, metadata, reader.Limits{})
	must(t, err)
	encoded, err := json.Marshal(active)
	must(t, err)
	if !bytes.Contains(encoded, []byte("synthetic current-only")) || bytes.Contains(encoded, []byte("synthetic outgoing-only")) {
		t.Fatal("selected source boundary changed")
	}
	recovered := false
	for _, revision := range metadata.History.Preserved {
		prior, readErr := reader.LoadRevision(t.Context(), peer, metadata, revision.RevisionID, reader.Limits{})
		must(t, readErr)
		encoded, err = json.Marshal(prior)
		must(t, err)
		recovered = recovered || bytes.Contains(encoded, []byte("synthetic outgoing-only"))
	}
	if !recovered || !reflect.DeepEqual(metadata.SourceBundle, pending.SourceReference()) {
		t.Fatal("independent retained history lost exact active/preserved sources")
	}
}
