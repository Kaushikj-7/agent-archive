package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Cancel only after the actual census owns a nonempty charged result.
// Generic context checks may change as caller cancellation propagates.
type censusCancellation struct {
	context.Context
	cancel          context.CancelFunc
	checks          int
	budget          *agentapi.NativeReadBudget
	chargedAtCancel int64
}

func (c *censusCancellation) Err() error {
	c.checks++
	if used, _ := c.budget.Charged(); used > 0 && c.chargedAtCancel == 0 {
		c.chargedAtCancel = used
		c.cancel()
	}
	return c.Context.Err()
}

func TestDurableCensusCancellationRetainsPartialObligations(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 257 {
		dir := filepath.Join(s.home, "publication-evidence", fmt.Sprintf("owed-%03d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "original"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if all, err := s.DurableStorageObligations(); err != nil || len(all) != 257 {
		t.Fatal("warm census", len(all), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	budget := agentapi.NewNativeReadBudget(128 << 20)
	observed := &censusCancellation{Context: ctx, cancel: cancel, budget: budget}
	scoped, closeScope := s.WithReadBudget(observed, budget)
	defer closeScope()
	partial, closePartial, err := scoped.DurableStorageObligationsOwned(observed)
	defer closePartial()
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDurableStorageRecovery) || len(partial) == 0 || len(partial) >= 257 || observed.chargedAtCancel == 0 {
		t.Fatalf("partial census certified complete/empty: count=%d err=%v checks=%d", len(partial), err, observed.checks)
	}
	closePartial()
	closePartial()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("canceled partial result leaked charge", used)
	}
	all, closeComplete, err := s.DurableStorageObligationsOwned(t.Context())
	defer closeComplete()
	// Both owned censuses enumerate fresh names after the legacy warm census.
	if err != nil || len(all) != 257 || s.durableInspection.configLoads != 1 || s.durableInspection.scans != 3 {
		t.Fatalf("cancel affected subsequent complete census: count=%d err=%v configs=%d scans=%d", len(all), err, s.durableInspection.configLoads, s.durableInspection.scans)
	}
}
