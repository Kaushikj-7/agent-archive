package discovery

import (
	"context"
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
