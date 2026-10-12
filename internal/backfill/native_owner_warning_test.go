package backfill

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestNativeOwnerUndoStopWarningIncludesResolvedChild(t *testing.T) {
	f := newUndoFixture(t)
	root := "/synthetic/native-owner"
	pid := f.include(root)
	batch := f.batch("2026-10-01-1", fixedNow.Add(-time.Hour), pid)
	for _, tc := range []struct {
		id     string
		native bool
	}{{"native-child", true}, {"linked-child", false}} {
		reg := archive.SessionRegistration{ArchiveSessionID: tc.id, NativeSessionID: tc.id, Harness: archive.Harness{Name: "codex"}, NativeChild: tc.native, ParentSessionID: "external-parent", ProjectRoot: root, ProjectID: pid, Origin: archive.SessionOriginDiscovery, SessionStartedAt: fixedNow, RegisteredAt: fixedNow, AdmittedAt: fixedNow}
		if err := f.store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanUndo(Environment{Home: f.home, Now: func() time.Time { return fixedNow }}, f.store, f.cfg, []Batch{batch}, batch, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.HookCapturedStopping != 1 || len(plan.ExcludeProjects) != 1 || len(plan.Sessions) != 0 {
		t.Fatalf("independent capture warning/deletion: %+v", plan)
	}
	var out bytes.Buffer
	RenderUndo(&out, plan)
	if !strings.Contains(out.String(), "1 hook-captured session") || !strings.Contains(out.String(), "stops uploading") {
		t.Fatal(out.String())
	}
}

func TestNativeOwnerUndoResumedWarningKeepsDeletionCounts(t *testing.T) {
	plan := UndoPlan{Sessions: []UndoSession{
		{Registration: archive.SessionRegistration{NativeChild: true, ParentSessionID: "external-parent"}, Resumed: true, InCurrentDestination: true},
		{Registration: archive.SessionRegistration{ParentSessionID: "external-parent"}, Resumed: true, InCurrentDestination: true},
	}}
	counts := plan.Counts()
	if counts.Resumed != 1 || counts.Subagents != 2 || counts.Sessions != 0 || counts.Deleted != 2 || counts.DeletedSessions != 0 {
		t.Fatalf("warning/deletion counts: %+v", counts)
	}
	var out bytes.Buffer
	renderUndoSessions(&out, plan, counts, func(string, ...any) {})
	if !strings.Contains(out.String(), "1 session resumed since the import") {
		t.Fatal(out.String())
	}
	plan.Sessions[0].Resumed = false
	if plan.Counts().Resumed != 0 {
		t.Fatal("linked child became independent warning owner")
	}
}
