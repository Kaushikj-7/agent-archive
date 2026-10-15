package collector

import (
	"sort"
	"time"

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

// leaveUnopenedForLookupAllowance reports whether the pass leaves reg for
// the next pass without opening it, for the Codex lookup allowance: it is
// held for a later attempt on its backoff schedule (holdLookupRetry), or
// the allowance is spent (codexLookupSpent), and then its wait on the
// allowance begins if none runs (startLookupAllowanceWait).
func (p *pass) leaveUnopenedForLookupAllowance(reg archive.SessionRegistration) bool {
	if _, held := p.lookupRetryHeld[reg.ArchiveSessionID]; held {
		return true
	}
	if !p.codexLookupSpent(reg) {
		return false
	}
	p.startLookupAllowanceWait(reg)
	return true
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
	p.markLookupWaiter(id)
}

// markLookupWaiter records that the session waits on the Codex lookup
// allowance this pass, so a failure for being left unopened past
// MaxDeferral (leaveForNextPass) puts it on the retry schedule
// (scheduleLookupRetry).
func (p *pass) markLookupWaiter(id string) {
	if p.lookupWaiters == nil {
		p.lookupWaiters = map[string]bool{}
	}
	p.lookupWaiters[id] = true
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
//
// A session whose wait has lasted MaxDeferral and that has failed for it is
// retried on a backoff schedule (nextLookupRetry) rather than on every pass:
// on a pass with an attempt due (turnRetryDue), the one with the oldest
// clock takes the retry lane, ahead of the waiter lane, so it is read with
// a fresh allowance; the others stay due for a later pass. Every other such
// session (turnRetryLater) is held for its next attempt
// (holdForLookupRetry), unopened. Native children waiting for their parent
// have no lane: the oldest due among them is read first among them.
func (p *pass) orderLookupAllowanceDeferralsLast(registrations []archive.SessionRegistration) {
	var slots []int
	var groups [2][lookupAllowanceTurns][]archive.SessionRegistration
	for i, reg := range registrations {
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
		if p.waitsOnLookupAllowance(reg.ArchiveSessionID, turn) {
			p.markLookupWaiter(reg.ArchiveSessionID)
		}
	}
	ordered := make([]archive.SessionRegistration, 0, len(slots))
	for waits := range groups {
		turns := &groups[waits]
		for turn := range turns {
			switch turn {
			case turnFirst:
			case turnRetryDue:
				p.oldestClockFirst(turns[turn])
			default:
				p.rotateWaiting(turns[turn])
			}
		}
		if due := turns[turnRetryDue]; len(due) > 0 {
			// One attempt a pass, in the retry lane; the others stay due.
			ordered = append(ordered, due[0])
			for _, held := range due[1:] {
				p.holdLookupRetry(held.ArchiveSessionID)
			}
			turns[turnRetryDue] = due[1:]
		}
		for _, held := range turns[turnRetryLater] {
			p.holdLookupRetry(held.ArchiveSessionID)
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
		registrations[slots[k]] = reg
	}
}

// waitsOnLookupAllowance reports whether a requested Codex session in turn
// waits on the lookup allowance: it is in one of the waiting groups, or its
// wait has lasted MaxDeferral and the previous pass deferred it for the
// allowance or left it unopened. An expired session that last failed for
// another reason, such as a source that kept changing, is not one.
func (p *pass) waitsOnLookupAllowance(id string, turn int) bool {
	switch turn {
	case turnFirst:
		return false
	case turnExpired:
		_, unreached := p.priorUnreached[id]
		return unreached || p.priorDeferred[id] == DeferredLookupAllowance
	default:
		return true
	}
}

// oldestClockFirst orders sessions by when their run of deferrals began
// (priorDeferredSince), oldest first, then by archive session ID.
func (p *pass) oldestClockFirst(group []archive.SessionRegistration) {
	sort.SliceStable(group, func(i, j int) bool {
		a, b := p.priorDeferredSince[group[i].ArchiveSessionID], p.priorDeferredSince[group[j].ArchiveSessionID]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return group[i].ArchiveSessionID < group[j].ArchiveSessionID
	})
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
	// failing go first. Once one that waits on the allowance has failed
	// for it, it is retried on a backoff schedule instead: turnRetryDue or
	// turnRetryLater.
	turnExpired
	// turnRetryDue: its wait has lasted MaxDeferral, it failed for it
	// (state.Status.NextRetryAt), and an attempt on its backoff schedule is
	// due (lookupRetryDue). The oldest of them takes the retry lane, ahead
	// of every other requested Codex session; the others are held, still
	// due.
	turnRetryDue
	// turnRetryLater: as turnRetryDue, with no attempt due yet. It is held
	// unopened until one is (holdForLookupRetry).
	turnRetryLater
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
		if _, retrying := p.priorRetryAt[id]; !retrying {
			return turnExpired
		}
		if due, _ := p.lookupRetryDue(id, since); due {
			return turnRetryDue
		}
		return turnRetryLater
	case deferred:
		return turnRanOut
	default:
		return turnLeftUnopened
	}
}

// Backoff for a session whose wait on the lookup allowance outlasted
// MaxDeferral and failed for it: its first attempt is due
// lookupRetryFirstGap after its clock (state.Status.DeferredSince) reached
// MaxDeferral, and the gap doubles after each attempt, up to
// LookupRetryMaxGap. Retries go on for as long as the session is pending:
// it is never dropped.
const (
	lookupRetryFirstGap = time.Hour
	// LookupRetryMaxGap bounds the gap between two attempts.
	LookupRetryMaxGap = 7 * 24 * time.Hour
)

// nextLookupRetry is the first attempt on the backoff schedule of a session
// whose clock started at since that falls strictly after after: since +
// MaxDeferral + 1h, + 3h, + 7h, and so on, the gap doubling up to
// LookupRetryMaxGap and staying there. The loop doubles at most eight
// times; the capped part is computed, in one step unless after is more
// than time.Duration's range (about 292 years) past since, as with a clock
// read as zero: after.Sub then saturates, and each step covers that range.
func nextLookupRetry(since, after time.Time) time.Time {
	gap := lookupRetryFirstGap
	at := since.Add(MaxDeferral + gap)
	for !at.After(after) && gap < LookupRetryMaxGap {
		gap = min(2*gap, LookupRetryMaxGap)
		at = at.Add(gap)
	}
	for !at.After(after) {
		// No overflow: the whole gaps in after.Sub(at) fit in a Duration.
		at = at.Add(gap * time.Duration(int64(after.Sub(at)/gap)))
		if !at.After(after) {
			at = at.Add(gap)
		}
	}
	return at
}

// lookupRetryDue reports whether an attempt is due on this pass for a
// session on the backoff schedule (state.Status.NextRetryAt), and when the
// attempt it is due for, or else its next one, is. One is due when an
// attempt falls after the previous pass and no later than now, or when the
// previous pass found one due without giving it the lane (its NextRetryAt
// is no later than that pass). A previous pass whose time is unknown, or
// after now, makes one due.
func (p *pass) lookupRetryDue(id string, since time.Time) (bool, time.Time) {
	now := p.now.UTC()
	if p.priorScanAt.IsZero() || p.priorScanAt.After(now) {
		return true, now
	}
	if prior := p.priorRetryAt[id]; !prior.After(p.priorScanAt) {
		return true, prior
	}
	at := nextLookupRetry(since, p.priorScanAt)
	return !at.After(now), at
}

// scheduleLookupRetry puts a session that failed this pass for waiting on
// the lookup allowance past MaxDeferral on its backoff schedule: its next
// attempt is the first after now (state.Status.NextRetryAt).
func (p *pass) scheduleLookupRetry(id string, since time.Time) {
	p.keepRetryAt(id, nextLookupRetry(since, p.now))
}

// keepRetryAt records at as the session's next attempt, for the next pass.
func (p *pass) keepRetryAt(id string, at time.Time) {
	if p.retryAt == nil {
		p.retryAt = map[string]time.Time{}
	}
	p.retryAt[id] = at.UTC()
}

// holdLookupRetry marks a session on the backoff schedule as one this pass
// leaves unopened (holdForLookupRetry).
func (p *pass) holdLookupRetry(id string) {
	_, at := p.lookupRetryDue(id, p.priorDeferredSince[id])
	if p.lookupRetryHeld == nil {
		p.lookupRetryHeld = map[string]time.Time{}
	}
	p.lookupRetryHeld[id] = at
}

// holdForLookupRetry leaves a session on the backoff schedule unopened when
// this pass makes no attempt for it: its clock, its next attempt and its
// issue (saveStatus) are carried to the next pass, it is counted as
// pending, and it is listed in Result.RetryLater, not in Errors, so the pass
// reports no new failure for it. It reports whether the session is held.
func (p *pass) holdForLookupRetry(id string) bool {
	at, held := p.lookupRetryHeld[id]
	if !held {
		return false
	}
	p.carryDeferredSince(id)
	p.retryLater(id, at)
	if !p.durableCounted[id] {
		p.pending++
	}
	return true
}

// retryLater records at as the next attempt of a session on the backoff
// schedule that this pass made no attempt for, and lists it in
// Result.RetryLater, so internal/cli still counts its carried issue.
func (p *pass) retryLater(id string, at time.Time) {
	p.keepRetryAt(id, at)
	if p.result.RetryLater == nil {
		p.result.RetryLater = map[string]time.Time{}
	}
	p.result.RetryLater[id] = at.UTC()
}

// holdDueLookupRetries holds each session in rest with an attempt due that
// a pass ending early (Stop or its deadline) never reached, as one held
// for a later retry would be, with the attempt it was due for. The pass
// made no attempt, so it neither fails the session nor moves its next
// attempt on to a later time on the schedule: the next pass makes it.
func (p *pass) holdDueLookupRetries(rest []archive.SessionRegistration) {
	for _, reg := range rest {
		id := reg.ArchiveSessionID
		if _, held := p.lookupRetryHeld[id]; held || reg.Harness.Name != archive.HarnessCodex || p.requests[id].Token == "" {
			continue
		}
		if p.lookupAllowanceTurn(id) == turnRetryDue {
			p.holdLookupRetry(id)
		}
	}
}
