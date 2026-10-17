package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

var errHistoryMetadataConflict = errors.New("remote history metadata changed; preserve pending publication and reconcile before retry")

func metadataSHA(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// checkFrozenHistoryMetadata is optimistic conflict detection under collector
// ownership, not distributed compare-and-swap. Only the frozen predecessor or
// exact final bytes can be replaced. An out-of-band writer racing GET-to-PUT is
// outside the single-machine collector.lock ownership contract.
func (s *sessionScan) checkFrozenHistoryMetadata(p state.PendingPublication) (bool, error) {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	if p.History == nil {
		return false, nil
	}
	if _, err := s.frozenHistoryMetadata(p); err != nil {
		return false, err
	}
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		if p.History.ExpectedMetadataSHA256 == "" {
			return false, nil
		}
		return false, errHistoryMetadataConflict
	}
	if err != nil {
		return false, err
	}
	if bytes.Equal(raw, p.MetadataBytes) {
		return true, nil
	}
	if p.History.ExpectedMetadataSHA256 == "" || metadataSHA(raw) != p.History.ExpectedMetadataSHA256 {
		return false, errHistoryMetadataConflict
	}
	var previous archive.Metadata
	if err := s.unmarshalRetained(raw, &previous); err != nil {
		return false, err
	}
	if _, err := previous.SourceReferences(); err != nil {
		return false, err
	}
	if err := s.validateAuthorityIdentity(previous); err != nil {
		return false, err
	}
	return false, nil
}

// recoverHistoryStages restores missing/corrupt owned stages only from the
// journal's checksum-verified remote references. Failure leaves the descriptor,
// all other stages and the request untouched; no new native evidence is guessed.
func (s *sessionScan) recoverHistoryStages(p state.PendingPublication) error {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	if p.History == nil {
		return nil
	}
	metadata, err := s.frozenHistoryMetadata(p)
	if err != nil {
		return err
	}
	for _, stage := range p.History.Sources {
		mark := len(s.retainedReleases)
		if _, err := s.historyStage(stage); err == nil {
			s.releaseRetainedAfter(mark)
			continue
		}
		raw, err := s.historyGet(stage.Reference.Key, int64(stage.Reference.CompressedBytes))
		if err != nil {
			return fmt.Errorf("recover frozen revision source: %w", err)
		}
		if _, err := s.decodeHistoryStage(metadata, p, stage.Reference, raw); err != nil {
			return fmt.Errorf("verify recovered revision source: %w", err)
		}
		restored, err := s.local.StagePublicationSource(s.id(), stage.Reference, raw)
		if err != nil {
			return err
		}
		if restored != stage {
			return errors.New("recovered revision stage identity changed")
		}
		s.releaseRetainedAfter(mark)
	}
	return nil
}

func (s *sessionScan) decodeHistoryStage(metadata archive.Metadata, p state.PendingPublication, ref archive.SourceReference, raw []byte) (archive.SourceBundle, error) {
	if _, err := metadata.SourceReferences(); err != nil {
		return archive.SourceBundle{}, err
	}
	if metadata.SourceBundle == ref {
		return s.decodeReferenced(metadata, raw)
	}
	if metadata.History != nil {
		for _, revision := range metadata.History.Preserved {
			if revision.Source != ref {
				continue
			}
			if p.History.Preparing {
				for _, input := range p.History.Inputs {
					if input.Reference == ref && input.RevisionID == revision.RevisionID {
						history := *metadata.History
						history.Preserved = []archive.RevisionReference{revision}
						history.Preserved[0].FilterVersion = input.FilterVersion
						metadata.History = &history
						break
					}
				}
			}
			return s.decodeRevision(metadata, revision.RevisionID, raw)
		}
	}
	return archive.SourceBundle{}, errors.New("stage is not referenced by the complete frozen source set")
}

// verifyHistoryReadback verifies exact final bytes and ALL referenced sources
// before any privacy obligation, published cache or request is acknowledged.
func (s *sessionScan) verifyHistoryReadback(p state.PendingPublication) error {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	if _, err := s.frozenHistoryMetadata(p); err != nil {
		return err
	}
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, p.MetadataBytes) {
		return errHistoryMetadataConflict
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(raw, &metadata); err != nil {
		return err
	}
	if err := s.verifyHistoryReferences(p, metadata); err != nil {
		return err
	}
	current, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, p.MetadataBytes) {
		return errHistoryMetadataConflict
	}
	if s.publicationAttempt != nil && p.Commit != nil && s.publicationAttempt.verifiedCommit != nil && *s.publicationAttempt.verifiedCommit == *p.Commit {
		commit := *p.Commit
		s.publicationAttempt.baselineCommit = &commit
	}
	return nil
}

const historyMetadataLimit int64 = 32 << 20

const historyCompressedLimit int64 = 32 << 20

func historyLimitedGet(ctx context.Context, store storage.ObjectStore, key string, limit int64) ([]byte, error) {
	if limit <= 0 || limit > historyCompressedLimit {
		return nil, storage.ErrObjectTooLarge
	}
	bounded, ok := store.(storage.LimitedGetter)
	if !ok {
		return nil, errors.New("history recovery requires allocation-bounded object reads")
	}
	raw, err := bounded.GetLimited(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, storage.ErrObjectTooLarge
	}
	return raw, nil
}

func (s *sessionScan) frozenHistoryMetadata(p state.PendingPublication) (archive.Metadata, error) {
	if err := p.ValidateHistoryBudgeted(s.id(), s.readBudget()); err != nil {
		return archive.Metadata{}, err
	}
	if int64(len(p.MetadataBytes)) > historyMetadataLimit {
		return archive.Metadata{}, storage.ErrObjectTooLarge
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(p.MetadataBytes, &metadata); err != nil {
		return archive.Metadata{}, err
	}
	if metadata.SessionID != s.id() || metadata.NativeSessionID != s.reg.NativeSessionID || metadata.Harness.Name != s.reg.Harness.Name || metadata.ProjectID != s.reg.ProjectID {
		return archive.Metadata{}, errors.New("frozen history does not belong to the current registration")
	}
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return archive.Metadata{}, err
	}
	return metadata, nil
}

// verifyHistoryReferences decodes each bounded immutable object before metadata
// replacement as well as after readback; a valid checksum alone is insufficient.
func (s *sessionScan) verifyHistoryReferences(p state.PendingPublication, metadata archive.Metadata) error {
	if s.publicationAttempt != nil {
		s.publicationAttempt.verifiedCommit = nil
	}
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	refs, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	if err := boundFrozenReferences(metadata); err != nil {
		return err
	}
	if p.Commit != nil {
		if len(refs) != len(p.Sources) {
			return state.ErrDurableStorageRecovery
		}
		for i, ref := range refs {
			if ref != p.Sources[i].Reference {
				return state.ErrDurableStorageRecovery
			}
		}
	}
	for _, ref := range refs {
		mark := len(s.retainedReleases)
		data, err := s.historyGet(ref.Key, int64(ref.CompressedBytes))
		if err != nil {
			return err
		}
		bundle, err := s.decodeHistoryStage(metadata, p, ref, data)
		if err != nil {
			return err
		}
		if !p.History.Preparing && ((p.History.FilterVersion != "" && bundle.Capture.FilterVersion != p.History.FilterVersion) || (p.History.AdapterVersion != "" && bundle.Capture.AdapterVersion != p.History.AdapterVersion)) {
			return errors.New("final reference disagrees with frozen all-reference privacy policy")
		}
		s.releaseRetainedAfter(mark)
	}
	if s.publicationAttempt != nil && p.Commit != nil && p.ValidatePublicationBudgeted(s.ctx, s.readBudget()) == nil {
		commit := *p.Commit
		s.publicationAttempt.verifiedCommit = &commit
		s.publicationAttempt.verifiedKey = p.MetadataKey
	}
	return nil
}
