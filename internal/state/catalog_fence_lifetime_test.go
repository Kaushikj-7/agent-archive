package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

type catalogFenceCheck string

const (
	catalogFenceSave    catalogFenceCheck = "save"
	catalogFenceRemoval catalogFenceCheck = "removal"
)

func checkCatalogFence(t *testing.T, s *Store, check catalogFenceCheck, pending PendingPublication) error {
	t.Helper()
	switch check {
	case catalogFenceSave:
		return s.checkCatalogPendingSave(s.durableContext(), "owned", pending)
	case catalogFenceRemoval:
		return s.checkCatalogPendingRemoval("owned")
	}
	t.Fatal("unknown fence check")
	return nil
}

func TestCatalogPendingFenceReadLeaseEndsWithCheck(t *testing.T) {
	for _, check := range []catalogFenceCheck{catalogFenceSave, catalogFenceRemoval} {
		t.Run(string(check), func(t *testing.T) {
			s := newTestStore(t)
			raw := []byte(`{"request_token":"owned","source_bytes":"cHJlc2VydmVk"}`)
			if err := local.WriteBytes(s.pendingPath("owned"), raw); err != nil {
				t.Fatal(err)
			}
			var pending PendingPublication
			if err := json.Unmarshal(raw, &pending); err != nil {
				t.Fatal(err)
			}
			const pressure = int64(1)
			budget := agentapi.NewNativeReadBudget(2*int64(len(raw)) + pressure)
			if !budget.Reserve(pressure) {
				t.Fatal("independent owner reservation")
			}
			defer budget.Release(pressure)
			scoped, closeScope := s.WithReadBudget(t.Context(), budget)
			defer closeScope()
			for range 2 {
				if err := checkCatalogFence(t, scoped, check, pending); err != nil {
					t.Fatal(err)
				}
				used, _ := budget.Charged()
				if used != pressure {
					t.Fatalf("completed fence retained decoded work: charged=%d want=%d", used, pressure)
				}
			}
			after, err := os.ReadFile(s.pendingPath("owned"))
			if err != nil || string(after) != string(raw) {
				t.Fatalf("inspection changed pending bytes: %q %v", after, err)
			}
		})
	}
}

func TestCatalogPendingFenceRefusalKeepsBytesAndBudget(t *testing.T) {
	for _, check := range []catalogFenceCheck{catalogFenceSave, catalogFenceRemoval} {
		t.Run(string(check), func(t *testing.T) {
			s := newTestStore(t)
			raw := []byte(`{"request_token":"owned","source_bytes":"cHJlc2VydmVk"}`)
			if err := local.WriteBytes(s.pendingPath("owned"), raw); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(2*int64(len(raw)) - 1)
			scoped, closeScope := s.WithReadBudget(t.Context(), budget)
			err := checkCatalogFence(t, scoped, check, PendingPublication{})
			if !agentapi.HasFailure(err, agentapi.Limit) || !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatalf("shared limit refusal: %v", err)
			}
			closeScope()
			used, _ := budget.Charged()
			if used != 0 {
				t.Fatalf("refused fence leaked %d bytes", used)
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			scoped, closeScope = s.WithReadBudget(canceled, budget)
			err = checkCatalogFence(t, scoped, check, PendingPublication{})
			closeScope()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation refusal: %v", err)
			}
			after, err := os.ReadFile(s.pendingPath("owned"))
			if err != nil || string(after) != string(raw) {
				t.Fatalf("refusal changed pending bytes: %q %v", after, err)
			}
		})
	}
}
