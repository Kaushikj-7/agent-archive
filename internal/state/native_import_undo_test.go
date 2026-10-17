package state

import (
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
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

// Model a stale ordinary snapshot overtaken after the removal record is staged,
// before the locked forget. Only the actual current owner can decide retirement.
func TestNativeImportUndoForgetRechecksOwnerAtCommit(t *testing.T) {
	source, protected, _ := releasedNativeImportFixture(t)
	s := newTestStore(t)
	cfg, _, err := config.Load(source.Home())
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	ordinary := registration(t)
	ordinary.ArchiveSessionID = protected.ArchiveSessionID
	if err := s.SaveRegistration(ordinary); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(ordinary.ArchiveSessionID, "feedback", time.Now()); err != nil {
		t.Fatal(err)
	}
	request, _, err := s.LoadRequest(ordinary.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	s.afterRemovalRecord = func() {
		changed = true
		if err := local.Write(s.registrationPath(protected.ArchiveSessionID), protected); err != nil {
			t.Fatal(err)
		}
	}
	key := agentmeta.SessionKey{Agent: agentmeta.ID(ordinary.Harness.Name), NativeID: ordinary.NativeSessionID}
	if _, err := s.ForgetIdleSession(ordinary.ArchiveSessionID, key, false, &RemovalRecord{Harness: ordinary.Harness.Name, Reason: RemovalReasonUndo, At: time.Now()}); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
		t.Fatalf("stale forget: %v", err)
	}
	if !changed {
		t.Fatal("did not reach precommit interleaving")
	}
	owner, found, err := s.LoadRegistration(protected.ArchiveSessionID)
	if err != nil || !found || owner.NativeImport == nil {
		t.Fatal("current owner lost", err)
	}
	current, found, err := s.LoadRequest(protected.ArchiveSessionID)
	if err != nil || !found || current.Token != request.Token {
		t.Fatal("independent request changed", err)
	}
}

func TestNativeImportUndoRejectsUnsafeLockIdentity(t *testing.T) {
	s := newTestStore(t)
	if err := s.CheckSessionUndo(archive.SessionRegistration{ArchiveSessionID: "../outside"}); err == nil {
		t.Fatal("unsafe request-lock identity accepted")
	}
}
