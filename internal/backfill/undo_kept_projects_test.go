package backfill

import (
	"bytes"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"testing"
	"time"
)

// Regression: 2026-09 review B-22. A adds P; undoing A keeps P for B; undoing
// B excludes P; setup includes P again; import C, which did not add P, has
// sessions there. A's leftover "kept" record used to let C's undo take P over
// and exclude it, stopping hook capture of a project the person included.
// Once any undo has excluded a project, no undo excludes it again.
func TestUndoNeverExcludesAProjectSetupIncludedAgain(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{false, true} {
		f := newUndoFixture(t)
		root := "/work/p"
		pid := f.include(root) // included again by setup after B's undo
		t0 := fixedNow.Add(-72 * time.Hour)
		undone := t0.Add(24 * time.Hour)
		a := f.batch("2026-09-20-1", t0, pid)
		a.UndoneAt, a.ProjectsKept = &undone, []string{pid}
		if recorded {
			a.ProjectsKeptFor = map[string][]string{pid: {"2026-09-20-2"}}
		}
		b := f.batch("2026-09-20-2", t0.Add(time.Hour))
		b.UndoneAt, b.ProjectsExcluded = &undone, []string{pid}
		c := f.batch("2026-09-23-1", fixedNow.Add(-time.Hour))
		f.register("c-session", root, c.ID, fixedNow.Add(-time.Hour+30*time.Second))
		f.register("hook-session", root, "", fixedNow.Add(-10*time.Minute))
		plan, err := PlanUndo(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}, f.store, f.cfg, []Batch{a, b, c}, c, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.ExcludeProjects) != 0 || len(plan.KeepProjects) != 0 || plan.HookCapturedStopping != 0 {
			t.Fatalf("recorded=%v: exclude %v keep %v hook-captured stopping %d", recorded, plan.ExcludeProjects, plan.KeepProjects, plan.HookCapturedStopping)
		}
	}
}

// Adjacent to B-22: the import that added P is undone after another import's
// undo excluded P and setup included it again. P is the person's now.
func TestUndoOfTheAddingImportLeavesAProjectAnotherUndoExcluded(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	root := "/work/p"
	pid := f.include(root)
	t0 := fixedNow.Add(-72 * time.Hour)
	undone := t0.Add(24 * time.Hour)
	a := f.batch("2026-09-20-1", t0, pid)
	f.register("a-session", root, a.ID, t0.Add(30*time.Second))
	x := f.batch("2026-09-20-2", t0.Add(time.Hour))
	x.UndoneAt, x.ProjectsExcluded = &undone, []string{pid}
	plan, err := PlanUndo(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}, f.store, f.cfg, []Batch{a, x}, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExcludeProjects) != 0 || len(plan.Sessions) != 1 {
		t.Fatalf("exclude %v, sessions %d", plan.ExcludeProjects, len(plan.Sessions))
	}
}

// B-22: undo records the imports it keeps a project for, and only their
// undos take it over; an import made after the keep that merely has
// sessions there does not. The plan lists a taken-over project apart from
// the ones the import added.
func TestUndoTakesOverOnlyForTheImportsAProjectWasKeptFor(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	root := "/work/p"
	pid := f.include(root)
	env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}
	t0 := fixedNow.Add(-72 * time.Hour)
	a := f.batch("2026-09-20-1", t0, pid)
	b := f.batch("2026-09-20-2", t0.Add(time.Hour))
	f.register("b-session", root, b.ID, b.StartedAt.Add(30*time.Second))

	// Undo A: P stays, kept for B.
	plan, err := PlanUndo(env, f.store, f.cfg, []Batch{a, b}, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeepProjects) != 1 || len(plan.ExcludeProjects) != 0 {
		t.Fatalf("undo A: keep %v exclude %v", plan.KeepProjects, plan.ExcludeProjects)
	}
	undoneA := t0.Add(2 * time.Hour)
	a.UndoneAt = &undoneA
	a.RecordKept(plan.KeepProjects)
	if got := a.ProjectsKeptFor[pid]; len(got) != 1 || got[0] != b.ID {
		t.Fatalf("kept for %v", got)
	}

	// C imports into P afterwards; it was not one A's undo kept P for.
	c := f.batch("2026-09-22-1", fixedNow.Add(-24*time.Hour))
	f.register("c-session", root, c.ID, c.StartedAt.Add(30*time.Second))
	batches := []Batch{a, b, c}
	plan, err = PlanUndo(env, f.store, f.cfg, batches, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeepProjects)+len(plan.ExcludeProjects) != 0 {
		t.Fatalf("undo C took P over: keep %v exclude %v", plan.KeepProjects, plan.ExcludeProjects)
	}

	// B takes P over from A, and keeps it for C, labelled as taken over.
	plan, err = PlanUndo(env, f.store, f.cfg, batches, b, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeepProjects) != 1 || strings.Join(plan.TakenOver[pid], ",") != a.ID {
		t.Fatalf("undo B: keep %v taken over %v", plan.KeepProjects, plan.TakenOver)
	}
	var out bytes.Buffer
	RenderUndo(&out, plan)
	if want := "left included by the undo of import " + a.ID; !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "added or took over") {
		t.Fatalf("plan lacks %q:\n%s", want, out.String())
	}
	undoneB := fixedNow.Add(-time.Hour)
	batches[1].UndoneAt = &undoneB
	batches[1].RecordKept(plan.KeepProjects)
	for _, s := range plan.Sessions {
		if _, err := f.store.ForgetIdleSession(s.Registration.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(s.Registration.Harness.Name)), NativeID: s.Registration.NativeSessionID}, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Now C's undo takes P over from B, and excludes it: nothing else is there.
	plan, err = PlanUndo(env, f.store, f.cfg, batches, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExcludeProjects) != 1 || strings.Join(plan.TakenOver[pid], ",") != b.ID {
		t.Fatalf("undo C: exclude %v taken over %v", plan.ExcludeProjects, plan.TakenOver)
	}
	out.Reset()
	RenderUndo(&out, plan)
	text := out.String()
	if !strings.Contains(text, "an earlier undo left included for this import's sessions") || strings.Contains(text, "the import added is excluded") ||
		!strings.Contains(text, root+" (left included by the undo of import "+b.ID+")") {
		t.Fatalf("taken-over project not listed apart:\n%s", text)
	}
}
