package collector

import (
	"sort"

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

// startLookupAllowanceWait starts a deferral clock (keepDeferredSince) for a
// requested Codex session the pass leaves unopened because the lookup
// allowance is spent (codexLookupSpent), if it has none running: the session
// is then waiting on the allowance, so the next pass puts it in the
// rotation among the sessions that wait (turnLeftUnopened) rather than back
// among the ones that do not, where new Codex work ahead of it could use up
// every pass's allowance. Its wait counts towards MaxDeferral from now; a
// running clock is carried as it is (leaveForNextPass).
func (p *pass) startLookupAllowanceWait(reg archive.SessionRegistration) {
	id := reg.ArchiveSessionID
	if p.requests[id].Token == "" {
		return
	}
	if _, running := p.priorDeferredSince[id]; !running {
		p.keepDeferredSince(id, p.now.UTC())
	}
}

// orderLookupAllowanceDeferralsLast orders the Codex sessions with a pending
// request by whether, and how, the Codex lookup allowance is keeping them
// waiting (lookupAllowanceTurn). It only exchanges sessions between the
// slots requested Codex sessions already hold, so every other session,
// including the Codex sessions with nothing requested, whose quiet checks
// also use the allowance, keeps its place in the pass's order. Native
// children waiting for their parent stay behind every other requested
// session, as orderRequests put them: the groups are formed among those
// children and among the rest separately, and only the rest have a lane.
//
// The sessions not waiting keep orderRequests' order. Those waiting are
// read in the order the previous pass left them unopened in
// (state.Status.UnreachedSessions), then the ones it read: so the session
// it read first and ran out on goes to the back, and every session waiting
// on the allowance has the first turn among them in rotation.
//
// The first of the slots goes to the waiter lane: the session the previous
// pass left unopened earliest in its queue (the head of turnLeftUnopened),
// ahead of the sessions not waiting, so it is read with a fresh allowance.
// Only one waiter takes it a pass, and never one that ran out on the
// previous pass (turnRanOut) or whose wait has lasted MaxDeferral
// (turnExpired). Without the lane, new Codex work that used up every pass's
// allowance would leave every waiting session unopened for good; with it,
// each waiting session leads in turn, and one that needs more than the
// whole allowance is opened, deferred, and fails at MaxDeferral like any
// other deferral.
//
// Without the rest, one session whose lookups alone need more than the
// whole allowance would be first of the Codex sessions on every pass: it
// would spend the allowance and be deferred, and every Codex session after
// it would be left for the next pass (codexLookupSpent) on every pass too.
// Moved last, it still has its turn once the others have had theirs.
func (p *pass) orderLookupAllowanceDeferralsLast() {
	var slots []int
	var groups [2][lookupAllowanceTurns][]archive.SessionRegistration
	for i, reg := range p.registrations {
		if reg.Harness.Name != archive.HarnessCodex || p.requests[reg.ArchiveSessionID].Token == "" {
			continue
		}
		slots = append(slots, i)
		waits := 0
		if waitsForParent(reg) {
			waits = 1
		}
		turn := p.lookupAllowanceTurn(reg.ArchiveSessionID)
		groups[waits][turn] = append(groups[waits][turn], reg)
	}
	ordered := make([]archive.SessionRegistration, 0, len(slots))
	for waits := range groups {
		turns := &groups[waits]
		for turn := range turns {
			if turn != turnFirst {
				p.rotateWaiting(turns[turn])
			}
		}
		if lane := turns[turnLeftUnopened]; waits == 0 && len(lane) > 0 {
			ordered = append(ordered, lane[0])
			turns[turnLeftUnopened] = lane[1:]
		}
		for _, group := range turns {
			ordered = append(ordered, group...)
		}
	}
	for k, reg := range ordered {
		p.registrations[slots[k]] = reg
	}
}

// rotateWaiting orders sessions waiting on the allowance as the previous
// pass left them unopened (priorUnreached), and those it did not leave
// unopened after them in their current order. A session whose wait the
// previous pass began (joinedLastPass) goes after every session already
// waiting before it: the previous pass reached it, as a session not
// waiting, ahead of those, so its rank alone would put each newcomer at the
// front of the queue, and a steady stream of new Codex work would take the
// waiter lane on every pass. Behind them, the waiters keep a queue: each
// joins at the back and leaves the front in turn.
func (p *pass) rotateWaiting(group []archive.SessionRegistration) {
	sort.SliceStable(group, func(i, j int) bool {
		if aNew, bNew := p.joinedLastPass(group[i].ArchiveSessionID), p.joinedLastPass(group[j].ArchiveSessionID); aNew != bNew {
			return bNew
		}
		a, aLeft := p.priorUnreached[group[i].ArchiveSessionID]
		b, bLeft := p.priorUnreached[group[j].ArchiveSessionID]
		if aLeft != bLeft {
			return aLeft
		}
		return aLeft && a < b
	})
}

// joinedLastPass reports whether the session's run of deferrals began on the
// previous pass: its clock (priorDeferredSince) is that pass's time
// (state.Status.LastScanAt), as startLookupAllowanceWait and deferScan
// start one.
func (p *pass) joinedLastPass(id string) bool {
	since, running := p.priorDeferredSince[id]
	return running && !p.priorScanAt.IsZero() && !since.Before(p.priorScanAt)
}

// The groups orderLookupAllowanceDeferralsLast puts requested Codex sessions
// in, first to last, but for the waiter lane (see turnLeftUnopened).
const (
	// turnFirst: not waiting on the allowance.
	turnFirst = iota
	// turnLeftUnopened: the previous pass kept the session's clock without
	// deferring it, which it does for a session it left unopened
	// (codexLookupSpent, which starts a clock if none runs, or an early
	// end). The first of them takes the waiter lane, ahead of turnFirst.
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
