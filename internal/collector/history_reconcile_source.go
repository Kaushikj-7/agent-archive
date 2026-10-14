package collector

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// revisions owns a serial read lease. The consumer must release its native
// values before returning; only bounded filtered/staged evidence may survive.
// A consumer that returns an error must leave its own state as it was before
// the call: a best-effort revision it refuses for a limit is left out.
func (r providerReader) revisions(ctx context.Context, adapter agentapi.TranscriptFilter, expected sourceState, maxBytes int64, related bool, consume func(archive.FilteredTranscript, *archive.CodexSourceBinding) error) (err error) {
	provider, _, err := r.binding()
	if err != nil {
		return err
	}
	pass, closePass, err := r.pass(ctx, provider, adapter.Name())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	historical, ok := pass.(agentapi.SourceRevisions)
	if !ok {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("historical revision reader unavailable"))
	}
	limits := agentapi.ReadLimits{RawBytes: maxRawBytes(r.harness, maxBytes), RecordBytes: recordLimit, SubagentMetadata: r.subagentMetadata}
	snap, err := pass.Read(ctx, r.ref, limits)
	if err != nil {
		return err
	}
	candidates, ok := snap.(agentapi.SourceRevisionCandidates)
	if !ok {
		return errors.Join(agentapi.Wrap(agentapi.Unavailable, errors.New("historical candidate coverage unavailable")), snap.Close())
	}
	if snap.Observation().Signature != expected.observation.Signature {
		return errors.Join(agentapi.Wrap(agentapi.Changed, errors.New("active revision changed during reconciliation")), snap.Close())
	}
	_, admissionErr := r.snapshotBinding(ctx, snap)
	if admissionErr != nil {
		return errors.Join(admissionErr, snap.Close())
	}
	set, candidateErr := candidates.RevisionCandidates(ctx)
	if err := errors.Join(candidateErr, snap.Close()); err != nil {
		return err
	}
	if r.discoveryRoot() == "" {
		if related || len(set.Chain) > 1 {
			return agentapi.Wrap(agentapi.Unavailable, errors.New("historical reconciliation requires an admitted source home"))
		}
		// Off-chain rollouts are only ever best effort; without an admitted
		// source home they are not read at all.
		set.BestEffort = nil
	}
	if len(set.Chain)+len(set.BestEffort) > archive.MaxHistorySpans {
		return agentapi.Wrap(agentapi.Limit, errors.New("historical candidate count exceeded"))
	}
	// Raw, retained records, and filtered bytes have independent aggregate bounds.
	budget := revisionBudget{raw: maxRawBytes(r.harness, maxBytes), filtered: 128 << 20, records: archive.MaxHistoryRecords}
	for _, ref := range set.Chain {
		if err := r.readRevision(ctx, historical, adapter, ref, limits, maxBytes, &budget, consume); err != nil {
			return err
		}
	}
	// A best-effort candidate that cannot be read, or no longer fits the
	// budget the chain left or the consumer's own limits, is left out: an
	// off-chain rollout never blocks the chain's own capture. Its consumer's
	// other failures still fail.
	for _, ref := range set.BestEffort {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := budget
		var consumeErr error
		err := r.readRevision(ctx, historical, adapter, ref, limits, maxBytes, &budget, func(filtered archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
			consumeErr = consume(filtered, binding)
			return consumeErr
		})
		if consumeErr != nil && !consumerLimit(consumeErr) || ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if err != nil {
			budget = before
		}
	}
	final, err := pass.Signature(ctx, r.ref)
	if err != nil {
		return err
	}
	if final.Signature != expected.observation.Signature {
		return agentapi.Wrap(agentapi.Changed, errors.New("active revision changed after reconciliation"))
	}
	return nil
}

// consumerLimit reports a revisions consumer refusing one revision for its
// own aggregate limits (stage count or bytes, retained or import budget).
func consumerLimit(err error) bool {
	return agentapi.HasFailure(err, agentapi.Limit) || errors.Is(err, agentapi.ErrReadBudget)
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
	binding, err = r.snapshotBinding(ctx, snapshot)
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
