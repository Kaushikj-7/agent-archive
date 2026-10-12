package backfill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// A worktree outside its repository goes through rule 2 on the repository it
// maps to: excluded with it, or imported under its configured spelling.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestWorktreeOutsideRepository(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	feature := tr.worktree("home/repo", "home/repo-feature", "repo-feature")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	aliased := filepath.Join(tr.path("alias"), "repo")
	cases := []struct {
		name string
		cfg  config.Config
		want resolution
	}{
		{"unconfigured", config.Config{}, resolution{root: repo, kind: ProjectKindRepository}},
		{"excluded", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(repo, false)}}}, resolution{root: repo, kind: ProjectKindRepository, skip: SkipExcludedProject}},
		{"included", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(repo, true)}}}, resolution{root: repo, kind: ProjectKindRepository, included: true}},
		{"included through a symlinked spelling", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(aliased, true)}}}, resolution{root: aliased, kind: ProjectKindRepository, included: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newResolver(tr.env(), tc.cfg, Filters{}).resolve(feature); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A worktree whose git directory is gone is not a repository of its own.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestWorktreeWithMissingRepository(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	codexWT := tr.path("home/.codex/worktrees/ab12/repo")
	tr.write("home/.codex/worktrees/ab12/repo/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "x")+"\n")
	claudeWT := tr.path("home/repo/.claude/worktrees/w1")
	tr.write("home/repo/.claude/worktrees/w1/.git", "gitdir: ../../../.git/worktrees/w1\n")
	cases := []struct {
		name string
		cwd  string
		want resolution
	}{
		{"Codex worktree", codexWT, resolution{skip: SkipWorktreeUnresolved}},
		{"Claude worktree maps by path", claudeWT, resolution{root: repo, kind: ProjectKindRepository}},
		{"missing worktree directly under home", filepath.Join(tr.home, ".claude", "worktrees", "x"), resolution{root: tr.home, kind: ProjectKindHome, skip: SkipHomeDirectory}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(tc.cwd); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A folder that no longer exists under a symlinked parent resolves through
// the parent, so its spelling and configured owner match the real path.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestMissingFolderUnderSymlink(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	proj := tr.mkdir("home/proj")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	r := newResolver(tr.env(), config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(proj, true)}}}, Filters{})
	if got, want := r.resolve(filepath.Join(tr.path("alias"), "gone", "x")), (resolution{root: filepath.Join(tr.home, "gone", "x"), kind: ProjectKindDirectory}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := r.resolve(filepath.Join(tr.path("alias"), "proj", "missing")); got.root != proj || !got.included {
		t.Fatalf("configured owner through a symlink: %+v", got)
	}
}

// A missing worktree under home maps to home by its path; when home itself
// is a configured, included project, that configuration wins.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestWorktreeMappedToConfiguredHome(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(tr.home, true)}}}
	got := newResolver(tr.env(), cfg, Filters{}).resolve(filepath.Join(tr.home, ".claude", "worktrees", "gone"))
	if want := (resolution{root: tr.home, kind: ProjectKindHome, included: true}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An existing worktree outside the apps' worktree folders whose git
// directory is gone cannot be mapped to its repository.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestExistingWorktreeWithMissingGitDir(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	wt := tr.mkdir("home/elsewhere/feature")
	tr.write("home/elsewhere/feature/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "feature")+"\n")
	if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(filepath.Join(wt, "pkg")); got != (resolution{skip: SkipWorktreeUnresolved}) {
		t.Fatalf("got %+v", got)
	}
}

// A repository in a temporary directory, such as a throwaway clone an agent
// made to review a pull request, is temporary (rule 6), not a project of its
// own (rule 4): adding it would capture every later session there. A
// configured project still owns it (rule 2), and a linked worktree in a
// temporary directory of a repository elsewhere still folds into that
// repository (rule 3). The repository, not the session's folder in it, is
// the would-be root, so --include-temp adds one project per clone and a
// worktree of the clone kept elsewhere joins it. The temporary root is
// matched through a symlinked spelling too, as /tmp is a link to
// /private/tmp on macOS. A temporary root that holds home (a sandbox or
// container whose HOME is under /tmp) leaves the repositories in home
// ordinary projects.
//
// Regression: backfill --dry-run proposed adding /private/tmp clones,
// 2026-10-08.
func TestRepositoryInTemporaryDirectory(t *testing.T) {
	tr := newTree(t)
	clone := tr.repo("tmp/pr331-review/checkout")
	tr.mkdir("tmp/pr331-review/checkout/internal/cli")
	home := tr.repo("home/agent-archive")
	wt := tr.worktree("home/agent-archive", "tmp/review-wt", "review-wt")
	tempRepo := tr.repo("tmp/scratch-repo")
	tempRepoWT := tr.worktree("tmp/scratch-repo", "home/scratch-wt", "scratch-wt")
	homeTempClone := tr.repo("home/tmp/clone")
	gone := filepath.Join(tr.path("tmp"), "fb-local-progression-review", "pkg")
	alias := tr.path("tmp-link")
	if err := os.Symlink(tr.path("tmp"), alias); err != nil {
		t.Fatal(err)
	}
	cloneSub := filepath.Join(clone, "internal", "cli")
	viaAlias := filepath.Join(alias, "pr331-review", "checkout", "internal", "cli")
	configured := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(clone, true)}}}
	configuredTempRepo := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(tempRepo, true)}}}
	include := Filters{IncludeTemp: true}
	cases := []struct {
		name    string
		temps   []string
		cfg     config.Config
		filters Filters
		cwd     string
		want    resolution
	}{
		{"clone is temporary", nil, config.Config{}, Filters{}, cloneSub, resolution{root: clone, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"clone with --include-temp", nil, config.Config{}, include, cloneSub, resolution{root: clone, kind: ProjectKindTemporary}},
		{"clone root", nil, config.Config{}, Filters{}, clone, resolution{root: clone, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"temporary root given as a symlink", []string{alias}, config.Config{}, Filters{}, viaAlias, resolution{root: clone, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"configured clone stays a project", nil, configured, Filters{}, cloneSub, resolution{root: clone, kind: ProjectKindTemporary, included: true}},
		{"worktree of a repository elsewhere folds into it", nil, config.Config{}, Filters{}, wt, resolution{root: home, kind: ProjectKindRepository}},
		{"worktree of a temporary repository is temporary", nil, config.Config{}, Filters{}, tempRepoWT, resolution{root: tempRepo, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"worktree of a temporary repository joins it with --include-temp", nil, config.Config{}, include, tempRepoWT, resolution{root: tempRepo, kind: ProjectKindTemporary}},
		{"worktree of a configured temporary repository", nil, configuredTempRepo, Filters{}, tempRepoWT, resolution{root: tempRepo, kind: ProjectKindTemporary, included: true}},
		{"temporary root holding home", []string{tr.root}, config.Config{}, Filters{}, home, resolution{root: home, kind: ProjectKindRepository}},
		{"temporary root in home", []string{tr.path("home/tmp")}, config.Config{}, Filters{}, homeTempClone, resolution{root: homeTempClone, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"temporary root holding home, clone outside home", []string{tr.root}, config.Config{}, Filters{}, cloneSub, resolution{root: clone, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"temporary repository itself", nil, config.Config{}, include, tempRepo, resolution{root: tempRepo, kind: ProjectKindTemporary}},
		{"missing clone folder", nil, config.Config{}, Filters{}, gone, resolution{root: gone, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tr.env()
			if tc.temps != nil {
				env.TempDirs = tc.temps
			}
			if got := newResolver(env, tc.cfg, tc.filters).resolve(tc.cwd); got != tc.want {
				t.Fatalf("resolve(%q) = %+v, want %+v", tc.cwd, got, tc.want)
			}
		})
	}
}
