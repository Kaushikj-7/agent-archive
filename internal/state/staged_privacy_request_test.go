package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestStagedPrivacyRequestUsesActualImmutableStageWithoutChangingOriginalAuthority(t *testing.T) {
	s, pending := stagedPrivacyActualFixture(t)
	id := pending.Bundle.ArchiveSessionID
	before := stagedAuthorityBytes(t, s, id)
	reg, _, err := s.LoadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	stage, _, err := s.LoadImportStage(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SkillEvidence = config.SkillEvidenceNone
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(32 << 20)
	defer budget.BeginScope(16 << 20)()
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
	request, err := read.FreezeStagedPrivacyRequest(context.Background(), id, filter.Version())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Close(); err != nil {
			t.Error(err)
		}
	}()
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := proof.Close(); err != nil {
			t.Error(err)
		}
	}()
	err = proof.WithVerifiedTransform(t.Context(), request, budget, func(_ context.Context, output archive.SourceBundle, _ archive.CompressedSource, facts retainedprivacy.Facts) error {
		if facts.StageSHA256 != stage.SHA256 || facts.OwnerSHA256 == "" || facts.CoveredToken != pending.RequestToken || !facts.HadRequest || facts.OriginalPolicy.SkillEvidence != string(stage.SkillEvidence) || facts.TargetPolicy.SkillEvidence != "none" || output.Capture.FilterVersion != archive.FilterVersion || !output.Capture.CapturedAt.Equal(stage.Bundle.Capture.CapturedAt) {
			t.Fatal("stage/request/policy/capture facts changed", facts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStagedAuthorityBytes(t, s, id, before)
}

type stagedPrivacyFault string

const (
	stagedPrivacyMissing   stagedPrivacyFault = "missing"
	stagedPrivacyAltered   stagedPrivacyFault = "altered"
	stagedPrivacyCancelled stagedPrivacyFault = "cancelled"
	stagedPrivacyUnscoped  stagedPrivacyFault = "unscoped"
	stagedPrivacyNewer     stagedPrivacyFault = "newer-request"
)

func TestStagedPrivacyRequestRefusesMissingOrAlteredStageAndOriginalContextCancellation(t *testing.T) {
	for _, mode := range []stagedPrivacyFault{stagedPrivacyMissing, stagedPrivacyAltered, stagedPrivacyCancelled, stagedPrivacyUnscoped, stagedPrivacyNewer} {
		t.Run(string(mode), func(t *testing.T) {
			s, pending, _ := stagedPreparedFixture(t, true)
			id := pending.Bundle.ArchiveSessionID
			path := filepath.Join(s.Home(), importStageRelative(id))
			switch mode {
			case stagedPrivacyCancelled, stagedPrivacyUnscoped: // Lifetime and ownership faults are applied below.
			case stagedPrivacyNewer:
				if err := s.SaveRequest(id, "newer", time.Now()); err != nil {
					t.Fatal(err)
				}
			case stagedPrivacyMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case stagedPrivacyAltered:
				if err := os.WriteFile(path, []byte(`{"version":2,"typed_stage":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := stagedAuthorityBytes(t, s, id)
			budget := agentapi.NewNativeReadBudget(32 << 20)
			if mode != stagedPrivacyUnscoped {
				defer budget.BeginScope(16 << 20)()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			read, closeRead := s.WithReadBudget(ctx, budget)
			defer closeRead()
			if mode == stagedPrivacyCancelled {
				cancel()
			}
			_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
			request, err := read.FreezeStagedPrivacyRequest(context.Background(), id, filter.Version())
			if err == nil || request != nil {
				t.Fatal("invalid stage/lifetime minted a request", err)
			}
			if mode == stagedPrivacyCancelled && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			assertStagedAuthorityBytes(t, s, id, before)
		})
	}
}

func stagedPrivacyActualFixture(t *testing.T) (*Store, PendingPublication) {
	t.Helper()
	s, pending, _ := stagedPreparedFixture(t, true, []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "native-owned"}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "synthetic retained prompt"}}}},
	})
	return s, pending
}

func TestStagedPrivacyFactoryKeepsOriginalOwnerAfterAmbientScopeChanges(t *testing.T) {
	s, pending := stagedPrivacyActualFixture(t)
	budget := agentapi.NewNativeReadBudget(32 << 20)
	endOriginal := budget.BeginScope(16 << 20)
	original := budget.Reservation()
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	var endOther func()
	var other *agentapi.NativeReadReservation
	defer func() {
		closeRead()
		if other != nil {
			other.Release(1)
		}
		if endOther != nil {
			endOther()
		}
		endOriginal()
	}()
	read.resourceReservation = func() stateReservation {
		if endOther == nil {
			endOther = budget.BeginScope(1)
			other = budget.Reservation()
			if !other.Reserve(1) {
				t.Fatal("ambient scope fixture")
			}
		}
		return original
	}
	_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
	request, err := read.FreezeStagedPrivacyRequest(t.Context(), pending.Bundle.ArchiveSessionID, filter.Version())
	if err != nil {
		t.Fatal("factory borrowed later ambient scope", err)
	}
	defer func() {
		if err := request.Close(); err != nil {
			t.Error(err)
		}
	}()
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal("producer borrowed later ambient scope", err)
	}
	defer func() {
		if err := proof.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := proof.WithVerifiedTransform(t.Context(), request, budget, func(context.Context, archive.SourceBundle, archive.CompressedSource, retainedprivacy.Facts) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStagedPrivacyFactoryStopsWorkWhenOriginalOwnerCancels(t *testing.T) {
	s, pending := stagedPrivacyActualFixture(t)
	before := stagedAuthorityBytes(t, s, pending.Bundle.ArchiveSessionID)
	budget := agentapi.NewNativeReadBudget(32 << 20)
	defer budget.BeginScope(16 << 20)()
	original, cancel := context.WithCancel(t.Context())
	defer cancel()
	read, closeRead := s.WithReadBudget(original, budget)
	defer closeRead()
	crossed := false
	read.stagedPrivacyCompressionBoundary = func(after bool) {
		if after {
			crossed = true
		} else {
			cancel()
		}
	}
	_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
	request, err := read.FreezeStagedPrivacyRequest(context.Background(), pending.Bundle.ArchiveSessionID, filter.Version())
	if crossed {
		t.Fatal("canonical compression completed after original owner cancelled")
	}
	if request != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("original cancellation lost", err)
	}
	assertStagedAuthorityBytes(t, s, pending.Bundle.ArchiveSessionID, before)
}

type stagedPrivacyOwnerFault uint8

const (
	stagedPrivacyForeignOwner stagedPrivacyOwnerFault = iota
	stagedPrivacyWrongPinnedScope
	stagedPrivacyNilOwner
	stagedPrivacyUnscopedOwner
	stagedPrivacyOpaqueOwner
)

type stagedPrivacyOpaqueReservation struct {
	*agentapi.NativeReadReservation
}

func TestStagedPrivacyFactoryRejectsForeignUnscopedOrUnrepresentableOwners(t *testing.T) {
	for _, fault := range []stagedPrivacyOwnerFault{stagedPrivacyForeignOwner, stagedPrivacyWrongPinnedScope, stagedPrivacyNilOwner, stagedPrivacyUnscopedOwner, stagedPrivacyOpaqueOwner} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			s, pending := stagedPrivacyActualFixture(t)
			before := stagedAuthorityBytes(t, s, pending.Bundle.ArchiveSessionID)
			budget := agentapi.NewNativeReadBudget(32 << 20)
			unscoped := budget.Reservation()
			defer budget.BeginScope(16 << 20)()
			original := budget.Reservation()
			read, closeRead := s.WithReadBudget(t.Context(), budget)
			defer closeRead()
			var owner stateReservation
			switch fault {
			case stagedPrivacyForeignOwner:
				foreign := agentapi.NewNativeReadBudget(32 << 20)
				defer foreign.BeginScope(16 << 20)()
				owner = foreign.Reservation()
			case stagedPrivacyWrongPinnedScope:
				pinned, err := budget.CaptureFromReservation(original)
				if err != nil {
					t.Fatal(err)
				}
				defer budget.BeginScope(16 << 20)()
				owner = budget.Reservation()
				read.resourceBudget = pinned
			case stagedPrivacyNilOwner:
				owner = (*agentapi.NativeReadReservation)(nil)
			case stagedPrivacyUnscopedOwner:
				owner = unscoped
			case stagedPrivacyOpaqueOwner:
				owner = &stagedPrivacyOpaqueReservation{original}
			}
			read.resourceReservation = func() stateReservation { return owner }
			_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
			request, err := read.FreezeStagedPrivacyRequest(t.Context(), pending.Bundle.ArchiveSessionID, filter.Version())
			if request != nil || !errors.Is(err, agentapi.ErrReadBudget) {
				t.Fatal("invalid captured owner reached actual stage work", err)
			}
			assertStagedAuthorityBytes(t, s, pending.Bundle.ArchiveSessionID, before)
		})
	}
}

func TestStagedPrivacyFactoryCannotBorrowLargerLaterScope(t *testing.T) {
	s, pending := stagedPrivacyActualFixture(t)
	before := stagedAuthorityBytes(t, s, pending.Bundle.ArchiveSessionID)
	budget := agentapi.NewNativeReadBudget(32 << 20)
	defer budget.BeginScope(128 << 10)()
	original := budget.Reservation()
	defer budget.BeginScope(16 << 20)()
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	read.resourceReservation = func() stateReservation { return original }
	_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
	request, err := read.FreezeStagedPrivacyRequest(t.Context(), pending.Bundle.ArchiveSessionID, filter.Version())
	if request != nil || !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("small original scope borrowed later credit", err)
	}
	if budget.ScopeExceeded() {
		t.Fatal("refusal was attributed to later ambient scope")
	}
	assertStagedAuthorityBytes(t, s, pending.Bundle.ArchiveSessionID, before)
}
