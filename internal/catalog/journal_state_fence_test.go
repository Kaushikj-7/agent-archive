package catalog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

func TestRecoveredJournalSaveAndRemovalRequireExactCompletion(t *testing.T) {
	store, _, localStore, pending, guard, _ := journalFixture(t)
	j := *pending.Catalog.Recovery
	ctx, settled, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || settled {
		t.Fatal(settled, err)
	}
	defer store.EndPublicationAttempt(j.MutationID)
	path := filepath.Join(localStore.Home(), "pending", j.SessionID+".json")
	for _, mutate := range []func(*state.PendingPublication){
		func(p *state.PendingPublication) { p.ReadyAt = time.Now() },
		func(p *state.PendingPublication) { p.RequestToken = "new-request" },
		func(p *state.PendingPublication) { p.SkillEvidence = "none" },
		func(p *state.PendingPublication) { p.Catalog = nil },
		func(p *state.PendingPublication) { p.Bundle.ProjectID = "different" },
	} {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		next := pending
		mutate(&next)
		if err = localStore.SavePending(j.SessionID, next); !errors.Is(err, state.ErrCatalogJournalFrozen) {
			t.Fatal("frozen overwrite accepted", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("failed save changed journal", err)
		}
	}
	if err = localStore.RemovePending(j.SessionID); !errors.Is(err, state.ErrCatalogJournalFrozen) {
		t.Fatal("generic unlink accepted", err)
	}
	if err = localStore.RemoveCatalogPending(j.SessionID, j, guard); err == nil {
		t.Fatal("missing tombstone authorized unlink")
	}
	pending.Attempted = true
	if err = localStore.SavePending(j.SessionID, pending); err != nil {
		t.Fatal("normalized bookkeeping refused", err)
	}
	if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.Publication(j.MutationID, pending.MetadataKey, j.ExpectedRevision).Put(ctx, pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.CompletePublication(ctx, j.MutationID, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if err = guard.RecordJournalRemoval(j); err != nil {
		t.Fatal(err)
	}
	if err = localStore.RemoveCatalogPending(j.SessionID, j, guard); err != nil {
		t.Fatal("exact completion refused", err)
	}
	proof, err := guard.ProveJournalRemoval(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AcknowledgeJournalRemoval(t.Context(), proof, guard); err != nil {
		t.Fatal(err)
	}
	if err = guard.RemoveJournalRemoval(j); err != nil {
		t.Fatal(err)
	}
}
