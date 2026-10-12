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
	refs, candidateErr := candidates.RevisionCandidates(ctx)
	if err := errors.Join(candidateErr, snap.Close()); err != nil {
		return err
	}
	if r.discoveryRoot() == "" && (related || len(refs) > 1) {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("historical reconciliation requires an admitted source home"))
	}
	if len(refs) > archive.MaxHistorySpans {
		return agentapi.Wrap(agentapi.Limit, errors.New("historical candidate count exceeded"))
	}
	// Raw, retained records, and filtered bytes have independent aggregate bounds.
	rawLeft, filteredLeft := maxRawBytes(r.harness, maxBytes), int64(128<<20)
	recordsLeft := archive.MaxHistoryRecords
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ref.Kind != archive.SourceKindFile || len(ref.Path) > 4096 || len(ref.Key) > 4096 {
			return agentapi.Wrap(agentapi.Unsafe, errors.New("invalid historical candidate locator"))
		}
		limits.RawBytes = min(maxRawBytes(r.harness, maxBytes), rawLeft)
		limits.Records = recordsLeft
		limits.FilteredBytes = min(maxBytes, filteredLeft)
		if limits.RawBytes <= 0 || filteredLeft <= 0 || recordsLeft <= 0 {
			return agentapi.Wrap(agentapi.Limit, errors.New("historical aggregate budget exceeded"))
		}
		snapshot, err := historical.ReadRevision(ctx, ref, r.admission, limits)
		if err != nil {
			return err
		}
		raw := snapshot.Observation().Size
		if raw < 0 || raw > rawLeft {
			return errors.Join(agentapi.Wrap(agentapi.Limit, errors.New("historical raw budget exceeded")), snapshot.Close())
		}
		filtered, binding, err := r.filterRevision(ctx, snapshot, adapter, ref, limits, min(maxBytes, filteredLeft), &recordsLeft)
		if err != nil {
			return err
		}
		rawLeft -= raw
		if filtered.Boundary.RetainedBytes < 0 || int64(filtered.Boundary.RetainedBytes) > filteredLeft {
			return agentapi.Wrap(agentapi.Limit, errors.New("historical filtered budget exceeded"))
		}
		filteredLeft -= int64(filtered.Boundary.RetainedBytes)
		if err := consume(filtered, binding); err != nil {
			return err
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
