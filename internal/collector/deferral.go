package collector

import (
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Reasons a scan was deferred (Result.Deferred, state.Status.DeferredSessions).
// They carry no path or content, and once written keep their spelling.
const (
	// DeferredSourceChanged: the source (a transcript, or the native index
	// naming it) changed while it was being read.
	DeferredSourceChanged = "source_changed"
	// DeferredReadBudget: the pass's shared read memory was held by others
	// when the session needed it. A session that needs more than its own
	// share (SessionReadCeiling) is a size-limit gap instead.
	DeferredReadBudget = "read_budget"
	// DeferredLookupAllowance: the pass's allowance for finding Codex
	// rollouts ran out while the session was being read
	// (agentapi.CauseLookupAllowanceSpent). The next pass has a fresh one.
	DeferredLookupAllowance = "lookup_allowance"
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
// it, so a refusal no later pass clears (a source changed on every read)
// cannot hide as "rechecking"
// for good. A pass that scans the session without deferring it (it fails
// otherwise, or succeeds) starts the clock again; a pass that ends before
// scanning it, or leaves it unopened, leaves the clock running, and fails it
// once the clock has run for MaxDeferral (leaveForNextPass). A Codex session
// that failed this way while waiting on the lookup allowance is then
// retried on a backoff schedule, not on every pass (nextLookupRetry).
const MaxDeferral = 24 * time.Hour

// DeferralExpiredProblem prefixes the error of a session whose deferral
// outlasted MaxDeferral.
var DeferralExpiredProblem = fmt.Sprintf("still not read after %d hours of retries", int(MaxDeferral/time.Hour))

// loadDeferredSince reads when each session's run of deferred scans began
// (state.Status.DeferredSince), why the previous pass deferred each
// (state.Status.DeferredSessions), where it left each it did not reach
// (state.Status.UnreachedSessions), when it ran (state.Status.LastScanAt),
// and which sessions are on the lookup allowance's backoff schedule
// (state.Status.NextRetryAt), from the previous pass's status. A status
// that cannot be read starts every clock again, delaying a failure by at
// most MaxDeferral.
func (p *pass) loadDeferredSince() {
	if previous, err := p.local.LoadStatus(); err == nil {
		p.priorDeferredSince = previous.DeferredSince
		p.priorDeferred = previous.DeferredSessions
		p.priorUnreached = previous.UnreachedSessions
		p.priorScanAt = previous.LastScanAt
		p.priorRetryAt = previous.NextRetryAt
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

// errLeftUnopened is the cause leaveForNextPass gives a session it fails for
// having been left unopened past MaxDeferral.
var errLeftUnopened = errors.New("left for the next pass without being opened")

// deferralReason reports whether a scan's error only defers the session, and
// why: every failure it joins is a source refusal the next pass is expected
// to clear, a source that changed while being read (agentapi.Changed) or a
// Codex lookup past the pass's allowance (agentapi.CauseLookupAllowanceSpent).
// One failure of any other kind makes the
// whole error a failure. The outermost SourceError of each failure decides,
// as agentapi.Failure does.
func deferralReason(err error) (string, bool) {
	if classified, ok := err.(agentapi.Classified); ok {
		switch kind, cause := classified.Classification(); {
		case kind == agentapi.Changed:
			return DeferredSourceChanged, true
		case kind == agentapi.Limit && cause == agentapi.CauseLookupAllowanceSpent:
			return DeferredLookupAllowance, true
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
			if reason == "" {
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
