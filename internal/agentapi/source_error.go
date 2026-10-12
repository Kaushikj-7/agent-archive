package agentapi

import (
	"context"
	"errors"
	"fmt"
)

// FailureKind controls retry and fallback without inspecting provider error text.
type FailureKind string

// Failure kinds distinguish source absence, retryable reads, refusals and cleanup.
const (
	Missing        FailureKind = "missing"
	Unavailable    FailureKind = "unavailable"
	Changed        FailureKind = "changed"
	FormatMismatch FailureKind = "format_mismatch"
	Unsafe         FailureKind = "unsafe"
	Limit          FailureKind = "limit"
	Cleanup        FailureKind = "cleanup"
)

// SourceCause names why a source failed, in fixed words that carry no path
// or content, where its kind alone would leave the reader guessing. Callers
// still switch on Kind; a cause only words the failure (see SourceError.Error)
// and lets a caller tell a deferral from a failure.
type SourceCause string

// Source causes.
const (
	// CauseHistoryCoverage: Codex has not yet established which rollout files
	// make up the thread's history, so the source cannot select its lineage
	// or list its historical candidates. A later pass, once coverage
	// completes, can.
	CauseHistoryCoverage SourceCause = "history_coverage"
)

// causeText words each cause after the kind's own words.
var causeText = map[SourceCause]string{
	CauseHistoryCoverage: "Codex history coverage incomplete",
}

// SourceError retains a local cause and a bounded public classification.
type SourceError struct {
	Kind     FailureKind
	Err      error
	Observed *SourceObservation
	// Cause, when set, words why the source failed (see SourceCause). It never
	// changes Kind.
	Cause SourceCause
}

func (e *SourceError) Error() string {
	text := "source " + string(e.Kind)
	if e.Kind == Unsafe || e.Kind == FormatMismatch {
		text = "unsafe source format"
	}
	if e.Kind == Changed {
		text = "source changed while reading; try again"
	}
	if cause, ok := causeText[e.Cause]; ok {
		text += ": " + cause
	}
	return text
}

func (e *SourceError) Unwrap() error { return e.Err }

// Classification reports this error's own kind and cause (see Classified).
func (e *SourceError) Classification() (FailureKind, SourceCause) { return e.Kind, e.Cause }

// Classified is implemented by SourceError alone. A caller that walks an error
// tree itself asks each node through it: errors.As would skip past a join to
// the first SourceError anywhere below.
type Classified interface {
	Classification() (FailureKind, SourceCause)
}

// Failure reports the outer classified failure, including joined cleanup errors.
func Failure(err error) FailureKind {
	var e *SourceError
	if errors.As(err, &e) {
		return e.Kind
	}
	return Unavailable
}

// Wrap classifies a native cause without losing errors.Is/As behavior.
func Wrap(k FailureKind, err error) error {
	if err == nil {
		return nil
	}
	return &SourceError{Kind: k, Err: err}
}

// WithCause returns err with cause recorded on its outermost SourceError,
// leaving the original unchanged. An err that is not itself a SourceError (a
// join, or one wrapped with more context) is returned as it is: a cause
// describes one failure, never a group of them.
func WithCause(err error, cause SourceCause) error {
	var e *SourceError
	if _, outermost := err.(Classified); !outermost || !errors.As(err, &e) {
		return err
	}
	copied := *e
	copied.Cause = cause
	return &copied
}

// ErrClosed rejects use of a borrowed input after its owner closes.
var ErrClosed = errors.New("source owner is closed")

// ErrRawLimit reports an aggregate source value limit.
var ErrRawLimit = errors.New("source exceeds raw size limit")

// RawLimitError is ErrRawLimit with the sizes that decided it, so a recorded
// gap can name the limit and how far over it the source is.
type RawLimitError struct {
	Size  int64
	Limit int64
}

func (e *RawLimitError) Error() string {
	return fmt.Sprintf("%v: %d bytes, limit %d bytes", ErrRawLimit, e.Size, e.Limit)
}

// Is matches ErrRawLimit.
func (e *RawLimitError) Is(target error) bool { return target == ErrRawLimit }

// RawLimit is a Limit failure for a source of size bytes over limit.
func RawLimit(size, limit int64, observed *SourceObservation) error {
	return &SourceError{Kind: Limit, Err: &RawLimitError{Size: size, Limit: limit}, Observed: observed}
}

// Deterministic permits caching only stable filter/limit failures without cleanup or retryable causes.
func Deterministic(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return deterministicTree(err)
}

func deterministicTree(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !deterministicTree(e) {
				return false
			}
		}
		return true
	}
	var e *SourceError
	if errors.As(err, &e) && e.Kind != Unsafe && e.Kind != FormatMismatch && e.Kind != Limit {
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return deterministicTree(wrapped.Unwrap())
	}
	return true
}

// ErrorObservation is the exact verified observation attached to a deterministic read failure.
func ErrorObservation(err error) (SourceObservation, bool) {
	var e *SourceError
	if errors.As(err, &e) && e.Observed != nil {
		return *e.Observed, true
	}
	return SourceObservation{}, false
}

// HasFailure inspects every joined failure, including secondary cleanup errors.
func HasFailure(err error, kind FailureKind) bool {
	if err == nil {
		return false
	}
	var e *SourceError
	if errors.As(err, &e) && e.Kind == kind {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if HasFailure(e, kind) {
				return true
			}
		}
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return HasFailure(single.Unwrap(), kind)
	}
	return false
}
