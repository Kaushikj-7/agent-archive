package sealed

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// LiveProof holds fixed live-image capture under a distinct consumer type.
// It cannot be substituted for settled proof or constructed outside this issuer.
type LiveProof struct{ proof *Proof }

// WithVerifiedLiveCapture lends the same original request and captured resource
// identity checks while preserving the separate live producer/writer boundary.
func (p *LiveProof) WithVerifiedLiveCapture(ctx context.Context, reg archive.SessionRegistration, expected *nativefacts.CaptureRequest, budget *agentapi.NativeReadBudget, visit func(context.Context, archive.SourceBundle, nativefacts.SettledImportFacts) error) error {
	if p == nil || p.proof == nil {
		return agentapi.ErrClosed
	}
	return p.proof.WithVerifiedCapture(ctx, reg, expected, budget, visit)
}

// Close releases shared terminal proof custody exactly once across copied handles.
func (p *LiveProof) Close() error {
	if p == nil {
		return nil
	}
	return p.proof.Close()
}

// NewLive transfers only the completed fixed live capture after scratch cleanup.
// Whole-source type/object gates restrict this mint to its fixed producer return.
func NewLive(reg archive.SessionRegistration, bundle archive.SourceBundle, facts nativefacts.SettledImportFacts, digest string, owner *Reservation, charged int64, request *nativefacts.CaptureRequest, lease *nativefacts.RequestLease, budget *agentapi.NativeReadBudget) *LiveProof {
	return &LiveProof{proof: &Proof{capture: &settledCapture{reg: reg, bundle: bundle, facts: facts, digest: digest, owner: owner, charged: charged, request: request, lease: lease, originalBudget: budget}}}
}
