// Package producer executes only fixed production retained privacy projection.
package producer

import (
	"errors"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy/internal/sealed"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Transform accepts expectations only; no callback or injected adapter can mint proof.
func Transform(request *sealed.Request) (proof *sealed.Proof, err error) {
	lease, err := request.Borrow()
	if err != nil {
		return nil, err
	}
	var releases []func()
	var once sync.Once
	release := func() {
		once.Do(func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		})
	}
	defer func() {
		if err != nil {
			release()
			lease.Close()
		}
	}()
	ctx, budget := lease.Context(), lease.Budget()
	if err := lease.Check(); err != nil {
		return nil, err
	}
	wire := lease.Wire
	adapter, err := fixedFilter(wire.Registration.Harness.Name)
	if err != nil {
		return nil, err
	}
	if wire.Target.FilterVersion != archive.FilterVersion || wire.Target.AdapterVersion != adapter.Version() || !supportedSkill(wire.Target.SkillEvidence) {
		return nil, errors.New("retained target privacy policy changed")
	}
	original, closeOriginal, err := agentapi.DecodeReferencedSourceLeased(ctx, wire.Metadata, lease.Source, agentapi.SourceReadLimits{}, budget)
	if err != nil {
		return nil, err
	}
	releases = append(releases, closeOriginal)
	if original.History != nil || original.SchemaVersion != archive.SourceSchemaVersion || original.NativeChild != wire.Registration.IsChild() || original.ParentSessionID != wire.Registration.ParentSessionID || original.PreviousGenerationID != wire.Registration.PreviousGenerationID || original.Capture.FilterVersion != wire.OldPolicy.FilterVersion || original.Capture.AdapterVersion != wire.OldPolicy.AdapterVersion || !supportedSkill(wire.OldPolicy.SkillEvidence) {
		return nil, errors.New("retained original capture facts changed")
	}
	canonical, closeCanonical, err := agentapi.CompressRetainedSource(ctx, original, budget)
	if err != nil {
		return nil, err
	}
	exact := canonical.SHA256 == wire.Metadata.SourceBundle.SHA256 && len(canonical.Bytes) == len(lease.Source)
	closeCanonical()
	if !exact {
		return nil, errors.New("retained original source is not canonical")
	}
	owner := budget.Reservation()
	evidenceCharge := int64(len(original.SupplementalEvidence))*128 + 4096
	if !owner.Reserve(evidenceCharge) {
		return nil, agentapi.ErrReadBudget
	}
	releases = append(releases, func() { owner.Release(evidenceCharge) })
	original.SupplementalEvidence = limitSkills(original.SupplementalEvidence, wire.Target.SkillEvidence)
	bundle, closeFiltered, err := agentapi.RefilterRetainedSource(ctx, wire.Registration, adapter, original, budget)
	if err != nil {
		return nil, err
	}
	releases = append(releases, closeFiltered)
	packed, closePacked, err := agentapi.CompressRetainedSource(ctx, bundle, budget)
	if err != nil {
		return nil, err
	}
	releases = append(releases, closePacked)
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		return nil, err
	}
	digest, err := sealed.Digest(ctx, budget, bundle)
	if err != nil {
		return nil, err
	}
	if err := lease.Check(); err != nil {
		return nil, err
	}
	ownerSHA, err := sealed.Digest(ctx, budget, wire.Registration)
	if err != nil {
		return nil, err
	}
	facts := sealed.Facts{OwnerSHA256: ownerSHA, CoveredToken: wire.CoveredToken, HadRequest: wire.HadRequest, StageSHA256: wire.StageSHA256, Input: wire.Metadata.SourceBundle, Output: archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, OriginalPolicy: wire.OldPolicy, TargetPolicy: wire.Target}
	return sealed.New(lease, bundle, packed, facts, digest, release), nil
}

func fixedFilter(harness string) (agentapi.TranscriptFilter, error) {
	switch agentmeta.ID(harness) {
	case agentmeta.Codex:
		return codex.Filter{}, nil
	case agentmeta.Claude:
		return claude.Filter{}, nil
	case agentmeta.Cursor:
		return cursor.Filter{}, nil
	case agentmeta.Muse:
		return muse.Filter{}, nil
	default:
		return nil, errors.New("retained privacy requires a supported production filter")
	}
}

type skillPolicy string

const (
	skillNone     skillPolicy = "none"
	skillMetadata skillPolicy = "metadata"
	skillBody     skillPolicy = "body"
)

func supportedSkill(mode string) bool {
	policy := skillPolicy(mode)
	return policy == skillNone || policy == skillMetadata || policy == skillBody
}

func limitSkills(in []archive.SupplementalEvidence, mode string) []archive.SupplementalEvidence {
	out := make([]archive.SupplementalEvidence, 0, len(in))
	for _, item := range in {
		if item.Kind == archive.EvidenceKindSkillSnapshot && mode != "body" || item.Kind == archive.EvidenceKindSkillInventory && mode == "none" {
			continue
		}
		out = append(out, item)
	}
	return out
}
