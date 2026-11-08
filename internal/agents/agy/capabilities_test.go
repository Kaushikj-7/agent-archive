package agy

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestCapabilityEvidenceCaptureEvidence(t *testing.T) {
	t.Parallel()
	ev := CapabilityEvidence{}.CaptureEvidence()

	if ev.SkillEvidence.State != agentapi.CapabilityUnavailable {
		t.Fatalf("expected SkillEvidence to be unavailable, got %s", ev.SkillEvidence.State)
	}
	if ev.Transcript.State != agentapi.CapabilityDocumented {
		t.Fatalf("expected Transcript to be documented, got %s", ev.Transcript.State)
	}
	if ev.Lifecycle.State != agentapi.CapabilityDocumented {
		t.Fatalf("expected Lifecycle to be documented, got %s", ev.Lifecycle.State)
	}
	if ev.FreshStart.State != agentapi.CapabilityDocumented {
		t.Fatalf("expected FreshStart to be documented, got %s", ev.FreshStart.State)
	}
	if ev.AdapterFixtures.State != agentapi.CapabilityDocumented {
		t.Fatalf("expected AdapterFixtures to be documented, got %s", ev.AdapterFixtures.State)
	}
	if ev.SubagentLinkage.State != agentapi.CapabilityUnavailable {
		t.Fatalf("expected SubagentLinkage to be unavailable, got %s", ev.SubagentLinkage.State)
	}
}
