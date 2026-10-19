package collector

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestSelectedPassPreservesUnselectedStagedRelease(t *testing.T) {
	store, pending, opts, remote := stagedCommittedOwnerFixture(t)
	id := pending.Bundle.ArchiveSessionID
	// Remote source+metadata already exist, just as when the provider committed
	// publication but the process lost its acknowledgement before local release.
	opts.SelectSession = func(archive.SessionRegistration) bool { return false }
	paths := []string{
		filepath.Join(store.Home(), "pending", id+".json"),
		filepath.Join(store.Home(), "sessions", id, "import-stage", "stage.json"),
	}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		var err error
		before[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := Run(t.Context(), store, remote, opts)
	defer result.CloseStagedResults()
	if err != nil {
		t.Fatal(err)
	}
	request, found, err := store.LoadRequest(id)
	if err != nil || !found || request.Token != pending.RequestToken {
		t.Fatalf("unselected release changed its covered request: found=%v err=%v", found, err)
	}
	for i, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before[i], after) {
			t.Fatalf("unselected release changed retained evidence: %v", err)
		}
	}
	if len(result.Published) != 0 || len(result.Errors) != 0 {
		t.Fatal("unselected owner entered publication or diagnostics", result.Published, result.Errors)
	}
	objects, err := remote.List(t.Context(), "")
	if err != nil || len(objects) != 2 {
		t.Fatal("unselected release changed remote membership", len(objects), err)
	}
	for key, want := range map[string][]byte{pending.SourceKey: pending.SourceBytes, pending.MetadataKey: pending.MetadataBytes} {
		got, err := remote.Get(t.Context(), key)
		if err != nil || !bytes.Equal(want, got) {
			t.Fatal("unselected release changed exact remote object", err)
		}
	}
}

type selectedCatalogRemovalStore struct {
	*storagetest.MemoryStore
	acknowledged int
}

func (s *selectedCatalogRemovalStore) BeginJournalPublication(ctx context.Context, _ string, _ []byte, _ local.CatalogJournal, _ *local.CollectorGuard) (context.Context, bool, error) {
	return ctx, false, nil
}

func (s *selectedCatalogRemovalStore) AcknowledgeJournalRemoval(_ context.Context, proof *local.JournalRemoval, guard *local.CollectorGuard) error {
	if _, err := proof.Journal(guard); err != nil {
		return err
	}
	s.acknowledged++
	return nil
}

func TestSelectedCatalogRecoveryPreservesUnselectedRemoval(t *testing.T) {
	store, _, opts, _ := stagedCommittedOwnerFixture(t)
	origin, err := opts.CollectorGuard.Origin()
	if err != nil {
		t.Fatal(err)
	}
	journal := local.CatalogJournal{Owner: strings.Repeat("a", 32), Origin: origin, Destination: "synthetic-destination", SessionID: "unrelated-completed", MutationID: "synthetic-mutation", SHA256: strings.Repeat("b", 64)}
	if err := opts.CollectorGuard.RecordJournalRemoval(journal); err != nil {
		t.Fatal(err)
	}
	remote := &selectedCatalogRemovalStore{MemoryStore: storagetest.NewMemoryStore()}
	opts.CatalogDestination = journal.Destination
	opts.SelectSession = func(archive.SessionRegistration) bool { return false }
	if err := recoverCatalogRemovals(t.Context(), store, remote, opts); err != nil {
		t.Fatal(err)
	}
	remaining, err := opts.CollectorGuard.JournalRemovals()
	if err != nil || len(remaining) != 1 || remaining[0] != journal || remote.acknowledged != 0 {
		t.Fatal("unselected completed owner was settled", remaining, remote.acknowledged, err)
	}
	// Ordinary recovery still consumes the exact positive unlink proof.
	opts.SelectSession = nil
	if err := recoverCatalogRemovals(t.Context(), store, remote, opts); err != nil {
		t.Fatal(err)
	}
	remaining, err = opts.CollectorGuard.JournalRemovals()
	if err != nil || len(remaining) != 0 || remote.acknowledged != 1 {
		t.Fatal("ordinary exact recovery did not settle", remaining, remote.acknowledged, err)
	}
}
