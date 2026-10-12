package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestReadyContextReadsCancelWithoutLosingOwnedEvidence(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	if err := s.SavePending(id, p); err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	pendingBytes, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	publishedBytes, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	if value, err := scoped.LoadPublishedStateContext(ctx, id); value != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("published read discarded cancellation: %v", err)
	}
	if value, found, err := scoped.LoadPendingContext(ctx, id); value.Selection != nil || found || !errors.Is(err, context.Canceled) {
		t.Fatalf("pending read discarded cancellation: %v %v", found, err)
	}
	if budget.Available() != 1<<20 {
		t.Fatal("canceled read charged or released unrelated owner")
	}
	got, err := os.ReadFile(s.pendingPath(id))
	if err != nil || !bytes.Equal(got, pendingBytes) {
		t.Fatal("canceled pending read changed evidence")
	}
	got, err = os.ReadFile(s.publishedPath(id))
	if err != nil || !bytes.Equal(got, publishedBytes) {
		t.Fatal("canceled publication read changed evidence")
	}
}
