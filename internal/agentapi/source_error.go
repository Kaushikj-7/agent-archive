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

// Native source refusal causes are fixed codes with no paths or content.
const (
	// CauseAggregateHistoryByteLimit identifies the aggregate history byte limit refusal.
	CauseAggregateHistoryByteLimit SourceCause = "aggregate_history_byte_limit"
	// CauseCurrentRolloutEvidenceUnavailable identifies the current rollout evidence unavailable refusal.
	CauseCurrentRolloutEvidenceUnavailable SourceCause = "current_rollout_evidence_unavailable"
	// CauseForkOwnershipBoundaryUnavailable identifies the fork ownership boundary unavailable refusal.
	CauseForkOwnershipBoundaryUnavailable SourceCause = "fork_ownership_boundary_unavailable"
	// CauseHistoricalCandidateLimit identifies the historical candidate limit refusal.
	CauseHistoricalCandidateLimit SourceCause = "historical_candidate_limit"
	// CauseHistoryDependencyUnavailable identifies the history dependency unavailable refusal.
	CauseHistoryDependencyUnavailable SourceCause = "history_dependency_unavailable"
	// CauseHistoryOrdinalOverflow identifies the history ordinal overflow refusal.
	CauseHistoryOrdinalOverflow SourceCause = "history_ordinal_overflow"
	// CauseHistoryRecordLimit identifies the history record limit refusal.
	CauseHistoryRecordLimit SourceCause = "history_record_limit"
	// CauseLegacyCompatibilityRequiresUnboundAdmission identifies the legacy compatibility requires unbound admission refusal.
	CauseLegacyCompatibilityRequiresUnboundAdmission SourceCause = "legacy_compatibility_requires_unbound_admission"
	// CauseNativeAdmissionEvidenceUnavailable identifies the native admission evidence unavailable refusal.
	CauseNativeAdmissionEvidenceUnavailable SourceCause = "native_admission_evidence_unavailable"
	// CauseNativeBoundaryScratchBudgetExhausted identifies the native boundary scratch budget exhausted refusal.
	CauseNativeBoundaryScratchBudgetExhausted SourceCause = "native_boundary_scratch_budget_exhausted"
	// CauseNativeCreationEvidenceUnavailable identifies the native creation evidence unavailable refusal.
	CauseNativeCreationEvidenceUnavailable SourceCause = "native_creation_evidence_unavailable"
	// CauseNativeRecordWindowBudgetExhausted identifies the native record window budget exhausted refusal.
	CauseNativeRecordWindowBudgetExhausted SourceCause = "native_record_window_budget_exhausted"
	// CauseOrdinaryNativeScratchBudgetExhausted identifies the ordinary native scratch budget exhausted refusal.
	CauseOrdinaryNativeScratchBudgetExhausted SourceCause = "ordinary_native_scratch_budget_exhausted"
	// CauseOriginalFactCandidateLimit identifies the original fact candidate limit refusal.
	CauseOriginalFactCandidateLimit SourceCause = "original_fact_candidate_limit"
	// CauseOwnTaskBeforeCreation identifies the own task before creation refusal.
	CauseOwnTaskBeforeCreation SourceCause = "own_task_before_creation"
	// CauseOwnTaskExecutionUnsupported identifies the own task execution unsupported refusal.
	CauseOwnTaskExecutionUnsupported SourceCause = "own_task_execution_unsupported"
	// CauseOwnTaskInvalid identifies the own task invalid refusal.
	CauseOwnTaskInvalid SourceCause = "own_task_invalid"
	// CauseOwnTaskMissing identifies the own task missing refusal.
	CauseOwnTaskMissing SourceCause = "own_task_missing"
	// CauseRelatedAdmissionRequiresConfinedSourceHome identifies the related admission requires confined source home refusal.
	CauseRelatedAdmissionRequiresConfinedSourceHome SourceCause = "related_admission_requires_confined_source_home"
	// CauseRelatedHistoryRequiresAdmittedConfinedSourceHome identifies the related history requires admitted confined source home refusal.
	CauseRelatedHistoryRequiresAdmittedConfinedSourceHome SourceCause = "related_history_requires_admitted_confined_source_home"
	// CauseRolloutCandidateLimit identifies the rollout candidate limit refusal.
	CauseRolloutCandidateLimit SourceCause = "rollout_candidate_limit"
	// CauseRolloutHistoryDepthLimit identifies the rollout history depth limit refusal.
	CauseRolloutHistoryDepthLimit SourceCause = "rollout_history_depth_limit"
	// CauseRolloutLocatorLimit identifies the rollout locator limit refusal.
	CauseRolloutLocatorLimit SourceCause = "rollout_locator_limit"
	// CauseRolloutMetadataIncomplete identifies the rollout metadata incomplete refusal.
	CauseRolloutMetadataIncomplete SourceCause = "rollout_metadata_incomplete"
	// CauseSharedRolloutFileLimit identifies the shared rollout file limit refusal.
	CauseSharedRolloutFileLimit SourceCause = "shared_rollout_file_limit"
	// CauseSourceAndLookupRequireTheSameChargeLedger identifies the source and lookup require the same charge ledger refusal.
	CauseSourceAndLookupRequireTheSameChargeLedger SourceCause = "source_and_lookup_require_the_same_charge_ledger"
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
