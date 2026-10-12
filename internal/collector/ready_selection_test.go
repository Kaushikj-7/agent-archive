package collector

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRunReadySelectionRetryKeepsExactProducerAuthority(t *testing.T) {
	local := newTestStore(t)
	cfg, _, err := config.Load(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	if err = config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
	if err = local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) == 0 || len(result.Published) != 0 {
		t.Fatal("failed metadata attempt was acknowledged")
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found || pending.JournalVersion != 2 || pending.Selection == nil || pending.Selection.Predecessor != "absent" {
		t.Fatalf("actual producer did not retain selecting journal: %+v %v", pending.Selection, err)
	}
	exact := bytes.Clone(pending.MetadataBytes)
	proof := *pending.Selection
	if err = local.RemovePending("session-1"); !errors.Is(err, state.ErrReadySelectionFrozen) {
		t.Fatalf("unacknowledged selecting work removable: %v", err)
	}
	// Reopen the actual store, retaining the failed publication bytes and proof.
	local, err = openTestStore(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err = local.LoadPending("session-1")
	if err != nil || !found || *pending.Selection != proof || !bytes.Equal(pending.MetadataBytes, exact) {
		t.Fatal("restart changed ready authority", err)
	}
	remote.failMetadata = false
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	raw, err := remote.Get(t.Context(), pending.MetadataKey)
	if err != nil || !bytes.Equal(raw, exact) {
		t.Fatal("retry changed exact selected metadata", err)
	}
	if _, found, err = local.LoadPending("session-1"); err != nil || found {
		t.Fatalf("acknowledged pending retained: %v %v", found, err)
	}
}

func TestReadySelectionUnknownPredecessorNeverBecomesAbsent(t *testing.T) {
	remote := storagetest.NewMemoryStore()
	scan := &sessionScan{ctx: context.Background(), remote: remote}
	p := state.PendingPublication{MetadataKey: "sessions/codex/synthetic/metadata.json", Selection: &state.ReadySelection{Predecessor: "unknown", MetadataSHA256: metadataSHA([]byte("exact"))}}
	selected, err := scan.checkReadySelectionRemote(p)
	if selected || !errors.Is(err, errHistoryMetadataConflict) {
		t.Fatalf("unknown missing predecessor became absent: %v %v", selected, err)
	}
	if err = remote.Put(t.Context(), p.MetadataKey, []byte("foreign")); err != nil {
		t.Fatal(err)
	}
	selected, err = scan.checkReadySelectionRemote(p)
	if selected || !errors.Is(err, errHistoryMetadataConflict) {
		t.Fatalf("foreign winner selected: %v %v", selected, err)
	}
	if err = remote.Put(t.Context(), p.MetadataKey, []byte("exact")); err != nil {
		t.Fatal(err)
	}
	selected, err = scan.checkReadySelectionRemote(p)
	if !selected || err != nil {
		t.Fatalf("exact readback not recognized: %v %v", selected, err)
	}
	if _, err = remote.Get(t.Context(), "source-not-written"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("preflight created source")
	}
}
