package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (s *sessionScan) sourceSetPublishPending(pending state.PendingPublication) (sessionOutcome, error) {
	endAttempt, attemptErr := s.beginPublicationAttempt()
	if attemptErr != nil {
		return outcomeSkipped, attemptErr
	}
	defer endAttempt()
	if err := s.bindPublicationAttempt(pending); err != nil {
		return outcomeSkipped, err
	}
	if pending.History != nil && pending.History.Preparing {
		return outcomeSkipped, archive.ErrHistoryMutationPending
	}
	if err := s.checkHistoryPublicationLocal(pending); err != nil {
		return outcomeSkipped, err
	}
	if err := s.sealPending(&pending); err != nil {
		return outcomeSkipped, err
	}
	if err := s.checkHistoryPublicationLocal(pending); err != nil {
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
	readback, err := s.sourceSetUpload(pending)
	if err != nil {
		return outcomeSkipped, err
	}
	body, etag, err := readback.Consume(pending.MetadataKey, pending.MetadataBytes)
	if err != nil {
		return outcomeSkipped, err
	}
	if pending.History == nil {
		frame := s.publicationAttempt
		frame.verifiedCommit = nil
		if frame.resolvedCommit == nil || *frame.resolvedCommit != *pending.Commit || frame.resolvedKey != pending.MetadataKey {
			return outcomeSkipped, state.ErrDurableStorageRecovery
		}
		for i := range pending.Sources {
			if !frame.resolved[i] {
				return outcomeSkipped, state.ErrDurableStorageRecovery
			}
		}
		for _, source := range pending.Sources {
			if err := s.verifyHistorySource(s.ctx, source.Reference.Key, source.Reference.SHA256, source.Reference.CompressedBytes); err != nil {
				return outcomeSkipped, err
			}
		}
		commit := *pending.Commit
		frame.verifiedCommit = &commit
		frame.verifiedKey = pending.MetadataKey
	}
	if pending.History != nil {
		metadata, err := s.frozenHistoryMetadata(pending)
		if err != nil {
			return outcomeSkipped, err
		}
		if err = s.verifyHistoryReferences(pending, metadata); err != nil {
			return outcomeSkipped, err
		}
	}
	return s.sourceSetAcknowledgeReadback(pending, false, body, etag)

}

func (s *sessionScan) sourceSetAcknowledgePublication(pending state.PendingPublication, keepPending bool) (sessionOutcome, error) {
	endAttempt, attemptErr := s.beginPublicationAttempt()
	if attemptErr != nil {
		return outcomeSkipped, attemptErr
	}
	defer endAttempt()
	if err := s.bindPublicationAttempt(pending); err != nil {
		return outcomeSkipped, err
	}
	body, etag, err := storage.ReadPublicationMetadataRevision(s.ctx, publicationTransactionStore{ObjectStore: s.sourceSetRemote(), scan: s, pending: &pending}, pending.MetadataKey, int64(len(pending.MetadataBytes)))
	if err != nil {
		return outcomeSkipped, err
	}
	if pending.Commit == nil || storage.SHA256Hex(body) != pending.Commit.MetadataSHA256 {
		return outcomeSkipped, storage.ErrPublicationConflict
	}
	return s.sourceSetAcknowledgeReadback(pending, keepPending, body, etag)
}

func (s *sessionScan) sourceSetAcknowledgeReadback(pending state.PendingPublication, keepPending bool, body []byte, etag string) (sessionOutcome, error) {
	endAttempt, attemptErr := s.beginPublicationAttempt()
	if attemptErr != nil {
		return outcomeSkipped, attemptErr
	}
	defer endAttempt()
	if pending.Commit == nil || storage.SHA256Hex(body) != pending.Commit.MetadataSHA256 {
		return outcomeSkipped, storage.ErrPublicationConflict
	}
	var listingErr error
	if remote, ok := s.sourceSetProvider().(storage.CatalogPublisher); ok && remote.CatalogMetadataAuthority() {
		// Catalog metadata and its listing commit under the admitted head.
		// No legacy auxiliary listing mutation is authorized here.
	} else if etag == "" {
		listingErr = storage.ErrVersionedReadUnavailable
	} else {
		revision, err := listingindex.NewRevision(pending.MetadataKey, body, etag)
		if err != nil {
			listingErr = err
		} else {
			listingErr = listingindex.RepairRevision(s.ctx, s.sourceSetRemote(), revision)
		}
	}
	if listingErr != nil {
		s.warn(fmt.Errorf("listing maintenance pending: %w", listingErr))
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
	if err := s.recordSourceSetRetirement(pending); err != nil {
		return outcomeSkipped, err
	}
	closing, err := s.closePublicationReadback(pending)
	if err != nil {
		return outcomeSkipped, err
	}
	if keepPending {
		err = closing.consumeBaselineOwed(s, pending)
	} else {
		err = closing.consume(s, pending)
	}
	if err != nil {
		return outcomeSkipped, err
	}

	if err := s.published.SaveCommittedPublication(pending, s.now); err != nil {
		return outcomeSkipped, fmt.Errorf("update published cache: %w", err)
	}
	// C may have indexed the exact response before a competing writer changes
	// metadata. Keep its repair obligation until D and the full local save have
	// both succeeded; neither a closing conflict nor a failed save settles it.
	if listingErr == nil {
		if err := s.local.RemoveListingRepair(s.id()); err != nil {
			s.warn(err)
		}
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
	if pending.RequestToken != "" && !keepPending {
		if _, err := s.local.CompleteRequest(s.id(), pending.RequestToken); err != nil {
			return outcomeSkipped, fmt.Errorf("complete published request: %w", err)
		}
	}
	if !keepPending {
		if err := s.finishSourceSetAcknowledgment(pending); err != nil {
			return outcomeSkipped, err
		}
	}
	return outcomePublished, nil
}

// finishSourceSetAcknowledgment runs only after closing proof, local commit and
// covered request completion; pending removal remains the final operation.
func (s *sessionScan) finishSourceSetAcknowledgment(pending state.PendingPublication) error {
	if pending.History == nil && pending.Commit.Purpose == state.PublicationPrivacyRewrite {
		if err := s.local.SettleOrdinaryPublicationPrivacy(s.id(), pending); err != nil {
			return err
		}
	}
	if pending.History != nil && pending.Commit.Purpose == state.PublicationPrivacyRewrite {
		if err := s.local.SettleHistoryPublicationPrivacy(s.id(), pending); err != nil {
			return fmt.Errorf("settle history privacy original: %w", err)
		}
	}
	if err := s.local.SettlePublicationMigration(s.id(), pending); err != nil {
		return err
	}
	if pending.Catalog != nil && s.ctx.Value(catalogSettledKey{}) != true {
		if remote, ok := s.sourceSetProvider().(storage.CatalogLifecycle); ok {
			if err := remote.CompletePublication(s.ctx, pending.Catalog.ID, pending.MetadataBytes); err != nil {
				return err
			}
		}
	}
	if err := s.removeCompletedCatalogJournal(pending); err != nil {
		return fmt.Errorf("remove completed source-set journal: %w", err)
	}
	return nil
}

func (s *sessionScan) sourceSetUpload(pending state.PendingPublication) (*storage.ValidatedPublicationReadback, error) {
	if err := s.local.SaveListingRepair(s.id(), state.ListingRepair{MetadataKey: pending.MetadataKey, DestinationID: s.reg.DestinationID}); err != nil {
		return nil, fmt.Errorf("journal listing repair: %w", err)
	}
	if err := pending.ValidatePublicationBudgeted(s.ctx, s.readBudget()); err != nil {
		return nil, err
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(pending.MetadataBytes, &metadata); err != nil {
		return nil, err
	}
	sources := make([]storage.SourcePublication, len(pending.Sources))
	for i, source := range pending.Sources {
		sources[i] = storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes}
	}
	prior := storage.MetadataPredecessor{Known: pending.Commit.Predecessor != state.PredecessorUnknown, Exists: pending.Commit.Predecessor == state.PredecessorPresent, SHA256: pending.Commit.PredecessorSHA256}
	frame := s.publicationAttempt
	if frame == nil || len(pending.Sources) > 65 {
		return nil, state.ErrDurableStorageRecovery
	}
	commit := *pending.Commit
	frame.resolvedCommit = &commit
	frame.resolvedKey = pending.MetadataKey
	frame.resolved = [65]bool{}
	resolver := func(ctx context.Context, index int) ([]byte, func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		source := pending.Sources[index]
		mark := len(s.retainedReleases)
		release := func() { s.releaseRetainedAfter(mark) }
		var data []byte
		var err error
		switch source.Payload.Kind {
		case state.PublicationInline:
			data = source.Payload.Inline
		case state.PublicationRemote:
			data, err = s.historyGet(source.Reference.Key, int64(source.Reference.CompressedBytes))
		case state.PublicationAdmissionStage:
			return nil, nil, state.ErrDurableStorageRecovery
		case state.PublicationHistoryStage:
			stage := state.PendingSource{Reference: source.Reference, Name: source.Reference.SHA256 + ".gz"}
			data, err = s.historyStage(stage)
			if errors.Is(err, os.ErrNotExist) {
				data, err = s.historyGet(source.Reference.Key, int64(source.Reference.CompressedBytes))
				if err == nil {
					_, err = s.local.StagePublicationSource(s.id(), source.Reference, data)
				}
			}
		default:
			err = errors.New("publication payload is unavailable")
		}
		if err != nil {
			release()
			return nil, nil, err
		}
		if len(data) != source.Reference.CompressedBytes || !storage.VerifySHA256(data, source.Reference.SHA256) {
			release()
			return nil, nil, storage.ErrChecksumMismatch
		}
		if index == 0 {
			_, err = s.decodeReferenced(metadata, data)
		} else {
			_, err = s.decodeRevision(metadata, source.Selection.RevisionID, data)
		}
		if err != nil {
			release()
			return nil, nil, err
		}
		frame.resolved[index] = true
		if source.Payload.Kind == state.PublicationRemote {
			data = nil
		} // Exact read proof never supplies rewrite bytes.
		return data, release, nil
	}
	return storage.PutResolvedSourceSetThenMetadataReadback(s.ctx, publicationTransactionStore{ObjectStore: s.sourceSetRemote(), scan: s, pending: &pending}, sources, pending.MetadataKey, pending.MetadataBytes, prior, s.opts.Retry, resolver, s.verifyHistorySource)
}

func (s *sessionScan) sealPending(p *state.PendingPublication) error {
	if s.usesExistingPublicationProtocol(p) {
		return nil
	}
	policy, err := s.publicationPolicy(p.Bundle)
	if p.History != nil && p.History.Preparing {
		adapter, e := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
		if e != nil {
			return e
		}
		if p.History.FilterVersion != archive.FilterVersion || p.History.AdapterVersion != adapter.Version() || pendingSkillMode(p.SkillEvidence) != s.opts.skillEvidence() {
			return errors.New("preparation policy no longer current")
		}
		policy = storage.SHA256Hex([]byte(archive.FilterVersion + "\x00" + adapter.Version() + "\x00" + string(s.opts.skillEvidence())))
		err = nil
	}
	if err != nil {
		return err
	}
	if p.JournalVersion == 5 && p.Commit != nil {
		if p.Commit.DestinationID != s.reg.DestinationID || p.Commit.PolicyContext != policy || p.Commit.AdmissionContext != s.publicationAdmission() {
			return errors.New("publication destination, admission or filter policy changed; retain pending evidence and reconcile")
		}
		return p.ValidatePublicationBudgeted(s.ctx, s.readBudget())
	}
	prior := s.published.PublicationPredecessor()
	purpose, err := s.pendingPublicationPurpose(p)
	if err != nil {
		return err
	}
	if purpose != state.PublicationPrivacyRewrite {
		if err := s.bindPublicationContinuity(&prior, *p); err != nil {
			return err
		}
	}
	if p.MetadataOnly {
		purpose = state.PublicationMetadata
	}
	if p.Preparation == nil {
		release, e := state.FreezePublicationNativeTarget(s.ctx, p, s.reg, s.readBudget())
		if e != nil {
			return e
		}
		s.retainedReleases = append(s.retainedReleases, release)
	}
	sealed, err := state.PreparePublicationV2Budgeted(s.ctx, s.readBudget(), *p, prior, s.reg.DestinationID, s.publicationAdmission(), policy, purpose)
	if err != nil {
		return fmt.Errorf("prepare publication: %w", err)
	}
	// Persist the sealed predecessor before any remote write, including legacy replay.
	if err := s.local.SavePending(s.id(), sealed); err != nil {
		return err
	}
	*p = sealed
	return nil
}

func (s *sessionScan) savePending(p *state.PendingPublication) error {
	if s.usesExistingPublicationProtocol(p) {
		return s.local.SavePending(s.id(), *p)
	}
	ready := p.JournalVersion == 5 && p.Commit != nil
	if err := s.sealPending(p); err != nil {
		return err
	}
	if ready {
		return s.local.SavePending(s.id(), *p)
	}
	return nil
}

func (s *sessionScan) usesExistingPublicationProtocol(p *state.PendingPublication) bool {
	if p.Commit != nil || p.History != nil {
		return false
	}
	if p.Selection != nil || p.StagedProof() != nil || p.Catalog != nil || s.reg.StagedImportSHA256 != "" {
		return true
	}
	// Catalog mutation authority is frozen by publishPending before upload.
	// Preserve that protocol even when this candidate has not reached its
	// catalog freeze yet; an ordinary candidate must not be sealed as v5 first.
	remote, ok := s.sourceSetProvider().(storage.CatalogPublisher)
	return ok && remote.CatalogMetadataAuthority()
}

func (s *sessionScan) publicationPolicy(bundle archive.SourceBundle) (string, error) {
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return "", fmt.Errorf("active publication privacy policy unavailable: %w", err)
	}
	if bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() {
		return "", errors.New("pending source privacy policy changed; retain evidence and refilter before publication")
	}
	return storage.SHA256Hex([]byte(archive.FilterVersion + "\x00" + adapter.Version() + "\x00" + string(s.opts.skillEvidence()))), nil
}

func (s *sessionScan) publicationAdmission() string {
	body, _ := json.Marshal(struct {
		Session   string                `json:"Session"`
		Native    string                `json:"Native"`
		Project   string                `json:"Project"`
		Admission string                `json:"Admission"`
		Origin    archive.SessionOrigin `json:"Origin"`
		Batch     archive.ImportBatch   `json:"Batch"`
	}{Session: s.reg.ArchiveSessionID, Native: s.reg.NativeSessionID, Project: s.reg.ProjectID, Admission: s.reg.Admitted().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), Origin: s.reg.Origin, Batch: s.reg.ImportBatch})
	return storage.SHA256Hex(body)
}

func (s *sessionScan) bindPublicationContinuity(prior *state.PublicationPredecessor, pending state.PendingPublication) error {
	if prior.State != state.PredecessorPresent || prior.Bundle.History == nil || pending.Bundle.History == nil || prior.Bundle.History.ActiveRolloutID != pending.Bundle.History.ActiveRolloutID {
		return nil
	}
	var previous archive.Metadata
	if err := json.Unmarshal(prior.Body, &previous); err != nil {
		return err
	}
	if previous.SourceBundle.SHA256 == pending.SourceSHA256 {
		return nil
	}
	a, b := prior.Bundle.Capture, pending.Bundle.Capture
	if a.FilterVersion != b.FilterVersion || a.AdapterVersion != b.AdapterVersion || a.SourceFormat != b.SourceFormat || a.AdapterName != b.AdapterName {
		return errors.New("same revision continuation requires matching filter and codec")
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return err
	}
	if a.FilterVersion != archive.FilterVersion || a.AdapterVersion != adapter.Version() || !adapter.EvidenceExtends(prior.Bundle, pending.Bundle) {
		return errors.New("native retained comparator refused same revision continuation")
	}
	prior.SameRevisionContinuity = &state.PublicationContinuity{PreviousSourceSHA256: previous.SourceBundle.SHA256, NextSourceSHA256: pending.SourceSHA256}
	return nil
}

func (s *sessionScan) recordSourceSetRetirement(pending state.PendingPublication) error {
	var next archive.Metadata
	if err := s.unmarshalRetained(pending.MetadataBytes, &next); err != nil {
		return err
	}
	refs, err := next.SourceReferences()
	if err != nil {
		return err
	}
	protected := map[string]bool{}
	for _, ref := range refs {
		protected[ref.Key] = true
	}
	if pending.History != nil {
		for _, retired := range pending.History.Retired {
			if err := s.local.RecordSupersededWithPrivacy(s.id(), retired.Reference.Key, retired.RetiredAt, retired.PrivacySensitive); err != nil {
				return fmt.Errorf("record retired revision cleanup: %w", err)
			}
		}
	}
	if pending.History == nil && pending.Cleanup != nil {
		for _, retired := range pending.Cleanup.Retired {
			if protected[retired.Reference.Key] {
				continue
			}
			if err := s.local.RecordSupersededWithPrivacy(s.id(), retired.Reference.Key, retired.RetiredAt, retired.PrivacySensitive); err != nil {
				return fmt.Errorf("record ordinary privacy cleanup: %w", err)
			}
		}
	}
	if previous, hadPrevious := s.published.LastPublishedSource(); hadPrevious && !protected[previous.Key] {
		priorBundle, _, havePrior := s.published.LastPublished()
		privacySensitive := havePrior && priorBundle.Capture.FilterVersion != pending.Bundle.Capture.FilterVersion
		if err := s.local.RecordSupersededWithPrivacy(s.id(), previous.Key, s.now, privacySensitive); err != nil {
			if privacySensitive {
				// Keep the pending publication for another attempt. Saving the
				// new published state here would lose the only retry path for
				// this old-filter source, leaving it until session expiry.
				return fmt.Errorf("record privacy-sensitive predecessor: %w", err)
			}
			s.warn(fmt.Errorf("record superseded source for cleanup: %w", err))
		}
	}
	return nil
}

func (s *sessionScan) pendingPublicationPurpose(p *state.PendingPublication) (state.PublicationPurpose, error) {
	purpose := state.PublicationCapture
	if p.Preparation != nil {
		purpose = p.Preparation.Purpose
	} else if p.History != nil && p.History.Preparing {
		for _, input := range p.History.Inputs {
			if input.FilterVersion != archive.FilterVersion {
				purpose = state.PublicationPrivacyRewrite
				break
			}
		}
		if p.Bundle.Capture.FilterVersion != archive.FilterVersion || p.Bundle.Capture.AdapterVersion != p.History.AdapterVersion {
			purpose = state.PublicationPrivacyRewrite
		}
	}
	if p.Preparation == nil && p.History != nil && p.History.Preparing {
		needed, e := s.freezeNativeHeaderInputs(p)
		if e != nil {
			return purpose, e
		}
		if needed {
			purpose = state.PublicationPrivacyRewrite
		}
	}
	return purpose, nil
}
