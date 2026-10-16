package collector

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (s *sessionScan) resumeRetentionDeletion() (sessionOutcome, bool, error) {
	j, found, err := s.local.LoadSessionDeletion(s.reg)
	if err != nil || !found {
		return outcomeSkipped, found, err
	}
	if j.Phase == state.DeletionRestored || j.Phase == state.DeletionCancelled {
		if pending, have, err := s.local.LoadPublicationPending(s.id()); err != nil {
			return outcomeSkipped, true, err
		} else if have {
			if j.Phase != state.DeletionRestored || pending.Preparation == nil || pending.Preparation.Retention == nil {
				return outcomeSkipped, true, state.ErrDurableStorageRecovery
			}
			return s.resumeRetentionAbsent(j)
		}
		// An uncertain terminal stage cleanup remains retriable without granting a
		// second restoration, native recapture or request acknowledgement.
		if err = s.local.RemoveRestoredDeletionStages(s.id()); err != nil {
			return outcomeSkipped, true, err
		}
		return outcomeSkipped, false, nil
	}
	if j.LocalRemoved || j.Reason != state.RemovalReasonRetention {
		return outcomeSkipped, true, state.ErrRemovalPending
	}
	if j.Phase == state.DeletionPrepared {
		if err := s.local.CancelUnstartedDeletion(s.reg, s.req); err != nil {
			return outcomeSkipped, true, err
		}
		if err := s.local.RemoveRestoredDeletionStages(s.id()); err != nil {
			return outcomeSkipped, true, err
		}
		return outcomeSkipped, false, nil
	}
	if j.Phase == state.DeletionDeleting {
		key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
		if err != nil {
			return outcomeSkipped, true, err
		}
		if _, err = s.historyGet(key, historyMetadataLimit); !errors.Is(err, storage.ErrNotFound) {
			return outcomeSkipped, true, errors.Join(state.ErrRemovalPending, err)
		}
		j, err = s.local.AdvanceSessionDeletion(j, state.DeletionAbsent)
		if err != nil {
			return outcomeSkipped, true, err
		}
	}
	end, err := s.beginPublicationAttempt()
	if err != nil {
		return outcomeSkipped, true, err
	}
	defer end()
	return s.resumeRetentionAbsent(j)
}

func (s *sessionScan) resumeRetentionAbsent(j state.SessionDeletion) (sessionOutcome, bool, error) {
	// The underlying destination coordinator and catalog context remain those of
	// the actual invocation; this path never unwraps or lists catalog objects.
	if remote, ok := s.remote.(interface{ CatalogMetadataAuthority() bool }); ok && remote.CatalogMetadataAuthority() {
		return outcomeSkipped, true, state.ErrDurableStorageRecovery
	}
	p, have, err := s.local.LoadPublicationPending(s.id())
	if err != nil {
		return outcomeSkipped, true, fmt.Errorf("load retained restoration journal: %w", err)
	}
	if have {
		if p.Preparation == nil || p.Preparation.Retention == nil {
			return outcomeSkipped, true, state.ErrDurableStorageRecovery
		}
		if err = s.local.ValidateRetentionRestoration(s.reg, p); err != nil {
			return outcomeSkipped, true, err
		}
		out, err := s.sourceSetPublishPending(p)
		return out, true, err
	}
	bundle, _, found := s.published.LastPublished()
	if !found {
		return outcomeSkipped, true, state.ErrDurableStorageRecovery
	}
	policy, err := s.publicationPolicy(bundle)
	if err != nil {
		return outcomeSkipped, true, err
	}
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return outcomeSkipped, true, err
	}
	before, err := s.historyGet(key, historyMetadataLimit)
	if !errors.Is(err, storage.ErrNotFound) && err != nil {
		return outcomeSkipped, true, err
	}
	if err == nil && !bytes.Equal(before, j.MetadataBytes) {
		return outcomeSkipped, true, storage.ErrPublicationConflict
	}
	if err = s.retainCharge(3*int64(len(j.MetadataBytes)) + int64(len(j.References))*64<<10); err != nil {
		return outcomeSkipped, true, err
	}
	next, err := s.local.PrepareRetentionRestoration(s.ctx, s.readBudget(), s.reg, s.req, s.published, policy, s.publicationAdmission(), string(s.opts.skillEvidence()))
	if err != nil {
		return outcomeSkipped, true, fmt.Errorf("prepare exact retained restoration: %w", err)
	}
	after, err := s.historyGet(key, historyMetadataLimit)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return outcomeSkipped, true, err
	}
	if !bytes.Equal(before, after) {
		return outcomeSkipped, true, storage.ErrPublicationConflict
	}
	if err = s.local.SavePending(s.id(), next); err != nil {
		return outcomeSkipped, true, fmt.Errorf("save exact retained restoration: %w", err)
	}
	return outcomeSkipped, true, nil
}
