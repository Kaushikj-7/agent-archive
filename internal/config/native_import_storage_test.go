package config

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeImportStoragePreparationDoesNotHoldHooksOrConvertOrdinaryGuard(t *testing.T) {
	home := t.TempDir()
	cfg := Config{NativeImportProtection: true, NativeAuthorityProtection: true, ImportStagingProtection: true, StagedSelectionProtection: true, DurableStorageProtection: true, PublicationCompositionProtection: true}
	cfg.Archive.Enabled = true
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	before := budget.Available()
	var saved NativeImportStoragePreparation
	err := WithNativeImportStorage(t.Context(), home, func() ReadReservation { return budget.Reservation() }, func(p NativeImportStoragePreparation) error {
		saved = p
		unlock, err := local.NamedLockWait(home, "hooks.lock", 0)
		if err != nil {
			return err
		}
		unlock()
		if err := p.CheckHome(home); err != nil {
			return err
		}
		var ordinary DurableStorageGuard
		if ordinary.CheckHome(home) == nil {
			t.Fatal("preparation converted to ordinary guard")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.CheckHome(home) == nil {
		t.Fatal("preparation outlived callback")
	}
	if budget.Available() != before {
		t.Fatal("configuration loans leaked")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err = WithNativeImportStorage(ctx, home, func() ReadReservation { return budget.Reservation() }, func(NativeImportStoragePreparation) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled preparation acquired writer", err)
	}
}
