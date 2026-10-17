package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// resumeSourceSetPrivacy replaces only ordinary retained work. Ready selections,
// staged imports, Cursor ownership and catalog journals keep their own protocols.
func (s *sessionScan) resumeSourceSetPrivacy(p state.PendingPublication) (sessionOutcome, bool, error) {
	if err := s.validateNativeImportRetained(); err != nil {
		return outcomeSkipped, true, err
	}
	if p.History != nil || s.usesExistingPublicationProtocol(&p) || p.Bundle.SchemaVersion != archive.SourceSchemaVersion {
		return outcomeSkipped, false, nil
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, true, err
	}
	if p.Bundle.Capture.FilterVersion == archive.FilterVersion && p.Bundle.Capture.AdapterVersion == adapter.Version() && pendingSkillMode(p.SkillEvidence) == s.opts.skillEvidence() {
		return outcomeSkipped, false, nil
	}
	end, err := s.beginPublicationAttempt()
	if err != nil {
		return outcomeSkipped, true, err
	}
	defer end()
	if err := s.bindPublicationAttempt(p); err != nil {
		return outcomeSkipped, true, err
	}
	prior, remoteBefore, err := s.ordinaryPrivacyPredecessor(p)
	if err != nil {
		return outcomeSkipped, true, err
	}
	next, err := s.ordinaryPrivacySuccessor(p, prior, adapter)
	if err != nil {
		return outcomeSkipped, true, err
	}
	_, remoteAfter, err := s.ordinaryPrivacyPredecessor(p)
	if err != nil || !bytes.Equal(remoteBefore, remoteAfter) {
		return outcomeSkipped, true, errors.Join(storage.ErrPublicationConflict, err)
	}
	// SavePending retains the old exact descriptor before selecting its stricter
	// successor. The covered request and native scan obligation remain owed.
	if err := s.local.SavePending(s.id(), next); err != nil {
		return outcomeSkipped, true, err
	}
	return outcomeSkipped, true, nil
}

func (s *sessionScan) ordinaryPrivacyPredecessor(p state.PendingPublication) (state.PublicationPredecessor, []byte, error) {
	prior := s.published.PublicationPredecessor()
	if p.Commit != nil {
		if err := p.ValidatePublicationBudgeted(s.ctx, s.readBudget()); err != nil {
			return prior, nil, err
		}
		if p.Commit.DestinationID != s.reg.DestinationID || p.Commit.AdmissionContext != s.publicationAdmission() {
			return prior, nil, state.ErrDurableStorageRecovery
		}
		if p.Commit.Predecessor != prior.State || prior.State == state.PredecessorPresent && storage.SHA256Hex(prior.Body) != p.Commit.PredecessorSHA256 {
			// The next body below remains independently recognizable after an
			// uncertain write; unrelated local state cannot authorize a predecessor.
			prior = state.PublicationPredecessor{State: state.PredecessorUnknown}
		}
	}
	body, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		if prior.State != state.PredecessorAbsent {
			return prior, nil, storage.ErrPublicationConflict
		}
		return prior, nil, nil
	}
	if err != nil {
		return prior, nil, err
	}
	if bytes.Equal(body, p.MetadataBytes) {
		return state.PublicationPredecessor{State: state.PredecessorPresent, Body: p.MetadataBytes, Bundle: p.Bundle}, body, nil
	}
	if prior.State != state.PredecessorPresent || !bytes.Equal(body, prior.Body) {
		return prior, nil, storage.ErrPublicationConflict
	}
	return prior, body, nil
}

func (s *sessionScan) ordinaryPrivacySuccessor(p state.PendingPublication, prior state.PublicationPredecessor, adapter agentapi.TranscriptFilter) (state.PendingPublication, error) {
	next := p
	freezeRelease, err := state.FreezePublicationNativeTarget(s.ctx, &next, s.reg, s.readBudget())
	if err != nil {
		return next, err
	}
	s.retainedReleases = append(s.retainedReleases, freezeRelease)
	policy := state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), SkillEvidence: s.opts.skillEvidence()}
	// Preparation owns a metadata copy and one input descriptor until selection.
	if err := s.retainCharge(int64(len(p.MetadataBytes)) + 16<<10); err != nil {
		return next, err
	}
	next, err = state.BeginOrdinaryPrivacyRewrite(s.ctx, s.readBudget(), next, prior, s.reg.DestinationID, s.publicationAdmission(), policy.Context())
	if err != nil {
		return next, err
	}
	var origin archive.Metadata
	if err := s.unmarshalRetained(next.Preparation.OriginMetadata, &origin); err != nil {
		return next, err
	}
	input := next.Preparation.Inputs[0]
	original, releaseBytes, err := s.ordinaryPrivacyInput(input)
	if err != nil {
		return next, err
	}
	defer releaseBytes()
	alternatives, err := s.ordinaryPrivacyAlternatives(input, prior, pendingSkillMode(p.SkillEvidence))
	if err != nil {
		return next, err
	}
	filtered, packed, ref, proof, release, err := state.RefilterCoveredPublicationInput(s.ctx, s.reg, adapter, origin, next.Preparation.OriginMetadata, input, 0, original, state.PublicationContext{NativeTarget: next.Preparation.NativeTarget, DestinationID: s.reg.DestinationID, AdmissionContext: s.publicationAdmission()}, state.PublicationPolicy{FilterVersion: input.FilterVersion, AdapterVersion: input.AdapterVersion, SkillEvidence: pendingSkillMode(input.SkillPolicy)}, policy, pendingSkillMode(p.SkillEvidence), s.readBudget(), publicationPrivacyReader{scan: s}, alternatives)
	if err != nil {
		return next, err
	}
	s.retainedReleases = append(s.retainedReleases, release)
	if filtered.SchemaVersion != archive.SourceSchemaVersion || filtered.History != nil || !filtered.Capture.CapturedAt.Equal(origin.CapturedAt) {
		return next, state.ErrDurableStorageRecovery
	}
	releaseReceipt, err := next.RecordPrivacyOutput(proof)
	if err != nil {
		return next, err
	}
	s.retainedReleases = append(s.retainedReleases, releaseReceipt)
	metadata, err := s.renderOrdinaryPrivacyMetadata(filtered, ref, origin)
	if err != nil {
		return next, err
	}
	// The final progress certificate owns a second metadata copy.
	if err := s.retainCharge(int64(len(metadata)) + 16<<10); err != nil {
		return next, err
	}
	next.Bundle, next.SourceKey, next.SourceSHA256, next.SourceBytes = filtered, ref.Key, ref.SHA256, packed.Bytes
	next.MetadataBytes, next.Sources, next.SkillEvidence = metadata, nil, string(s.opts.skillEvidence())
	next, err = state.PreparePublicationV2Budgeted(s.ctx, s.readBudget(), next, prior, s.reg.DestinationID, s.publicationAdmission(), policy.Context(), state.PublicationPrivacyRewrite)
	if err != nil {
		return next, err
	}
	retired := []archive.SourceReference{input.Reference}
	if prior.State == state.PredecessorPresent {
		var previous archive.Metadata
		if err := s.unmarshalRetained(prior.Body, &previous); err != nil {
			return next, err
		}
		retired = append(retired, previous.SourceBundle)
	}
	err = next.RetireOrdinaryPrivacySources(s.ctx, s.readBudget(), retired, s.now)
	return next, err
}

func (s *sessionScan) ordinaryPrivacyInput(input state.PreparationInput) ([]byte, func(), error) {
	if input.Payload.Kind == state.PublicationInline {
		return input.Payload.Inline, func() {}, nil
	}
	return (publicationPrivacyReader{scan: s}).ReadPublicationPrivacySource(s.ctx, input)
}

func (s *sessionScan) ordinaryPrivacyAlternatives(input state.PreparationInput, prior state.PublicationPredecessor, ceiling config.SkillEvidence) ([]state.PublicationPrivacyAlternative, error) {
	if prior.State != state.PredecessorPresent {
		return nil, nil
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(prior.Body, &metadata); err != nil {
		return nil, err
	}
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return nil, err
	}
	if metadata.History != nil || prior.Bundle.SchemaVersion != archive.SourceSchemaVersion {
		return nil, state.ErrDurableStorageRecovery
	}
	if metadata.SourceBundle == input.Reference {
		return nil, nil
	}
	parent, child := prior.Bundle.ParentSessionID, prior.Bundle.NativeChild
	selection := state.PublicationSelection{Role: state.PublicationCurrent, CapturedAt: metadata.CapturedAt.UTC(), SourceSchemaVersion: archive.SourceSchemaVersion}
	other := state.PreparationInput{ParentSessionID: &parent, NativeChild: &child, Reference: metadata.SourceBundle, Selection: selection, FilterVersion: metadata.FilterVersion, AdapterVersion: metadata.Adapter.Version, SkillPolicy: string(ceiling), Payload: state.PublicationPayload{Kind: state.PublicationRemote}}
	return []state.PublicationPrivacyAlternative{{Metadata: metadata, MetadataBody: prior.Body, Input: other, Policy: state.PublicationPolicy{FilterVersion: other.FilterVersion, AdapterVersion: other.AdapterVersion, SkillEvidence: ceiling}}}, nil
}

func (s *sessionScan) renderOrdinaryPrivacyMetadata(bundle archive.SourceBundle, ref archive.SourceReference, origin archive.Metadata) ([]byte, error) {
	analysis, parseErr := agentapi.Analyze(s.ctx, s.resolveParser(), bundle)
	if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) {
		return nil, parseErr
	}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, origin.MachineID, origin.StartedAt, s.now, ref, archive.ParserInfo{Version: s.parserVersion()})
	if err != nil && !archive.IsParseError(err) {
		return nil, fmt.Errorf("derive privacy metadata: %w", err)
	}
	metadata.ApplyRegistrationProvenance(s.reg)
	applyProjectName(&metadata, s.reg, s.opts)
	metadata.ApplyRepoKey(origin.RepoKey)
	metadata.ApplyGitHead(s.reg)
	metadata.ApplyReplay(s.reg)
	return s.marshalRetained(metadata)
}
