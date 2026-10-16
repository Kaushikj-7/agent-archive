package state

import (
	"context"
	"errors"
	"testing"
)

func TestGenerationCommittedAuthorityHonorsCallerAndCapturedContext(t *testing.T) {
	t.Parallel()
	for _, captured := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "captured-owner"}[captured], func(t *testing.T) {
			t.Parallel()
			s, reg, _ := generationFixture(t)
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			read, end := s.WithReadBudget(owner, nil)
			defer end()
			if captured {
				cancelOwner()
			} else {
				cancelCaller()
			}
			if _, _, err := read.generationCommittedAuthority(caller, reg); !errors.Is(err, context.Canceled) {
				t.Fatalf("committed authority ignored cancellation: %v", err)
			}
		})
	}
}

func TestCatalogPendingSaveHonorsCallerAndCapturedContext(t *testing.T) {
	t.Parallel()
	for _, captured := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "captured-owner"}[captured], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			read, end := s.WithReadBudget(owner, nil)
			defer end()
			if captured {
				cancelOwner()
			} else {
				cancelCaller()
			}
			if err := read.checkCatalogPendingSave(caller, "synthetic-pending", PendingPublication{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("catalog pending fence ignored cancellation: %v", err)
			}
		})
	}
}
