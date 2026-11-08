package agy

import "github.com/wangjohn/agent-archive/internal/agentapi"

// CapabilityEvidence holds documented and fixture evidence without host observations.
type CapabilityEvidence struct{}

var _ agentapi.CapabilityEvidenceProvider = CapabilityEvidence{}

// CaptureEvidence declares evidence strength independently of implemented ports and health.
func (CapabilityEvidence) CaptureEvidence() agentapi.CaptureCapabilities {
	documented := func(evidence string) agentapi.CapabilityEvidence {
		return agentapi.CapabilityEvidence{State: agentapi.CapabilityDocumented, Evidence: evidence}
	}
	unavailable := func(evidence, next string) agentapi.CapabilityEvidence {
		return agentapi.CapabilityEvidence{State: agentapi.CapabilityUnavailable, Evidence: evidence, NextAction: next}
	}

	profile := agentapi.CaptureCapabilities{
		SkillEvidence:   unavailable("No supported native eligibility/use contract has been verified.", "Treat eligibility comparisons as unavailable."),
		SubagentLinkage: unavailable("Subagent linkage is not yet established for Antigravity sessions.", "Validate subagent transcript and lifecycle evidence."),
		AdapterFixtures: documented("Synthetic fixtures exercise the bounded adapter; they do not prove an installed version."),
		FreshStart:      documented("SessionStart hook and transcript initialization determine fresh session start."),
		Transcript:      documented("Hooks and brain transcripts provide access to the native JSONL transcript under brain/<conversationId>/.system_generated/logs/transcript.jsonl."),
		Lifecycle:       documented("SessionStart, PreInvocation, PostInvocation, and Stop lifecycle events are documented."),
	}
	return profile
}
