package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// admittedRollout registers one discovered Codex session and returns the
// pass's lookup after discovery has run with it, as a scheduled pass does.
func admittedRollout(t *testing.T) (*state.Store, config.Config, string, time.Time, *CodexRolloutLookup) {
	t.Helper()
	store, cfg, at, root := fixture(t)
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	now := at.Add(2 * time.Minute)
	if h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return now }}, registeredAdapters()); err != nil || h.Registered != 1 {
		t.Fatalf("admission: %#v %v", h, err)
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lookup.Close() })
	if _, err := Run(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Rollouts: lookup, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	return store, cfg, root, now, lookup
}

func collectWith(t *testing.T, store *state.Store, bucket storage.ObjectStore, cfg config.Config, root string, now time.Time, lookup agentapi.CodexRolloutLookup) collector.Result {
	t.Helper()
	result, err := collector.Run(context.Background(), store, bucket, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), CodexRollouts: lookup, ConfiguredCodexHomes: []string{root}, MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Regression: discovery takes as much of the pass's lookup allowance as it
// can use, and the collector shared what was left, so after a large
// discovery backlog every Codex session failed with "filter transcript:
// source limit" (the lookup's "native lookup pass deadline"). The collector
// now has its own allowance.
func TestCollectorHasItsOwnLookupAllowanceAfterDiscoverySpendsIt(t *testing.T) {
	t.Parallel()
	store, cfg, root, now, lookup := admittedRollout(t)
	lookup.remaining = 0 // discovery spent all of it
	result := collectWith(t, store, storagetest.NewMemoryStore(), cfg, root, now, lookup)
	if len(result.Published) != 1 || len(result.Errors) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("collector shared discovery's spent allowance: %+v", result)
	}
}

func TestReadOnlyPreparationReportsSpentLookupAllowance(t *testing.T) {
	t.Parallel()
	for _, remaining := range []time.Duration{0, time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			t.Parallel()
			store, _, _, root := fixture(t)
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lookup.CloseReadOnly() })
			// No sleeps: model preparation reaching its reserved checkpoint
			// boundary, including a fully spent allowance, before inspection.
			lookup.remaining = remaining
			_, err = lookup.ObserveReadOnly(t.Context())
			var source *agentapi.SourceError
			if !errors.As(err, &source) || source.Kind != agentapi.Limit || source.Cause != agentapi.CauseLookupAllowanceSpent {
				t.Fatalf("preparation lost scheduling cause: %v", err)
			}
		})
	}
}

// exhaustingLookup spends the pass's whole lookup allowance on the first
// Thread call it serves, as a slow native index read would.
type exhaustingLookup struct{ *CodexRolloutLookup }

func (l exhaustingLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	set, err := l.CodexRolloutLookup.Thread(ctx, id)
	l.remaining = 0
	return set, err
}

// A Codex session whose scan runs out of the collector's lookup allowance
// part way is deferred with its reason, not failed, and the next pass, with
// a fresh allowance, captures it.
func TestLookupAllowanceRunningOutMidScanDefersTheSession(t *testing.T) {
	t.Parallel()
	store, cfg, root, now, lookup := admittedRollout(t)
	bucket := storagetest.NewMemoryStore()
	result := collectWith(t, store, bucket, cfg, root, now, exhaustingLookup{lookup})
	if len(result.Errors) != 0 || len(result.Published) != 0 || len(result.Deferred) != 1 {
		t.Fatalf("result=%+v, want one session deferred", result)
	}
	for id, reason := range result.Deferred {
		if reason != collector.DeferredLookupAllowance {
			t.Fatalf("deferred %s for %q, want %q", id, reason, collector.DeferredLookupAllowance)
		}
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(status.SessionIssues) != 0 || len(status.DeferredSessions) != 1 {
		t.Fatalf("status=%+v, want a deferral and no issue", status)
	}

	next, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	result = collectWith(t, store, bucket, cfg, root, now.Add(time.Minute), next)
	if len(result.Published) != 1 || len(result.Errors) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("next pass result=%+v, want the session published", result)
	}

	// A published session whose change check runs out the same way (an
	// unconfirmed signature means a full scan) is deferred too, not failed.
	third, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	result = collectWith(t, store, bucket, cfg, root, now.Add(2*time.Minute), exhaustingLookup{third})
	if len(result.Errors) != 0 || len(result.Deferred) != 1 {
		t.Fatalf("change check result=%+v, want one session deferred", result)
	}
}

// nearlySpentLookup begins each collector allowance with almost none of it
// left, so the first lookup operation's deadline passes inside it, as when
// the allowance runs out part way through a slow native index read.
type nearlySpentLookup struct{ *CodexRolloutLookup }

func (l nearlySpentLookup) BeginCollection() {
	l.CodexRolloutLookup.BeginCollection()
	l.remaining = time.Nanosecond
}

// The allowance usually runs out inside a lookup call, whose interrupted step
// then reports a context deadline or an unavailable index rather than the
// allowance. That session is deferred too, not failed with "context deadline
// exceeded".
func TestLookupAllowanceRunningOutInsideAnOperationDefersTheSession(t *testing.T) {
	t.Parallel()
	store, cfg, root, now, lookup := admittedRollout(t)
	result := collectWith(t, store, storagetest.NewMemoryStore(), cfg, root, now, nearlySpentLookup{lookup})
	if len(result.Errors) != 0 || len(result.Published) != 0 || len(result.Deferred) != 1 {
		t.Fatalf("result=%+v, want one session deferred", result)
	}
	for id, reason := range result.Deferred {
		if reason != collector.DeferredLookupAllowance {
			t.Fatalf("deferred %s for %q, want %q", id, reason, collector.DeferredLookupAllowance)
		}
	}
}

// slowThreadLookup makes each Thread call a lookup operation of the shared
// lookup that takes at least delay, as a slow native read would.
type slowThreadLookup struct {
	*CodexRolloutLookup
	delay time.Duration
	calls int
}

func (l *slowThreadLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	l.calls++
	_, done, err := l.beginOperation(ctx)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	time.Sleep(l.delay)
	done(&err)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	return l.CodexRolloutLookup.Thread(ctx, id)
}

// Discovery charges its time to the lookup's allowance once: the lookup
// calls it makes are part of that time, and charge themselves, so they are
// not charged again (which spent the allowance up to twice as fast).
func TestDiscoveryChargesItsLookupCallsOnce(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lookup.Close() })
	slow := &slowThreadLookup{CodexRolloutLookup: lookup, delay: 500 * time.Millisecond}
	before := lookup.remaining
	started := time.Now()
	if h, err := runWithAdapters(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Rollouts: lookup, CodexRollouts: slow, Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters()); err != nil {
		t.Fatalf("%v (health %+v)", err, h)
	}
	elapsed := time.Since(started)
	if slow.calls == 0 {
		t.Fatal("discovery made no lookup call")
	}
	if lookup.remaining < before-elapsed {
		t.Fatalf("discovery charged %v of the allowance in %v", before-lookup.remaining, elapsed)
	}
	// Nor does it charge less than its lookup calls took: discovery cannot
	// spend more of the allowance than it is charged.
	if spent := time.Duration(slow.calls) * slow.delay; before-lookup.remaining < spent {
		t.Fatalf("discovery charged %v of the allowance, less than its lookup calls' %v", before-lookup.remaining, spent)
	}
}

// The collector's allowance includes its own header probes: discovery using
// all of the pass's does not leave the collector unable to read a thread's
// current rollout header.
func TestCollectorAllowanceHasItsOwnHeaderProbes(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	addHint(t, hintDatabase(t, root, false), native, path, at.Add(time.Minute))
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lookup.Close() })
	lookup.probes = HeaderProbes // discovery probed as many headers as a pass may
	lookup.BeginCollection()
	set, err := lookup.Thread(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	if set.Current == nil || set.Current.Path != path {
		t.Fatalf("current=%+v, want %s", set.Current, path)
	}
}

// A native index read cut off by a deadline (discovery's, or an allowance
// running out) says nothing about the index. It is not kept as unavailable,
// so a later allowance in the same pass, such as the collector's, still finds
// threads' current rollouts in it.
func TestIndexReadCutOffByADeadlineIsNotKeptAsUnavailable(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	addHint(t, hintDatabase(t, root, false), native, path, at.Add(time.Minute))
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lookup.Close() })
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := lookup.index(expired, root, false); err == nil {
		t.Fatal("index read under an expired deadline succeeded")
	}
	lookup.BeginCollection()
	set, err := lookup.Thread(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	if set.Current == nil || set.Current.Path != path {
		t.Fatalf("current=%+v, want %s from the native index", set.Current, path)
	}
}

// An operation's end turns its error into the allowance-spent refusal only
// when the allowance ran out inside it: its own deadline passed while its
// caller's context had not ended. A failure after the caller's context ended
// (the pass's deadline, or a cancellation), or while allowance was left,
// keeps its own error, so a genuine failure is not put off as a spent
// allowance.
func TestOperationEndConvertsOnlyAnAllowanceRunningOut(t *testing.T) {
	t.Parallel()
	genuine := agentapi.Wrap(agentapi.Unsafe, errors.New("genuine failure"))
	tests := []struct {
		name      string
		allowance time.Duration
		callerFor time.Duration // the caller's deadline; zero for none
		cancel    bool          // the caller cancels before the operation ends
		err       error
		converted bool
	}{
		{name: "allowance ran out", allowance: time.Millisecond, err: genuine, converted: true},
		{name: "allowance ran out, context error", allowance: time.Millisecond, err: context.DeadlineExceeded, converted: true},
		{name: "allowance ran out, no error", allowance: time.Millisecond},
		{name: "allowance left", allowance: time.Hour, err: genuine},
		{name: "caller cancelled", allowance: time.Millisecond, cancel: true, err: genuine},
		{name: "caller cancelled, context error", allowance: time.Millisecond, cancel: true, err: context.Canceled},
		{name: "caller deadline first", allowance: time.Hour, callerFor: time.Millisecond, err: context.DeadlineExceeded},
		{name: "caller deadline just after the allowance", allowance: time.Millisecond, callerFor: 2 * time.Millisecond, err: genuine},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.Context()
			if tt.callerFor > 0 {
				var cancelDeadline context.CancelFunc
				parent, cancelDeadline = context.WithTimeout(parent, tt.callerFor)
				defer cancelDeadline()
			}
			caller, cancel := context.WithCancel(parent)
			defer cancel()
			lookup := &CodexRolloutLookup{remaining: tt.allowance}
			operation, done, err := lookup.beginOperation(caller)
			if err != nil {
				t.Fatal(err)
			}
			if tt.allowance < time.Hour || tt.callerFor > 0 {
				<-operation.Done()
				// Let a caller deadline just after the allowance pass too.
				time.Sleep(10 * time.Millisecond)
			}
			if tt.cancel {
				cancel()
			}
			err = tt.err
			done(&err)
			converted := false
			var source *agentapi.SourceError
			if errors.As(err, &source) {
				converted = source.Kind == agentapi.Limit && source.Cause == agentapi.CauseLookupAllowanceSpent
			}
			if converted != tt.converted {
				t.Fatalf("err=%v converted=%v, want converted=%v", err, converted, tt.converted)
			}
			if !tt.converted && !errors.Is(err, tt.err) {
				t.Fatalf("err=%v, want %v kept", err, tt.err)
			}
		})
	}
}
