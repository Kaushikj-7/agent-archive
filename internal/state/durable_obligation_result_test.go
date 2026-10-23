package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestDurableObligationResultBoundsCacheWithoutTruncatingInventory(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const count = 5000
	for i := range count {
		id := fmt.Sprintf("%s-%04d", strings.Repeat("s", 235), i)
		if err := os.WriteFile(s.pendingPath(id), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	obligations, closeResult, err := read.DurableStorageObligationsOwned(t.Context())
	if err != nil || len(obligations) != count {
		closeResult()
		t.Fatal("complete named inventory", len(obligations), err)
	}
	if len(s.durableInspection.obligations) != 0 || s.durableInspection.valid {
		t.Fatal("oversized cache retained the result")
	}
	if used, _ := budget.Charged(); used == 0 {
		t.Fatal("result vectors/strings unfunded")
	}
	alias := obligations
	if alias[count-1].SessionID == "" {
		t.Fatal("borrowed result invalid before close")
	}
	closeResult()
	closeResult()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("result/cache close leaked charge", used)
	}
	pressureOwner := budget.Reservation()
	pressure := budget.Available() - 1024
	if !pressureOwner.Reserve(pressure) {
		t.Fatal("pressure")
	}
	partial, closePartial, err := read.DurableStorageObligationsOwned(t.Context())
	if !errors.Is(err, agentapi.ErrReadBudget) || len(partial) == count {
		closePartial()
		t.Fatal("unfunded result certified complete", len(partial), err)
	}
	closePartial()
	pressureOwner.Release(pressure)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("partial result leaked charge", used)
	}
}

func TestDurableObligationCacheEndsWithCapturedOwner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := os.WriteFile(s.pendingPath("synthetic"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(2 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	obligations, closeResult, err := read.DurableStorageObligationsOwned(t.Context())
	if err != nil || len(obligations) != 1 || !s.durableInspection.valid {
		closeResult()
		t.Fatal("funded cache/result", len(obligations), err)
	}
	closeResult()
	if s.durableInspection.valid || s.durableInspection.obligations != nil {
		t.Fatal("cache escaped captured result owner")
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("cache/result charge escaped close", used)
	}
}

// An opaque import-stage marker is recovery debt even without a registration.
// On result pressure the census must stop explicitly incomplete, rather than
// join an uncharged error for every remaining physical owner.
func TestDurableObligationPressureStopsWithBoundedRefusal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const count = 512
	for i := range count {
		dir := filepath.Join(s.home, "sessions", fmt.Sprintf("opaque-%04d", i), "import-stage")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "unknown"), []byte("synthetic recovery marker"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	budget := agentapi.NewNativeReadBudget(4096)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	partial, closePartial, err := read.DurableStorageObligationsOwned(t.Context())
	defer closePartial()
	if !errors.Is(err, agentapi.ErrReadBudget) || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("result pressure lost explicit incompleteness", err)
	}
	if len(partial) == 0 || len(partial) >= count {
		t.Fatal("partial result claimed complete debt inventory", len(partial))
	}
	if len(err.Error()) > 1024 {
		t.Fatal("refusal retained per-owner error chain", len(err.Error()))
	}
	if used, _ := budget.Charged(); used == 0 {
		t.Fatal("live partial result lost its charge")
	}
	closePartial()
	closePartial()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("refused census owner leaked charge", used)
	}
	// A fresh adequately funded result must still see all physical debt.
	complete, closeComplete, err := s.DurableStorageObligationsOwned(t.Context())
	defer closeComplete()
	if err != nil || len(complete) != count {
		t.Fatal("pressure created false completion or recovery credit", len(complete), err)
	}
}
