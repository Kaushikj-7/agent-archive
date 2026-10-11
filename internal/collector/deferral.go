package collector

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
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
)

// coverageLookupTimeout bounds the one coverage lookup a failed Codex scan
// may cost (see pass.nameCodexCoverage).
const coverageLookupTimeout = 5 * time.Second

// deferralReason reports whether a scan's error only defers the session, and
// why: every failure it joins is a source refusal the next pass is expected
// to clear, a history coverage refusal (agentapi.CauseHistoryCoverage) or a
// source that changed while being read (agentapi.Changed). One failure of any
// other kind makes the whole error a failure. The outermost SourceError of
// each failure decides, as agentapi.Failure does.
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

// nameCodexCoverage records agentapi.CauseHistoryCoverage on the refusals in
// a failed Codex scan's error when the thread's history coverage is not
// complete: the Codex source refuses to read a thread whose rollout lineage
// or history candidates are not fully known yet, which a later pass, once
// coverage completes, reads. The source's refusal itself names no cause, so
// the pass asks the coverage lookup the source read, once, and only for a
// scan whose error holds such a refusal (see coverageRefusal). The lookup
// answers from the pass's own cache once the source has asked it about the
// thread. Any other error is returned unchanged.
func (p *pass) nameCodexCoverage(ctx context.Context, reg archive.SessionRegistration, err error) error {
	lookup := p.opts.CodexRollouts
	if err == nil || lookup == nil || reg.Harness.Name != archive.HarnessCodex || reg.NativeSessionID == "" || !holdsCoverageRefusal(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, coverageLookupTimeout)
	defer cancel()
	set, lookupErr := lookup.Thread(ctx, reg.NativeSessionID)
	if lookupErr != nil || set.Complete {
		return err
	}
	return nameCoverageRefusals(err)
}

// coverageRefusal reports whether err is itself a bare refusal of the kind a
// Codex source gives while history coverage is incomplete: an Unavailable
// SourceError with no cause yet, wrapping no further error (an I/O failure, a
// cancellation, an exhausted read budget or a closed owner wraps one, or is
// one).
func coverageRefusal(err error) bool {
	classified, ok := err.(agentapi.Classified)
	if !ok {
		return false
	}
	if kind, cause := classified.Classification(); kind != agentapi.Unavailable || cause != "" {
		return false
	}
	native := errors.Unwrap(err)
	if native == nil || errors.Unwrap(native) != nil {
		return false
	}
	if _, joined := native.(interface{ Unwrap() []error }); joined {
		return false
	}
	for _, other := range []error{context.Canceled, context.DeadlineExceeded, agentapi.ErrReadBudget, agentapi.ErrClosed} {
		if errors.Is(native, other) {
			return false
		}
	}
	return true
}

// holdsCoverageRefusal reports whether any failure err holds is a
// coverageRefusal.
func holdsCoverageRefusal(err error) bool {
	if _, ok := err.(agentapi.Classified); ok {
		// Its own kind decides, as in deferralReason.
		return coverageRefusal(err)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return slices.ContainsFunc(joined.Unwrap(), func(part error) bool { return part != nil && holdsCoverageRefusal(part) })
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return holdsCoverageRefusal(wrapped.Unwrap())
	}
	return false
}

// nameCoverageRefusals returns err with CauseHistoryCoverage recorded on each
// coverageRefusal it holds directly or through errors.Join. A refusal wrapped
// with more words (fmt.Errorf) is left as it is, since the wrapper cannot be
// rebuilt around a named copy; deferralReason then still finds it unnamed.
func nameCoverageRefusals(err error) error {
	if _, ok := err.(agentapi.Classified); ok {
		if coverageRefusal(err) {
			return agentapi.WithCause(err, agentapi.CauseHistoryCoverage)
		}
		return err
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	parts := joined.Unwrap()
	texts := make([]string, 0, len(parts))
	named := make([]error, 0, len(parts))
	for _, part := range parts {
		if part != nil {
			texts = append(texts, part.Error())
			named = append(named, nameCoverageRefusals(part))
		}
	}
	// Only a plain errors.Join, whose text is its parts' on lines of their
	// own, can be rebuilt as one.
	if err.Error() != strings.Join(texts, "\n") {
		return err
	}
	return errors.Join(named...)
}
