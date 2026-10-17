package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	if p.Catalog != nil {
		return outcomeSkipped, state.ErrCatalogJournalFrozen
	}
	endAttempt, attemptErr := s.beginPublicationAttempt()
	if attemptErr != nil {
		return outcomeSkipped, attemptErr
	}
	defer endAttempt()
	committed, err := s.checkFrozenHistoryMetadata(p)
	if err != nil {
		return outcomeSkipped, err
	}
	if committed {
		if p.History.Preparing {
			return outcomeSkipped, errors.New("remote sidecar matches incomplete private preparation; refuse acknowledgement")
		}
		if p.JournalVersion != 5 {
			legacyPolicy := (state.PublicationPolicy{FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SkillEvidence: pendingSkillMode(p.SkillEvidence)}).Context()
			prior := s.published.PublicationPredecessor()
			sealed, e := state.PreparePublicationV2(p, prior, s.reg.DestinationID, s.publicationAdmission(), legacyPolicy, state.PublicationCapture)
			if e != nil {
				return outcomeSkipped, e
			}
			if e = s.local.SavePending(s.id(), sealed); e != nil {
				return outcomeSkipped, e
			}
			p = sealed
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
			if p.JournalVersion == 5 {
				if err := s.local.SavePending(s.id(), p); err != nil {
					return outcomeSkipped, err
				}
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
	if err := s.savePending(&successor); err != nil {
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
	acknowledged, acknowledgedBody, expected, err := s.stricterAcknowledgedPredecessor(p, metadata, committed)
	if err != nil {
		return state.PendingPublication{}, err
	}
	var ackInputs []state.HistoryInput
	inputs, ackInputs, err = s.addAcknowledgedHistoryInputs(&metadata, acknowledged, inputs)
	if err != nil {
		return state.PendingPublication{}, err
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
	next := newStricterPreparingPublication(p, adapter.Version(), expected)

	nativeRelease, e := state.FreezePublicationNativeTarget(s.ctx, &next, s.reg, s.readBudget())
	if e != nil {
		return state.PendingPublication{}, e
	}
	s.retainedReleases = append(s.retainedReleases, nativeRelease)

	finalMetadata := metadata
	if err = s.retainCharge(int64(len(metadata.History.Preserved)+1) * 2048); err != nil {
		return state.PendingPublication{}, err
	}
	originalHistory := *metadata.History
	originalHistory.Preserved = append([]archive.RevisionReference(nil), metadata.History.Preserved...)
	metadata.History = &originalHistory
	finalBody := p.MetadataBytes
	// Freeze the chosen original selection before transforming any output.
	// The acknowledged body remains a distinct singleton predecessor authority.
	next.History.Inputs = append([]state.HistoryInput(nil), inputs...)
	if err := s.stageStricterOriginalInputs(&next, &metadata, inputs); err != nil {
		return state.PendingPublication{}, err
	}
	next.SourceBytes = nil
	next.MetadataBytes, err = s.marshalRetained(metadata)
	if err != nil {
		return state.PendingPublication{}, err
	}
	priorState := state.PredecessorAbsent
	var priorBody []byte
	var priorBundle archive.SourceBundle
	if expected != "" {
		priorState = state.PredecessorPresent
		priorBody = acknowledgedBody
		priorBundle, err = s.loadHistoryInput(acknowledged, ackInputs[0])
		if err != nil {
			return state.PendingPublication{}, err
		}
	}
	prior := state.PublicationPredecessor{State: priorState, Body: priorBody, Bundle: priorBundle}
	hookObservations := ownedPublicationHookObservations(p.Bundle.SupplementalEvidence)
	if err := s.freezePublicationHooks(&next, hookObservations, adapter.Version()); err != nil {
		return state.PendingPublication{}, err
	}
	next, err = state.PreparePublicationV2(next, prior, s.reg.DestinationID, s.publicationAdmission(), storage.SHA256Hex([]byte(archive.FilterVersion+"\x00"+adapter.Version()+"\x00"+string(s.opts.skillEvidence()))), state.PublicationPrivacyRewrite)
	if err != nil {
		return state.PendingPublication{}, err
	}
	next.SkillEvidence = string(s.opts.skillEvidence())
	if err := s.transformStricterHistoryInputs(&next, &metadata, inputs, finalMetadata, finalBody, finalInputs, acknowledged, acknowledgedBody, ackInputs, adapter, pendingSkillMode(p.SkillEvidence)); err != nil {
		return state.PendingPublication{}, err
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
	next.JournalVersion = 5
	next.Phase = state.PublicationReady
	next, err = state.PreparePublicationV2(next, prior, s.reg.DestinationID, s.publicationAdmission(), next.Preparation.PolicyContext, state.PublicationPrivacyRewrite)
	if err != nil {
		return state.PendingPublication{}, err
	}
	err = next.ValidateHistoryBudgeted(s.id(), s.readBudget())
	return next, err
}

// pendingManifestInputs upgrades original parent provenance before verifying the
// mixed original/prepared manifest, without reading current native data.
func (s *sessionScan) pendingManifestInputs(metadata archive.Metadata, p state.PendingPublication, acknowledged, inputs []state.HistoryInput) ([]state.HistoryInput, error) {
	for i := range inputs {
		if inputs[i].ParentSessionID != nil && inputs[i].NativeChild != nil && inputs[i].AdapterVersion != "" {
			continue
		}
		for _, original := range acknowledged {
			if inputs[i].RevisionID == original.RevisionID && inputs[i].Reference == original.Reference {
				inputs[i].ParentSessionID = original.ParentSessionID
				inputs[i].NativeChild = original.NativeChild
				inputs[i].AdapterVersion = original.AdapterVersion
			}
		}
		if inputs[i].ParentSessionID != nil && inputs[i].NativeChild != nil && inputs[i].AdapterVersion != "" {
			continue
		}
		mark := len(s.retainedReleases)
		bundle, err := s.loadHistoryInput(metadata, inputs[i])
		if err != nil {
			return nil, err
		}
		keep := len(s.retainedReleases)
		if err := s.retainCharge(int64(len(bundle.ParentSessionID) + len(bundle.Capture.AdapterVersion))); err != nil {
			return nil, err
		}
		parent := strings.Clone(bundle.ParentSessionID)
		inputs[i].ParentSessionID = &parent
		marker := bundle.NativeChild
		inputs[i].NativeChild = &marker
		inputs[i].AdapterVersion = strings.Clone(bundle.Capture.AdapterVersion)
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
	success := false
	defer func() {
		if !success {
			s.releaseRetainedAfter(mark)
		}
	}()
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
		keep := len(s.retainedReleases)
		if err := s.retainCharge(int64(len(selected.ParentSessionID)+len(bundle.Capture.FilterVersion)+len(bundle.Capture.AdapterVersion)) + 128); err != nil {
			return nil, err
		}
		parent := strings.Clone(selected.ParentSessionID)
		marker := bundle.NativeChild
		inputs = append(inputs, state.HistoryInput{NativeChild: &marker, ParentSessionID: &parent, Reference: ref, RevisionID: id, CapturedAt: bundle.Capture.CapturedAt, FilterVersion: strings.Clone(bundle.Capture.FilterVersion), AdapterVersion: strings.Clone(bundle.Capture.AdapterVersion), SourceSchemaVersion: bundle.SchemaVersion})
		s.keepRetainedFrom(mark, keep)
	}
	success = true
	return inputs, nil
}

// prepareRetainedHistoryWork runs before the temporary mutation fence, enabling
// only private preparation and exact already-committed acknowledgement. Actual
// publication, retention and generation changes still require checkpoint 5.
func (s *sessionScan) prepareRetainedHistoryWork(ctx context.Context) (sessionOutcome, bool, error) {
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
	if p, found, err := s.local.LoadPublicationPending(s.id()); err != nil {
		return outcomeSkipped, true, err
	} else if found {
		if p.History == nil {
			return outcomeSkipped, true, errors.New("ordinary pending journal cannot replace retained history")
		}
		outcome, err := s.resumeHistory(ctx, p)
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

func (s *sessionScan) prepareStricterHistoryInput(next *state.PendingPublication, index int, input state.HistoryInput, finalMetadata archive.Metadata, finalBody []byte, finalInputs []state.HistoryInput, acknowledged archive.Metadata, acknowledgedBody []byte, ackInputs []state.HistoryInput, adapter agentapi.TranscriptFilter, ceiling config.SkillEvidence, total *int) (archive.SourceBundle, archive.SourceReference, state.PendingSource, func(), error) {
	frozen := next.Preparation.Inputs[index]
	reader := publicationPrivacyReader{scan: s}
	raw, closeBytes, err := reader.ReadPublicationPrivacySource(s.ctx, frozen)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	defer closeBytes()
	if len(raw) > (128<<20)-*total {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, storage.ErrObjectTooLarge
	}
	*total += len(raw)
	var alternatives []state.PublicationPrivacyAlternative
	add := func(owner archive.Metadata, body []byte, others []state.HistoryInput) {
		for _, other := range others {
			if other.RevisionID != input.RevisionID || other.Reference == input.Reference {
				continue
			}
			alt := state.PreparationInput{ParentSessionID: other.ParentSessionID, NativeChild: other.NativeChild, Reference: other.Reference, Selection: state.PublicationSelection{Role: frozen.Selection.Role, RevisionID: other.RevisionID, CapturedAt: other.CapturedAt.UTC(), SourceSchemaVersion: other.SourceSchemaVersion}, FilterVersion: other.FilterVersion, AdapterVersion: other.AdapterVersion, SkillPolicy: string(ceiling)}
			policy := state.PublicationPolicy{FilterVersion: alt.FilterVersion, AdapterVersion: alt.AdapterVersion, SkillEvidence: ceiling}
			alternatives = append(alternatives, state.PublicationPrivacyAlternative{Metadata: owner, MetadataBody: body, Input: alt, Policy: policy})
		}
	}
	add(finalMetadata, finalBody, finalInputs)
	add(acknowledged, acknowledgedBody, ackInputs)
	var origin archive.Metadata
	if err = s.unmarshalRetained(next.Preparation.OriginMetadata, &origin); err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	filtered, packed, ref, proof, closeFactory, err := state.RefilterCoveredPublicationInput(s.ctx, s.reg, adapter, origin, next.Preparation.OriginMetadata, frozen, index, raw, state.PublicationContext{NativeTarget: next.Preparation.NativeTarget, DestinationID: s.reg.DestinationID, AdmissionContext: s.publicationAdmission()}, state.PublicationPolicy{FilterVersion: frozen.FilterVersion, AdapterVersion: frozen.AdapterVersion, SkillEvidence: pendingSkillMode(frozen.SkillPolicy)}, state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), SkillEvidence: s.opts.skillEvidence()}, ceiling, s.readBudget(), reader, alternatives)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	// The caller promotes the actual factory output lease before ending input.
	s.retainedReleases = append(s.retainedReleases, closeFactory)
	if len(packed.Bytes) > (128<<20)-*total {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, storage.ErrObjectTooLarge
	}
	*total += len(packed.Bytes)
	stage, err := s.local.StagePublicationSource(s.id(), ref, packed.Bytes)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	receiptRelease, recordErr := next.RecordPrivacyOutput(proof)
	if recordErr != nil {
		err = recordErr
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	return filtered, ref, stage, receiptRelease, nil
}

func (s *sessionScan) freezeRetainedMaintenance(authority archive.Metadata, bundle archive.SourceBundle, adapterVersion string) (state.PendingPublication, error) {
	// Header maintenance transforms the exact committed current input through
	// the same frozen receipt factory as preserved inputs. Pre-freeze hook
	// normalization must not replace that original reference.
	headerMaintenance := nativeParentResolved(s.reg, bundle) || nativeChildMarkerPending(s.reg, bundle)
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
		stage, err := s.local.StagePublicationSource(s.id(), input.Reference, raw)
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
	// Same-policy observations are a new capture selection, normalized before
	// the immutable preparation freezes its inputs. Privacy transformations keep
	// the exact old input and require their factory-owned transformation receipt.
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return state.PendingPublication{}, err
	}
	if err := s.normalizeRetainedMaintenanceCurrent(&p, &authority, bundle, adapter, headerMaintenance); err != nil {
		return state.PendingPublication{}, err
	}
	if headerMaintenance || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		if err := s.freezePublicationHooks(&p, observations, adapter.Version()); err != nil {
			return state.PendingPublication{}, err
		}
	}
	if err := s.savePending(&p); err != nil {
		return state.PendingPublication{}, err
	}
	return p, nil
}

func (s *sessionScan) normalizeRetainedMaintenanceCurrent(p *state.PendingPublication, authority *archive.Metadata, bundle archive.SourceBundle, adapter agentapi.TranscriptFilter, headerMaintenance bool) error {
	for i, input := range p.History.Inputs {
		if input.RevisionID != authority.History.CurrentRevision {
			continue
		}
		if headerMaintenance || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
			break
		}
		if err := s.prepareHistoryInput(p, authority, input); err != nil {
			return err
		}
		parent := p.Bundle.ParentSessionID
		marker := p.Bundle.NativeChild
		p.History.Inputs[i] = state.HistoryInput{ParentSessionID: &parent, NativeChild: &marker, Reference: authority.SourceBundle, RevisionID: input.RevisionID, CapturedAt: input.CapturedAt, FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SourceSchemaVersion: p.Bundle.SchemaVersion}
		refs, err := authority.SourceReferences()
		if err != nil {
			return err
		}
		stages := p.History.Sources[:0]
		for _, stage := range p.History.Sources {
			if slices.Contains(refs, stage.Reference) {
				stages = append(stages, stage)
			}
		}
		p.History.Sources = stages
		p.MetadataBytes, err = s.marshalRetained(*authority)
		if err != nil {
			return err
		}
		break
	}
	return nil
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

// freezePublicationHooks consumes only the owned request/candidate facts before
// the first preparation; later slices cannot consult newer request observations.
func (s *sessionScan) freezePublicationHooks(p *state.PendingPublication, observations []archive.SupplementalEvidence, adapterVersion string) error {
	if len(observations) == 0 {
		return nil
	}
	raw, err := s.marshalRetained(observations)
	if err != nil {
		return err
	}
	policy := state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion, SkillEvidence: s.opts.skillEvidence()}
	return state.FreezePublicationHookObservations(s.ctx, p, raw, s.reg.DestinationID, s.publicationAdmission(), policy.Context(), s.readBudget())
}

func (s *sessionScan) stageStricterOriginalInputs(next *state.PendingPublication, metadata *archive.Metadata, inputs []state.HistoryInput) error {
	for _, input := range inputs {
		if input.RevisionID == metadata.History.CurrentRevision {
			metadata.SourceBundle, metadata.CapturedAt, metadata.FilterVersion = input.Reference, input.CapturedAt, input.FilterVersion
			var err error
			next.Bundle, err = s.loadHistoryInput(*metadata, input)
			if err != nil {
				return err
			}
			next.SourceKey, next.SourceSHA256, next.SourceSize = input.Reference.Key, input.Reference.SHA256, input.Reference.CompressedBytes
		} else {
			for i := range metadata.History.Preserved {
				r := &metadata.History.Preserved[i]
				if r.RevisionID == input.RevisionID {
					r.Source, r.CapturedAt, r.FilterVersion, r.SourceSchemaVersion = input.Reference, input.CapturedAt, input.FilterVersion, input.SourceSchemaVersion
				}
			}
		}
		mark := len(s.retainedReleases)
		raw, e := s.readRetainedInputBytes(input.Reference)
		if e != nil {
			return e
		}
		_, e = s.local.StagePublicationSource(s.id(), input.Reference, raw)
		s.releaseRetainedAfter(mark)
		if e != nil {
			return e
		}
	}
	return nil
}

func (s *sessionScan) addAcknowledgedHistoryInputs(metadata *archive.Metadata, acknowledged archive.Metadata, inputs []state.HistoryInput) ([]state.HistoryInput, []state.HistoryInput, error) {
	var ackInputs []state.HistoryInput
	var err error
	if acknowledged.SessionID != "" {
		ackInputs, err = s.retainedManifestInputs(acknowledged, nil)
		if err != nil {
			return nil, nil, err
		}
		for _, input := range ackInputs {
			if hasRevisionInput(inputs, input.RevisionID) {
				continue
			}
			if len(metadata.History.Preserved) >= archive.MaxHistorySpans {
				return nil, nil, errors.New("stricter successor exceeds retained revision limit")
			}
			metadata.History.Preserved = append(metadata.History.Preserved, archive.RevisionReference{RevisionID: input.RevisionID, CapturedAt: input.CapturedAt, Source: input.Reference, FilterVersion: input.FilterVersion, SourceSchemaVersion: input.SourceSchemaVersion})
			inputs = append(inputs, input)
		}
	}
	return inputs, ackInputs, nil
}

func (s *sessionScan) stricterAcknowledgedPredecessor(p state.PendingPublication, metadata archive.Metadata, committed bool) (archive.Metadata, []byte, string, error) {
	acknowledged := archive.Metadata{}
	var acknowledgedBody []byte
	expected := p.History.ExpectedMetadataSHA256
	if committed {
		expected = metadataSHA(p.MetadataBytes)
		acknowledged = metadata
		acknowledgedBody = p.MetadataBytes
	} else if expected != "" {
		raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
		if err != nil {
			return archive.Metadata{}, nil, "", err
		}
		acknowledgedBody = raw
		if metadataSHA(raw) != expected {
			return archive.Metadata{}, nil, "", errHistoryMetadataConflict
		}
		if err := s.unmarshalRetained(raw, &acknowledged); err != nil {
			return archive.Metadata{}, nil, "", err
		}
		if err := s.validateAuthorityIdentity(acknowledged); err != nil {
			return archive.Metadata{}, nil, "", err
		}
	}
	return acknowledged, acknowledgedBody, expected, nil
}

func ownedPublicationHookObservations(evidence []archive.SupplementalEvidence) []archive.SupplementalEvidence {
	var hookObservations []archive.SupplementalEvidence
	for _, item := range evidence {
		if item.Kind == archive.EvidenceKindLinkedSession || item.Kind == archive.EvidenceKindExplicitFeedback {
			hookObservations = append(hookObservations, item)
		}
	}
	return hookObservations
}

func (s *sessionScan) transformStricterHistoryInputs(next *state.PendingPublication, metadata *archive.Metadata, inputs []state.HistoryInput, finalMetadata archive.Metadata, finalBody []byte, finalInputs []state.HistoryInput, acknowledged archive.Metadata, acknowledgedBody []byte, ackInputs []state.HistoryInput, adapter agentapi.TranscriptFilter, ceiling config.SkillEvidence) error {
	var total int
	for index, input := range inputs {
		mark := len(s.retainedReleases)
		filtered, ref, stage, receiptRelease, err := s.prepareStricterHistoryInput(next, index, input, finalMetadata, finalBody, finalInputs, acknowledged, acknowledgedBody, ackInputs, adapter, ceiling, &total)
		if err != nil {
			return err
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
			return err
		}
		if err := replacePreparedReference(next, metadata, input, filtered, ref, data); err != nil {
			return err
		}
		next.History.Sources = append(next.History.Sources, stage)
		s.releaseStricterAlternative(mark, input.RevisionID, metadata.History.CurrentRevision)
		s.retainedReleases = append(s.retainedReleases, receiptRelease)
	}
	return nil
}

func newStricterPreparingPublication(p state.PendingPublication, adapterVersion, expected string) state.PendingPublication {
	next := p
	next.JournalVersion = 0
	next.Phase = ""
	next.Commit = nil
	next.Preparation = nil
	next.Progress = nil
	next.Cleanup = nil
	next.Sources = nil
	// A retained policy successor did not consume native input under this
	// policy. Preserve its pending native-read obligation, never a settled proof.
	next.ScanSignature = nil
	next.History = &state.PendingHistory{Version: 1, Preparing: true, FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion, ExpectedMetadataSHA256: expected, Retired: append([]state.RetiredSource(nil), p.History.Retired...)}
	next.Attempted = false
	next.MetadataOnly = false
	next.SkillEvidence = p.SkillEvidence
	// Keep the covered request owed until the stricter selecting successor.

	return next
}
