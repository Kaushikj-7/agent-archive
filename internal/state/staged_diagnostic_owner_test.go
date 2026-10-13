package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

type stagedOwnedResumeControl interface {
	ResumeStagedReleases(context.Context, StagedReadback, *local.CollectorGuard) (map[string]error, func(), error)
}

type stagedBeforeResumeControl interface {
	ResumeStagedReleases(context.Context, StagedReadback, *local.CollectorGuard) (map[string]error, error)
}

// Cleanup-only signature adapter permits identical source on frozen5c. It
// grants no authority and changes no before production implementation.
func callStagedResumeControl(ctx context.Context, s *Store, remote StagedReadback, guard *local.CollectorGuard) (map[string]error, func(), error) {
	if owned, ok := any(s).(stagedOwnedResumeControl); ok {
		holds, closeDiagnostics, err := owned.ResumeStagedReleases(ctx, remote, guard)
		if closeDiagnostics == nil {
			closeDiagnostics = func() {}
		}
		return holds, closeDiagnostics, err
	}
	holds, err := any(s).(stagedBeforeResumeControl).ResumeStagedReleases(ctx, remote, guard)
	return holds, func() {}, err
}

func TestStagedScheduledDiagnosticOwnerKeepsRawHoldThroughAliases(t *testing.T) {
	s, p, guard := stagedSelectionFixture(t, true)
	id := p.Bundle.ArchiveSessionID
	if err := s.SaveRequest(id, "hook", p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	request, _, err := s.LoadRequest(id)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(2 << 20)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	before := budget.Available()
	holds, closeDiagnostics, err := callStagedResumeControl(t.Context(), scoped, stagedRemote(p), guard)
	defer closeDiagnostics()
	if err != nil || !errors.Is(holds[id], ErrReadySelectionFrozen) {
		t.Fatalf("per-owner raw hold: %v %v", holds, err)
	}
	alias := holds
	if !errors.Is(alias[id], ErrReadySelectionFrozen) {
		t.Fatal("alias lost raw cause")
	}
	if before-budget.Available() != int64(len(id)) {
		t.Fatalf("diagnostic ID escaped owner: %d", before-budget.Available())
	}
	current, found, err := s.LoadRequest(id)
	if err != nil || !found || current.Token != request.Token {
		t.Fatal("hold consumed successor request", err)
	}
	closeDiagnostics()
	closeDiagnostics()
	if budget.Available() != before {
		t.Fatal("diagnostic closeDiagnostics was not exact/idempotent")
	}
}

func TestStagedScheduledPressureRefusesBeforeReleaseEffects(t *testing.T) {
	s, p, guard := stagedSelectionFixture(t, true)
	before := stagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID)
	remote := stagedRemote(p)
	budget := agentapi.NewNativeReadBudget(1)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	holds, closeDiagnostics, err := callStagedResumeControl(t.Context(), scoped, remote, guard)
	defer closeDiagnostics()
	if !errors.Is(err, agentapi.ErrReadBudget) || len(holds) != 0 || remote.reads != 0 {
		t.Fatalf("pressure before effects: %v %v %d", holds, err, remote.reads)
	}
	assertStagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID, before)
}

func TestStagedTypedDigestClassifiesOnlyLimitFailures(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	reg, _, err := s.LoadRegistration(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	stage, found, err := s.LoadImportStage(t.Context(), reg)
	if err != nil || !found {
		t.Fatalf("typed stage: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = stage.digest(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatalf("cancellation misclassified: %v", err)
	}
	unsupported := stage
	unsupported.Bundle.NativeRecords = []map[string]any{{"value": make(chan struct{})}}
	if _, err = unsupported.digest(t.Context()); err == nil || errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatalf("unsupported value misclassified: %v", err)
	}
	oversized := stage
	oversized.Bundle.NativeRecords = []map[string]any{{"content": strings.Repeat("x", int(MaxImportStageBytes)+1)}}
	if _, err = oversized.digest(t.Context()); !errors.Is(err, ErrDurableStorageCapacity) || !errors.Is(err, agentmeta.ErrJSONWireLimit) {
		t.Fatalf("limit/raw cause classification: %v", err)
	}
}
