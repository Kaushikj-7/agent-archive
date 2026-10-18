package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

type cookieAdapter struct {
	codexAdapter
	next     map[int64]int64
	complete int64
	calls    int
}

func TestForegroundChildDirectoryCapacityFailureRemainsFailure(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "sessions", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	store, _, _, _ := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Fatal(err)
		}
	}()
	parent := directory{Root: root, Path: "sessions"}
	if !lookup.reserveCatalog(directoryByteBound(parent)) {
		t.Fatal("reserve parent directory")
	}
	lookup.catalog = &catalog{Version: catalogVersion, Roots: []string{root}, Queue: []directory{parent}, Cache: map[string]cached{}}
	// Leave enough for the fixed cursor but not the child directory. The
	// underlying refusal must survive the same-cookie no-progress guard.
	external := lookup.NativeReadBudget().Available() - 24
	if !lookup.NativeReadBudget().ReserveUnscoped(external) {
		t.Fatal("reserve external owner")
	}
	defer lookup.NativeReadBudget().ReleaseUnscoped(external)
	health, err := lookup.ObserveReadOnly(t.Context())
	if !errors.Is(err, agentapi.ErrReadBudget) || agentapi.Failure(err) != agentapi.Limit || !health.Pending {
		t.Fatal("child directory capacity did not remain a pending resource failure", health, err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, peak := lookup.NativeReadBudget().Charged(); used != external || peak > agentapi.PassReadBudget {
		t.Fatal("directory capacity refusal leaked or reset another owner", used, external, peak)
	}
}

func TestChildDirectoryCapacityKeepsBackgroundPolicy(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	store, _, _, _ := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Fatal(err)
		}
	}()
	external := lookup.NativeReadBudget().Available() - 24
	if !lookup.NativeReadBudget().ReserveUnscoped(external) {
		t.Fatal("reserve external owner")
	}
	defer lookup.NativeReadBudget().ReleaseUnscoped(external)
	c, health := &catalog{}, &Health{}
	s := scan{ctx: t.Context(), catalog: c, health: health, rollouts: lookup}
	if s.enqueueDirectory(directory{Root: root, Path: "sessions"}, filepath.Join("sessions", "child")) || health.failure != nil || len(c.Queue) != 0 {
		t.Fatal("background capacity scheduling changed", health.failure, c.Queue)
	}
}

func (*cookieAdapter) InitialDirectories() []string { return []string{"sessions"} }

func (a *cookieAdapter) Enumerate(_ context.Context, _, _ string, cookie int64) (SourceBatch, error) {
	a.calls++
	if cookie == a.complete {
		return SourceBatch{Complete: true}, nil
	}
	next, ok := a.next[cookie]
	if !ok {
		return SourceBatch{}, errors.New("invalid synthetic cookie")
	}
	return SourceBatch{Continuation: next}, nil
}

// Native directory cookies are opaque seek locations, not ordered integers.
// The actual inventory orchestration must accept lower cookies and bound cycles
// even when a cycle crosses several renewable slices.
func TestForegroundDirectoryCookiesAdvanceWithoutNumericOrdering(t *testing.T) {
	for _, tc := range []struct {
		name     string
		next     map[int64]int64
		complete int64
		failure  bool
		minCalls int
	}{
		{name: "lower distinct cookies", next: map[int64]int64{0: 9, 9: 4, 4: 2}, complete: 2},
		{name: "same cookie", next: map[int64]int64{0: 0}, complete: -1, failure: true},
		{name: "cycle across slices", next: map[int64]int64{0: 9, 9: 8, 8: 7, 7: 6, 6: 5, 5: 4, 4: 3, 3: 2, 2: 1, 1: 9}, complete: -1, failure: true, minCalls: 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := chainRoot(t)
				store, _, _, _ := fixture(t)
				calls := 0
				lookup, err := NewCodexRolloutLookupWithOptions(t.Context(), store, []string{root}, CodexLookupOptions{Policy: ForegroundLookup, Now: func() time.Time {
					calls++
					if calls%2 == 0 {
						time.Sleep(500 * time.Millisecond)
					}
					return time.Now()
				}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := lookup.CloseReadOnly(); err != nil {
						t.Fatal(err)
					}
				}()
				adapter := &cookieAdapter{next: tc.next, complete: tc.complete}
				var health Health
				for range 8 {
					health, err = runWithAdapters(lookup.workContext(t.Context()), store, config.Config{}, Options{Rollouts: lookup, nativeOnly: true}, []SourceAdapter{adapter})
					if err != nil || !health.Pending {
						break
					}
				}
				if tc.failure {
					var source *agentapi.SourceError
					if !errors.As(err, &source) || source.Cause != agentapi.CauseLookupNoProgress || adapter.calls > 32 || adapter.calls < tc.minCalls {
						t.Fatal("cookie loop was not bounded", adapter.calls, err)
					}
				} else if err != nil || health.Pending || adapter.calls != 4 {
					t.Fatal("distinct opaque cookies refused", adapter.calls, health.Pending, err)
				}
				if err := lookup.CloseReadOnly(); err != nil {
					t.Fatal(err)
				}
				if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
					t.Fatal("cookie work leaked charges", used)
				}
			})
		})
	}
}
