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
// of deferrals that the Codex lookup allowance may be keeping going
// (lookupAllowanceTurn) behind every other Codex session with a pending
// request, keeping each group's order. It only exchanges sessions between
// the slots requested Codex sessions already hold, so every other session,
// including the Codex sessions with nothing requested, whose quiet checks
// also use the allowance, keeps its place in the pass's order.
//
// Without it, one session whose lookups alone need more than the whole
// allowance would be first of the Codex sessions on every pass: it would
// spend the allowance and be deferred, and every Codex session after it
// would be left for the next pass (codexLookupSpent) on every pass too.
// Moved last, it still has its turn once the others have had theirs.
func (p *pass) orderLookupAllowanceDeferralsLast() {
	var slots []int
	var turns [lookupAllowanceTurns][]archive.SessionRegistration
	for i, reg := range p.registrations {
		if reg.Harness.Name != archive.HarnessCodex || p.requests[reg.ArchiveSessionID].Token == "" {
			continue
		}
		slots = append(slots, i)
		turn := p.lookupAllowanceTurn(reg.ArchiveSessionID)
		turns[turn] = append(turns[turn], reg)
	}
	k := 0
	for _, group := range turns {
		for _, reg := range group {
			p.registrations[slots[k]] = reg
			k++
		}
	}
}

// The groups orderLookupAllowanceDeferralsLast puts requested Codex sessions
// in, first to last.
const (
	// turnFirst: not waiting on the allowance.
	turnFirst = iota
	// turnLeftUnopened: the previous pass kept the session's clock without
	// deferring it, which it does for a session it left unopened
	// (codexLookupSpent, or an early end).
	turnLeftUnopened
	// turnRanOut: the previous pass read the session and deferred it for
	// DeferredLookupAllowance. At most one session a pass ends this way;
	// putting it behind the ones left unopened gives each session waiting
	// on the allowance the first turn among them in rotation, so one whose
	// lookups alone need the whole allowance cannot hold back for good a
	// session that only ran out after others had used part of it.
	turnRanOut
	// turnExpired: its run of deferrals has lasted MaxDeferral, so a
	// deferral now fails it (as the previous pass may have done, keeping its
	// clock without deferring it). Sessions that can still be read without
	// failing go first.
	turnExpired
	lookupAllowanceTurns
)

// lookupAllowanceTurn reports which group a requested Codex session is read
// in. A session with a running deferral clock (priorDeferredSince) waits on
// the allowance when the previous pass deferred it for
// DeferredLookupAllowance or kept its clock without deferring it. A session
// deferred for another reason keeps its place: a live Codex session whose
// rollout changed while being read is not put behind a backlog.
func (p *pass) lookupAllowanceTurn(id string) int {
	since, running := p.priorDeferredSince[id]
	if !running {
		return turnFirst
	}
	reason, deferred := p.priorDeferred[id]
	switch {
	case deferred && reason != DeferredLookupAllowance:
		return turnFirst
	case p.now.Sub(since) >= MaxDeferral:
		return turnExpired
	case deferred:
		return turnRanOut
	default:
		return turnLeftUnopened
	}
}
