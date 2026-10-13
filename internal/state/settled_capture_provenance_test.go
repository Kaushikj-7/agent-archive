package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

func TestSettledCaptureRejectsGenuineForeignLedgerBeforeEffects(t *testing.T) {
	s, reg, request, proof, a := settledStateCaptureFixture(t)
	b := agentapi.NewNativeReadBudget(128 << 20)
	wrong, closeWrong := s.WithReadBudget(t.Context(), b)
	defer closeWrong()
	before := stagedAuthorityBytes(t, s, reg.ArchiveSessionID)
	availableA, availableB := a.Available(), b.Available()
	if _, err := wrong.SaveSettledCursorImportStage(t.Context(), reg, request, proof); err == nil {
		t.Fatal("foreign ledger accepted")
	}
	assertStagedAuthorityBytes(t, s, reg.ArchiveSessionID, before)
	if a.Available() != availableA || b.Available() != availableB {
		t.Fatal("foreign refusal charged a ledger")
	}
	right, closeRight := s.WithReadBudget(t.Context(), a)
	if _, err := right.SaveSettledCursorImportStage(t.Context(), reg, request, proof); err != nil {
		t.Fatal("original adequate ledger refused", err)
	}
	closeRight()
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	request.Close()
	if a.Available() != 128<<20 || b.Available() != 128<<20 {
		t.Fatal("capture ownership leaked")
	}
}

func TestSettledCaptureGenuineForeignRequestRefusesWithoutLoss(t *testing.T) {
	for _, mode := range []foreignCaptureMode{foreignCaptureSame, foreignCaptureLocator, foreignCaptureTimestamp, foreignCapturePolicy} {
		t.Run(string(mode), func(t *testing.T) {
			s, reg, expected, _, budget := settledStateCaptureFixture(t)
			lease, err := expected.Borrow()
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			locator, at, policy := lease.Locator(), lease.At(), lease.Policy()
			switch mode {
			case foreignCaptureSame: // Same values still require the original descriptor identity.
			case foreignCaptureLocator:
				raw, err := os.ReadFile(locator)
				if err != nil {
					t.Fatal(err)
				}
				locator = filepath.Join(t.TempDir(), "state.vscdb")
				if err = os.WriteFile(locator, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case foreignCaptureTimestamp:
				at = at.Add(time.Second)
			case foreignCapturePolicy:
				policy.SkillEvidence = nativefacts.CaptureSkillMetadata
			}
			foreign, err := nativefacts.FreezeCaptureRequest(t.Context(), reg, lease.Reviewed(), locator, at, policy, budget)
			if err != nil {
				t.Fatal(err)
			}
			defer foreign.Close()
			proof, err := cursor.CaptureSettledRequest(foreign)
			if err != nil {
				t.Fatal("genuine foreign capture setup", err)
			}
			defer func() {
				if err := proof.Close(); err != nil {
					t.Error(err)
				}
			}()
			read, closeRead := s.WithReadBudget(t.Context(), budget)
			defer closeRead()
			before := stagedAuthorityBytes(t, s, reg.ArchiveSessionID)
			available := budget.Available()
			if _, err := read.SaveSettledCursorImportStage(t.Context(), reg, expected, proof); err == nil {
				t.Fatal("foreign request accepted")
			}
			assertStagedAuthorityBytes(t, s, reg.ArchiveSessionID, before)
			if budget.Available() != available {
				t.Fatal("foreign request loan charged")
			}
		})
	}
}

func TestSettledCaptureOriginalCancellationReachesLiveLoanWriter(t *testing.T) {
	s, reg, original, _, budget := settledStateCaptureFixture(t)
	lease, err := original.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := nativefacts.FreezeCaptureRequest(ctx, reg, lease.Reviewed(), lease.Locator(), lease.At(), lease.Policy(), budget)
	if err != nil {
		t.Fatal(err)
	}
	defer request.Close()
	proof, err := cursor.CaptureSettledRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := proof.Close(); err != nil {
			t.Error(err)
		}
	}()
	// The current loan caller remains live, while the real original source context cancels.
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	before := stagedAuthorityBytes(t, s, reg.ArchiveSessionID)
	reached := false
	err = proof.WithVerifiedCapture(t.Context(), reg, request, budget, func(loanCtx context.Context, _ archive.SourceBundle, _ nativefacts.SettledImportFacts) error {
		reached = true
		cancel()
		return loanCtx.Err()
	})
	if !reached || !errors.Is(err, context.Canceled) {
		t.Fatal("original cancellation masked", reached, err)
	}
	if _, err = read.SaveSettledCursorImportStage(t.Context(), reg, request, proof); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled source wrote", err)
	}
	assertStagedAuthorityBytes(t, s, reg.ArchiveSessionID, before)
}

func TestSettledCaptureAndStageKeepOriginalScopeAcrossBeginScope(t *testing.T) {
	s, reg, request, proof, budget := settledStateCaptureFixture(t)
	other := budget.Reservation()
	if !other.Reserve(1) {
		t.Fatal("adequate original handle")
	}
	other.Release(1)
	end := budget.BeginScope(512)
	defer end()
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	if _, err := read.SaveSettledCursorImportStage(t.Context(), reg, request, proof); err != nil {
		t.Fatal("state recaptured tight ambient scope", err)
	}
	closeRead()
	if budget.ScopeExceeded() {
		t.Fatal("original operation charged new scope")
	}
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	request.Close()
	end()
	if budget.Available() != 128<<20 {
		t.Fatal("captured operation leaked")
	}
}

func TestSettledCaptureRequestCloseBeforeRenameKeepsAuthority(t *testing.T) {
	s, reg, request, proof, budget := settledStateCaptureFixture(t)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	before := stagedAuthorityBytes(t, s, reg.ArchiveSessionID)
	reached := false
	read.onWriteSync = func() { reached = true; request.Close() }
	if _, err := read.SaveSettledCursorImportStage(t.Context(), reg, request, proof); err == nil {
		t.Fatal("closed request committed")
	}
	if !reached {
		t.Fatal("actual synchronized precommit boundary not reached")
	}
	assertStagedAuthorityBytes(t, s, reg.ArchiveSessionID, before)
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.Available() != 128<<20 {
		t.Fatal("revoked views leaked")
	}
}

func TestSettledCaptureProofCopiesShareTerminalClose(t *testing.T) {
	s, reg, request, proof, budget := settledStateCaptureFixture(t)
	copy := *proof
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	before := stagedAuthorityBytes(t, s, reg.ArchiveSessionID)
	if _, err := read.SaveSettledCursorImportStage(t.Context(), reg, request, &copy); err == nil {
		t.Fatal("closed copied proof accepted")
	}
	assertStagedAuthorityBytes(t, s, reg.ArchiveSessionID, before)
	if budget.Available() != 128<<20 {
		t.Fatal("copy double-close lost charges")
	}
}

func TestSettledLegacyCaptureOnlyCannotInventStageExpectation(t *testing.T) {
	s, reg, expected, _, budget := settledStateCaptureFixture(t)
	lease, err := expected.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	proof, err := cursor.CaptureSettledImport(t.Context(), lease.Locator(), reg, lease.Reviewed(), lease.At(), budget)
	if err != nil {
		t.Fatal("actual legacy capture-only setup", err)
	}
	defer func() {
		if err := proof.Close(); err != nil {
			t.Error(err)
		}
	}()
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	before := stagedAuthorityBytes(t, s, reg.ArchiveSessionID)
	if _, err = read.SaveSettledCursorImportStage(t.Context(), reg, expected, proof); err == nil {
		t.Fatal("returned proof replaced original expectation")
	}
	assertStagedAuthorityBytes(t, s, reg.ArchiveSessionID, before)
}

type foreignCaptureMode string

const (
	foreignCaptureSame      foreignCaptureMode = "same-fields"
	foreignCaptureLocator   foreignCaptureMode = "locator"
	foreignCaptureTimestamp foreignCaptureMode = "timestamp"
	foreignCapturePolicy    foreignCaptureMode = "policy"
)
