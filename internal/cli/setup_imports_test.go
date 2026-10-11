package cli

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ImportedHarnesses is operational state backfill writes. Setup carries it
// over from the committed configuration, never from a draft, and drops an
// app once setup installs its hooks.
func TestSetupCarriesImportedHarnessesFromCommittedState(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	committed, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	// What backfill commits for apps imported without hooks.
	committed.ImportedHarnesses = []string{"claude", "cursor"}
	if err := config.Save(home, committed); err != nil {
		t.Fatal(err)
	}
	exe, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}

	// A draft saved before the import has no list; one edited by hand has a
	// different one. Neither wins.
	for _, draftList := range [][]string{nil, {"codex"}} {
		old, _, _ := config.Load(home)
		next := old
		next.ImportedHarnesses = draftList
		if err := applySetup(home, userHome, exe, old, &next, nil, env); err != nil {
			t.Fatal(err)
		}
		saved, _, _ := config.Load(home)
		if !reflect.DeepEqual(saved.ImportedHarnesses, []string{"claude", "cursor"}) {
			t.Fatalf("draft %v: imported harnesses=%v", draftList, saved.ImportedHarnesses)
		}
	}

	// Installing Claude Code's hooks moves it to Harnesses.
	old, _, _ := config.Load(home)
	next := old
	next.Harnesses = []string{"codex", "claude"}
	if err := applySetup(home, userHome, exe, old, &next, nil, env); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := config.Load(home)
	if !reflect.DeepEqual(saved.ImportedHarnesses, []string{"cursor"}) || !reflect.DeepEqual(saved.Harnesses, []string{"codex", "claude"}) {
		t.Fatalf("harnesses=%v imported=%v", saved.Harnesses, saved.ImportedHarnesses)
	}

	// A backfill that commits while setup is open is a concurrent change.
	old = saved
	concurrent := saved
	concurrent.ImportedHarnesses = append(concurrent.ImportedHarnesses, "codex")
	if err := config.Save(home, concurrent); err != nil {
		t.Fatal(err)
	}
	next = old
	if err := applySetup(home, userHome, exe, old, &next, nil, env); err == nil || !setupContainsText(err.Error(), "settings changed while setup was open") {
		t.Fatalf("err=%v", err)
	}
}

// Setup's review lists the apps that have only imported sessions, and its
// app edit step offers to stop publishing each one's imports.
func TestSetupShowsImportedOnlyAppsAndCanStopPublishingThem(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	committed, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	committed.ImportedHarnesses = []string{"claude", "cursor"}
	if err := config.Save(home, committed); err != nil {
		t.Fatal(err)
	}

	// Keep the retention, then edit apps: leave Codex as the only app, stop
	// publishing the Claude Code imports, keep the Cursor ones, and save.
	out := setupRun(t, env, "retention\n\nedit\napps\nn\nn\ny\ny\n", 0)
	first := strings.Index(out, "Claude Code, Cursor (sessions imported")
	stop := strings.Index(out, "Keep publishing Claude Code sessions imported by backfill?")
	after := strings.LastIndex(out, "Cursor (sessions imported")
	if first < 0 || stop < first || after < stop {
		t.Fatalf("review and prompts out of order:\n%s", out)
	}
	saved, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.ImportedHarnesses, []string{"cursor"}) || !reflect.DeepEqual(saved.Harnesses, []string{"codex"}) {
		t.Fatalf("harnesses=%v imported=%v", saved.Harnesses, saved.ImportedHarnesses)
	}
	claudeImport := archive.SessionRegistration{Harness: archive.Harness{Name: "claude"}, Origin: archive.SessionOriginImport, ProjectRoot: saved.Archive.Projects[0].Root, AdmittedAt: time.Now().UTC()}
	if saved.AcceptSession(claudeImport) {
		t.Fatal("Claude Code imports are still published")
	}
	claudeImport.Harness.Name = "cursor"
	if !saved.AcceptSession(claudeImport) {
		t.Fatal("Cursor imports stopped publishing")
	}

	// Without imported-only apps the review has no such line.
	saved.ImportedHarnesses = nil
	if err := config.Save(home, saved); err != nil {
		t.Fatal(err)
	}
	if out := setupRun(t, env, "retention\n\ny\n", 0); strings.Contains(out, "\n  Imported ") || strings.Contains(out, "\n* Imported ") {
		t.Fatalf("empty imported list shown:\n%s", out)
	}
}

// An import can add projects whose folders no longer exist. Setup keeps
// working afterwards: only a project the setup newly includes must exist.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
//
//lint:ignore tparallel the test is parallel, but its subtests share one fixture, in order
func TestSetupAfterImportWithMissingFolders(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	oldNotes := filepath.Join(f.userHome, "old-notes")
	env := setupTestEnv(t, f.data, f.userHome, newFakeKeychain(), backfillNow)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	setupRun(t, env, "retention\ncustom\n30\ny\n", 0)
	cfg, _, _ := config.Load(f.data)
	if cfg.RetentionDays != 30 {
		t.Fatalf("retention %d", cfg.RetentionDays)
	}
	kept := false
	for _, p := range cfg.Archive.Projects {
		kept = kept || (p.Root == oldNotes && p.Included)
	}
	if !kept {
		t.Fatal("the imported project with a missing folder was dropped")
	}

	// A missing folder the setup newly includes, or includes again after
	// excluding it, is still refused.
	executable := testExecutable(t)
	for _, tc := range []struct {
		name string
		edit func(next *config.Config)
	}{
		{"new", func(next *config.Config) {
			root := filepath.Join(f.userHome, "never-existed")
			next.Archive.Projects = append(next.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true})
		}},
		{"re-included", func(next *config.Config) {
			for i := range next.Archive.Projects {
				if next.Archive.Projects[i].Root == oldNotes {
					next.Archive.Projects[i].Included = true
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, _ := config.Load(f.data)
			for i := range old.Archive.Projects {
				if old.Archive.Projects[i].Root == oldNotes {
					old.Archive.Projects[i].Included = false
				}
			}
			if err := config.Save(f.data, old); err != nil {
				t.Fatal(err)
			}
			next := old
			next.Archive.Projects = append([]archive.ProjectActivation(nil), old.Archive.Projects...)
			tc.edit(&next)
			err := applySetup(f.data, f.userHome, executable, old, &next, nil, env)
			if err == nil || !setupContainsText(err.Error(), "no longer a directory") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// The projects backfill added are kept or dropped with one question; the
// others are still asked about one by one.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestSetupGroupsBackfilledProjects(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 4)
	var existing []archive.ProjectActivation
	backfilled := map[string]bool{}
	for i, root := range roots {
		rule := archive.ProjectActivation{ProjectID: archive.ProjectID(root.Root), Root: root.Root, Included: true}
		existing = append(existing, rule)
		if i > 0 {
			backfilled[rule.ProjectID] = true
		}
	}
	for _, tc := range []struct {
		input string
		want  int
	}{{"all\n", 4}, {"specific\n2-4\n\n", 1}} {
		var out bytes.Buffer
		projects, err := promptProjects(newPrompter(strings.NewReader(tc.input), &out), existing, backfilled, nil)
		must(t, err)
		if len(projects) != 4 || includedProjects(projects) != tc.want {
			t.Fatalf("%q %+v", tc.input, projects)
		}
		for _, p := range projects {
			if backfilled[p.ProjectID] && p.Included != (tc.want == 4) {
				t.Fatalf("import consent lost %+v", p)
			}
		}
		if !setupContainsText(out.String(), "Which projects?") {
			t.Fatal(out.String())
		}
	}
}

// Exclusions, such as those undo leaves, survive a setup edit of the
// projects, and typing an excluded project's path includes it again.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestSetupKeepsExclusions(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 3)
	var existing []archive.ProjectActivation
	for i, root := range roots {
		existing = append(existing, archive.ProjectActivation{ProjectID: archive.ProjectID(root.Root), Root: root.Root, Included: i == 0})
	}
	var out bytes.Buffer
	projects, err := promptProjects(newPrompter(strings.NewReader("specific\n\n"), &out), existing, nil, nil)
	must(t, err)
	if len(projects) != 3 || includedProjects(projects) != 1 {
		t.Fatalf("rules %+v", projects)
	}
	projects, err = promptProjects(newPrompter(strings.NewReader("specific\np\n"+roots[2].Root+"\n\n"), &out), existing, nil, nil)
	must(t, err)
	if len(projects) != 3 || includedProjects(projects) != 2 || projects[1].Included {
		t.Fatalf("rules %+v", projects)
	}
}

// A setup draft saved before an import, then continued, keeps the projects
// the import added.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestSetupDraftKeepsBackfilledProjects(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	before, _, _ := config.Load(f.data)
	if err := local.Write(filepath.Join(f.data, "setup-draft.json"), setupDraft{Version: 1, Step: 2, Config: before}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	env := setupTestEnv(t, f.data, f.userHome, newFakeKeychain(), backfillNow)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	setupRun(t, env, "continue\ny\n", 0)
	cfg, _, _ := config.Load(f.data)
	if len(cfg.Archive.Projects) != 5 || includedProjects(cfg.Archive.Projects) != 5 {
		t.Fatalf("projects after continuing the draft: %+v", cfg.Archive.Projects)
	}
}
