package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

type sourceSetCleanupFault string

const (
	cleanupRequestToken sourceSetCleanupFault = "request-token"
	cleanupCatalog      sourceSetCleanupFault = "catalog"
	cleanupPhase        sourceSetCleanupFault = "phase"
	cleanupWire         sourceSetCleanupFault = "wire"
	cleanupCancel       sourceSetCleanupFault = "cancel"
)

func TestExactSourceSetCleanupRefusesChangedWireAndAuthority(t *testing.T) {
	t.Parallel()
	for _, fault := range []sourceSetCleanupFault{cleanupRequestToken, cleanupCatalog, cleanupPhase, cleanupWire, cleanupCancel} {
		t.Run(string(fault), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			p := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
			p, err := PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
			if err != nil {
				t.Fatal(err)
			}
			id := p.Bundle.ArchiveSessionID
			if err = s.SavePending(id, p); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.pendingPath(id))
			if err != nil {
				t.Fatal(err)
			}
			owner := s
			switch fault {
			case cleanupRequestToken:
				p.RequestToken = "different-owned-request"
			case cleanupCatalog:
				p.Catalog = &CatalogPublication{Protocol: 11, ID: "foreign"}
			case cleanupPhase:
				p.Phase = PublicationPreparing
			case cleanupWire:
				before = append(before, ' ')
				if err = os.WriteFile(s.pendingPath(id), before, 0600); err != nil {
					t.Fatal(err)
				}
			case cleanupCancel:
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				var closeOwner func()
				owner, closeOwner = s.WithReadBudget(ctx, agentapi.NewNativeReadBudget(1<<20))
				defer closeOwner()
			}
			if err = owner.RemoveExactSourceSetPending(id, p); err == nil {
				t.Fatal("changed source-set journal was removed")
			}
			after, readErr := os.ReadFile(s.pendingPath(id))
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("refused cleanup changed journal: %v", readErr)
			}
			if fault == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("wrong cancellation: %v", err)
			}
		})
	}
}

func TestExactSourceSetCleanupOwnedOrdinaryUsesBoundedMemory(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := accountingFixture(t, "ordinary-source-set", true, true)
	p, err := PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	id := p.Bundle.ArchiveSessionID
	if err = s.SavePending(id, p); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeOwner := s.WithReadBudget(t.Context(), budget)
	defer closeOwner()
	pressure := budget.Reservation()
	charged := budget.Available() - (128 << 10)
	if !pressure.Reserve(charged) {
		t.Fatal("pressure setup")
	}
	defer pressure.Release(charged)
	if err = scoped.RemoveExactSourceSetPending(id, p); err != nil {
		t.Fatalf("owned ordinary cleanup decoded another large body: %v", err)
	}
	if _, err = os.Stat(s.pendingPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact owned journal still present: %v", err)
	}
}
