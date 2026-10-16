package backfill

import (
	"bytes"
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// summaryFor is the plan's row for root.
func summaryFor(t *testing.T, p Plan, root string) ProjectSummary {
	t.Helper()
	for _, s := range p.Projects() {
		if s.Root == root {
			return s
		}
	}
	t.Fatalf("no row for %s in %+v", root, p.Projects())
	return ProjectSummary{}
}

// Regression: 2026-09 review B-20. A session run once in a plain parent
// folder (~/code) made the import add that folder, after which hooks
// captured every new session in any unconfigured repository under it,
// without a word in the plan. The plan now says the folder captures its
// subfolders, and finds what inside it must stay out of capture: nested
// repositories and linked worktrees at any depth, and app workspace
// folders. Symlinks, configured projects, projects the same import adds,
// and dependency folders are not looked into.
func TestPlanKeepsNestedRepositoriesOutOfAnAddedFolder(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	code := tr.mkdir("home/code")
	tr.write("home/"+claudeFile("s", "parent"), claudeTranscript("parent", code, fixedNow.Add(-48*time.Hour)))
	secret := tr.repo("home/code/secret-repo")
	deep := tr.repo("home/code/clients/acme/app")
	tr.repo("home/elsewhere/main")
	linked := tr.worktree("home/elsewhere/main", "home/code/feature-wt", "wt")
	tr.mkdir("home/code/notes/drafts")
	tr.repo("home/code/node_modules/pkg")
	if err := os.Symlink(tr.path("home/elsewhere/main"), filepath.Join(code, "linked-main")); err != nil {
		t.Fatal(err)
	}
	configured := tr.repo("home/code/configured")
	added := tr.repo("home/code/added")
	tr.write("home/"+claudeFile("s", "in-added"), claudeTranscript("in-added", added, fixedNow.Add(-24*time.Hour)))
	cfg := config.Config{RetentionDays: 90, Archive: archive.Config{Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(configured), Root: configured, Included: false},
	}}}

	p := plan(t, tr.env(), nil, cfg, Filters{})
	s := summaryFor(t, p, code)
	if !s.CapturesSubfolders() || !s.NestedComplete {
		t.Fatalf("row %+v", s)
	}
	want := []string{deep, linked, secret}
	if strings.Join(s.KeptOut, "\n") != strings.Join(want, "\n") {
		t.Fatalf("kept out\n%s\nwant\n%s", strings.Join(s.KeptOut, "\n"), strings.Join(want, "\n"))
	}
	if r := summaryFor(t, p, added); r.CapturesSubfolders() || len(r.KeptOut) != 0 {
		t.Fatalf("repository row %+v", r)
	}

	var out bytes.Buffer
	RenderText(&out, p)
	for _, line := range []string{
		"Every future session in a folder under it that isn't in a nearer project",
		"3 folders inside it, repositories or app folders, are added as excluded",
		"    ~/code/secret-repo",
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("plan lacks %q:\n%s", line, out.String())
		}
	}

	// The import adds the folder included and each kept-out folder
	// excluded, and the batch records them.
	admitted := fixedNow.UTC()
	changes, err := ApplyToConfig(&cfg, p, admitted)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.KeptOut) != 3 {
		t.Fatalf("changes %+v", changes)
	}
	for _, folder := range want {
		found := false
		for _, project := range cfg.Archive.Projects {
			if project.Root == folder {
				found = !project.Included
			}
		}
		if !found {
			t.Fatalf("%s not added excluded: %+v", folder, cfg.Archive.Projects)
		}
	}
	var b Batch
	b.AddChanges(changes)
	if len(b.ProjectsKeptOut) != 3 {
		t.Fatalf("batch %+v", b)
	}
}

// Adjacent to B-20: the desktop apps' workspace folders and temporary
// directories inside an added folder are kept out too, and a temporary
// directory added with --include-temp keeps its repositories out.
func TestPlanKeepsAppFoldersAndTempDirsOut(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	docs := tr.mkdir("home/Documents")
	codexWorkspaces := tr.mkdir("home/Documents/Codex/2026-09-20/w")
	tr.write("home/"+claudeFile("s", "docs"), claudeTranscript("docs", docs, fixedNow.Add(-48*time.Hour)))
	run := tr.mkdir("tmp/run-1")
	repoInTemp := tr.repo("tmp/run-1/checkout")
	tr.write("home/"+claudeFile("s", "tmp"), claudeTranscript("tmp", run, fixedNow.Add(-48*time.Hour)))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{IncludeTemp: true})
	if s := summaryFor(t, p, docs); strings.Join(s.KeptOut, ",") != filepath.Dir(filepath.Dir(codexWorkspaces)) {
		t.Fatalf("Documents keeps out %v", s.KeptOut)
	}
	if s := summaryFor(t, p, run); !s.CapturesSubfolders() || strings.Join(s.KeptOut, ",") != repoInTemp {
		t.Fatalf("temporary row %+v", s)
	}
}

// Regression (PR #53 review): a plain folder inside home keeps nothing out
// when the same import adds home with --include-home, the explicit choice
// to capture everything under home; keeping its repositories out would
// hide sessions the person asked for. Without it they are kept out. The
// look stops when the plan is cancelled.
func TestPlanKeepsNothingOutUnderAnIncludedHome(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	home := tr.path("home")
	code := tr.mkdir("home/code")
	secret := tr.repo("home/code/secret-repo")
	tr.write("home/"+claudeFile("s", "home"), claudeTranscript("home", home, fixedNow.Add(-72*time.Hour)))
	tr.write("home/"+claudeFile("s", "code"), claudeTranscript("code", code, fixedNow.Add(-48*time.Hour)))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{IncludeHome: true})
	if s := summaryFor(t, p, code); len(s.KeptOut) != 0 {
		t.Fatalf("--include-home still keeps out %v", s.KeptOut)
	}
	p = plan(t, tr.env(), nil, config.Config{}, Filters{})
	if s := summaryFor(t, p, code); strings.Join(s.KeptOut, ",") != secret {
		t.Fatalf("without --include-home: kept out %v", s.KeptOut)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := planNested(ctx, newResolver(tr.env(), config.Config{}, Filters{}), &p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled look: %v", err)
	}
}

// B-20 hardening: when not every folder can be looked in, the plan says so
// rather than claim the folder's repositories are all kept out.
func TestPlanSaysWhenNestedCheckIsIncomplete(t *testing.T) {
	tr := newTree(t)
	code := tr.mkdir("home/code")
	for _, d := range []string{"a/b/c", "d/e", "f"} {
		tr.mkdir("home/code/" + d)
	}
	tr.write("home/"+claudeFile("s", "parent"), claudeTranscript("parent", code, fixedNow.Add(-48*time.Hour)))
	saved := nestedScanBudget
	nestedScanBudget = 2
	t.Cleanup(func() { nestedScanBudget = saved })
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if s := summaryFor(t, p, code); s.NestedComplete {
		t.Fatalf("row %+v", s)
	}
	var out bytes.Buffer
	RenderText(&out, p)
	if !strings.Contains(out.String(), "Not every folder inside it could be checked for repositories") || !strings.Contains(out.String(), "repository in it that was not found is captured too. To keep capture out\n  of this folder, exclude it later in agent-archive setup") {
		t.Fatalf("plan:\n%s", out.String())
	}
}

// B-20: undoing the import that added a folder removes the kept-out
// entries it added, once nothing included contains them; while another
// import's sessions keep the folder included, they stay.
func TestUndoRemovesKeptOutEntriesOnlyWhenNothingContainsThem(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}
	code := "/work/code"
	codeID := f.include(code)
	secretID := archive.ProjectID(code + "/secret")
	f.cfg.Archive.Projects = append(f.cfg.Archive.Projects, archive.ProjectActivation{ProjectID: secretID, Root: code + "/secret", Included: false})
	a := f.batch("2026-09-22-1", fixedNow.Add(-24*time.Hour), codeID)
	a.ProjectsKeptOut = []string{secretID}
	f.register("a-session", code, a.ID, a.StartedAt.Add(time.Minute))
	b := f.batch("2026-09-23-1", fixedNow.Add(-time.Hour))
	f.register("b-session", code, b.ID, b.StartedAt.Add(time.Minute))

	plan, err := PlanUndo(env, f.store, f.cfg, []Batch{a, b}, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeepProjects) != 1 || len(plan.RemoveKeptOut) != 0 {
		t.Fatalf("kept %v, remove kept-out %v", plan.KeepProjects, plan.RemoveKeptOut)
	}

	// Once B's sessions are gone, A's undo excludes the folder and removes
	// the entry.
	id, _, err := f.store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "b-session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ForgetIdleSession(id, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "b-session"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{a, b}, a, ""); err != nil {
		t.Fatal(err)
	}
	if len(plan.ExcludeProjects) != 1 || len(plan.RemoveKeptOut) != 1 || plan.RemoveKeptOut[0].ProjectID != secretID {
		t.Fatalf("exclude %v, remove kept-out %v", plan.ExcludeProjects, plan.RemoveKeptOut)
	}
	cfg := cloneConfig(t, f.cfg)
	changes := plan.ApplyToConfig(&cfg)
	if len(changes.RemovedKeptOut) != 1 || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Included {
		t.Fatalf("changes %+v, projects %+v", changes, cfg.Archive.Projects)
	}
	var out bytes.Buffer
	RenderUndo(&out, plan)
	if !strings.Contains(out.String(), "1 project the import added as excluded, to keep folders inside a project") {
		t.Fatalf("plan:\n%s", out.String())
	}

	// Regression (PR #53 review): undoing another import never removes the
	// entries of an import still in place, even while setup has the folder
	// excluded: including the folder again must keep them out.
	c := f.batch("2026-09-24-1", fixedNow.Add(-30*time.Minute))
	f.cfg.Archive.Projects[0].Included = false
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{a, b, c}, c, ""); err != nil || len(plan.RemoveKeptOut) != 0 {
		t.Fatalf("undoing another import removed a live import's kept-out entry: %v %v", plan.RemoveKeptOut, err)
	}
	undoneAt := fixedNow
	a.UndoneAt = &undoneAt
	// Second review of PR #53: an import marked undone that still has a
	// session registered (a --project undo of another of its projects, or
	// a removal that failed) is still in place, so its entries stay.
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{a, b, c}, c, ""); err != nil || len(plan.RemoveKeptOut) != 0 {
		t.Fatalf("a partly undone import's kept-out entry was removed: %v %v", plan.RemoveKeptOut, err)
	}
	aSession, _, err := f.store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "a-session"})
	if err != nil {
		t.Fatal(err)
	}
	aReg, _, err := f.store.LoadRegistration(aSession)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ForgetIdleSession(aSession, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "a-session"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{a, b, c}, c, ""); err != nil || len(plan.RemoveKeptOut) != 1 {
		t.Fatalf("an undone import's kept-out entry stayed: %v %v", plan.RemoveKeptOut, err)
	}
	if len(plan.Sessions) != 0 || len(plan.ExcludeProjects) != 0 || len(plan.RemoveApps) != 0 || plan.RestoreRetention != nil {
		t.Fatalf("expected kept-out-only cleanup: %+v", plan)
	}
	if plan.Empty() || !plan.Grew(UndoPlan{}) {
		t.Fatalf("kept-out-only cleanup was skipped or omitted from the confirmation recheck: %+v", plan)
	}
	if err := f.store.SaveRegistration(aReg); err != nil {
		t.Fatal(err)
	}
	a.UndoneAt = nil
	f.cfg.Archive.Projects[0].Included = true

	// A kept-out entry setup has since included is the person's.
	f.cfg.Archive.Projects[1].Included = true
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{a, b}, a, ""); err != nil || len(plan.RemoveKeptOut) != 0 {
		t.Fatalf("included kept-out removed: %v %v", plan.RemoveKeptOut, err)
	}
	// So is one with a session registered in it (captured while setup had
	// it included), even once it is excluded again.
	f.register("secret-session", code+"/secret", "", fixedNow.Add(-time.Minute))
	f.cfg.Archive.Projects[0].Included, f.cfg.Archive.Projects[1].Included = false, false
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{a, b}, a, ""); err != nil || len(plan.RemoveKeptOut) != 0 {
		t.Fatalf("kept-out entry with a session removed: %v %v", plan.RemoveKeptOut, err)
	}
}
