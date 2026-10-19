package collector

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestNativeReconciliationPreservesDeletionOwnerUntilTerminalCleanup(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	parent := nativeLinkRegistration("parent", "00000000-0000-0000-0000-000000000001", "", home, at)
	child := nativeLinkRegistration("child", "00000000-0000-0000-0000-000000000002", parent.NativeSessionID, home, at)
	if err := local.SaveRegistration(child); err != nil {
		t.Fatal(err)
	}
	if _, err := local.PrepareSessionDeletion(child, state.RemovalReasonRetention, nil, at.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	p := &pass{local: local, registrations: []archive.SessionRegistration{child, parent}, now: at.Add(2 * time.Hour)}
	p.reconcileNativeLinks()
	current, found, err := local.LoadRegistration(child.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if current.ParentSessionID != child.ParentSessionID {
		t.Errorf("reconciliation retargeted active deletion owner to %q", current.ParentSessionID)
	}
	if _, found, err := local.LoadSessionDeletion(current); err != nil || !found {
		t.Errorf("actual registration can no longer resume/cancel its own deletion: found=%v err=%v", found, err)
	}

	if p.result.NativeLinkPendingReasons["session_removal_pending"] != 1 {
		t.Fatal("deferred relationship was not actionable", p.result)
	}
	if err := local.SaveRequest(child.ArchiveSessionID, "stop", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	req, found, err := local.LoadRequest(child.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := local.CancelUnstartedDeletion(child, req); err != nil {
		t.Fatal(err)
	}
	if err := local.RemoveRestoredDeletionStages(child.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	p.reconcileNativeLinks()
	current, found, err = local.LoadRegistration(child.ArchiveSessionID)
	if err != nil || !found || current.ParentSessionID != parent.ArchiveSessionID || current.NativeLinkVersion != 1 {
		t.Fatal("terminal cleanup did not release relationship reconciliation", current, found, err)
	}
}
