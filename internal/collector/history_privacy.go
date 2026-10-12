package collector

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// resumeStricterHistory resolves exact remote status before preparing any
// replacement. Unknown status keeps the attempted descriptor and its evidence.
func (s *sessionScan) resumeStricterHistory(p state.PendingPublication) (sessionOutcome, error) {
	committed, err := s.checkFrozenHistoryMetadata(p)
	if err != nil {
		return outcomeSkipped, err
	}
	if committed {
		if p.History.Preparing {
			return outcomeSkipped, errors.New("remote sidecar matches incomplete private preparation; refuse acknowledgement")
		}
		if err := s.verifyHistoryReadback(p); err != nil {
			return outcomeSkipped, err
		}
		metadata, err := s.frozenHistoryMetadata(p)
		if err != nil {
			return outcomeSkipped, err
		}
		data, err := s.historyGet(metadata.SourceBundle.Key, int64(metadata.SourceBundle.CompressedBytes))
		if err != nil {
			return outcomeSkipped, err
		}
		p.Bundle, err = s.decodeReferenced(metadata, data)
		if err != nil {
			return outcomeSkipped, err
		}
		// Queue before acknowledging: a crash cannot leave a committed broader set
		// with neither an exact retry nor a durable maintenance obligation.
		if !p.History.MaintenanceOwed {
			p.History.MaintenanceOwed = true
			if err := s.local.SavePending(s.id(), p); err != nil {
				return outcomeSkipped, err
			}
		}
		if _, err := s.acknowledgePublication(p, true); err != nil {
			return outcomeSkipped, err
		}
	}
	successor, err := s.stricterHistorySuccessor(p, committed)
	if err != nil {
		return outcomeSkipped, err
	}
	// Staging can take several bounded reads. A predecessor that changes during
	// that work cannot authorize replacement of the original retry descriptor.
	stillCommitted, err := s.checkFrozenHistoryMetadata(p)
	if err != nil {
		return outcomeSkipped, err
	}
	if stillCommitted != committed {
		return outcomeSkipped, errHistoryMetadataConflict
	}
	// Every retained input and stricter output is staged before replacement.
	if err := s.local.SavePending(s.id(), successor); err != nil {
		return outcomeSkipped, err
	}
	return outcomeSkipped, archive.ErrHistoryMutationPending
}

// stricterHistorySuccessor consumes retained original stages, final sources and
// acknowledged sources sequentially. It never reads a current native source or
// skill inventory, and never assigns maintenance time as a capture time.
func (s *sessionScan) stricterHistorySuccessor(p state.PendingPublication, committed bool) (state.PendingPublication, error) {
	metadata, err := s.frozenHistoryMetadata(p)
	if err != nil {
		return state.PendingPublication{}, err
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return state.PendingPublication{}, err
	}
	inputs := append([]state.HistoryInput(nil), p.History.Inputs...)
	acknowledged := archive.Metadata{}
	expected := p.History.ExpectedMetadataSHA256
	if committed {
		expected = metadataSHA(p.MetadataBytes)
		acknowledged = metadata
	} else if expected != "" {
		raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
		if err != nil {
			return state.PendingPublication{}, err
		}
		if metadataSHA(raw) != expected {
			return state.PendingPublication{}, errHistoryMetadataConflict
		}
		if err := s.unmarshalRetained(raw, &acknowledged); err != nil {
			return state.PendingPublication{}, err
		}
		if err := s.validateAuthorityIdentity(acknowledged); err != nil {
			return state.PendingPublication{}, err
		}
	}
	var ackInputs []state.HistoryInput
	if acknowledged.SessionID != "" {
		ackInputs, err = s.retainedManifestInputs(acknowledged, nil)
		if err != nil {
			return state.PendingPublication{}, err
		}
		for _, input := range ackInputs {
			if hasRevisionInput(inputs, input.RevisionID) {
				continue
			}
			if len(metadata.History.Preserved) >= archive.MaxHistorySpans {
				return state.PendingPublication{}, errors.New("stricter successor exceeds retained revision limit")
			}
			metadata.History.Preserved = append(metadata.History.Preserved, archive.RevisionReference{RevisionID: input.RevisionID, CapturedAt: input.CapturedAt, Source: input.Reference, FilterVersion: input.FilterVersion, SourceSchemaVersion: input.SourceSchemaVersion})
			inputs = append(inputs, input)
		}
	}
	// Older ready journals may lack Inputs. Their bounded validated final bytes
	// supply provenance individually; no active provenance is copied to siblings.
	finalInputs, err := s.pendingManifestInputs(metadata, p, ackInputs, inputs)
	if err != nil {
		return state.PendingPublication{}, err
	}
	for _, input := range finalInputs {
		if !hasRevisionInput(inputs, input.RevisionID) {
			inputs = append(inputs, input)
		}
	}
	if len(inputs) > archive.MaxHistorySpans+1 {
		return state.PendingPublication{}, errors.New("stricter successor exceeds input limit")
	}
	next := p
	// A retained policy successor did not consume native input under this
	// policy. Preserve its pending native-read obligation, never a settled proof.
	next.ScanSignature = nil
	next.History = &state.PendingHistory{Version: 1, Preparing: true, FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), ExpectedMetadataSHA256: expected, Retired: append([]state.RetiredSource(nil), p.History.Retired...)}
	next.Attempted = false
	next.MetadataOnly = false
	next.SkillEvidence = string(s.opts.skillEvidence())
	if committed {
		next.RequestToken = ""
	}
	var total int
	for _, input := range inputs {
		mark := len(s.retainedReleases)
		filtered, ref, stage, err := s.prepareStricterHistoryInput(metadata, input, append(append([]state.HistoryInput(nil), finalInputs...), ackInputs...), adapter, pendingSkillMode(p.SkillEvidence), &total)
		if err != nil {
			return state.PendingPublication{}, err
		}

		// Use the chosen original's immutable capture provenance throughout.
		if input.RevisionID == metadata.History.CurrentRevision {
			metadata.SourceBundle, metadata.CapturedAt, metadata.FilterVersion = input.Reference, input.CapturedAt, input.FilterVersion
		} else {
			for i := range metadata.History.Preserved {
				r := &metadata.History.Preserved[i]
				if r.RevisionID == input.RevisionID {
					r.Source, r.CapturedAt = input.Reference, input.CapturedAt
				}
			}
		}
		data, err := s.historyStage(stage)
		if err != nil {
			return state.PendingPublication{}, err
		}
		if err := replacePreparedReference(&next, &metadata, input, filtered, ref, data); err != nil {
			return state.PendingPublication{}, err
		}
		next.History.Sources = append(next.History.Sources, stage)
		next.History.Inputs = append(next.History.Inputs, input)
		s.releaseStricterAlternative(mark, input.RevisionID, metadata.History.CurrentRevision)
	}
	if err := retireStricterHistoryReferences(&next, metadata, append(append(append([]state.HistoryInput(nil), inputs...), finalInputs...), ackInputs...), s.now); err != nil {
		return state.PendingPublication{}, err
	}
	next.History.Preparing = false
	next.History.PrivacyCursor = len(next.History.Inputs)
	next.History.PreparedAt = s.now
	if err := s.derivePreparedHistory(&next, metadata); err != nil {
		return state.PendingPublication{}, err
	}
	var derived archive.Metadata
	if err := s.unmarshalRetained(next.MetadataBytes, &derived); err != nil {
		return state.PendingPublication{}, err
	}
	if err := boundFrozenReferences(derived); err != nil {
		return state.PendingPublication{}, err
	}
	return next, next.ValidateHistoryBudgeted(s.id(), s.readBudget())
}

// pendingManifestInputs upgrades original parent provenance before verifying the
// mixed original/prepared manifest, without reading current native data.
func (s *sessionScan) pendingManifestInputs(metadata archive.Metadata, p state.PendingPublication, acknowledged, inputs []state.HistoryInput) ([]state.HistoryInput, error) {
	for i := range inputs {
		if inputs[i].ParentSessionID != nil {
			continue
		}
		for _, original := range acknowledged {
			if inputs[i].RevisionID == original.RevisionID && inputs[i].Reference == original.Reference {
				inputs[i].ParentSessionID = original.ParentSessionID
			}
		}
		if inputs[i].ParentSessionID != nil {
			continue
		}
		mark := len(s.retainedReleases)
		bundle, err := s.loadHistoryInput(metadata, inputs[i])
		if err != nil {
			return nil, err
		}
		keep := len(s.retainedReleases)
		if err := s.retainCharge(int64(len(bundle.ParentSessionID))); err != nil {
			return nil, err
		}
		parent := strings.Clone(bundle.ParentSessionID)
		inputs[i].ParentSessionID = &parent
		s.keepRetainedFrom(mark, keep)
	}
	frozen := p
	history := *p.History
	history.Inputs = inputs
	frozen.History = &history
	return s.retainedManifestInputs(metadata, &frozen)
}

func hasRevisionInput(inputs []state.HistoryInput, id string) bool {
	for _, input := range inputs {
		if input.RevisionID == id {
			return true
		}
	}
	return false
}

func (s *sessionScan) retainedManifestInputs(metadata archive.Metadata, pending *state.PendingPublication) ([]state.HistoryInput, error) {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return nil, err
	}
	if err := boundFrozenReferences(metadata); err != nil {
		return nil, err
	}
	refs, err := metadata.SourceReferences()
	if err != nil {
		return nil, err
	}
	inputs := make([]state.HistoryInput, 0, len(refs))
	for i, ref := range refs {
		mark := len(s.retainedReleases)
		id := metadata.NativeSessionID
		if metadata.History != nil {
			id = metadata.History.CurrentRevision
		}
		if i > 0 {
			id = metadata.History.Preserved[i-1].RevisionID
		}
		data, err := s.historyStage(state.PendingSource{Reference: ref, Name: ref.SHA256 + ".gz"})
		if err != nil {
			data, err = s.historyGet(ref.Key, int64(ref.CompressedBytes))
		}
		if err != nil {
			return nil, err
		}
		selected := metadata
		if pending != nil {
			for _, original := range pending.History.Inputs {
				if original.RevisionID != id {
					continue
				}
				if original.Reference == ref && original.ParentSessionID != nil {
					selected.ParentSessionID = *original.ParentSessionID
				} else if original.Reference != ref && pending.Bundle.NativeChild {
					selected.ParentSessionID = pending.Bundle.ParentSessionID
				}
			}
		}
		var bundle archive.SourceBundle
		if metadata.History == nil {
			bundle, err = s.decodeReferenced(selected, data)
		} else {
			bundle, err = s.decodeRevision(selected, id, data)
		}
		if err != nil {
			return nil, fmt.Errorf("validate retained successor input: %w", err)
		}
		parent := selected.ParentSessionID
		inputs = append(inputs, state.HistoryInput{ParentSessionID: &parent, Reference: ref, RevisionID: id, CapturedAt: bundle.Capture.CapturedAt, FilterVersion: bundle.Capture.FilterVersion, SourceSchemaVersion: bundle.SchemaVersion})
		s.releaseRetainedAfter(mark)
	}
	return inputs, nil
}

// prepareRetainedHistoryWork runs before the temporary mutation fence, enabling
// only private preparation and exact already-committed acknowledgement. Actual
// publication, retention and generation changes still require checkpoint 5.
func (s *sessionScan) prepareRetainedHistoryWork() (sessionOutcome, bool, error) {
	if len(s.published.Metadata()) == 0 {
		return outcomeSkipped, false, nil
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(s.published.Metadata(), &metadata); err != nil {
		return outcomeSkipped, false, err
	}
	if metadata.History == nil {
		return outcomeSkipped, false, nil
	}
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return outcomeSkipped, true, err
	}
	if s.reg.RetainedOnly() {
		if err := s.retainedOnlyAuthority(); err != nil {
			return outcomeSkipped, true, err
		}
	} else if err := s.local.GenerationCaptureAllowed(s.reg); err != nil {
		return outcomeSkipped, true, err
	}
	if p, found, err := s.local.LoadPending(s.id()); err != nil {
		return outcomeSkipped, true, err
	} else if found {
		if p.History == nil {
			return outcomeSkipped, true, errors.New("ordinary pending journal cannot replace retained history")
		}
		outcome, err := s.resumeHistory(p)
		if err == nil && (s.reg.RetainedOnly()) {
			err = s.recordFrozenSignature()
		}
		return outcome, true, err
	}
	// Strict acknowledged authority is independent of ordinary stale-filter
	// compatibility. Each input is decoded before constructing this journal.
	authority, found, err := s.published.LastPublishedMetadata()
	if err != nil || !found {
		return outcomeSkipped, true, errors.Join(errors.New("complete acknowledged history authority required for maintenance"), err)
	}
	bundle, _, found := s.published.LastPublished()
	if !found {
		return outcomeSkipped, true, errors.New("acknowledged history bundle is missing")
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, true, err
	}
	// A newly resolved native parent changes every retained source header. Reuse
	// sequential maintenance against the original authority before native
	// reconciliation can combine old-parent inputs with a new-parent sidecar.
	needed := s.retainedHistoryMaintenanceNeeded(authority, bundle, adapter.Version())
	for _, revision := range authority.History.Preserved {
		needed = needed || revision.FilterVersion != archive.FilterVersion
	}
	if !needed {
		return outcomeSkipped, false, nil
	}
	p, err := s.freezeRetainedMaintenance(authority, bundle, adapter.Version())
	if err != nil {
		return outcomeSkipped, true, err
	}
	// Prepare one retained ref per pass, freezing policy in the descriptor.
	if err := s.advanceHistoryPreparation(&p); err != nil {
		return outcomeSkipped, true, err
	}
	return outcomeSkipped, true, archive.ErrHistoryMutationPending
}

func (s *sessionScan) retainedHistoryMaintenanceNeeded(authority archive.Metadata, bundle archive.SourceBundle, adapterVersion string) bool {
	projectRenamed := s.reg.HistoricalProject != nil && authority.ProjectName != s.opts.historicalProjectName(s.reg)
	return projectRenamed || nativeParentResolved(s.reg, bundle) || nativeChildMarkerPending(s.reg, bundle) || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapterVersion || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) || authority.Parser.Version != s.parserVersion() || s.reg.RetainedOnly() && s.req.Token != "" || headFingerprint(s.reg.LastHead) != s.publishedLastHead()
}

func (s *sessionScan) prepareStricterHistoryInput(metadata archive.Metadata, input state.HistoryInput, others []state.HistoryInput, adapter agentapi.TranscriptFilter, ceiling config.SkillEvidence, total *int) (archive.SourceBundle, archive.SourceReference, state.PendingSource, error) {
	scope := len(s.retainedReleases)
	bundle, err := s.loadHistoryInput(metadata, input)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	originalBytes := len(s.retainedReleases)
	original, err := s.readRetainedInputBytes(input.Reference)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	if len(original) > (128<<20)-*total {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, errors.New("stricter successor exceeds staged input limit")
	}
	*total += len(original)
	if _, err := s.local.StagePendingSource(s.id(), input.Reference, original); err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	s.releaseRetainedIndex(originalBytes)
	// Retained originals preserve interrupted work, but a broader later policy
	// cannot restore evidence the frozen policy already removed.
	bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, ceiling)
	bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
	filtered, err := s.refilterRetained(adapter, bundle)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	// Final/acknowledged versions must add no evidence absent from the frozen
	// original. Contradiction refuses replacement rather than guessing a union.
	for _, other := range others {
		if other.RevisionID != input.RevisionID || other.Reference == input.Reference {
			continue
		}
		otherMark := len(s.retainedReleases)
		candidate, err := s.loadHistoryInput(metadata, other)
		if err != nil {
			return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
		}
		candidate.SupplementalEvidence = limitSkillEvidence(candidate.SupplementalEvidence, s.opts.skillEvidence())
		candidate, err = s.refilterRetained(adapter, candidate)
		if err != nil {
			return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
		}
		if !ownedEvidenceCovered(adapter, candidate, filtered) {
			return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, errors.New("frozen original does not cover acknowledged revision evidence")
		}
		filtered.SupplementalEvidence = archive.MergeSupplementalEvidence(filtered.SupplementalEvidence, candidate.SupplementalEvidence)
		filtered.Capture.Gaps = mergeCaptureGaps(filtered.Capture.Gaps, candidate.Capture.Gaps)
		// Merged supplemental/header values become an independently owned
		// envelope before the discarded alternative ends its lifetime.
		keep := len(s.retainedReleases)
		filtered, err = s.detachRetainedEnvelope(filtered)
		if err != nil {
			return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
		}
		s.keepRetainedFrom(otherMark, keep)
	}
	returned := len(s.retainedReleases)
	filtered, err = s.refilterRetained(adapter, filtered)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	filtered, err = s.detachRetainedEnvelope(filtered)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	s.keepRetainedFrom(scope, returned)
	packed, err := s.compressSource(filtered)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	if int64(len(packed.Bytes)) > historyCompressedLimit {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, storage.ErrObjectTooLarge
	}
	if len(packed.Bytes) > (128<<20)-*total {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, errors.New("stricter successor exceeds staged output limit")
	}
	*total += len(packed.Bytes)
	key, err := archive.SourceObjectKey(filtered, packed.SHA256)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	stage, err := s.local.StagePendingSource(s.id(), ref, packed.Bytes)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, err
	}
	return filtered, ref, stage, nil
}

func (s *sessionScan) freezeRetainedMaintenance(authority archive.Metadata, bundle archive.SourceBundle, adapterVersion string) (state.PendingPublication, error) {
	inputs, err := s.retainedManifestInputs(authority, nil)
	if err != nil {
		return state.PendingPublication{}, err
	}
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return state.PendingPublication{}, err
	}
	var observations []archive.SupplementalEvidence
	for _, evidence := range s.req.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession || evidence.Kind == archive.EvidenceKindExplicitFeedback {
			observations = append(observations, evidence)
		}
	}
	bundle.SupplementalEvidence = mergeSupplementalEvidence(bundle.SupplementalEvidence, observations)
	// Freeze the target relationship in the existing output envelope; inputs
	// retain their original authority until every header has been prepared.
	if nativeParentResolved(s.reg, bundle) {
		bundle.ParentSessionID = s.reg.ParentSessionID
		bundle.Capture.Gaps = withoutNativeParentPendingGap(bundle.Capture.Gaps)
	}
	if nativeChildMarkerPending(s.reg, bundle) {
		bundle.NativeChild = true
	}
	// Live requests still owe native reads; frozen requests cover retained observations.
	requestToken := ""
	if s.reg.RetainedOnly() {
		requestToken = s.req.Token
	}
	p := state.PendingPublication{SkillEvidence: string(s.opts.skillEvidence()), Bundle: bundle, MetadataKey: key, MetadataBytes: append([]byte(nil), s.published.Metadata()...), RequestToken: requestToken, ReadyAt: s.now, History: &state.PendingHistory{Version: 1, Preparing: true, Inputs: inputs, FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion, ExpectedMetadataSHA256: metadataSHA(s.published.Metadata())}}
	if err := s.local.SweepPendingSources(s.id()); err != nil {
		return state.PendingPublication{}, err
	}
	for _, input := range inputs {
		mark := len(s.retainedReleases)
		raw, err := s.readRetainedInputBytes(input.Reference)
		if err != nil {
			return state.PendingPublication{}, err
		}
		stage, err := s.local.StagePendingSource(s.id(), input.Reference, raw)
		if err != nil {
			return state.PendingPublication{}, err
		}
		p.History.Sources = append(p.History.Sources, stage)
		if input.RevisionID == authority.History.CurrentRevision {
			p.SourceKey, p.SourceSHA256, p.SourceBytes = input.Reference.Key, input.Reference.SHA256, raw
		} else {
			s.releaseRetainedAfter(mark)
		}
	}
	if err := s.local.SavePending(s.id(), p); err != nil {
		return state.PendingPublication{}, err
	}
	return p, nil
}

func retireStricterHistoryReferences(next *state.PendingPublication, metadata archive.Metadata, inputs []state.HistoryInput, at time.Time) error {
	// Keep every obsolete final/acknowledged/input cleanup obligation, including
	// objects prepared but not uploaded. Protection is by the complete final set.
	protected, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	for _, input := range inputs {
		live := false
		for _, ref := range protected {
			live = live || ref == input.Reference
		}
		if live {
			continue
		}
		found := false
		for i := range next.History.Retired {
			if next.History.Retired[i].Reference == input.Reference {
				next.History.Retired[i].PrivacySensitive = true
				found = true
			}
		}
		if !found {
			next.History.Retired = append(next.History.Retired, state.RetiredSource{Reference: input.Reference, RetiredAt: at, PrivacySensitive: true})
		}
	}
	// Remove reactivated refs from inherited cleanup obligations.
	retired := next.History.Retired[:0]
	for _, old := range next.History.Retired {
		live := false
		for _, ref := range protected {
			live = live || old.Reference == ref
		}
		if !live {
			if old.RetiredAt.IsZero() {
				old.RetiredAt = at
			}
			retired = append(retired, old)
		}
	}
	next.History.Retired = retired
	return nil
}

func (s *sessionScan) releaseStricterAlternative(mark int, revision, current string) {
	if revision != current {
		s.releaseRetainedAfter(mark)
	}
}
