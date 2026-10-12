package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// blockThenRemember is block for a size limit, which for a Cursor chat also
// remembers the state it was reached at (see rememberFailedRead).
func (s *sessionScan) blockThenRemember(reason state.BlockedReason, read sourceRead) (sessionOutcome, error) {
	outcome, err := s.block(reason, nil, &read.observed)
	if err != nil {
		return outcome, err
	}
	return outcome, rememberFailedRead(s.local, s.reg, read.adapter, read.observed, s.opts, nil, reason, s.sizeLimit, s.publishedLastHead())
}

// block records a terminal capture gap for the session and completes its
// request, so the pass ends cleanly and the session is not counted as
// pending. candidate, when known, becomes the cached comparison bundle so an
// unchanged transcript is skipped on the next pass; otherwise the previously
// cached bundle (or the last published one) is kept. The last published
// snapshot is retained untouched either way. observed is the source state the
// gap was reached at, which the next pass compares against to skip the
// session while it stands (nil: not known, so it is scanned again).
func (s *sessionScan) block(reason state.BlockedReason, candidate *archive.SourceBundle, observed *sourceState) (sessionOutcome, error) {
	s.gap = reason
	cached, _, _, haveCached := s.published.Cached()
	lastPublished, lastPublishedAt, _ := s.published.LastPublished()
	bundle := lastPublished
	if haveCached {
		bundle = cached
	}
	if candidate != nil {
		bundle = *candidate
	}
	previousReason, blocked := s.published.Blocked()
	alreadyBlocked := blocked && previousReason == reason && candidate == nil
	// A block that can end holds on to the hook evidence its acknowledgement
	// would otherwise discard, so a final response that arrived just before
	// the application deleted its transcript still publishes if the file
	// comes back. A permanent block has nothing to hand it back to.
	var held []archive.SupplementalEvidence
	if reason.Recoverable() {
		held = s.req.HookEvidence
	}
	if !alreadyBlocked || len(held) > 0 {
		if err := s.published.SaveBlocked(bundle, lastPublishedAt, reason, held...); err != nil {
			return outcomeSkipped, fmt.Errorf("cache blocked session: %w", err)
		}
	}
	// The gap stands until the source changes from the state it was reached
	// at, a missing transcript's absence included (one that comes back is
	// read at once): until then each pass skips it on a stat, rather than
	// decoding its published state and journaling a scan to reach the same
	// gap again.
	if err := s.recordBlockedSignature(reason, observed); err != nil {
		return outcomeSkipped, err
	}
	return outcomeSkipped, s.completeRequest("complete blocked request")
}

// publishPending uploads a pending publication and records it: source then
// metadata (or, for a metadata-only publication that carries no source
// bytes, a check of the recorded source then metadata), the superseded
// source in the ledger, the new published state, the covered request, and
// finally the removal of the pending file.
func (s *sessionScan) publishPending(pending state.PendingPublication) (sessionOutcome, error) {
	if err := s.checkHistoryPublication(pending); err != nil {
		return outcomeSkipped, err
	}
	if !pending.CarriesNoSource() && !storage.VerifySHA256(pending.SourceBytes, pending.SourceSHA256) {
		return outcomeSkipped, errors.New("pending source checksum does not match its persisted bytes")
	}
	// Marking it attempted rewrites the whole file, source bytes and bundle
	// included, so it is done once: a retry of an attempted publication, or
	// one saved already marked because it was due at once, skips it.
	if !pending.Attempted {
		pending.Attempted = true
		if err := s.local.SavePending(s.id(), pending); err != nil {
			return outcomeSkipped, fmt.Errorf("mark pending publication attempted: %w", err)
		}
	}
	if pending.History != nil {
		committed, err := s.checkFrozenHistoryMetadata(pending)
		if err != nil {
			return outcomeSkipped, err
		}
		if !committed {
			if err := s.recoverHistoryStages(pending); err != nil {
				return outcomeSkipped, err
			}
		}
	}
	if err := s.upload(pending); err != nil {
		return outcomeSkipped, err
	}
	if s.reg.StagedImportSHA256 != "" && pending.History == nil {
		if err := s.verifyImportPublication(pending); err != nil {
			return outcomeSkipped, err
		}
	}
	if pending.History != nil {
		if err := s.verifyHistoryReadback(pending); err != nil {
			return outcomeSkipped, err
		}
	}
	return s.acknowledgePublication(pending, false)
}

// acknowledgePublication follows verified exact readback. A committed privacy
// retry keeps its journal until the all-reference successor replaces it durably.
func (s *sessionScan) acknowledgePublication(pending state.PendingPublication, keepPending bool) (sessionOutcome, error) {
	if err := listingindex.PublishRevision(s.ctx, s.remote, pending.MetadataKey, pending.MetadataBytes); err != nil {
		s.warn(fmt.Errorf("listing maintenance pending: %w", err))
	} else if err := s.local.RemoveListingRepair(s.id()); err != nil {
		s.warn(err)
	}
	// The object this publication replaced is the one recorded when it was
	// uploaded, never one rebuilt from its bundle now (see
	// state.Published.LastPublishedSource). If it is unknown, only state from
	// an old version without cached metadata, nothing is recorded: the old
	// object then stays until the whole session expires, which is safe.
	//
	// For the same reason recording it is best effort: the publication has
	// reached storage, and failing it here, before it is saved, would upload
	// it again on every pass (a ledger that no longer decodes did exactly
	// that). A failure is reported once the publication is recorded.
	var next archive.Metadata
	if err := s.unmarshalRetained(pending.MetadataBytes, &next); err != nil {
		return outcomeSkipped, err
	}
	refs, err := next.SourceReferences()
	if err != nil {
		return outcomeSkipped, err
	}
	protected := map[string]bool{}
	for _, ref := range refs {
		protected[ref.Key] = true
	}
	if pending.History != nil {
		for _, retired := range pending.History.Retired {
			if err := s.local.RecordSupersededWithPrivacy(s.id(), retired.Reference.Key, retired.RetiredAt, retired.PrivacySensitive); err != nil {
				return outcomeSkipped, fmt.Errorf("record retired revision cleanup: %w", err)
			}
		}
	}
	if err := s.recordImportRetired(pending, protected); err != nil {
		return outcomeSkipped, err
	}
	if previous, hadPrevious := s.published.LastPublishedSource(); hadPrevious && !protected[previous.Key] {
		priorBundle, _, havePrior := s.published.LastPublished()
		privacySensitive := havePrior && priorBundle.Capture.FilterVersion != pending.Bundle.Capture.FilterVersion
		if err := s.local.RecordSupersededWithPrivacy(s.id(), previous.Key, s.now, privacySensitive); err != nil {
			if privacySensitive {
				// Keep the pending publication for another attempt. Saving the
				// new published state here would lose the only retry path for
				// this old-filter source, leaving it until session expiry.
				return outcomeSkipped, fmt.Errorf("record privacy-sensitive predecessor: %w", err)
			}
			s.warn(fmt.Errorf("record superseded source for cleanup: %w", err))
		}
	}
	var saveErr error
	if pending.MetadataOnly {
		saveErr = s.published.SaveRepublishedMetadata(pending, s.now)
	} else {
		saveErr = s.published.SavePublication(pending.Bundle, s.now, pending.SourceReference(), pending.MetadataBytes)
	}
	if err := saveErr; err != nil {
		return outcomeSkipped, fmt.Errorf("update published cache: %w", err)
	}
	if pending.History != nil && pending.ScanSignature != nil && !keepPending {
		proof := *pending.ScanSignature
		summary := s.published.Summary()
		proof.SourceSetDigest, proof.CurrentRevision = summary.SourceSetDigest, summary.CurrentRevision
		proof.SourceSchemaVersion, proof.MetadataSchemaVersion = summary.SourceSchemaVersion, summary.MetadataSchemaVersion
		proof.SourceSetComplete, proof.MeaningfulCapturedAt = summary.SourceSetComplete, summary.MeaningfulCapturedAt
		if err := s.local.SaveScanSignature(s.id(), proof); err != nil {
			return outcomeSkipped, fmt.Errorf("settle acknowledged native history: %w", err)
		}
	}
	// Whatever kept this session's metadata from being refreshed described
	// the publication just replaced.
	if err := s.local.RemoveRefreshSkip(s.id()); err != nil {
		return outcomeSkipped, err
	}
	if pending.RequestToken != "" {
		if _, err := s.local.CompleteRequest(s.id(), pending.RequestToken); err != nil {
			return outcomeSkipped, fmt.Errorf("complete published request: %w", err)
		}
	}
	if !keepPending {
		if err := s.local.RemovePending(s.id()); err != nil {
			return outcomeSkipped, err
		}
	}
	if s.reg.StagedImportSHA256 != "" && !keepPending {
		if err := s.local.RemoveImportStage(s.id()); err != nil {
			return outcomeSkipped, err
		}
	}
	return outcomePublished, nil
}

// upload writes a pending publication to storage.
func (s *sessionScan) upload(pending state.PendingPublication) error {
	if err := s.local.SaveListingRepair(s.id(), state.ListingRepair{MetadataKey: pending.MetadataKey, DestinationID: s.reg.DestinationID}); err != nil {
		return fmt.Errorf("journal listing repair: %w", err)
	}
	if pending.History != nil {
		committed, err := s.checkFrozenHistoryMetadata(pending)
		if err != nil {
			return err
		}
		if committed {
			return s.verifyHistoryReadback(pending)
		}
		if !pending.CarriesNoSource() {
			if err := s.ensureHistorySource(pending.SourceKey, pending.SourceSHA256, pending.SourceBytes); err != nil {
				return err
			}
		}
		for _, stage := range pending.History.Sources {
			mark := len(s.retainedReleases)
			if !pending.CarriesNoSource() && stage.Reference == pending.SourceReference() {
				continue
			}
			data, err := s.historyStage(stage)
			if err != nil {
				return err
			}
			if err := s.ensureHistorySource(stage.Reference.Key, stage.Reference.SHA256, data); err != nil {
				return err
			}
			s.releaseRetainedAfter(mark)
		}
		var m archive.Metadata
		if err := s.unmarshalRetained(pending.MetadataBytes, &m); err != nil {
			return err
		}
		if err := s.verifyHistoryReferences(pending, m); err != nil {
			return err
		}
		if err := s.checkHistoryPublication(pending); err != nil {
			return err
		}
		if committed, err := s.checkFrozenHistoryMetadata(pending); err != nil {
			return err
		} else if committed {
			return s.verifyHistoryReadback(pending)
		}
		return s.remote.Put(s.ctx, pending.MetadataKey, pending.MetadataBytes)
	}
	if !pending.CarriesNoSource() && s.reg.StagedImportSHA256 != "" {
		return s.uploadImportPending(pending)
	}
	if !pending.CarriesNoSource() {
		if err := storage.PutSourceThenMetadataIndexed(s.ctx, s.remote, pending.SourceKey, pending.MetadataKey, pending.SourceBytes, pending.MetadataBytes, s.opts.Retry, nil); err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		return nil
	}
	err := storage.PutMetadataForSourceIndexed(s.ctx, s.remote, pending.SourceKey, pending.SourceSHA256, pending.SourceSize, pending.MetadataKey, pending.MetadataBytes, s.opts.Retry, nil)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrChecksumMismatch) {
		// The recorded source is not in storage as recorded, and without its
		// bytes this publication can never succeed. Dropping it keeps it from
		// holding back normal capture; the metadata still points at whatever
		// it pointed at before. If this build can build the bundle after all,
		// the next refresh publishes it as a new source; if not, the refresh
		// is recorded as impossible so it is not retried (with a download)
		// on every pass.
		if removeErr := s.local.RemovePending(s.id()); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
		if _, buildErr := archive.BuildCompressedSource(pending.Bundle); buildErr != nil {
			skip := state.RefreshSkip{ParserVersion: s.parserVersion(), SourceKey: pending.SourceKey, Reason: state.RefreshSkipSourceUnavailable}
			if skipErr := s.local.SaveRefreshSkip(s.id(), skip); skipErr != nil {
				err = errors.Join(err, skipErr)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("publish metadata: %w", err)
	}
	return nil
}

// ensureHistorySource reuses exact immutable remote bytes on an interrupted
// source-first attempt. Typed all-reference verification still precedes metadata.
func (s *sessionScan) ensureHistorySource(key, sha string, data []byte) error {
	return storage.PutVerifiedSource(s.ctx, s.remote, key, sha, data, s.opts.Retry, s.verifyHistorySource)
}

// verifyHistorySource keeps HEAD checksum verification cheap. Older/multipart
// objects without a digest need an exact-size charged read. Its bytes end before
// retrying or moving to a sibling reference.
func (s *sessionScan) verifyHistorySource(ctx context.Context, key, sha string, size int) error {
	if statter, ok := s.remote.(storage.ObjectStatter); ok {
		info, err := statter.Stat(ctx, key)
		if err != nil {
			return err
		}
		if info.SHA256 != "" {
			if !strings.EqualFold(info.SHA256, strings.TrimSpace(sha)) || info.Size != int64(size) {
				return fmt.Errorf("%w for %q", storage.ErrChecksumMismatch, key)
			}
			return nil
		}
	}
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	raw, err := s.historyGet(key, int64(size))
	if err != nil {
		return err
	}
	if len(raw) != size || !storage.VerifySHA256(raw, sha) {
		return fmt.Errorf("%w for %q", storage.ErrChecksumMismatch, key)
	}
	return nil
}

func (s *sessionScan) recordImportRetired(pending state.PendingPublication, protected map[string]bool) error {
	for _, retired := range pending.ImportRetired {
		if !protected[retired.Key] {
			if err := s.local.RecordSupersededWithPrivacy(s.id(), retired.Key, s.now, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sessionScan) uploadImportPending(pending state.PendingPublication) error {
	if err := s.ensureHistorySource(pending.SourceKey, pending.SourceSHA256, pending.SourceBytes); err != nil {
		return err
	}
	// Verify source ownership before publishing the pointer. A retry preserves
	// the exact pending bytes and may not overwrite another archive owner.
	raw, err := s.historyGet(pending.MetadataKey, historyMetadataLimit)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if errors.Is(err, storage.ErrNotFound) && pending.ImportExpectedMetadataSHA256 != "" {
		return errHistoryMetadataConflict
	}
	if err == nil && metadataSHA(raw) != metadataSHA(pending.MetadataBytes) {
		if pending.ImportExpectedMetadataSHA256 == "" || metadataSHA(raw) != pending.ImportExpectedMetadataSHA256 {
			return errHistoryMetadataConflict
		}
		var previous archive.Metadata
		if err := s.unmarshalRetained(raw, &previous); err != nil {
			return err
		}
		if err := s.validateAuthorityIdentity(previous); err != nil {
			return err
		}
	}

	return s.remote.Put(s.ctx, pending.MetadataKey, pending.MetadataBytes)
}
