package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func retentionBinding(r *RetentionRestoration, raw []byte) []byte {
	if r == nil {
		return nil
	}
	return append([]byte("\x00retention-restoration/v1\x00"), raw...)
}
func (a PreparationAuthority) validateRetention() error {
	if a.Retention != nil {
		r := a.Retention
		if a.Purpose != PublicationCapture || a.Predecessor != PredecessorAbsent || !validPublicationDigest(r.IntentSHA256) || !validPublicationDigest(r.OwnerSHA256) || r.MetadataSHA256 != a.OriginMetadataSHA256 || r.RequestToken == "" {
			return ErrDurableStorageRecovery
		}
	}
	for _, input := range a.Inputs {
		if a.Retention != nil && input.HookObservations != nil {
			return ErrDurableStorageRecovery
		}
		if input.Payload.Stage != nil && input.Payload.Stage.Namespace == "session-deletion-sources" && a.Retention == nil {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func validRestorationRequest(j SessionDeletion, req Request) bool {
	if req.Token == "" || req.Token == j.RequestToken || req.RequestedAt.Before(j.At) {
		return false
	}
	for _, reason := range req.Reasons {
		if reason != "backfill" {
			return true
		}
	}
	return false
}

// PrepareRetentionRestoration reseals only the exact previously acknowledged
// baseline. The actual request remains owed: these bytes do not contain its new
// hook evidence. The caller verifies current remote absence before persistence.
func (s *Store) PrepareRetentionRestoration(ctx context.Context, budget *agentapi.NativeReadBudget, reg archive.SessionRegistration, req Request, published *Published, policy, admission, skill string) (PendingPublication, error) {
	var p PendingPublication
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return p, err
	}
	if !found || j.Reason != RemovalReasonRetention || j.LocalRemoved || (j.Phase != DeletionAbsent && j.Phase != DeletionCleaned && j.Phase != DeletionRestoring) || !validRestorationRequest(j, req) {
		return p, ErrRemovalPending
	}
	if !restorationBaselineMatches(published, j, reg, policy, admission) {
		return p, ErrDurableStorageRecovery
	}
	refs, err := published.CommittedSources()
	if err != nil || !slices.Equal(refs, j.References) {
		return p, errors.Join(ErrDurableStorageRecovery, err)
	}
	bundle, _, have := published.LastPublished()
	if !have {
		return p, ErrDurableStorageRecovery
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(j.MetadataBytes, &metadata); err != nil {
		return p, err
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return p, err
	}
	p = PendingPublication{Bundle: bundle, SourceKey: refs[0].Key, SourceSHA256: refs[0].SHA256, MetadataKey: key, MetadataBytes: slices.Clone(j.MetadataBytes), SkillEvidence: skill}
	if metadata.History != nil {
		p.History = &PendingHistory{Version: 2}
	}
	if err := populateRestorationPayloads(&p, metadata, refs, published, reg, admission); err != nil {
		return p, err
	}

	release, err := FreezePublicationNativeTarget(ctx, &p, reg, budget)
	if err != nil {
		return p, err
	}
	defer release()
	// Allow the private constructor's stage namespace only after the immutable
	// retention receipt has been installed by initializePreparation.
	sources, err := selectedPublicationSources(p, reg.DestinationID, admission)
	if err != nil {
		return p, err
	}
	if err = p.initializePreparation(PublicationPredecessor{State: PredecessorAbsent}, metadata, reg.DestinationID, admission, policy, PublicationCapture, sources, nil); err != nil {
		return p, err
	}
	p.Preparation.Retention = &RetentionRestoration{IntentSHA256: deletionIntentSHA(j), OwnerSHA256: j.OwnerSHA256, MetadataSHA256: j.MetadataSHA256, RequestToken: req.Token}
	p.Preparation.SHA256 = preparationSHAWithFacts(*p.Preparation, nil)
	p, err = PreparePublicationV2Budgeted(ctx, budget, p, PublicationPredecessor{State: PredecessorAbsent}, reg.DestinationID, admission, policy, PublicationCapture)
	if err != nil {
		return p, err
	}
	if j.Phase == DeletionRestoring && (j.RestoringToken != req.Token || j.RestorationSHA256 != p.Commit.MetadataSHA256) {
		return p, ErrDurableStorageRecovery
	}
	j.Phase = DeletionRestoring
	j.RestoringToken = req.Token
	j.RestorationSHA256 = p.Commit.MetadataSHA256
	if err = s.saveSessionDeletion(j, &req.Token); err != nil {
		return p, err
	}
	return p, nil
}

func populateRestorationPayloads(p *PendingPublication, metadata archive.Metadata, refs []archive.SourceReference, published *Published, reg archive.SessionRegistration, admission string) error {
	for i, ref := range refs {
		var original *PublicationSource
		for k := range published.state.Payloads {
			candidate := &published.state.Payloads[k]
			if candidate.Reference == ref {
				original = candidate
				break
			}
		}
		if original == nil {
			return ErrDurableStorageRecovery
		}
		selection := original.Selection
		if p.History != nil {
			version := metadata.FilterVersion
			if i > 0 {
				version = metadata.History.Preserved[i-1].FilterVersion
			}
			p.History.Inputs = append(p.History.Inputs, HistoryInput{Reference: ref, RevisionID: selection.RevisionID, CapturedAt: selection.CapturedAt, SourceSchemaVersion: selection.SourceSchemaVersion, FilterVersion: version})
		}
		handle := &ImmutableStageHandle{Version: 1, Kind: PublicationHistoryStage, Namespace: "session-deletion-sources", SessionID: reg.ArchiveSessionID, OwnerSHA256: publicationOwner(metadata, reg.DestinationID, admission), DestinationID: reg.DestinationID, AdmissionContext: admission, SourceSHA256: ref.SHA256, CompressedBytes: ref.CompressedBytes, Selection: selection}
		p.Sources = append(p.Sources, PublicationSource{Reference: ref, Selection: selection, Payload: PublicationPayload{Kind: PublicationHistoryStage, Stage: handle}})
	}
	return nil
}

// ValidateRetentionRestoration rechecks actual durable intent for every replay;
// a copied seal or missing journal never reconstructs absent predecessor proof.
func (s *Store) ValidateRetentionRestoration(reg archive.SessionRegistration, p PendingPublication) error {
	if p.Preparation == nil || p.Preparation.Retention == nil {
		return nil
	}
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	r := p.Preparation.Retention
	if !found || j.Reason != RemovalReasonRetention || j.LocalRemoved || (j.Phase != DeletionRestoring && j.Phase != DeletionRestored) || deletionIntentSHA(j) != r.IntentSHA256 || j.OwnerSHA256 != r.OwnerSHA256 || j.RestoringToken != r.RequestToken || j.MetadataSHA256 != r.MetadataSHA256 || !bytes.Equal(j.MetadataBytes, p.MetadataBytes) || p.Commit == nil || p.Commit.Predecessor != PredecessorAbsent || p.RequestToken != "" {
		return ErrDurableStorageRecovery
	}
	return p.ValidatePublication()
}

// CompleteRetentionRestoration follows the normal attempt-bound closing proof
// and full local selecting save; it never completes a hook request.
func (s *Store) CompleteRetentionRestoration(reg archive.SessionRegistration, p PendingPublication, published *Published) error {
	if p.Preparation == nil || p.Preparation.Retention == nil {
		return nil
	}
	if err := s.ValidateRetentionRestoration(reg, p); err != nil {
		return err
	}
	if published == nil || published.state.Commit == nil || *published.state.Commit != *p.Commit || !bytes.Equal(published.Metadata(), p.MetadataBytes) {
		return ErrDurableStorageRecovery
	}
	j, _, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	j.Phase = DeletionRestored
	return s.saveSessionDeletion(j, nil)
}

func restorationBaselineMatches(published *Published, j SessionDeletion, reg archive.SessionRegistration, policy, admission string) bool {
	return published != nil && published.state.Commit != nil && published.state.Commit.PolicyContext == policy && published.state.Commit.DestinationID == reg.DestinationID && published.state.Commit.AdmissionContext == admission && bytes.Equal(published.Metadata(), j.MetadataBytes)
}

func (p PendingPublication) validateRetentionEnvelope() error {
	a := p.Preparation
	if a.Retention != nil && (p.RequestToken != "" || p.ScanSignature != nil || p.MetadataOnly || publicationSHA256(p.MetadataBytes) != a.OriginMetadataSHA256) {
		return ErrDurableStorageRecovery
	}
	return nil
}
