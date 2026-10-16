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
			_, err = provider.revisions(scan.ctx, read.adapter, read.observed, scan.opts.maxTranscriptBytes(), active.History != nil, true, func(_ archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
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
		name        string
		err         error
		consumer    bool
		overCeiling bool
		out         bool
	}{
		{"missing", os.ErrNotExist, false, false, true},
		{"unsafe", agentapi.Wrap(agentapi.Unsafe, errors.New("unsafe source format")), false, false, true},
		{"format", agentapi.Wrap(agentapi.FormatMismatch, errors.New("format")), false, false, true},
		{"base unavailable", agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, errors.New("base")), agentapi.CauseHistoryBaseUnavailable), false, false, false},
		{"plain unavailable", agentapi.Wrap(agentapi.Unavailable, errors.New("read")), false, false, false},
		{"unavailable at the session ceiling", errors.Join(agentapi.ReadBudgetLimit(errors.New("pass")), agentapi.Wrap(agentapi.Unavailable, errors.New("read"))), false, true, false},
		{"consumer unavailable", agentapi.Wrap(agentapi.Unavailable, errors.New("consumer")), true, false, false},
		{"budget left by the chain", agentapi.Wrap(agentapi.Limit, errors.New("historical raw budget exceeded")), false, false, true},
		{"shared read budget", agentapi.ReadBudgetLimit(errors.New("pass")), false, false, false},
		{"changed", agentapi.Wrap(agentapi.Changed, errors.New("rollout dependency changed")), false, false, false},
		{"canceled", context.Canceled, false, false, false},
		{"deadline", agentapi.Wrap(agentapi.Unavailable, context.DeadlineExceeded), false, false, false},
		{"consumer limit", agentapi.Wrap(agentapi.Limit, errors.New("revision stage limit exceeded")), true, false, true},
		{"consumer import budget", errors.Join(agentapi.ErrReadBudget, errRevisionDoesNotFit), true, false, true},
		{"consumer shared budget", agentapi.ReadBudgetLimit(errors.New("retained")), true, false, false},
		{"consumer other", errors.New("store unavailable"), true, false, false},
		{"read budget at the session ceiling", agentapi.ReadBudgetLimit(errors.New("pass")), false, true, true},
		{"consumer budget at the session ceiling", agentapi.ReadBudgetLimit(errors.New("retained")), true, true, true},
		{"changed at the session ceiling", errors.Join(agentapi.ReadBudgetLimit(errors.New("pass")), agentapi.Wrap(agentapi.Changed, errors.New("changed"))), false, true, false},
		{"canceled at the session ceiling", errors.Join(agentapi.ReadBudgetLimit(errors.New("pass")), context.Canceled), false, true, false},
	} {
		if got := leaveOutBestEffort(c.err, c.consumer, c.overCeiling); got != c.out {
			t.Errorf("%s: left out %v, want %v", c.name, got, c.out)
		}
	}
}

// An off-chain rollout that would take the session past its own read
// ceiling, whether reading it or staging it, is left out: the ceiling is the
// session's own, so a later pass meets it again, and failing would record the
// whole session as too large to read although its own chain fits. Leaving it
// out also releases what it held and forgets the ceiling refusal, so a later
// refusal in the scan is classified on its own. A ceiling refusal the scan
// met before the candidate is not the candidate's, and is kept.
func TestBestEffortRevisionPastTheSessionCeilingIsLeftOut(t *testing.T) {
	for _, c := range []struct {
		where    string
		exceeded bool
	}{{"read", false}, {"consumer", false}, {"read", true}, {"consumer", true}} {
		where := c.where
		name := where
		if c.exceeded {
			name += " after an earlier ceiling refusal"
		}
		t.Run(name, func(t *testing.T) {
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
			budget := scan.readBudget()
			end := budget.BeginScope(64 << 20)
			defer end()
			held, holding := budget.Reservation(), int64(0)
			defer func() { held.Release(holding) }()
			if c.exceeded {
				// Something earlier in the scan was refused at the ceiling.
				budget.Refuse(1 << 40)
			}
			var consumed []string
			var retained int
			got, err := provider.revisions(scan.ctx, read.adapter, read.observed, scan.opts.maxTranscriptBytes(), active.History != nil, true, func(_ archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
				id := binding.PhysicalRolloutID
				if id == revisionB {
					if budget.Reservation().Reserve(128 << 20) {
						t.Error("reserved past the session ceiling")
					}
					return errRetainedBudget
				}
				consumed = append(consumed, id)
				if id == revisionThread && where == "read" {
					// The chain's own retained work fills the session's share.
					holding = held.Available()
					if !held.Reserve(holding) {
						t.Error("could not fill the session ceiling")
					}
				}
				retained = len(scan.retainedReleases)
				return nil
			})
			if err != nil {
				t.Fatal("an off-chain rollout past the session ceiling blocked the chain: ", err)
			}
			// C, the chain's tip, is the current revision the scan's own read
			// holds, so it is not read again.
			if !slices.Contains(consumed, revisionThread) || slices.Contains(consumed, revisionC) || slices.Contains(consumed, revisionB) {
				t.Fatalf("consumed %v", consumed)
			}
			if budget.ScopeExceeded() != c.exceeded {
				t.Fatalf("scope exceeded %v after leaving out the revision, want %v (an earlier refusal kept, the revision's own forgotten)", budget.ScopeExceeded(), c.exceeded)
			}
			if got.offChainKept != 0 || got.offChainLeftOut != 1 {
				t.Fatalf("off-chain kept %d, left out %d", got.offChainKept, got.offChainLeftOut)
			}
			if len(scan.retainedReleases) != retained {
				t.Fatalf("the left-out revision kept %d retained leases", len(scan.retainedReleases)-retained)
			}
		})
	}
}

// A native revision the planner refuses releases what staging it retained.
func TestRefusedNativeRevisionReleasesWhatItRetained(t *testing.T) {
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
	// No stage bytes: every historical revision is refused once compressed.
	planner := revisionPlanner{budget: scan.readBudget(), plan: &revisionPlan{Current: revisionC}, active: active, adapter: read.adapter, compress: scan.compressSource}
	refused := 0
	_, err = provider.revisions(scan.ctx, read.adapter, read.observed, scan.opts.maxTranscriptBytes(), active.History != nil, true, func(filtered archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
		mark := len(scan.retainedReleases)
		used, _ := scan.readBudget().Charged()
		err := scan.addNativeRevision(&planner, filtered, binding, archive.Metadata{}, archive.SourceBundle{}, archive.SourceBundle{})
		if err == nil {
			return nil
		}
		refused++
		after, _ := scan.readBudget().Charged()
		if len(scan.retainedReleases) != mark || after != used {
			t.Errorf("refused revision kept %d leases and %d bytes", len(scan.retainedReleases)-mark, after-used)
		}
		return err
	})
	if refused == 0 || !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("no revision refused for its stage bytes: %v", err)
	}
}
