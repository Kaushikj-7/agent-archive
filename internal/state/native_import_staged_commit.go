package state

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeImportStagedState struct {
	candidate    string
	owner        string
	published    string
	acknowledged bool
}

// Final typed historical writes serialize with the exact batch undo marker,
// then the session owner/request. An actual selecting ACK permits cleanup to
// outlive the creating batch; no captured boolean can create that exception.
func (s *Store) nativeImportStagedCommit(ctx context.Context, id string, candidate *PendingPublication, published *publishedState) (func(*local.AtomicWriteWitness, func() error) error, error) {
	var q *StagedSelection
	if candidate != nil {
		q = candidate.StagedProof()
	}
	if published != nil && published.LastPublished != nil {
		q = published.LastPublished.StagedProof()
	}
	if q == nil || q.Core.NativeImportAuthority == "" {
		return nil, nil
	}
	before, batchID, err := s.nativeImportStagedState(ctx, id, candidate, published)
	if err != nil {
		return nil, err
	}
	return func(witness *local.AtomicWriteWitness, rename func() error) error {
		commit := func(held *local.RootedHome) error {
			unlock, err := local.RootedLockWait(held, requestLockName(id), time.Second)
			if err != nil {
				return err
			}
			defer unlock()
			scoped := *s
			scoped.atomicWitness = witness
			after, afterBatch, err := scoped.nativeImportStagedState(ctx, id, candidate, published)
			if err != nil {
				return err
			}
			if before != after || batchID != afterBatch {
				return ErrReadySelectionFrozen
			}
			if err := errors.Join(ctx.Err(), held.Check()); err != nil {
				return err
			}
			return rename()
		}
		if !before.acknowledged {
			return WithImportBatchLock(ctx, s.home, batchID, commit)
		}
		// This path holds no batch lock. The exact current persisted ACK is checked
		// again under the request lock before cleanup can ignore the batch/root.
		held, err := local.OpenRootedHome(s.home)
		if err != nil {
			return err
		}
		defer held.Close()
		return commit(held)
	}, nil
}

func (s *Store) nativeImportStagedState(ctx context.Context, id string, candidate *PendingPublication, next *publishedState) (nativeImportStagedState, string, error) {
	var result nativeImportStagedState
	read, closeRead := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeRead()
	reg, found, err := read.LoadRegistration(id)
	if err != nil || !found || reg.NativeImport == nil {
		return result, "", errors.Join(ErrReadySelectionFrozen, err)
	}
	batchID := reg.NativeImport.Confirmation.BatchID
	fail := func(err error) (nativeImportStagedState, string, error) {
		return result, batchID, errors.Join(ErrReadySelectionFrozen, err)
	}
	cfg, found, err := config.Load(s.home)
	if err != nil || !found || nativeImportStageFlags(cfg, reg) != nil || !cfg.SupportsStagedSelection() || cfg.Paused || !cfg.Archive.Enabled || !cfg.AcceptSession(reg) || cfg.DestinationID() != reg.DestinationID {
		return fail(err)
	}
	var q *StagedSelection
	var metadata []byte
	if candidate != nil {
		if candidate.ValidateReadySlot(id) != nil {
			return fail(nil)
		}
		if err := read.validateStagedSelection(ctx, *candidate); err != nil {
			return fail(err)
		}
		q = candidate.StagedProof()
		metadata = candidate.MetadataBytes
	}
	if next != nil {
		if next.LastPublished == nil || next.LastPublished.StagedProof() == nil {
			return fail(nil)
		}
		selected := next.LastPublished.StagedProof()
		if q != nil && (!sameStagedSelection(*q, *selected) || !bytes.Equal(metadata, next.MetadataBytes)) {
			return fail(nil)
		}
		q = selected
		metadata = next.MetadataBytes
		view := &Published{store: read, id: id}
		copyNext := *next
		if err := view.validateStagedPublished(ctx, &copyNext); err != nil {
			return fail(err)
		}
	}
	if q == nil || q.Version != 3 || q.Core.NativeImportAuthority == "" {
		return fail(nil)
	}
	current, err := read.LoadPublishedStateContext(ctx, id)
	if err != nil {
		return fail(err)
	}
	if current.found && current.state.PublicationVersion == 3 && current.state.LastPublished != nil {
		selected := current.state.LastPublished.StagedProof()
		result.acknowledged = selected != nil && sameStagedSelection(*selected, *q) && bytes.Equal(current.state.MetadataBytes, metadata)
	}
	if !result.acknowledged {
		if candidate == nil {
			return fail(nil)
		}
		if err := errors.Join(nativeImportConfiguredRoot(cfg, reg), read.nativeImportCurrentBatch(ctx, reg)); err != nil {
			return fail(err)
		}
		if current.found && (q.Core.Predecessor != ReadyPredecessorPresent || readySHA(current.state.MetadataBytes) != q.Core.PredecessorSHA256) {
			return fail(nil)
		}
		if !current.found && q.Core.Predecessor == ReadyPredecessorPresent {
			return fail(nil)
		}
		request, requested, err := read.LoadRequest(id)
		if err != nil || requested != q.HadRequest || requested && request.Token != q.CoveredToken {
			return fail(err)
		}
	}
	if next != nil {
		result.candidate, err = read.readyDigest(ctx, *next)
	} else {
		result.candidate, err = read.readyDigest(ctx, publicationWire(*candidate))
	}
	if err != nil {
		return fail(err)
	}
	result.owner, err = read.readyDigest(ctx, reg)
	if err != nil {
		return fail(err)
	}
	if current.found {
		result.published, err = read.readyDigest(ctx, current.state)
		if err != nil {
			return fail(err)
		}
	}
	return result, batchID, ctx.Err()
}
