package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// B, the observed rollout off the current chain A then C, reads cleanly but
// its consumer refuses it for an aggregate limit (the planner's stage count or
// bytes, or an import's stage budget). B is left out and the chain's
// reconciliation still succeeds. A refusal that can clear on its own (the
// pass's shared read budget, a changed source) and a consumer's other
// failures still fail, so the session is retried rather than B lost.
func TestNativeRevisionsLeaveOutOffChainRolloutTheConsumerCannotHold(t *testing.T) {
	for _, refusal := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"limit", agentapi.Wrap(agentapi.Limit, errors.New("revision stage byte limit exceeded")), true},
		{"import stage budget", errors.Join(agentapi.ErrReadBudget, errRevisionDoesNotFit), true},
		{"shared read budget", agentapi.ReadBudgetLimit(errors.New("retained work exceeds shared data budget")), false},
		{"changed", agentapi.Wrap(agentapi.Changed, errors.New("preserved physical revision changed")), false},
		{"other", errors.New("store unavailable"), false},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			scan, _ := reconciliationFixture(t)
			read, ok, err := scan.read()
			if err != nil || !ok {
				t.Fatal("native read", err)
			}
			active, _, err := scan.build(read)
			if err != nil {
				t.Fatal(err)
			}
			r, ok := newSourceReader(scan.reg, scan.opts)
			if !ok {
				t.Fatal("no source reader")
			}
			provider, ok := r.(providerReader)
			if !ok {
				t.Fatal("no provider reader")
			}
			var consumed []string
			err = provider.revisions(scan.ctx, read.adapter, read.observed, scan.opts.maxTranscriptBytes(), active.History != nil, func(_ archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
				if binding.PhysicalRolloutID == revisionB {
					return refusal.err
				}
				consumed = append(consumed, binding.PhysicalRolloutID)
				return nil
			})
			if !refusal.ok {
				if err == nil {
					t.Fatal("a consumer failure on an off-chain rollout was ignored")
				}
				return
			}
			if err != nil {
				t.Fatal("an off-chain rollout the consumer cannot hold blocked the chain: ", err)
			}
			if !slices.Contains(consumed, revisionThread) || slices.Contains(consumed, revisionB) {
				t.Fatalf("consumed %v", consumed)
			}
		})
	}
}

// A planner refusal leaves the plan as it was, even when the refused revision
// would have replaced an earlier stage of the same rollout, so a best-effort
// revision can be left out without losing that stage.
func TestRevisionPlannerRefusalLeavesThePlanUnchanged(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal("native read", err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	first, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	reconciliationNative(t, scan.reg.ProjectRoot, revisionB, 2, true, "B-only", "B-more")
	read, ok, err = scan.read()
	if err != nil || !ok {
		t.Fatal("native read", err)
	}
	active, _, err = scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	second, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	var extended *archive.SourceBundle
	for i := range second.Sources {
		if revisionID(second.Sources[i].Bundle) == revisionB {
			extended = &second.Sources[i].Bundle
		}
	}
	if extended == nil {
		t.Fatal("no extended B stage")
	}
	plan := &revisionPlan{Current: first.Current, ObservedAt: first.ObservedAt, Preserved: slices.Clone(first.Preserved), Sources: slices.Clone(first.Sources), MeaningfulAt: first.MeaningfulAt}
	stageKeys := func() []string {
		keys := make([]string, 0, len(plan.Sources))
		for _, stage := range plan.Sources {
			keys = append(keys, stage.Reference.SHA256)
		}
		return keys
	}
	before, err := json.Marshal(plan.Preserved)
	if err != nil {
		t.Fatal(err)
	}
	stages := stageKeys()
	if !slices.ContainsFunc(plan.Sources, func(stage revisionStage) bool { return revisionID(stage.Bundle) == revisionB }) {
		t.Fatal("first plan has no B stage to replace")
	}
	// No stage bytes left: the extended B only fits by replacing the earlier
	// B stage, and does not fit even then.
	planner := revisionPlanner{budget: scan.readBudget(), plan: plan, active: active, adapter: read.adapter, compress: scan.compressSource}
	if err := planner.add(scan.ctx, *extended); !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatal("extended B fit no stage bytes: ", err)
	}
	after, err := json.Marshal(plan.Preserved)
	if err != nil {
		t.Fatal(err)
	}
	if got := stageKeys(); planner.bytesLeft != 0 || !bytes.Equal(before, after) || !slices.Equal(stages, got) || !plan.MeaningfulAt.Equal(first.MeaningfulAt) {
		t.Fatalf("refusal changed the plan: stages %v -> %v, bytes left %d", stages, got, planner.bytesLeft)
	}
}

// A best-effort revision that cannot be read is left out only for a failure
// that will not clear on its own; one that can (the shared read budget, a
// source changed while reading, cancellation) fails so a later pass retries
// it instead of recording a signature an abandoned branch never changes.
func TestLeaveOutBestEffortOnlyForLastingFailures(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		err      error
		consumer bool
		out      bool
	}{
		{"missing", os.ErrNotExist, false, true},
		{"unsafe", agentapi.Wrap(agentapi.Unsafe, errors.New("unsafe source format")), false, true},
		{"format", agentapi.Wrap(agentapi.FormatMismatch, errors.New("format")), false, true},
		{"base unavailable", agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, errors.New("base")), agentapi.CauseHistoryBaseUnavailable), false, true},
		{"budget left by the chain", agentapi.Wrap(agentapi.Limit, errors.New("historical raw budget exceeded")), false, true},
		{"shared read budget", agentapi.ReadBudgetLimit(errors.New("pass")), false, false},
		{"changed", agentapi.Wrap(agentapi.Changed, errors.New("rollout dependency changed")), false, false},
		{"canceled", context.Canceled, false, false},
		{"deadline", agentapi.Wrap(agentapi.Unavailable, context.DeadlineExceeded), false, false},
		{"consumer limit", agentapi.Wrap(agentapi.Limit, errors.New("revision stage limit exceeded")), true, true},
		{"consumer import budget", errors.Join(agentapi.ErrReadBudget, errRevisionDoesNotFit), true, true},
		{"consumer shared budget", agentapi.ReadBudgetLimit(errors.New("retained")), true, false},
		{"consumer other", errors.New("store unavailable"), true, false},
	} {
		if got := leaveOutBestEffort(c.err, c.consumer); got != c.out {
			t.Errorf("%s: left out %v, want %v", c.name, got, c.out)
		}
	}
}
