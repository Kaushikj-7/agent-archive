package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/state"
)

// filteredLimitError is errTranscriptTooLarge for what the filter kept.
type filteredLimitError struct {
	size  int64
	limit int64
}

func (e *filteredLimitError) Error() string {
	return fmt.Sprintf("%v of %d bytes after filtering (%d bytes)", errTranscriptTooLarge, e.limit, e.size)
}

func (e *filteredLimitError) Unwrap() error { return errTranscriptTooLarge }

// errSessionReadCeiling is a read the session's own share of the pass's
// ledger (SessionReadCeiling) refused: waiting does not change it.
var errSessionReadCeiling = errors.New("session needs more memory to read than one session may hold in a pass")

// overSessionCeiling reports a read refused at the session's own share of
// the pass's ledger (see SessionReadCeiling), with nothing else, such as a
// change or a cancellation, that a later read could clear.
func (s *sessionScan) overSessionCeiling(err error) bool {
	return errors.Is(err, agentapi.ErrReadBudget) && s.readBudget().ScopeExceeded() &&
		!agentapi.HasFailure(err, agentapi.Cleanup) && !agentapi.HasFailure(err, agentapi.Changed) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// overCeiling ends a scan whose work other than the source read itself
// (refreshing the last publication, building, compressing or reconciling
// what was read, saving it) needed more than the session's share of the
// pass: the same size-limit gap a read over it records, at the state the
// source was read at, or is in now when the scan never read it. Work
// refused only because others held the ledger is deferred to a later pass,
// as a source read is. So is a scan that left a publication pending: a gap
// would leave the upload owed and retried every pass without ever reading
// the source again, while a deferral fails once it has lasted MaxDeferral.
func (s *sessionScan) overCeiling(err error) (sessionOutcome, error) {
	if !s.overSessionCeiling(err) {
		if s.sharedPressure(err) {
			return outcomeSkipped, &readBudgetDeferralError{err: err}
		}
		return outcomeSkipped, err
	}
	if owed, owedErr := s.local.Outstanding(s.reg, false); owedErr != nil {
		return outcomeSkipped, errors.Join(err, owedErr)
	} else if owed.Upload {
		return outcomeSkipped, &readBudgetDeferralError{err: err}
	}
	s.releaseRetained()
	var read sourceRead
	if s.lastRead != nil {
		read = *s.lastRead
	} else {
		reader, available := newSourceReader(s.reg, s.opts)
		adapter, adapterErr := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
		if !available || adapterErr != nil {
			return outcomeSkipped, err
		}
		observed, signatureErr := reader.Signature(s.ctx)
		if signatureErr != nil {
			return outcomeSkipped, errors.Join(err, signatureErr)
		}
		read = sourceRead{adapter: adapter, observed: observed}
	}
	s.sizeLimit = sizeLimitOf(errSessionReadCeiling, read.observed, s.reg.Harness.Name, s.opts.maxTranscriptBytes())
	return s.blockThenRemember(state.BlockedReasonTranscriptTooLarge, read)
}

// readCeilingChanged reports a per-session read gap reached under another
// ceiling than this build's: raising the ceiling reads the session again.
func readCeilingChanged(signature state.ScanSignature) bool {
	limit := signature.SizeLimit
	return limit != nil && limit.Kind == state.SizeLimitReadBudget && limit.LimitBytes != sessionReadCeiling
}

// sharedPressure reports a refusal the session met within its own ceiling
// because others held the pass's ledger: a later pass, starting with it
// free, need not meet it.
func (s *sessionScan) sharedPressure(err error) bool {
	return onlyReadBudget(err) && s.readBudget().ScopePressured() && !s.readBudget().ScopeExceeded()
}

// onlyReadBudget reports a refusal of the read ledger and nothing a
// deferral would hide: every failure err joins is such a refusal, or the
// sizing that led to one (jsonwire.ErrLimit), with no change, cleanup
// failure or cancellation, and no deferral already.
func onlyReadBudget(err error) bool {
	if agentapi.HasFailure(err, agentapi.Cleanup) || agentapi.HasFailure(err, agentapi.Changed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	refused, other := readBudgetParts(err)
	return refused && !other
}

// readBudgetParts reports whether err holds a ledger refusal, and whether it
// holds any failure that is neither one nor the sizing behind one.
func readBudgetParts(err error) (refused, other bool) {
	if _, deferred := err.(interface{ deferralReason() string }); deferred {
		return false, true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			if part == nil {
				continue
			}
			r, o := readBudgetParts(part)
			refused, other = refused || r, other || o
		}
		return refused, other
	}
	if errors.Is(err, agentapi.ErrReadBudget) {
		return true, false
	}
	if errors.Is(err, jsonwire.ErrLimit) {
		return false, false
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return readBudgetParts(wrapped)
	}
	return false, true
}

// sizeLimitOf names the limit a size-limit failure stopped at, or nil when
// err carries no sizes (observed supplies the source's size then).
func sizeLimitOf(err error, observed sourceState, harness string, maxBytes int64) *state.SizeLimitGap {
	var raw *agentapi.RawLimitError
	var filtered *filteredLimitError
	switch {
	case errors.As(err, &raw):
		return &state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: raw.Limit, SizeBytes: raw.Size}
	case errors.As(err, &filtered):
		return &state.SizeLimitGap{Kind: state.SizeLimitFiltered, LimitBytes: filtered.limit, SizeBytes: filtered.size}
	case errors.Is(err, errSessionReadCeiling):
		return &state.SizeLimitGap{Kind: state.SizeLimitReadBudget, LimitBytes: sessionReadCeiling}
	case errors.Is(err, errRecordTooLarge), errors.Is(err, archive.ErrRecordTooLarge):
		return &state.SizeLimitGap{Kind: state.SizeLimitRecord, LimitBytes: recordLimit}
	case errors.Is(err, errTranscriptTooLarge):
		return &state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: maxRawBytes(harness, maxBytes), SizeBytes: observed.size()}
	}
	return nil
}

// SizeLimitLabel words a size-limit gap's limit, as status and the pass's
// problem name it: "Codex raw 512 MiB".
func SizeLimitLabel(harness string, limit state.SizeLimitGap) string {
	mib := limitBytes(limit.LimitBytes)
	switch limit.Kind {
	case state.SizeLimitRaw:
		if harness == archive.HarnessCodex {
			return "Codex raw " + mib
		}
		return "raw " + mib
	case state.SizeLimitFiltered:
		return "filtered " + mib
	case state.SizeLimitRecord:
		return "record " + mib
	case state.SizeLimitReadBudget:
		return "per-session read memory " + mib
	}
	return mib
}

// limitBytes words a limit in the largest binary unit it reaches, rounded
// to a tenth: "512 MiB", "1.5 MiB", "64 KiB", "100 bytes".
func limitBytes(n int64) string {
	for _, unit := range []struct {
		shift uint
		name  string
	}{{20, "MiB"}, {10, "KiB"}} {
		if n >= 1<<unit.shift {
			value := strconv.FormatFloat(float64(n)/float64(int64(1)<<unit.shift), 'f', 1, 64)
			return strings.TrimSuffix(value, ".0") + " " + unit.name
		}
	}
	return fmt.Sprintf("%d bytes", n)
}

// sizeLimitProblemPrefix begins every SizeLimitProblem.
const sizeLimitProblemPrefix = " session(s) stopped being captured at a size limit"

// SizeLimitProblem is the problem a pass records in its Status when n
// sessions sit in a size-limit gap: not a failure, but capture of them has
// stopped at their last snapshot. limits are the limits they stopped at
// (SizeLimitLabel), named once each.
func SizeLimitProblem(n int, limits ...string) string {
	named := slices.Compact(slices.Sorted(slices.Values(limits)))
	text := fmt.Sprintf("%d%s", n, sizeLimitProblemPrefix)
	if len(named) > 0 {
		text += " (" + strings.Join(named, ", ") + ")"
	}
	return text + ", kept at their last snapshot; agent-archive status APP lists them"
}

// IsSizeLimitProblem reports whether problem is a SizeLimitProblem, for any
// count and limits.
func IsSizeLimitProblem(problem string) bool {
	var n int
	if _, err := fmt.Sscanf(problem, "%d ", &n); err != nil || n < 1 {
		return false
	}
	// A status written before the limits were named.
	if problem == fmt.Sprintf("%d session(s) stopped being captured: over the transcript size limit, kept at their last snapshot", n) {
		return true
	}
	rest, ok := strings.CutPrefix(problem, fmt.Sprintf("%d%s", n, sizeLimitProblemPrefix))
	if !ok {
		return false
	}
	const suffix = ", kept at their last snapshot; agent-archive status APP lists them"
	if rest == suffix {
		return true
	}
	inner, ok := strings.CutSuffix(rest, ")"+suffix)
	return ok && strings.HasPrefix(inner, " (") && !strings.ContainsAny(inner[2:], "()")
}
