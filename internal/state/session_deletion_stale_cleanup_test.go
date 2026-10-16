package state

import (
	"testing"
	"time"
)

func TestStaleCancelledCleanupCannotUnlinkNewIntent(t *testing.T) {
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
	cancelled, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := s.RemoveRestoredDeletionStages(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	newer, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, p.MetadataBytes, old.At.Add(time.Hour), req.Token)
	if err != nil || newer.Phase != DeletionPrepared {
		t.Fatal("new intent", newer.Phase, err)
	}
	staleErr := s.removeDeletionStages(cancelled)
	if _, found, err := s.LoadSessionDeletion(reg); err != nil || !found {
		t.Fatal("stale cleanup damaged newer intent", found, err, "cleanup returned", staleErr)
	}
	if staleErr == nil {
		t.Fatal("stale cleanup acquired newer intent")
	}
}
