package state

import (
	"bytes"
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleDeletionSourceCannotStageIntoNewIntent(t *testing.T) {
	t.Parallel()
	s, reg, p, initial := interruptedDeletionPreparation(t, RemovalReasonRetention, false)
	a, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, p.MetadataBytes, initial.At, "")
	if err != nil {
		t.Fatal(err)
	}
	b := publicationFixture(t, "33333333-3333-4333-8333-333333333333", p.Bundle.Capture.CapturedAt.Add(time.Hour))
	if b.SourceSHA256 == p.SourceSHA256 {
		t.Fatal("source union must differ")
	}
	var intentBefore, requestBefore []byte
	var token string
	// Capture the same closed current intent as the public precheck, then model
	// suspension before its guarded producer callback.
	captured, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || captured.Phase != DeletionPrepared {
		t.Fatal(found, err)
	}
	func() {
		if err := s.SaveRequest(reg.ArchiveSessionID, "late-hook", a.At.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		req, found, err := s.LoadRequest(reg.ArchiveSessionID)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		token = req.Token
		if err := s.CancelUnstartedDeletion(reg, req); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveRestoredDeletionStages(reg.ArchiveSessionID); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		observed := deletionBoundaryCancellation{Context: ctx, path: filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)), cancel: cancel}
		scoped, closeScope := s.WithReadBudget(observed, agentapi.NewNativeReadBudget(agentapi.PassReadBudget))
		defer closeScope()
		if _, err := scoped.PrepareSessionDeletion(reg, RemovalReasonRetention, b.MetadataBytes, a.At.Add(time.Hour), token); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		intentBefore, err = os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
		if err != nil {
			t.Fatal(err)
		}
		requestBefore, err = os.ReadFile(s.requestPath(reg.ArchiveSessionID))
		if err != nil {
			t.Fatal(err)
		}
	}()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	staleErr := config.WithDeletionProtection(s.home, func(g config.DurableStorageGuard) error {
		return scoped.stageDeletionSourceGuarded(g, captured, p.SourceReference(), p.SourceBytes)
	})
	closeScope()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("stale source leaked read owner", used)
	}
	if raw, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID))); err != nil || !bytes.Equal(raw, intentBefore) {
		t.Fatal("new intent changed", err)
	}
	if raw, err := os.ReadFile(s.requestPath(reg.ArchiveSessionID)); err != nil || !bytes.Equal(raw, requestBefore) {
		t.Fatal("new request changed", err)
	}
	if _, err := os.Stat(filepath.Join(s.home, deletionDirectory, reg.ArchiveSessionID, "sources", p.SourceSHA256+".gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale source appeared under foreign union", err, "stage returned", staleErr)
	}
	if staleErr == nil {
		t.Fatal("stale source stage acquired new intent")
	}
	newer, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, b.MetadataBytes, a.At.Add(time.Hour), token)
	if err != nil {
		t.Fatal("new exact retry stranded", err)
	}
	if err := s.StageDeletionSource(newer, b.SourceReference(), b.SourceBytes); err != nil {
		t.Fatal("new exact union could not stage", err)
	}
}
