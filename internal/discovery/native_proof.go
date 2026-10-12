package discovery

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

var errOwnTaskRejected = errors.New("first own task decisively rejects native admission")

// One serial agent-owned pass per authorized source home shares handles and
// immutable ancestor summaries. No parent-scoped enumeration occurs here.
type nativeProofPasses struct {
	sources  agentapi.SourcesLookup
	lookup   agentapi.CodexRolloutLookup
	passes   map[string]agentapi.SourcePass
	bytes    int64
	reads    int64
	opens    int64
	attempts int64
}

func (p *nativeProofPasses) Close() error {
	var err error
	for _, pass := range p.passes {
		err = errors.Join(err, pass.Close())
	}
	p.passes = nil
	return err
}

func (p *nativeProofPasses) Prove(ctx context.Context, c Candidate) (out Candidate, resultErr error) {
	p.attempts++
	pass := p.passes[c.Source.Root]
	if pass == nil {
		// Keep only one home active: each provider shares the pass ledger, so closing
		// a previous home also keeps the aggregate discovery charge bounded.
		if err := p.Close(); err != nil {
			return c, err
		}
		p.passes = map[string]agentapi.SourcePass{}
		if p.sources == nil {
			return c, agentapi.Wrap(agentapi.Unavailable, errors.New("native source lookup unavailable"))
		}
		provider, _, ok := p.sources.LookupSources(c.Agent)
		if !ok {
			return c, agentapi.Wrap(agentapi.Unavailable, errors.New("native provider unavailable"))
		}

		var budget *agentapi.NativeReadBudget
		if shared, ok := p.lookup.(agentapi.CodexRolloutResourceBudget); ok {
			budget = shared.NativeReadBudget()
		}
		var err error
		pass, err = provider.OpenPass(ctx, agentapi.SourceEnvironment{ReadBudget: budget, CodexRollouts: p.lookup, Files: proofOpener{RootOpener: sourcefacts.RootOpener{Root: c.Source.Root}, owner: p}, Policy: transcriptio.OpenPolicy{Root: c.Source.Root, RejectSymlinks: true}})
		if err != nil {
			return c, err
		}
		p.passes[c.Source.Root] = pass
	}
	snapshot, err := pass.Read(ctx, agentapi.SourceRef{Kind: c.Source.Kind, Path: c.Source.Locator, Key: c.NativeSessionID}, agentapi.ReadLimits{RawBytes: archive.MaxRawTranscriptBytesFor(c.Agent), RecordBytes: archive.MaxRecordBytes})
	if err != nil {
		return c, err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	evidenceReader, ok := snapshot.(agentapi.SourceAdmissionEvidence)
	if !ok {
		return c, agentapi.Wrap(agentapi.Unavailable, errors.New("native admission evidence unavailable"))
	}
	evidence, err := evidenceReader.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: c.NativeSessionID})
	if err != nil {
		return c, err
	}
	facts, task := evidence.Binding, evidence.Task

	if !task.ValidNativeCreation(facts.NativeCreatedAt) {
		if task.Seen {
			return c, agentapi.Wrap(agentapi.Unavailable, errOwnTaskRejected)
		}
		return c, agentapi.Wrap(agentapi.Unavailable, errors.New("native own task unavailable"))
	}
	if facts.NativeThreadID != c.NativeSessionID {
		return c, agentapi.Wrap(agentapi.Unsafe, errors.New("native thread identity changed"))
	}
	if facts.FirstNativeTaskAt.IsZero() {
		facts.FirstNativeTaskAt = task.StartedAt
		facts.FirstNativeTaskID = task.TurnID
	}
	c.Binding = &facts
	c.NativeChild, c.ParentNativeID, c.RootNativeID, c.OwnStart = facts.Child, facts.ParentID, facts.RootID, facts.OwnStart
	c.StartedAt, c.FirstTaskAt = facts.NativeCreatedAt, task.StartedAt
	c.WorkingDirectory, c.ProducerSource = facts.Cwd, facts.ProducerSource
	c.SnapshotProven = true
	return c, nil
}

func nativeProofOutcome(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "own_task_unavailable"
	}
	if errors.Is(err, errOwnTaskRejected) && !agentapi.HasFailure(err, agentapi.Changed) && !agentapi.HasFailure(err, agentapi.Cleanup) && !agentapi.HasFailure(err, agentapi.Limit) && !agentapi.HasFailure(err, agentapi.Unsafe) && !agentapi.HasFailure(err, agentapi.FormatMismatch) {
		return "own_task_rejected"
	}
	if agentapi.HasFailure(err, agentapi.Changed) {
		return "source_changed"
	}
	if agentapi.HasFailure(err, agentapi.Limit) {
		return "source_budget_pending"
	}
	if agentapi.HasFailure(err, agentapi.Unsafe) || agentapi.HasFailure(err, agentapi.FormatMismatch) {
		return "source_validation_pending"
	}
	return "own_task_unavailable"
}

// Counters charge real source I/O separately from provider in-flight data.
type proofOpener struct {
	sourcefacts.RootOpener
	owner *nativeProofPasses
}

type proofFile struct {
	transcriptio.File
	owner *nativeProofPasses
}

func (o proofOpener) OpenRegular(path string) (transcriptio.File, error) {
	file, err := o.RootOpener.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	o.owner.opens++
	return proofFile{file, o.owner}, nil
}

func (f proofFile) ReadAt(buffer []byte, offset int64) (int, error) {
	n, err := f.File.ReadAt(buffer, offset)
	f.owner.reads++
	f.owner.bytes += int64(n)
	return n, err
}
