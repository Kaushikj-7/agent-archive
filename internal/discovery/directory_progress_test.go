package discovery

import (
	"context"
	"errors"
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
