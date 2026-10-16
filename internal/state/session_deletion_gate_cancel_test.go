package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestDeletionCapabilityRefusesAlreadyCancelledSmallConfig(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(`{"schema_version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	scoped, closeScope := s.WithReadBudget(ctx, agentapi.NewNativeReadBudget(128<<10))
	t.Cleanup(closeScope)
	if inactive, err := scoped.deletionInactive(); inactive || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled guard granted legacy bypass", inactive, err)
	}
}

func TestDeletionCapabilityCancellationInterruptsTrailingWhitespace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(`{"schema_version":1}`+strings.Repeat(" ", 128<<10)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := &accountingCancelContext{Context: t.Context(), remaining: 5}
	scoped, closeScope := s.WithReadBudget(ctx, agentapi.NewNativeReadBudget(128<<10))
	t.Cleanup(closeScope)
	if inactive, err := scoped.deletionInactive(); inactive || !errors.Is(err, context.Canceled) {
		t.Fatal("trailing whitespace ignored captured cancellation", inactive, err)
	}
}

func TestDeletionCapabilityKeepsCancellationWithoutReadBudget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	scoped, closeScope := s.WithReadBudget(ctx, nil)
	t.Cleanup(closeScope)
	if inactive, err := scoped.deletionInactive(); inactive || !errors.Is(err, context.Canceled) {
		t.Fatal("nil budget discarded captured cancellation", inactive, err)
	}
}
