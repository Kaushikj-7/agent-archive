package state

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// BeginOrdinaryPrivacyRewrite freezes one ordinary retained input for an atomic
// in-memory transformation. It grants no selecting commit and cannot be saved
// until PreparePublicationV2Budgeted validates the actual factory output.
func BeginOrdinaryPrivacyRewrite(ctx context.Context, budget *agentapi.NativeReadBudget, p PendingPublication, prior PublicationPredecessor, destination, admission, policy string) (PendingPublication, error) {
	if p.History != nil || p.Bundle.SchemaVersion != archive.SourceSchemaVersion || p.Selection != nil || p.StagedProof() != nil || p.Catalog != nil || prior.State == PredecessorUnknown {
		return PendingPublication{}, ErrDurableStorageRecovery
	}
	if p.Commit != nil {
		if err := p.ValidatePublicationBudgeted(ctx, budget); err != nil {
			return PendingPublication{}, err
		}
		if p.Commit.DestinationID != destination || p.Commit.AdmissionContext != admission {
			return PendingPublication{}, ErrDurableStorageRecovery
		}
	} else if p.JournalVersion != 0 {
		return PendingPublication{}, ErrDurableStorageRecovery
	}
	var origin archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &origin); err != nil {
		return PendingPublication{}, err
	}
	if origin.History != nil {
		return PendingPublication{}, ErrDurableStorageRecovery
	}
	sources, err := selectedPublicationSources(p, destination, admission)
	if err != nil || len(sources) != 1 {
		return PendingPublication{}, ErrDurableStorageRecovery
	}
	facts, release, err := newPayloadDigestFacts(ctx, budget, 1)
	if err != nil {
		return PendingPublication{}, err
	}
	defer release()
	p.JournalVersion, p.Phase = 0, ""
	p.Commit, p.Preparation, p.Progress, p.Cleanup = nil, nil, nil, nil
	p.privacyOutputs = nil
	p.ScanSignature = nil
	p.Attempted, p.MetadataOnly = false, false
	p.Sources = sources
	if p.SkillEvidence == "" {
		p.SkillEvidence = string(config.SkillEvidenceBody)
	}
	if err := p.initializePreparation(prior, origin, destination, admission, policy, PublicationPrivacyRewrite, sources, facts); err != nil {
		return PendingPublication{}, err
	}
	if err := p.Preparation.validateWithFacts(facts); err != nil {
		return PendingPublication{}, err
	}
	return p, nil
}

func (p *PendingPublication) initializeOrdinaryPrivacyProgress(sources []PublicationSource, facts *payloadDigestFacts) error {
	if len(sources) != 1 || len(p.Preparation.Inputs) != 1 || p.History != nil {
		return ErrDurableStorageRecovery
	}
	proof, ok := p.privacyOutputs[0]
	if !ok {
		return ErrDurableStorageRecovery
	}
	copyProof := proof
	progress := &PreparationProgress{Version: 1, AuthoritySHA256: p.Preparation.SHA256, Cursor: 1, WorkingMetadata: append([]byte(nil), p.MetadataBytes...), Outputs: []PreparedOutput{{InputIndex: 0, Source: sources[0], Privacy: &copyProof}}}
	progress.SHA256 = progressSHAWithFacts(*progress, facts)
	p.Progress = progress
	return nil
}

// RetireOrdinaryPrivacySources journals cleanup separately from selecting
// authority. Currently selected bytes are always protected.
func (p *PendingPublication) RetireOrdinaryPrivacySources(ctx context.Context, budget *agentapi.NativeReadBudget, refs []archive.SourceReference, at time.Time) error {
	if p.History != nil || p.Commit == nil || p.Commit.Purpose != PublicationPrivacyRewrite || p.Cleanup == nil || p.Preparation == nil || at.IsZero() {
		return ErrDurableStorageRecovery
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	selected, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	allowed := make(map[archive.SourceReference]bool, len(p.Preparation.Inputs)+1)
	for _, input := range p.Preparation.Inputs {
		allowed[input.Reference] = true
	}
	if body := privacyPreviousBody(*p.Preparation); len(body) > 0 {
		var previous archive.Metadata
		if err := json.Unmarshal(body, &previous); err != nil {
			return err
		}
		priorRefs, err := previous.SourceReferences()
		if err != nil {
			return err
		}
		for _, ref := range priorRefs {
			allowed[ref] = true
		}
	}
	for _, ref := range refs {
		if !allowed[ref] || !validSourceReference(ref) {
			return ErrDurableStorageRecovery
		}
	}
	for _, ref := range refs {
		if slices.Contains(selected, ref) {
			continue
		}
		duplicate := false
		for _, old := range p.Cleanup.Retired {
			duplicate = duplicate || old.Reference == ref
		}
		if !duplicate {
			p.Cleanup.Retired = append(p.Cleanup.Retired, RetiredSource{Reference: ref, RetiredAt: at, PrivacySensitive: true})
		}
	}
	p.Cleanup.SHA256 = cleanupSHA(*p.Cleanup)
	return p.ValidatePublicationBudgeted(ctx, budget)
}
