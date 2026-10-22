package collector

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func (o Options) skillEvidence() config.SkillEvidence {
	if o.SkillEvidence == "" {
		return config.SkillEvidenceBody
	}
	return o.SkillEvidence
}

func pendingSkillMode(value string) config.SkillEvidence {
	if value == "" {
		return config.SkillEvidenceBody
	}
	return config.SkillEvidence(value)
}

// skillPolicyRank orders skill-evidence ceilings from narrowest to broadest.
// An unrecognised policy ranks narrowest, so it never outranks a known one.
func skillPolicyRank(mode config.SkillEvidence) int {
	switch mode {
	case config.SkillEvidenceNone:
		return 0
	case config.SkillEvidenceMetadata:
		return 1
	case config.SkillEvidenceBody:
		return 2
	}
	return 0
}

// skillPolicyBroadens reports whether an origin policy authorizes more skill
// evidence than the current ceiling. An unrecognised policy on either side
// keeps the current ceiling.
func skillPolicyBroadens(origin, current config.SkillEvidence) bool {
	return config.ValidSkillEvidence(origin) && config.ValidSkillEvidence(current) && skillPolicyRank(origin) > skillPolicyRank(current)
}

// limitSkillEvidence also removes skill evidence retained in an older local
// candidate. Otherwise a policy change would re-upload cached snapshots.
func limitSkillEvidence(in []archive.SupplementalEvidence, mode config.SkillEvidence) []archive.SupplementalEvidence {
	return state.LimitPublicationSkillEvidence(in, mode)
}

func sourceEvidenceWithinPolicy(in []archive.SupplementalEvidence, mode config.SkillEvidence) bool {
	for _, item := range in {
		if item.Kind == archive.EvidenceKindSkillSnapshot && mode != config.SkillEvidenceBody {
			return false
		}
		if item.Kind == archive.EvidenceKindSkillInventory && mode == config.SkillEvidenceNone {
			return false
		}
	}
	return true
}
