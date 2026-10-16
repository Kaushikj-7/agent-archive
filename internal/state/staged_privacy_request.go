package state

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// FreezeStagedPrivacyRequest snapshots the actual immutable admitted stage for
// the fixed retained producer. It grants no selecting or release authority and
// does not change the original stage, pending selection, or covered request.
// The first transition supports ordinary Codex stages without child handoffs;
// other typed owners remain explicitly owed until their protocol is implemented.
func (s *Store) FreezeStagedPrivacyRequest(ctx context.Context, id, adapterVersion string) (*retainedprivacy.Request, error) {
	if s.resourceBudget == nil {
		return nil, agentapi.ErrReadBudget
	}
	original := s
	originalOwner, ok := s.readReservation().(*agentapi.NativeReadReservation)
	if !ok {
		return nil, agentapi.ErrReadBudget
	}
	captured, err := s.resourceBudget.CaptureFromReservation(originalOwner)
	if err != nil {
		return nil, err
	}
	s, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	s, closeWork := s.WithReadBudget(ctx, captured)
	defer closeWork()
	s.resourceReservation = func() stateReservation { return captured.Reservation() }
	if err := s.resourceContext.Err(); err != nil {
		return nil, err
	}
	cfg, reg, stage, err := s.stagedContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if reg.Harness.Name != archive.HarnessCodex || stage.Version != 2 || len(stage.Children) != 0 || stage.CursorFacts != nil || stage.ReleaseOnly || stage.Canceled || stage.CancellationOnly || reg.IsChild() || !readySelectionOwnerMatches(reg, stage.Bundle) {
		return nil, ErrReadySelectionFrozen
	}
	request, hadRequest, err := s.LoadRequest(id)
	if err != nil {
		return nil, err
	}
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	pending, foundPending, pendingErr := scratch.catalogPendingForFence(id)
	closeScratch()
	if pendingErr != nil {
		return nil, pendingErr
	}
	if foundPending {
		original := pending.StagedProof()
		if original == nil || original.StageSHA256 != stage.SHA256 || original.HadRequest != hadRequest || original.CoveredToken != request.Token {
			return nil, ErrReadySelectionFrozen
		}
	}
	if s.stagedPrivacyCompressionBoundary != nil {
		s.stagedPrivacyCompressionBoundary(false)
	}
	packed, closePacked, err := agentapi.CompressRetainedSource(ctx, stage.Bundle, s.resourceBudget)
	if err != nil {
		return nil, err
	}
	defer closePacked()
	if s.stagedPrivacyCompressionBoundary != nil {
		s.stagedPrivacyCompressionBoundary(true)
	}
	key, err := archive.SourceObjectKey(stage.Bundle, packed.SHA256)
	if err != nil {
		return nil, err
	}
	capture := stage.Bundle.Capture
	metadata := archive.Metadata{
		SchemaVersion: archive.MetadataSchemaVersion, SessionID: stage.Bundle.ArchiveSessionID,
		NativeSessionID: stage.Bundle.NativeSessionID, ProjectID: stage.Bundle.ProjectID,
		Harness: capture.Harness, ParentSessionID: stage.Bundle.ParentSessionID, NativeChild: stage.Bundle.NativeChild,
		CapturedAt: capture.CapturedAt, FilterVersion: capture.FilterVersion,
		Adapter:      archive.AdapterInfo{Name: capture.AdapterName, Version: capture.AdapterVersion},
		SourceBundle: archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)},
	}
	expected := retainedprivacy.Expectation{
		Registration: reg, StageSHA256: stage.SHA256, Metadata: metadata,
		HadRequest: hadRequest, CoveredToken: request.Token,
		OldPolicy: retainedprivacy.Policy{FilterVersion: capture.FilterVersion, AdapterVersion: capture.AdapterVersion, SkillEvidence: string(stage.SkillEvidence)},
		Target:    retainedprivacy.Policy{FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion, SkillEvidence: string(cfg.EffectiveSkillEvidence())},
	}
	// The original receiver context owns the detached request lifetime. A fresh
	// caller context cannot erase its cancellation or select a later scope.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return original.freezeStagedPrivacySnapshot(expected, packed.Bytes, originalOwner)
}

// The Store read lease owns this context; caller contexts cannot replace it.
func (s *Store) freezeStagedPrivacySnapshot(expected retainedprivacy.Expectation, source []byte, owner *agentapi.NativeReadReservation) (*retainedprivacy.Request, error) {
	return retainedprivacy.FreezeRequestCaptured(s.resourceContext, expected, source, s.resourceBudget, owner)
}
