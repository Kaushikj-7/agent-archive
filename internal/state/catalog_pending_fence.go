package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrCatalogJournalFrozen refuses replacement or removal of exact admitted work.
// Policy conflicts require an explicit supported owner disposition; broader
// evidence cannot be replayed merely to settle that owner.
var ErrCatalogJournalFrozen = errors.Join(ErrDurableStorageRecovery, errors.New("catalog journal is frozen; exact owner recovery or completion is required"))

func (s *Store) catalogPendingForFence(id string) (PendingPublication, bool, error) {
	var pending PendingPublication
	err := s.readBudgeted(s.pendingPath(id), &pending, true)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	return pending, err == nil, err
}

func recoveredCatalogJournal(p PendingPublication) bool {
	return p.Catalog != nil && p.Catalog.Recovery != nil
}

func (s *Store) checkCatalogPendingSave(ctx context.Context, id string, next PendingPublication) error {
	// Fence-owned decoded views must end after their exact comparisons.
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeScratch()
	old, found, err := scratch.catalogPendingForFence(id)
	if err != nil {
		return errors.Join(ErrCatalogJournalFrozen, err)
	}
	if !found || !recoveredCatalogJournal(old) {
		if recoveredCatalogJournal(next) {
			j := next.Catalog.Recovery
			digest, err := scratch.catalogJournalDigest(ctx, id, next)
			if j.Validate() != nil || j.SessionID != id || j.MutationID != next.Catalog.ID || j.ExpectedRevision != next.Catalog.ExpectedRevision || err != nil || digest != j.SHA256 {
				return errors.Join(ErrCatalogJournalFrozen, err)
			}
		}
		return nil
	}
	if !recoveredCatalogJournal(next) || *old.Catalog.Recovery != *next.Catalog.Recovery {
		return ErrCatalogJournalFrozen
	}
	before, err := scratch.catalogJournalDigest(ctx, id, old)
	if err != nil || before != old.Catalog.Recovery.SHA256 {
		return errors.Join(ErrCatalogJournalFrozen, err)
	}
	after, err := scratch.catalogJournalDigest(ctx, id, next)
	if err != nil || after != before {
		return errors.Join(ErrCatalogJournalFrozen, err)
	}
	return nil
}

// RemoveCatalogPending removes exact recovered work only after the originating
// held collector guard positively recorded its completion tombstone. It joins
// the guard operation through staged cleanup, unlink and directory fsync.
func (s *Store) RemoveCatalogPending(id string, j local.CatalogJournal, guard *local.CollectorGuard) error {
	if !safeFileComponent(id) || id != j.SessionID {
		return ErrCatalogJournalFrozen
	}
	done, err := guard.HoldJournalRemoval(j)
	if err != nil {
		return errors.Join(ErrCatalogJournalFrozen, err)
	}
	defer done()
	home, err := guard.Home()
	expected, resolveErr := filepath.EvalSymlinks(s.home)
	if err != nil || resolveErr != nil || home != expected {
		return errors.Join(ErrCatalogJournalFrozen, err, resolveErr)
	}
	scratch, closeScratch := s.WithReadBudget(s.durableContext(), s.resourceBudget)
	defer closeScratch()
	pending, found, err := scratch.catalogPendingForFence(id)
	if err != nil || !found || !recoveredCatalogJournal(pending) || *pending.Catalog.Recovery != j {
		return errors.Join(ErrCatalogJournalFrozen, err)
	}
	if err = scratch.verifyCatalogJournalRemoval(id, pending, j); err != nil {
		return err
	}
	return s.removePending(id)
}

func (s *Store) verifyCatalogJournalRemoval(id string, pending PendingPublication, j local.CatalogJournal) error {
	digest, err := s.CatalogJournalDigest(id, pending)
	if err != nil || digest != j.SHA256 {
		return errors.Join(ErrCatalogJournalFrozen, err)
	}
	return nil
}

func (s *Store) checkCatalogPendingRemoval(id string) error {
	scratch, closeScratch := s.WithReadBudget(s.durableContext(), s.resourceBudget)
	defer closeScratch()
	pending, found, err := scratch.catalogPendingForFence(id)
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if found && recoveredCatalogJournal(pending) {
		return ErrCatalogJournalFrozen
	}
	return nil
}
