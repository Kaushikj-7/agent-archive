package state

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
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
	if err != nil || nativeImportStagedPolicyMismatch(cfg, reg, found) {
		return fail(err)
	}
	q, metadata, valid, err := read.nativeImportStagedCandidate(ctx, id, candidate, next)
	if err != nil || !valid {
		return fail(err)
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
		valid, err := read.nativeImportUnacknowledgedState(ctx, id, cfg, reg, candidate, current, q)
		if err != nil || !valid {
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

func nativeImportStagedPolicyMismatch(cfg config.Config, reg archive.SessionRegistration, found bool) bool {
	return !found || nativeImportStageFlags(cfg, reg) != nil || !cfg.SupportsStagedSelection() || cfg.Paused || !cfg.Archive.Enabled || !cfg.AcceptSession(reg) || cfg.DestinationID() != reg.DestinationID
}

func (s *Store) nativeImportStagedCandidate(ctx context.Context, id string, candidate *PendingPublication, next *publishedState) (*StagedSelection, []byte, bool, error) {
	var q *StagedSelection
	var metadata []byte
	if candidate != nil {
		if candidate.ValidateReadySlot(id) != nil {
			return nil, nil, false, nil
		}
		if err := s.validateStagedSelection(ctx, *candidate); err != nil {
			return nil, nil, false, err
		}
		q = candidate.StagedProof()
		metadata = candidate.MetadataBytes
	}
	if next != nil {
		if next.LastPublished == nil || next.LastPublished.StagedProof() == nil {
			return nil, nil, false, nil
		}
		selected := next.LastPublished.StagedProof()
		if q != nil && (!sameStagedSelection(*q, *selected) || !bytes.Equal(metadata, next.MetadataBytes)) {
			return nil, nil, false, nil
		}
		q = selected
		metadata = next.MetadataBytes
		view := &Published{store: s, id: id}
		copyNext := *next
		if err := view.validateStagedPublished(ctx, &copyNext); err != nil {
			return nil, nil, false, err
		}
	}
	return q, metadata, true, nil
}

func (s *Store) nativeImportUnacknowledgedState(ctx context.Context, id string, cfg config.Config, reg archive.SessionRegistration, candidate *PendingPublication, current *Published, q *StagedSelection) (bool, error) {
	if candidate == nil {
		return false, nil
	}
	if err := errors.Join(nativeImportConfiguredRoot(cfg, reg), s.nativeImportCurrentBatch(ctx, reg)); err != nil {
		return false, err
	}
	if current.found && (q.Core.Predecessor != ReadyPredecessorPresent || readySHA(current.state.MetadataBytes) != q.Core.PredecessorSHA256) {
		return false, nil
	}
	if !current.found && q.Core.Predecessor == ReadyPredecessorPresent {
		return false, nil
	}
	request, requested, err := s.LoadRequest(id)
	if err != nil || requested != q.HadRequest || requested && request.Token != q.CoveredToken {
		return false, err
	}
	return true, nil
}
