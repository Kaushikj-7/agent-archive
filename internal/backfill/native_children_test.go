package backfill

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNativeChildImportedNowBelongsToNewBatchAndUndoRetainsOldParent(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	at := fixedNow.UTC()
	cfg := config.Config{ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: at}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parentID := "00000000-0000-0000-0000-000000000001"
	childID := "00000000-0000-0000-0000-000000000002"
	parentPath := filepath.Join(root, "parent.jsonl")
	childPath := filepath.Join(root, "child.jsonl")
	for _, path := range []string{parentPath, childPath} {
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := Registration{Home: home, Store: store, Batch: "2026-09-22-1", AdmittedAt: at}
	oldResult, err := old.Run([]Candidate{{Harness: "codex", NativeSessionID: parentID, TranscriptPath: parentPath, ProjectRoot: root, StartedAt: at.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}})
	if err != nil || len(oldResult.Sessions) != 1 {
		t.Fatalf("parent import %+v %v", oldResult, err)
	}
	newer := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: at.Add(time.Minute)}
	result, err := newer.Run([]Candidate{{Harness: "codex", NativeSessionID: childID, NativeChild: true, ParentNativeID: parentID, RootNativeID: parentID, NativeHome: root, TranscriptPath: childPath, ProjectRoot: root, StartedAt: at.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}})
	if err != nil || len(result.Sessions) != 0 || len(result.Subagents) != 1 {
		t.Fatalf("native import result %+v %v", result, err)
	}
	child, found, err := store.LoadRegistration(result.Subagents[0])
	if err != nil || !found || child.NativeSessionID != childID || !child.InBatch(newer.Batch) || child.InBatch(old.Batch) || !child.HookObservedAt.IsZero() {
		t.Fatalf("child membership %+v %v", child, err)
	}
	batch := Batch{ID: newer.Batch}
	if err := batch.Reconcile(store); err != nil || len(batch.Subagents) != 1 || len(batch.Sessions) != 0 {
		t.Fatalf("reconciliation %+v %v", batch, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	selected := selectUndoRegistrations(regs, newer.Batch, map[string]bool{root: true})
	if len(selected) != 1 || selected[0].ArchiveSessionID != child.ArchiveSessionID {
		t.Fatalf("undo leaked into old batch: %+v", selected)
	}
}

func TestNestedUndoOrdersDescendantsAndRejectsCycles(t *testing.T) {
	sessions := []UndoSession{{Registration: archive.SessionRegistration{ArchiveSessionID: "root"}}, {Registration: archive.SessionRegistration{ArchiveSessionID: "child", ParentSessionID: "root"}}, {Registration: archive.SessionRegistration{ArchiveSessionID: "grandchild", ParentSessionID: "child"}}}
	ordered, err := orderUndoChildrenFirst(sessions)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"grandchild", "child", "root"} {
		if ordered[i].Registration.ArchiveSessionID != id {
			t.Fatalf("delete order %+v", ordered)
		}
	}
	ordered[2].Registration.ParentSessionID = "grandchild"
	if _, err := orderUndoChildrenFirst(ordered); err == nil {
		t.Fatal("cyclic ownership accepted for deletion")
	}
}

func TestNestedUndoRejectsOverdepthRegardlessOfMemoizedParentOrder(t *testing.T) {
	var sessions []UndoSession
	for i := range 66 {
		parent := ""
		if i > 0 {
			parent = strconv.Itoa(i - 1)
		}
		reg := archive.SessionRegistration{ArchiveSessionID: strconv.Itoa(i), ParentSessionID: parent}
		sessions = append(sessions, UndoSession{Registration: reg})
	}
	for range 50 {
		if _, err := orderUndoChildrenFirst(sessions); err == nil {
			t.Fatal("overdepth relationship accepted")
		}
	}
}

func TestUndoProjectsPreservesExternalNativeChildAndTransfersResponsibility(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(strconv.FormatBool(resolved), func(t *testing.T) {
			f := newUndoFixture(t)
			root := "/synthetic/project"
			pid := f.include(root)
			older := f.batch("2026-09-20-1", fixedNow.Add(-72*time.Hour), pid)
			newer := f.batch("2026-09-21-1", fixedNow.Add(-48*time.Hour))
			parent := ""
			if resolved {
				parent = "external-parent"
			}
			child := archive.SessionRegistration{ArchiveSessionID: "native-child", NativeSessionID: "00000000-0000-0000-0000-000000000002", Harness: archive.Harness{Name: "codex"}, NativeChild: true, ParentSessionID: parent, ProjectRoot: root, ProjectID: pid, TranscriptPath: "/synthetic/child.jsonl", SessionStartedAt: newer.StartedAt, RegisteredAt: newer.StartedAt, AdmittedAt: newer.StartedAt, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(newer.ID)}
			if err := f.store.SaveRegistration(child); err != nil {
				t.Fatal(err)
			}
			env := Environment{Home: f.home, Now: func() time.Time { return fixedNow }}
			plan, err := planUndoForTesting(t, env, f.store, f.cfg, []Batch{older, newer}, older, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.ExcludeProjects) != 0 || len(plan.KeepProjects) != 1 || plan.KeepProjects[0].Sessions != 1 || len(plan.Sessions) != 0 {
				t.Fatalf("external native child lost project permission: %+v", plan)
			}
			plan.ApplyToConfig(&f.cfg)
			if !f.cfg.Archive.Projects[0].Included {
				t.Fatal("undo disabled external child's project")
			}
			older.RecordKept(plan.KeepProjects)
			undone := fixedNow.Add(-time.Hour)
			older.UndoneAt = &undone
			plan, err = planUndoForTesting(t, env, f.store, f.cfg, []Batch{older, newer}, newer, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.ExcludeProjects) != 1 || len(plan.Sessions) != 1 || len(plan.TakenOver[pid]) != 1 || plan.TakenOver[pid][0] != older.ID {
				t.Fatalf("native child batch could not finish kept project cleanup: %+v", plan)
			}
		})
	}
}

func TestUndoRefusesReusedBatchWithEarlierIndependentNativeChild(t *testing.T) {
	f := newUndoFixture(t)
	root := "/synthetic/project"
	f.include(root)
	batch := f.batch("2026-09-23-1", fixedNow.Add(time.Hour))
	child := archive.SessionRegistration{ArchiveSessionID: "earlier-native-child", NativeSessionID: "00000000-0000-0000-0000-000000000002", Harness: archive.Harness{Name: "codex"}, NativeChild: true, ParentSessionID: "external-parent", ProjectRoot: root, ProjectID: archive.ProjectID(root), TranscriptPath: "/synthetic/child.jsonl", SessionStartedAt: fixedNow, RegisteredAt: fixedNow, AdmittedAt: fixedNow, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(batch.ID)}
	if err := f.store.SaveRegistration(child); err != nil {
		t.Fatal(err)
	}
	_, err := planUndoForTesting(t, Environment{Home: f.home}, f.store, f.cfg, []Batch{batch}, batch, "")
	var shared *SharedBatchIDError
	if !errors.As(err, &shared) || shared.Sessions != 1 {
		t.Fatalf("reused batch allowed deletion of earlier independent child: %v", err)
	}
}

func TestUndoRetentionWarningsIncludeIndependentNativeChildren(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	root := "/synthetic/retention"
	f.include(root)
	f.cfg.RetentionDays = 365
	batch := f.batch("2026-09-23-1", fixedNow.Add(-time.Hour))
	batch.Retention = &RetentionChange{From: 30, To: 365}
	day := 24 * time.Hour
	for _, id := range []string{"resolved", "unresolved", "dependent"} {
		at := fixedNow.Add(-100 * day)
		parent, harness := "external-parent", "codex"
		if id == "unresolved" {
			parent = ""
		}
		if id == "dependent" {
			harness = "claude"
		}
		reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: id, Harness: archive.Harness{Name: harness}, ProjectID: archive.ProjectID(root), ProjectRoot: root, TranscriptPath: "/synthetic/" + id + ".jsonl", SessionStartedAt: at, RegisteredAt: at, AdmittedAt: at, NativeChild: id != "dependent", ParentSessionID: parent}
		if err := f.store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := planUndoForTesting(t, Environment{Home: f.home, Now: func() time.Time { return fixedNow }}, f.store, f.cfg, []Batch{batch}, batch, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.RetentionDeletes != 2 {
		t.Fatal("shorter retention omitted independent resolved native child", plan.RetentionDeletes)
	}
	fewer := plan
	fewer.RetentionDeletes = 1
	if !plan.Grew(fewer) {
		t.Fatal("new resolved-child retention loss bypassed confirmation recheck")
	}
	// Retention uses the independent child's capture clock, then excludes
	// selected undo removals and other destinations from the warning.
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "resolved", Capture: archive.SourceCapture{CapturedAt: fixedNow.Add(-7 * day)}}
	if err := statetest.SavePublished(f.store, "resolved", bundle, fixedNow, state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	regs, err := f.store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	count, err := retentionDeletes(f.store, f.cfg, regs, nil, *batch.Retention, fixedNow)
	if err != nil || count != 1 {
		t.Fatal("resolved child ignored its own recent capture", count, err)
	}
	for _, reg := range regs {
		if reg.ArchiveSessionID == "unresolved" {
			count, err = retentionDeletes(f.store, f.cfg, regs, []UndoSession{{Registration: reg}}, *batch.Retention, fixedNow)
			if err != nil || count != 0 {
				t.Fatal("selected native child counted twice as retention loss", count, err)
			}
		}
	}
	for i := range regs {
		regs[i].DestinationID = "other-destination"
	}
	count, err = retentionDeletes(f.store, f.cfg, regs, nil, *batch.Retention, fixedNow)
	if err != nil || count != 0 {
		t.Fatal("retention warning crossed destinations", count, err)
	}
}
