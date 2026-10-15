package discovery

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestCurrentLocatorUsesRemainingOperationBudget(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"scheduling_delay", "caller_cancel", "caller_deadline", "remaining_budget"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				native := hintDatabase(t, root, false)
				addHint(t, native, "thread", "selected", time.Now())
				snapshot, err := snapshotCurrentIndex(t.Context(), root, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = snapshot.close() }()
				db, err := openPrivateCurrent(snapshot.path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				// Occupying the only connection makes acquisition block without
				// changing any native file or requiring a production test hook.
				held, err := db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = held.Close() }()
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "caller_deadline" {
					var stop context.CancelFunc
					parent, stop = context.WithTimeout(parent, 3*indexHintTimeout)
					defer stop()
				}
				remaining := Budget
				if mode == "remaining_budget" {
					remaining = 3 * indexHintTimeout
				}
				lookup := &CodexRolloutLookup{remaining: remaining}
				ctx, done, err := lookup.beginOperation(parent)
				if err != nil {
					t.Fatal(err)
				}
				var operationErr error
				defer done(&operationErr)
				type result struct {
					path  string
					found bool
					err   error
				}
				results := make(chan result, 1)
				go func() {
					path, found, err := currentLocator(ctx, db, "thread")
					results <- result{path, found, err}
				}()
				synctest.Wait()
				time.Sleep(2 * indexHintTimeout)
				select {
				case got := <-results:
					t.Fatalf("private query stopped at advisory hint allowance: path=%q found=%t err=%v", got.path, got.found, got.err)
				default:
				}
				if mode == "scheduling_delay" {
					if err := held.Close(); err != nil {
						t.Fatal(err)
					}
					got := <-results
					if got.err != nil || !got.found || got.path != "selected" {
						t.Fatal(got)
					}
					return
				}
				want := context.DeadlineExceeded
				if mode == "caller_cancel" {
					cancel()
					want = context.Canceled
				}
				got := <-results
				if !errors.Is(got.err, want) || got.found || got.path != "" {
					t.Fatalf("private query ignored operation context: path=%q found=%t err=%v; want %v", got.path, got.found, got.err, want)
				}
			})
		})
	}
}
