package state

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"path/filepath"
	"testing"
	"time"
)

func TestStalePreparingRetryCannotStageIntoNewIntent(t *testing.T) {
	s, reg, p, old := interruptedDeletionPreparation(t, RemovalReasonRetention, false)
	if err := s.SaveRequest(reg.ArchiveSessionID, "late-hook", old.At.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := s.CancelUnstartedDeletion(reg, req); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRestoredDeletionStages(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.Title = "new selecting metadata"
	nextRaw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := deletionBoundaryCancellation{Context: ctx, path: filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)), cancel: cancel}
	scoped, closeScope := s.WithReadBudget(observed, agentapi.NewNativeReadBudget(agentapi.PassReadBudget))
	defer closeScope()
	if _, err := scoped.PrepareSessionDeletion(reg, RemovalReasonRetention, nextRaw, old.At.Add(time.Hour), req.Token); !errors.Is(err, context.Canceled) {
		t.Fatal("new intent boundary", err)
	}
	newer, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || newer.Phase != DeletionPreparing {
		t.Fatal("new intent before stale attempt", newer.Phase, found, err)
	}
	if _, err := s.finishDeletionPreparation(old, p.MetadataBytes); err == nil {
		t.Fatal("stale completion", err)
	}
	if _, found, err := s.LoadSessionDeletion(reg); err != nil || !found {
		t.Fatal("stale attempt damaged newer intent", found, err)
	}
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, nextRaw, newer.At, req.Token); err != nil {
		t.Fatal("new exact retry stranded", err)
	}
}
