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
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The retained local manifest/receipt cannot authorize a downgraded remote
// pointer, even when its machine, active source and publication age still match.
func TestHistoryPresenceDowngradeRefusesVerificationAndPrivacyReceipt(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	remote := storagetest.NewMemoryStore()
	cfg, store := publishVerificationHistory(t, home, remote, at)
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatal("registration fixture", err)
	}
	reg := regs[0]
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := reader.ReadMetadata(t.Context(), remote, key)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found := published.LastPublished()
	if !found {
		t.Fatal("publication fixture")
	}
	if expected.History == nil || len(expected.History.Preserved) == 0 || bundle.History == nil || bundle.SchemaVersion != archive.HistorySourceSchemaVersion {
		t.Fatal("real history publication fixture lost its retained source set")
	}
	if _, err := verifyPublication(t.Context(), cfg, remote, reg, published); err != nil {
		t.Fatal("unchanged real history publication", err)
	}
	raw := published.Metadata()
	remoteRaw, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(raw, remoteRaw) {
		t.Fatal("local history authority differs from exact remote metadata", err)
	}
	downgraded := expected
	downgraded.SchemaVersion = archive.MetadataSchemaVersion
	downgraded.History = nil
	set, err := expected.SourceSetDigest()
	if err != nil {
		t.Fatal(err)
	}
	receipt := verificationEvidence{ConfigurationID: sessionVerificationConfigurationID(cfg, reg), PublishedAt: publishedAt, SourceSHA256: expected.SourceBundle.SHA256, SourceSetDigest: set, MetadataDigest: storage.SHA256Hex(raw), Outcome: verificationOutcomeVerified, VerifiedAt: publishedAt.Add(time.Minute)}
	if err := local.Write(verificationPath(home, reg.ArchiveSessionID), receipt); err != nil {
		t.Fatal(err)
	}
	if !privacyPublicationVerified(home, cfg, store, reg, expected) {
		t.Fatal("unchanged history receipt refused")
	}
	if privacyPublicationVerified(home, cfg, store, reg, downgraded) {
		t.Fatal("remote history removal accepted stale receipt")
	}
	downgradedRaw, err := json.Marshal(downgraded)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(t.Context(), key, downgradedRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyPublication(context.Background(), cfg, remote, reg, published); !errors.Is(err, errVerificationMismatch) {
		t.Fatalf("remote history removal must mismatch: %v", err)
	}
}

// Publish through native capture and the selecting source-set writer. The two
// physical rollouts exercise both the current and preserved source references.
func publishVerificationHistory(t *testing.T, home string, remote storage.ObjectStore, at time.Time) (config.Config, *state.Store) {
	t.Helper()
	userHome, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	project := filepath.Join(userHome, "synthetic-project")
	nativeHome := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
	const thread = "11111111-1111-4111-8111-111111111111"
	const physical = "22222222-2222-4222-8222-222222222222"
	meta := func(base map[string]any, ordinal int) []byte {
		payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
		if base != nil {
			payload["history_base"] = base
		}
		raw, marshalErr := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "timestamp": at.Format(time.RFC3339Nano), "payload": payload})
		must(t, marshalErr)
		return append(raw, '\n')
	}
	task := []byte(fmt.Sprintf(`{"type":"event_msg","ordinal":1,"timestamp":%q,"payload":{"type":"task_started","turn_id":"synthetic-task","root_turn_id":"synthetic-task","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)))
	seed := filepath.Join(nativeHome, "sessions", "rollout-2026-09-28T12-00-00-"+thread+".jsonl")
	prefix := append(meta(nil, 0), task...)
	must(t, os.WriteFile(seed, append(prefix, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic preserved"}]}}`+"\n")...), 0600))
	current := filepath.Join(nativeHome, "sessions", "rollout-2026-09-28T12-00-00-"+physical+".jsonl")
	raw := meta(map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
	must(t, os.WriteFile(current, append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic current"}]}}`+"\n")...), 0600))
	db, err := sql.Open("sqlite", filepath.Join(nativeHome, "state_5.sqlite"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
	must(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, current)
	must(t, err)
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}}
	must(t, config.Save(home, cfg))
	store, err := state.Open(home)
	must(t, err)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	env.repoKey = func(string) string { t.Fatal("historical source invoked Git"); return "" }
	mapping := writeHistoricalMapping(t, project)
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", mapping}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("history admission %d: %s %s", code, errOut.String(), out.String())
	}
	must(t, db.Close())
	must(t, os.RemoveAll(nativeHome))
	for range 4 {
		result, runErr := runOnePass(env, true)
		must(t, runErr)
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatal("history publication", issue)
			}
		}
	}
	return cfg, store
}
