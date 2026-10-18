package evidence

import (
	"encoding/hex"
	"sort"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// This companion shares the observation's existing reads. Only safe S enters
// this operation; rejected/redacted candidates never get an original hash.
type skillContentDigests struct {
	reservation     *agentapi.NativeReadReservation
	held            int64
	values          map[agentapi.SkillContentDigest]bool
	acceptedDigests []agentapi.SkillContentDigest
	closed          bool
}

// Observers run on the serialized scan owner. Preflight optional growth before
// Reserve so capacity refusal cannot poison the owner's pressure flags.
func (s *skillContentDigests) charge(n int64) bool {
	if s.closed {
		return false
	}
	if n > s.reservation.Available() || !s.reservation.Reserve(n) {
		s.close()
		return false
	}
	s.held += n
	return true
}

func (s *skillContentDigests) add(d agentapi.SkillContentDigest) error {
	if s.closed || s.values[d] {
		return nil
	}
	n := int64(128)
	if s.values == nil {
		n += 1024
	}
	if !s.charge(n) {
		return nil
	}
	if s.values == nil {
		s.values = make(map[agentapi.SkillContentDigest]bool)
	}
	s.values[d] = true
	return nil
}

func (s *skillContentDigests) accepted(evidence []archive.SupplementalEvidence) ([]agentapi.SkillContentDigest, error) {
	if len(s.values) == 0 {
		return nil, nil
	}
	if !s.charge(int64(len(s.values)) * 32) {
		return nil, nil
	}
	out := make([]agentapi.SkillContentDigest, 0, len(s.values))
	accept := func(payload map[string]any) {
		value, _ := payload["sha256"].(string)
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != 32 {
			return
		}
		var d agentapi.SkillContentDigest
		copy(d[:], raw)
		if s.values[d] {
			out = append(out, d)
			delete(s.values, d)
		}
	}
	// A private candidate whose enclosing plugin/read later failed stability
	// cannot survive merely because it was once read: require accepted evidence.
	for _, item := range evidence {
		switch item.Kind {
		case archive.EvidenceKindSkillSnapshot, archive.EvidenceKindSkillInventory:
			accept(item.Payload)
			if skills, ok := item.Payload["skills"].([]any); ok {
				for _, raw := range skills {
					if skill, ok := raw.(map[string]any); ok {
						accept(skill)
					}
				}
			}
		case archive.EvidenceKindSessionLabels, archive.EvidenceKindSkillDiscovered, archive.EvidenceKindSkillInvocation, archive.EvidenceKindSkillRead, archive.EvidenceKindLifecycleHook, archive.EvidenceKindFinalResponse, archive.EvidenceKindExplicitFeedback, archive.EvidenceKindLinkedSession, archive.EvidenceKindCaptureGap:
			// Other evidence kinds do not grant installed-content digests.
		default:
			// Unknown kinds keep the existing no-authority behavior.
		}
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i][:]) < string(out[j][:]) })
	s.acceptedDigests = out
	return out, nil
}

func (s *skillContentDigests) close() {
	if s.closed {
		return
	}
	s.closed = true
	clear(s.values)
	clear(s.acceptedDigests)
	s.acceptedDigests = nil
	s.values = nil
	s.reservation.Release(s.held)
	s.held = 0
}

func (b *skillReadBudget) observeSkillContent(original, redacted string, safe [32]byte) error {
	if b.content == nil || !archive.MarkerFreeRedactionFixedPoint(original, redacted) {
		return nil
	}
	return b.content.add(agentapi.SkillContentDigest(safe))
}
