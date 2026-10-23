package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestDurableObligationCacheYieldsToMandatoryResult(t *testing.T) {
	for _, enough := range []bool{true, false} {
		name := "one-byte-short"
		if enough {
			name = "exact-result"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			if err := os.WriteFile(s.pendingPath("synthetic"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(2 << 20)
			endScope := budget.BeginScope(2 << 20)
			defer endScope()
			read, closeRead := s.WithReadBudget(t.Context(), budget)
			defer closeRead()
			first, closeFirst, err := read.DurableStorageObligationsOwned(t.Context())
			defer closeFirst()
			if err != nil || len(first) != 1 || !s.durableInspection.valid {
				t.Fatal("funded initial result/cache", len(first), err)
			}
			firstUsed, _ := budget.Charged()
			cacheBytes := int64(512 + 32 + len(first[0].SessionID))
			mandatory := firstUsed - cacheBytes
			if mandatory <= cacheBytes {
				t.Fatal("missing independently funded result vectors", firstUsed)
			}
			available := mandatory - cacheBytes
			if !enough {
				available--
			}
			pressure := budget.Available() - available
			inherited := budget.Reservation()
			if !inherited.Reserve(pressure) {
				t.Fatal("inherited pressure")
			}
			defer inherited.Release(pressure)
			second, closeSecond, err := read.DurableStorageObligationsOwned(t.Context())
			defer closeSecond()
			if enough {
				if err != nil || len(second) != 1 || second[0] != first[0] {
					t.Fatal("optional cache crowded mandatory complete result", second, err)
				}
				if budget.Available() != 0 || budget.ScopePressured() || budget.ScopeExceeded() {
					t.Fatal("exact funded result recorded false pressure", budget.Available())
				}
			} else if !errors.Is(err, agentapi.ErrReadBudget) || len(second) == 1 {
				t.Fatal("unfunded mandatory result claimed complete", second, err)
			}
			if s.durableInspection.valid || s.durableInspection.obligations != nil || first[0].SessionID != "synthetic" {
				t.Fatal("cache eviction invalidated returned result or retained optional names")
			}
			closeFirst()
			closeFirst()
			if enough && second[0].SessionID != "synthetic" {
				t.Fatal("closing prior result invalidated current result")
			}
			closeSecond()
			closeSecond()
			if used, _ := budget.Charged(); used != pressure {
				t.Fatal("result close changed inherited pressure or leaked owned charges", used, pressure)
			}
		})
	}
}

func TestDurableObligationResultPreservesBothCancellationOwners(t *testing.T) {
	for _, captured := range []bool{false, true} {
		name := "caller"
		if captured {
			name = "captured-owner"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			path := s.pendingPath("synthetic")
			before := []byte("{}")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			budget := agentapi.NewNativeReadBudget(1 << 20)
			read, closeRead := s.WithReadBudget(owner, budget)
			defer closeRead()
			funded, closeFunded, err := read.DurableStorageObligationsOwned(caller)
			if err != nil || len(funded) != 1 {
				closeFunded()
				t.Fatal("funded actual census", funded, err)
			}
			closeFunded()
			if captured {
				cancelOwner()
			} else {
				cancelCaller()
			}
			_, closeCanceled, err := read.DurableStorageObligationsOwned(caller)
			closeCanceled()
			closeCanceled()
			if !errors.Is(err, context.Canceled) {
				t.Fatal("canceled obligation owner was replaced", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("canceled census changed durable work", readErr)
			}
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("canceled result retained charge", used)
			}
		})
	}
}
