package collector

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// revisionsRead counts what one revisions call did with the thread's
// best-effort (off-chain) candidates: kept were read and consumed, so they
// still hold what the consumer retained; leftOut were not.
type revisionsRead struct {
	offChainKept    int
	offChainLeftOut int
}

// revisions owns a serial read lease. The consumer must release its native
// values before returning; only bounded filtered/staged evidence may survive.
// A consumer that returns an error must leave its own state as it was before
// the call: a best-effort revision it refuses for a limit is left out.
// offChain false leaves out every best-effort candidate without reading it.
func (r providerReader) revisions(ctx context.Context, adapter agentapi.TranscriptFilter, expected sourceState, maxBytes int64, related, offChain bool, consume func(archive.FilteredTranscript, *archive.CodexSourceBinding) error) (read revisionsRead, err error) {
	provider, _, err := r.binding()
	if err != nil {
		return read, err
	}
	pass, closePass, err := r.pass(ctx, provider, adapter.Name())
	if err != nil {
		return read, err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	historical, ok := pass.(agentapi.SourceRevisions)
	if !ok {
		return read, agentapi.Wrap(agentapi.Unavailable, errors.New("historical revision reader unavailable"))
	}
	limits := agentapi.ReadLimits{RawBytes: maxRawBytes(r.harness, maxBytes), RecordBytes: recordLimit, SubagentMetadata: r.subagentMetadata}
	snap, err := pass.Read(ctx, r.ref, limits)
	if err != nil {
		return read, err
	}
	candidates, ok := snap.(agentapi.SourceRevisionCandidates)
	if !ok {
		return read, errors.Join(agentapi.Wrap(agentapi.Unavailable, errors.New("historical candidate coverage unavailable")), snap.Close())
	}
	if snap.Observation().Signature != expected.observation.Signature {
		return read, errors.Join(agentapi.Wrap(agentapi.Changed, errors.New("active revision changed during reconciliation")), snap.Close())
	}
	_, admissionErr := r.snapshotBinding(ctx, snap)
	if admissionErr != nil {
		return read, errors.Join(admissionErr, snap.Close())
	}
	set, candidateErr := candidates.RevisionCandidates(ctx)
	if err := errors.Join(candidateErr, snap.Close()); err != nil {
		return read, err
	}
	if r.discoveryRoot() == "" {
		if related || len(set.Chain) > 1 {
			return read, agentapi.Wrap(agentapi.Unavailable, errors.New("historical reconciliation requires an admitted source home"))
		}
		// Off-chain rollouts are only ever best effort; without an admitted
		// source home they are not read at all.
		set.BestEffort = nil
	}
	if !offChain {
		read.offChainLeftOut, set.BestEffort = len(set.BestEffort), nil
	}
	if len(set.Chain)+len(set.BestEffort) > archive.MaxHistorySpans {
		return read, agentapi.Wrap(agentapi.Limit, errors.New("historical candidate count exceeded"))
	}
	// Raw, retained records, and filtered bytes have independent aggregate bounds.
	budget := revisionBudget{raw: maxRawBytes(r.harness, maxBytes), filtered: 128 << 20, records: archive.MaxHistoryRecords}
	for _, ref := range set.Chain {
		if err := r.readRevision(ctx, historical, adapter, ref, limits, maxBytes, &budget, consume); err != nil {
			return read, err
		}
	}
	// A best-effort candidate that cannot be read, or no longer fits the
	// budget the chain left or the consumer's own limits, is left out: an
	// off-chain rollout never blocks the chain's own capture. A failure that
	// clears on its own (the pass's shared read budget, a source changed while
	// reading, cancellation) still fails, so the session defers and a later
	// pass reads the candidate: leaving it out would record a signature that
	// an abandoned branch never changes again. The consumer's other failures
	// still fail. A refusal at the session's own read ceiling is lasting, not
	// the shared budget's pressure: the session meets it again every pass, and
	// failing would record the whole session as too large although its chain
	// fits, so the candidate is left out. A left-out candidate releases what
	// it retained and its ceiling refusal is forgotten, so neither counts
	// against what the scan does next.
	readBudget := r.retainedBudget()
	for _, ref := range set.BestEffort {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		before, mark, exceeded := budget, r.retainedMark(), readBudget.ScopeExceeded()
		var consumeErr error
		err := r.readRevision(ctx, historical, adapter, ref, limits, maxBytes, &budget, func(filtered archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
			consumeErr = consume(filtered, binding)
			return consumeErr
		})
		if err == nil {
			read.offChainKept++
			continue
		}
		if ctx.Err() != nil || !leaveOutBestEffort(err, consumeErr != nil, readBudget.ScopeExceeded()) {
			return read, errors.Join(err, ctx.Err())
		}
		budget = before
		r.releaseRetainedAfter(mark)
		if !exceeded {
			readBudget.ForgetScopeExceeded()
		}
		read.offChainLeftOut++
	}
	final, err := pass.Signature(ctx, r.ref)
	if err != nil {
		return read, err
	}
	if final.Signature != expected.observation.Signature {
		return read, agentapi.Wrap(agentapi.Changed, errors.New("active revision changed after reconciliation"))
	}
	return read, nil
}

// errRevisionDoesNotFit marks a revisions consumer's refusal of one revision
// for a fixed limit of its own that a later pass would meet again (an
// import's stage budget), even when the refusal also reads as a read-budget
// failure.
var errRevisionDoesNotFit = errors.New("historical revision does not fit")

// leaveOutBestEffort reports whether a best-effort revision's failure only
// leaves it out: a failure that will not clear on its own (a missing,
// unreadable or unsafe rollout, a limit, or a read budget refusal at the
// session's own ceiling, overCeiling). One that can clear (the pass's shared
// read budget held by others, a source changed while reading, cancellation)
// fails the reconciliation so it is retried. A consumer's failures are left
// out only for its limits, which leave its state unchanged.
func leaveOutBestEffort(err error, consumer, overCeiling bool) bool {
	if errors.Is(err, errRevisionDoesNotFit) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || agentapi.HasFailure(err, agentapi.Changed) {
		return false
	}
	if errors.Is(err, agentapi.ErrReadBudget) {
		return overCeiling
	}
	return !consumer || agentapi.HasFailure(err, agentapi.Limit)
}

// retainedBudget is the ledger the session's scan charges, whose scope
// tells a refusal at the session's own ceiling from shared pressure.
func (r providerReader) retainedBudget() *agentapi.NativeReadBudget {
	if r.resourceOwner == nil {
		return nil
	}
	return r.resourceOwner.readBudget()
}

// retainedMark and releaseRetainedAfter bound what one revision retained in
// the session's scan, so a left-out revision releases it.
func (r providerReader) retainedMark() int {
	if r.resourceOwner == nil {
		return 0
	}
	return len(r.resourceOwner.retainedReleases)
}

func (r providerReader) releaseRetainedAfter(mark int) {
	if r.resourceOwner != nil {
		r.resourceOwner.releaseRetainedAfter(mark)
	}
}

// revisionBudget is what one reconciliation's historical reads may still
// use: raw source bytes, filtered bytes and retained records.
type revisionBudget struct {
	raw      int64
	filtered int64
	records  int
}

// readRevision reads, filters and consumes one historical candidate within
// budget, charging it only for what that candidate used.
func (r providerReader) readRevision(ctx context.Context, historical agentapi.SourceRevisions, adapter agentapi.TranscriptFilter, ref agentapi.SourceRef, limits agentapi.ReadLimits, maxBytes int64, budget *revisionBudget, consume func(archive.FilteredTranscript, *archive.CodexSourceBinding) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref.Kind != archive.SourceKindFile || len(ref.Path) > 4096 || len(ref.Key) > 4096 {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("invalid historical candidate locator"))
	}
	limits.RawBytes = min(maxRawBytes(r.harness, maxBytes), budget.raw)
	limits.Records = budget.records
	limits.FilteredBytes = min(maxBytes, budget.filtered)
	if limits.RawBytes <= 0 || budget.filtered <= 0 || budget.records <= 0 {
		return agentapi.Wrap(agentapi.Limit, errors.New("historical aggregate budget exceeded"))
	}
	snapshot, err := historical.ReadRevision(ctx, ref, r.admission, limits)
	if err != nil {
		return err
	}
	raw := snapshot.Observation().Size
	if raw < 0 || raw > budget.raw {
		return errors.Join(agentapi.Wrap(agentapi.Limit, errors.New("historical raw budget exceeded")), snapshot.Close())
	}
	filtered, binding, err := r.filterRevision(ctx, snapshot, adapter, ref, limits, min(maxBytes, budget.filtered), &budget.records)
	if err != nil {
		return err
	}
	if filtered.Boundary.RetainedBytes < 0 || int64(filtered.Boundary.RetainedBytes) > budget.filtered {
		return agentapi.Wrap(agentapi.Limit, errors.New("historical filtered budget exceeded"))
	}
	budget.raw -= raw
	budget.filtered -= int64(filtered.Boundary.RetainedBytes)
	return consume(filtered, binding)
}

func (r providerReader) filterRevision(ctx context.Context, snapshot agentapi.SourceSnapshot, adapter agentapi.TranscriptFilter, ref agentapi.SourceRef, limits agentapi.ReadLimits, maxBytes int64, recordsLeft *int) (filtered archive.FilteredTranscript, binding *archive.CodexSourceBinding, err error) {
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	carrier, admissionErr := r.snapshotBinding(ctx, snapshot)
	binding, err = carrier.CodexBinding(), admissionErr
	if err != nil || binding == nil {
		return filtered, binding, errors.Join(err, missingRevisionBinding(binding))
	}
	input := snapshot.Input()
	if input.Records != nil {
		input.Records = &revisionRecords{input: input.Records, left: recordsLeft}
	}
	filtered, err = r.filterOwned(ctx, adapter, input, agentapi.FilterContext{Filename: filepath.Base(ref.Path), StartedAt: r.startedAt, Limits: limits})
	if err != nil {
		return filtered, binding, err
	}
	if input.File != nil {
		if len(filtered.Records) > *recordsLeft {
			return filtered, binding, agentapi.Wrap(agentapi.Limit, errors.New("historical record budget exceeded"))
		}
		*recordsLeft -= len(filtered.Records)
	}
	return filtered, binding, checkFilteredSize(filtered, maxBytes)
}

func missingRevisionBinding(binding *archive.CodexSourceBinding) error {
	if binding == nil {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("historical revision admission facts unavailable"))
	}
	return nil
}

type revisionRecords struct {
	input agentapi.RecordInput
	left  *int
}

func (r *revisionRecords) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	record, ok, err := r.input.Next(ctx)
	if err != nil || !ok {
		return record, ok, err
	}
	if *r.left <= 0 {
		return agentapi.NativeRecord{}, false, agentapi.Wrap(agentapi.Limit, errors.New("historical record budget exceeded"))
	}
	*r.left--
	return record, true, nil
}
