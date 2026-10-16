package state

import (
	"bytes"
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

func TestRetentionPreparationBindsExplicitContextOnUnscopedStore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(reg.ArchiveSessionID, "late-hook", reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	requestPath := s.requestPath(reg.ArchiveSessionID)
	requestBefore, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerBefore, err := os.ReadFile(s.registrationPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(`{"schema_version":1}`+strings.Repeat(" ", 128<<10)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := &accountingCancelContext{Context: t.Context(), remaining: 5}
	if _, err := s.PrepareRetentionRestoration(ctx, nil, reg, Request{}, nil, "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatal("explicit caller context lost before deletion control read", err)
	}
	if s.resourceContext != nil {
		t.Fatal("context binding mutated shared Store")
	}
	for path, before := range map[string][]byte{requestPath: requestBefore, s.registrationPath(reg.ArchiveSessionID): ownerBefore} {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
			t.Fatal("cancelled entry changed owed evidence", path, err)
		}
	}

}

type deletionCancellingReservation struct {
	stateReservation
	cancel context.CancelFunc
}

func (r deletionCancellingReservation) Reserve(n int64) bool {
	held := r.stateReservation.Reserve(n)
	if held {
		r.cancel()
	}
	return held
}

func TestRetentionPreparationKeepsOriginalReadOwnerCancellation(t *testing.T) {
	t.Parallel()
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "already-cancelled", true: "during-allocation"}[during], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			reg := registration(t)
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveRequest(reg.ArchiveSessionID, "late-hook", reg.RegisteredAt); err != nil {
				t.Fatal(err)
			}
			requestPath := s.requestPath(reg.ArchiveSessionID)
			before, err := os.ReadFile(requestPath)
			if err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(128 << 10)
			owner, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			scoped, closeScope := s.WithReadBudget(owner, budget)
			t.Cleanup(closeScope)
			if during {
				reservation := deletionCancellingReservation{stateReservation: scoped.readReservation(), cancel: cancel}
				scoped.resourceReservation = func() stateReservation { return reservation }
			} else {
				cancel()
			}
			if _, err := scoped.PrepareRetentionRestoration(t.Context(), nil, reg, Request{}, nil, "", "", ""); !errors.Is(err, context.Canceled) {
				t.Fatal("live caller revived original read owner", err)
			}
			if after, err := os.ReadFile(requestPath); err != nil || !bytes.Equal(before, after) {
				t.Fatal("cancelled owner changed owed request", err)
			}
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("cancelled guard leaked original reservation", used)
			}
		})
	}
}
