package state

import (
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestNativeImportUndoUsesCurrentOwner(t *testing.T) {
	s, reg, _ := releasedNativeImportFixture(t)
	stale := reg
	stale.NativeImport = nil
	for _, snapshot := range []archive.SessionRegistration{reg, stale} {
		if err := s.CheckSessionUndo(snapshot); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
			t.Fatalf("undo owner check: %v", err)
		}
		if _, err := s.PrepareSessionDeletion(snapshot, RemovalReasonUndo, nil, time.Now(), ""); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
			t.Fatalf("prepare undo: %v", err)
		}
	}
	key := agentmeta.SessionKey{Agent: agentmeta.ID(reg.Harness.Name), NativeID: reg.NativeSessionID}
	if _, err := s.ForgetIdleSession(reg.ArchiveSessionID, key, false, &RemovalRecord{Harness: reg.Harness.Name, Reason: RemovalReasonUndo, At: time.Now()}); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
		t.Fatalf("forget undo: %v", err)
	}
	if err := s.SaveRequest(reg.ArchiveSessionID, "feedback", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.checkSessionUndoLocked(reg.ArchiveSessionID); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
		t.Fatalf("final current-owner guard: %v", err)
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("owner lost", err)
	}
	if _, found, err := s.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("later request lost", err)
	}
}

func TestNativeImportUndoControlCannotReplay(t *testing.T) {
	s, reg, _ := releasedNativeImportFixture(t)
	j := SessionDeletion{Version: 1, Registration: reg, OwnerSHA256: deletionOwner(reg), At: time.Now(), Reason: RemovalReasonUndo}
	j.SHA256 = deletionSHA(j)
	if err := j.validateOwner(reg.ArchiveSessionID); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
		t.Fatalf("undo journal accepted: %v", err)
	}
	if _, err := s.AdvanceSessionDeletion(j, DeletionDeleting); err == nil {
		t.Fatal("undo replay advanced")
	}
	j.Reason = RemovalReasonRetention
	j.SHA256 = deletionSHA(j)
	if err := j.validateOwner(reg.ArchiveSessionID); err != nil {
		t.Fatalf("retention owner refused: %v", err)
	}
}
