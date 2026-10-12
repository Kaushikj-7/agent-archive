package collector

import (
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Reasons a scan was deferred (Result.Deferred, state.Status.DeferredSessions).
// They carry no path or content, and once written keep their spelling.
const (
	// DeferredHistoryCoverage: Codex has not yet established which rollout
	// files make up the thread's history.
	DeferredHistoryCoverage = "codex_history_coverage"
	// DeferredSourceChanged: the source (a transcript, or the native index
	// naming it) changed while it was being read.
	DeferredSourceChanged = "source_changed"
	// DeferredReadBudget: the pass's shared read memory was held by others
	// when the session needed it. A session that needs more than its own
	// share (SessionReadCeiling) is a size-limit gap instead.
	DeferredReadBudget = "read_budget"
)

// readBudgetDeferralError marks a source read the pass's shared read ledger
// refused while others held it, the session itself being within its own
// share (see SessionReadCeiling): a later pass, starting with the ledger
// free, reads it. Only the session's own source read is marked; any other
// refusal of the ledger stays a failure.
type readBudgetDeferralError struct{ err error }

func (e *readBudgetDeferralError) Error() string { return e.err.Error() }

func (e *readBudgetDeferralError) Unwrap() error { return e.err }

// deferralReason is the reason deferralReason, which asks only the outermost
// error of each failure it joins, reports for the type.
func (*readBudgetDeferralError) deferralReason() string { return DeferredReadBudget }

// MaxDeferral bounds how long a session's scans may be deferred: once every
// pass for this long has deferred them, the next deferral is a failure,
// reported as one (DeferralExpiredProblem), and so is every deferral after
// it, so a refusal no later pass clears (coverage that never completes for
// the thread, a source changed on every read) cannot hide as "rechecking"
// for good. A pass that scans the session without deferring it (it fails
// otherwise, or succeeds) starts the clock again; a pass that ends before
// scanning it leaves the clock running.
const MaxDeferral = 24 * time.Hour

// DeferralExpiredProblem prefixes the error of a session whose deferral
// outlasted MaxDeferral.
var DeferralExpiredProblem = fmt.Sprintf("still not read after %d hours of retries", int(MaxDeferral/time.Hour))

// loadDeferredSince reads when each session's run of deferred scans began
// (state.Status.DeferredSince) from the previous pass's status. A status
// that cannot be read starts every clock again, delaying a failure by at
// most MaxDeferral.
func (p *pass) loadDeferredSince() {
	if previous, err := p.local.LoadStatus(); err == nil {
		p.priorDeferredSince = previous.DeferredSince
	}
}

// deferredSince is when the run of deferred scans this deferral continues
// began: the previous pass's, when it deferred the session too, or now.
func (p *pass) deferredSince(id string) time.Time {
	now := p.now.UTC()
	if since, ok := p.priorDeferredSince[id]; ok && !since.IsZero() && !since.After(now) {
		return since
	}
	return now
}

// keepDeferredSince records since as when the session's run of deferred
// scans began, for the next pass to continue.
func (p *pass) keepDeferredSince(id string, since time.Time) {
	if p.deferredAt == nil {
		p.deferredAt = map[string]time.Time{}
	}
	p.deferredAt[id] = since
}

// carryDeferredSince keeps the previous pass's clock for a session this pass
// ended before scanning (it stopped early, or ran out of time with the
// session in flight): not scanning a session is not scanning it without a
// deferral, so its run of deferred scans goes on.
func (p *pass) carryDeferredSince(id string) {
	if since, ok := p.priorDeferredSince[id]; ok {
		p.keepDeferredSince(id, since)
	}
}

// deferralReason reports whether a scan's error only defers the session, and
// why: every failure it joins is a source refusal the next pass is expected
// to clear, a history coverage refusal (agentapi.CauseHistoryCoverage, which
// the Codex source names where it refuses) or a source that changed while
// being read (agentapi.Changed). One failure of any other kind makes the
// whole error a failure. The outermost SourceError of each failure decides,
// as agentapi.Failure does.
func deferralReason(err error) (string, bool) {
	if classified, ok := err.(agentapi.Classified); ok {
		kind, cause := classified.Classification()
		switch {
		case cause == agentapi.CauseHistoryCoverage:
			return DeferredHistoryCoverage, true
		case kind == agentapi.Changed:
			return DeferredSourceChanged, true
		}
		return "", false
	}
	if deferral, ok := err.(interface{ deferralReason() string }); ok {
		return deferral.deferralReason(), true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		reason := ""
		for _, part := range joined.Unwrap() {
			if part == nil {
				continue
			}
			partReason, ok := deferralReason(part)
			if !ok {
				return "", false
			}
			// Coverage is the more specific reason, so it names the deferral.
			if reason == "" || partReason == DeferredHistoryCoverage {
				reason = partReason
			}
		}
		return reason, reason != ""
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return deferralReason(wrapped.Unwrap())
	}
	return "", false
}
