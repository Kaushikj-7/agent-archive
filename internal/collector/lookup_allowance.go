package collector

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// beginCodexLookupAllowance gives this collector pass its own Codex lookup
// allowance (agentapi.CodexRolloutAllowance), separate from whatever the
// lookup's earlier users in the same pass, discovery or import
// registration, spent of it.
func beginCodexLookupAllowance(opts Options) {
	if allowance, ok := opts.CodexRollouts.(agentapi.CodexRolloutAllowance); ok {
		allowance.BeginCollection()
	}
}

// codexLookupSpent reports whether reg is a Codex session that this pass can
// no longer read because the pass's Codex lookup allowance is used up: every
// lookup its scan needs would be refused. Such a session is left for the next
// pass without being opened, so the rest of the pass (Claude Code and Cursor
// sessions) does not wait on scans bound to fail.
func (p *pass) codexLookupSpent(reg archive.SessionRegistration) bool {
	if reg.Harness.Name != archive.HarnessCodex {
		return false
	}
	allowance, ok := p.opts.CodexRollouts.(agentapi.CodexRolloutAllowance)
	return ok && allowance.AllowanceSpent()
}

// orderLookupAllowanceDeferralsLast moves the Codex sessions still in a run
// of deferrals that began with the Codex lookup allowance running out
// (waitsOnLookupAllowance) behind every other Codex session, keeping each
// group's order. It only exchanges sessions between the slots Codex sessions
// already hold, so every other session keeps its place in the pass's order.
//
// Without it, one session whose lookups alone need more than the whole
// allowance would be first of the Codex sessions on every pass: it would
// spend the allowance and be deferred, and every Codex session after it
// would be left for the next pass (codexLookupSpent) on every pass too.
// Moved last, it still has its turn once the others have had theirs.
func (p *pass) orderLookupAllowanceDeferralsLast() {
	var slots []int
	var others, waiting []archive.SessionRegistration
	for i, reg := range p.registrations {
		if reg.Harness.Name != archive.HarnessCodex {
			continue
		}
		slots = append(slots, i)
		if p.waitsOnLookupAllowance(reg.ArchiveSessionID) {
			waiting = append(waiting, reg)
		} else {
			others = append(others, reg)
		}
	}
	for k, reg := range append(others, waiting...) {
		p.registrations[slots[k]] = reg
	}
}

// waitsOnLookupAllowance reports a session whose run of deferred scans
// (priorDeferredSince) the Codex lookup allowance may be keeping going: the
// previous pass deferred it for DeferredLookupAllowance, or kept its clock
// without deferring it, which it does for a session it left unopened
// (codexLookupSpent, or an early end) and for one it failed once the run
// outlasted MaxDeferral. A session deferred for another reason keeps its
// place: a live Codex session whose rollout changed while being read is
// not put behind a backlog.
func (p *pass) waitsOnLookupAllowance(id string) bool {
	if _, running := p.priorDeferredSince[id]; !running {
		return false
	}
	reason, deferred := p.priorDeferred[id]
	return !deferred || reason == DeferredLookupAllowance
}
