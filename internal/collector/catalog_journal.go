package collector

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type catalogSettledKey struct{}

func (s *sessionScan) beginCatalogJournal(ctx context.Context, pending state.PendingPublication, remote storage.CatalogLifecycle) (state.PendingPublication, context.Context, bool, error) {
	if s.opts.CollectorGuard == nil {
		if pending.Catalog.Recovery != nil {
			return pending, ctx, false, state.ErrDurableStorageRecovery
		}
		admitted, err := remote.BeginPublication(ctx, pending.Catalog.ID, pending.MetadataBytes)
		return pending, admitted, false, err
	}
	journalRemote, ok := s.remote.(storage.CatalogJournalLifecycle)
	if !ok {
		return pending, ctx, false, state.ErrDurableStorageRecovery
	}
	var err error
	pending, err = s.prepareCatalogJournal(pending)
	if err != nil {
		return pending, ctx, false, err
	}
	admitted, settled, err := journalRemote.BeginJournalPublication(ctx, pending.Catalog.ID, pending.MetadataBytes, *pending.Catalog.Recovery, s.opts.CollectorGuard)
	if settled && err == nil {
		admitted = context.WithValue(admitted, catalogSettledKey{}, true)
	}
	return pending, admitted, settled, err
}

func (s *sessionScan) prepareCatalogJournal(pending state.PendingPublication) (state.PendingPublication, error) {
	if pending.Catalog.Recovery == nil {
		origin, err := s.opts.CollectorGuard.Origin()
		if err != nil {
			return pending, err
		}
		digest, err := s.local.CatalogJournalDigest(s.id(), pending)
		if err != nil {
			return pending, err
		}
		owner, err := local.ID()
		if err != nil {
			return pending, err
		}
		pending.Catalog.Recovery = &local.CatalogJournal{Owner: owner, Origin: origin, Destination: s.opts.CatalogDestination, SessionID: s.id(), MutationID: pending.Catalog.ID, ExpectedRevision: pending.Catalog.ExpectedRevision, SHA256: digest}
		if err = s.local.SavePending(s.id(), pending); err != nil {
			return pending, err
		}
	}
	return pending, s.local.VerifyCatalogJournal(s.id(), pending, s.opts.CollectorGuard, s.opts.CatalogDestination)
}

func (s *sessionScan) removeCompletedCatalogJournal(pending state.PendingPublication) error {
	if pending.Catalog == nil && pending.History == nil && pending.Bundle.History == nil && pending.Bundle.SchemaVersion == archive.SourceSchemaVersion {
		return s.local.RemoveExactLegacyPending(s.id(), pending)
	}
	if pending.Catalog == nil || pending.Catalog.Recovery == nil {
		return s.local.RemovePending(s.id())
	}
	j := *pending.Catalog.Recovery
	if err := s.opts.CollectorGuard.RecordJournalRemoval(j); err != nil {
		return err
	}
	if err := s.local.RemoveCatalogPending(s.id(), j, s.opts.CollectorGuard); err != nil {
		return err
	}
	return acknowledgeCatalogRemoval(s.ctx, s.remote, s.opts.CollectorGuard, j)
}

func acknowledgeCatalogRemoval(ctx context.Context, remote storage.ObjectStore, guard *local.CollectorGuard, journal local.CatalogJournal) error {
	publisher, ok := remote.(storage.CatalogJournalLifecycle)
	if !ok {
		return state.ErrDurableStorageRecovery
	}
	proof, err := guard.ProveJournalRemoval(journal)
	if err != nil {
		return err
	}
	if err = publisher.AcknowledgeJournalRemoval(ctx, proof, guard); err != nil {
		return err
	}
	return guard.RemoveJournalRemoval(journal)
}

func recoverCatalogRemovals(ctx context.Context, localStore *state.Store, remote storage.ObjectStore, opts Options) error {
	if opts.CollectorGuard == nil {
		return nil
	}
	journals, err := opts.CollectorGuard.JournalRemovals()
	if err != nil {
		return err
	}
	for _, j := range journals {
		if j.Destination != opts.CatalogDestination {
			return state.ErrDurableStorageRecovery
		}
		pending, found, err := localStore.LoadPending(j.SessionID)
		if err != nil {
			return err
		}
		if found {
			if pending.Catalog == nil || pending.Catalog.Recovery == nil || *pending.Catalog.Recovery != j || localStore.VerifyCatalogJournal(j.SessionID, pending, opts.CollectorGuard, opts.CatalogDestination) != nil {
				return state.ErrDurableStorageRecovery
			}
			if err = localStore.RemoveCatalogPending(j.SessionID, j, opts.CollectorGuard); err != nil {
				return err
			}
		}
		if err = acknowledgeCatalogRemoval(ctx, remote, opts.CollectorGuard, j); err != nil {
			return errors.Join(state.ErrDurableStorageRecovery, err)
		}
	}
	return nil
}
