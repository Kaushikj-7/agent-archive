package gitremote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// observeProjectIdentity gives each standalone fixture a fresh observation pass.
// Production callers share an IdentityObserver across their recovery pass.
func observeProjectIdentity(ctx context.Context, root string) sourcefacts.RepositoryIdentity {
	return (&IdentityObserver{}).Lookup(ctx, root)
}

func TestProjectIdentityTracksIncludedConfiguration(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "")
	include := filepath.Join(t.TempDir(), "remote.cfg")
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://user:synthetic-secret@example.test/acme/repo.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".git", "config")
	if err := os.WriteFile(config, []byte("[core]\n repositoryformatversion = 0\n bare = false\n[include]\n path = "+include+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := observeProjectIdentity(context.Background(), root)
	if !id.Known || id.Key != archive.RepoKey("git@example.test:acme/repo.git") || !identityEvidenceCurrent(id) {
		t.Fatalf("%+v", id)
	}
	raw, err := json.Marshal(id)
	if err != nil || strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "https://") {
		t.Fatal("raw URL entered cached evidence")
	}
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://example.test/acme/other.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if identityEvidenceCurrent(id) {
		t.Fatal("included remote change left stale identity")
	}
	changed := observeProjectIdentity(context.Background(), root)
	if !changed.Known || changed.Key == id.Key {
		t.Fatal(changed)
	}
}

func TestProjectIdentitySeparatesScratchAndUnreadableClone(t *testing.T) {
	gitOrSkip(t)
	scratch := t.TempDir()
	if id := observeProjectIdentity(context.Background(), scratch); !id.Known || id.Root != "" {
		t.Fatal(id)
	}
	if id := observeProjectIdentity(context.Background(), filepath.Join(t.TempDir(), "gone")); id.Known {
		t.Fatal("missing clone classified as scratch")
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, ".git"), []byte("gitdir: /synthetic/deleted-gitdir\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if id := observeProjectIdentity(context.Background(), broken); id.Known {
		t.Fatal("broken clone classified as scratch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id := observeProjectIdentity(ctx, scratch); id.Known {
		t.Fatal("cancelled Git observation became evidence")
	}
}

func TestProjectIdentityTracksBranchConditionalConfiguration(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/original")
	include := filepath.Join(t.TempDir(), "branch.cfg")
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://example.test/acme/branch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[includeIf \"onbranch:selected\"]\n path = " + include + "\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || !identityEvidenceCurrent(id) {
		t.Fatalf("initial identity unavailable: %+v", id)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/selected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if identityEvidenceCurrent(id) {
		t.Fatal("branch conditional origin changed without invalidating cached identity")
	}
	// The include adds a second origin URL naming another repository. No
	// single key is proven, so the identity is unknown, not keyless.
	if changed := observeProjectIdentity(t.Context(), root); changed.Known || changed.Root != "" || changed.Key != "" {
		t.Fatalf("changed identity: %+v", changed)
	}
}

func TestProjectIdentityTracksEmptyIncludeConfiguration(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/original")
	include := filepath.Join(t.TempDir(), "empty.cfg")
	if err := os.WriteFile(include, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[include]\n path = " + include + "\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || !identityEvidenceCurrent(id) {
		t.Fatalf("initial identity unavailable: %+v", id)
	}
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://example.test/acme/changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if identityEvidenceCurrent(id) {
		t.Fatal("empty included file changed without invalidating cached identity")
	}
}

func TestProjectIdentityTracksScratchAncestor(t *testing.T) {
	gitOrSkip(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "scratch")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || !identityEvidenceCurrent(id) {
		t.Fatal(id)
	}
	if err := os.Mkdir(filepath.Join(parent, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if identityEvidenceCurrent(id) {
		t.Fatal("scratch gained a Git ancestor without invalidating known absence")
	}
}

// A newly created top-level config can change origin even when it contributed
// no keys to Git's original --show-origin inventory.
func TestProjectIdentityTracksAbsentGlobalConfiguration(t *testing.T) {
	for _, path := range []string{".gitconfig", ".config/git/config"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%v", path, empty), func(t *testing.T) {
				git := gitOrSkip(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
				configPath := filepath.Join(home, path)
				if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
					t.Fatal(err)
				}
				if empty {
					if err := os.WriteFile(configPath, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				root := initRepo(t, git, "")
				id := observeProjectIdentity(t.Context(), root)
				if !id.Known || id.Key != "" || !identityEvidenceCurrent(id) {
					t.Fatalf("initial identity unavailable: %+v", id)
				}
				if err := os.WriteFile(configPath, []byte("[remote \"origin\"]\n url = https://example.test/acme/new\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if identityEvidenceCurrent(id) {
					t.Fatal("new global configuration did not invalidate known absence")
				}
				if changed := observeProjectIdentity(t.Context(), root); !changed.Known || changed.Key != archive.RepoKey("https://example.test/acme/new") {
					t.Fatalf("changed identity: %+v", changed)
				}
			})
		}
	}
}

func TestTopLevelConfigDependenciesRespectGitPathQueryContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		global    string
		system    string
		globalErr error
		systemErr error
		want      bool
		count     int
	}{
		{name: "multiple candidate paths", global: "/synthetic/xdg/git/config\n/synthetic/home/.gitconfig\n", system: "/synthetic/etc/gitconfig\n", want: true, count: 3},
		{name: "disabled system", global: "/synthetic/home/.gitconfig\n", systemErr: projectExitStatusError(1), want: true, count: 1},
		{name: "no global paths", globalErr: projectExitStatusError(1), system: "/synthetic/etc/gitconfig\n", want: true, count: 1},
		{name: "both no value", globalErr: projectExitStatusError(1), systemErr: projectExitStatusError(1), want: true},
		{name: "unsupported variable", globalErr: projectExitStatusError(129)},
		{name: "failed output", global: "/synthetic/home/.gitconfig\n", globalErr: projectExitStatusError(1)},
		{name: "relative path", global: "relative\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[len(args)-1] == "GIT_CONFIG_GLOBAL" {
					return []byte(tc.global), tc.globalErr
				}
				return []byte(tc.system), tc.systemErr
			}
			paths := map[string]bool{}
			if got := topLevelConfigDependencies(t.Context(), "/synthetic/repo", paths, run); got != tc.want || (got && len(paths) != tc.count) {
				t.Fatal(got, paths)
			}
		})
	}
}

func TestProjectIdentityTracksNewNestedRepositoryAncestor(t *testing.T) {
	git := gitOrSkip(t)
	target := initRepo(t, git, "https://example.test/acme/target")
	outer := initRepo(t, git, "https://example.test/acme/outer")
	nested := filepath.Join(outer, "nested")
	configured := filepath.Join(nested, "configured")
	if err := os.MkdirAll(configured, 0700); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://example.test/acme/target")
	projects := []archive.ProjectActivation{{Root: target, Included: true}, {Root: configured, Included: false}}
	resolve := func(p string) string {
		v, e := filepath.EvalSymlinks(p)
		if e != nil {
			return filepath.Clean(p)
		}
		return v
	}
	r := sourcefacts.NewRecoveryResolver(projects, nil, resolve, observeProjectIdentity, nil)
	r.Validate = ProjectIdentityCurrent
	gone := filepath.Join(t.TempDir(), "gone")
	proof, outcome := r.Recover(t.Context(), gone, key)
	if outcome != "" || !r.Current(proof) {
		t.Fatalf("initial %+v %s", proof, outcome)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://example.test/acme/target"}} {
		cmd := exec.CommandContext(t.Context(), git, append([]string{"-C", nested}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, output)
		}
	}
	fresh := observeProjectIdentity(t.Context(), configured)
	if !fresh.Known || fresh.Key != key {
		t.Fatalf("fresh nested clone invalid: %+v", fresh)
	}
	freshResolver := sourcefacts.NewRecoveryResolver(projects, nil, resolve, observeProjectIdentity, nil)
	freshResolver.Validate = ProjectIdentityCurrent
	_, freshOutcome := freshResolver.Recover(t.Context(), gone, key)
	if freshOutcome != sourcefacts.RecoverySubtreeUnavailable {
		t.Fatalf("fresh outcome %s", freshOutcome)
	}
	if r.Current(proof) {
		t.Fatalf("stale proof still certifies target %s despite newly excluded nested clone; fresh inventory yields %s", proof.Root, freshOutcome)
	}
	if _, outcome := r.Recover(t.Context(), gone, key); outcome != sourcefacts.RecoveryInventoryUnavailable {
		t.Fatalf("stale cached uniqueness must stay pending: %s", outcome)
	}
}

// Semantic evidence needs a second Git observation; stamps alone cannot detect
// the creation of a previously absent top-level config on legacy Git.
func identityEvidenceCurrent(id sourcefacts.RepositoryIdentity) bool {
	if !ProjectIdentityCurrent(id) {
		return false
	}
	if id.Validation != "semantic" {
		return true
	}
	fresh := observeProjectIdentity(context.Background(), id.ObservedRoot)
	return fresh.Known && fresh.Root == id.Root && fresh.Key == id.Key
}

func TestLegacyGitIdentityUsesSemanticProofWithoutInventedConfigPaths(t *testing.T) {
	git := gitOrSkip(t)
	version, err := exec.CommandContext(t.Context(), git, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(string(version)))
	root := initRepo(t, git, "https://example.test/acme/repo")
	// Force the capability result to the legacy path independent of CI's Git.
	scope, ok := identityObservationScope()
	if !ok {
		t.Fatal("observer scope")
	}
	observer := &IdentityObserver{scope: scope, probed: true, legacy: true}
	id := observer.Lookup(t.Context(), root)
	if !id.Known || id.Validation != "semantic" || id.Key != archive.RepoKey("https://example.test/acme/repo") {
		t.Fatal(id)
	}
	for _, dep := range id.Dependencies {
		if strings.HasSuffix(dep.Path, ".gitconfig") {
			t.Fatal("invented absent config stamp", dep)
		}
	}
}

func TestConfigurationInventoryHasItsOwnBoundedOutput(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/repo")
	config := filepath.Join(root, ".git", "config")
	f, err := os.OpenFile(config, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if _, err := fmt.Fprintf(f, "\n[synthetic]\n key%d = ignored\n", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecRunner(t.Context(), root, "-C", root, "config", "--show-origin", "--name-only", "-z", "--list"); !errors.Is(err, ErrOutputLimit) {
		t.Fatal("short query cap", err)
	}
	if id := observeProjectIdentity(t.Context(), root); !id.Known {
		t.Fatal("large name inventory lost identity", id)
	}
	f, err = os.OpenFile(config, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[remote \"origin\"]\n url = https://example.test/" + strings.Repeat("x", 5000) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, known := ProjectKey(t.Context(), root, nil); known {
		t.Fatal("oversized origin bypassed short cap")
	}
}

func TestOptionalCapabilitiesAreProbedOnceAndScopedToEnvironment(t *testing.T) {
	git := gitOrSkip(t)
	roots := []string{initRepo(t, git, "https://example.test/acme/one"), initRepo(t, git, "https://example.test/acme/two")}
	probes := 0
	observer := &IdentityObserver{Run: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) >= 4 && args[2] == "var" {
			probes++
			return nil, projectExitStatusError(129)
		}
		return ExecRunner(ctx, dir, args...)
	}}
	for _, root := range roots {
		if id := observer.Lookup(t.Context(), root); !id.Known || id.Validation != "semantic" {
			t.Fatal(id)
		}
	}
	if probes != 1 {
		t.Fatalf("optional capability probed %d times", probes)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if id := observer.Lookup(t.Context(), roots[0]); !id.Known {
		t.Fatal(id)
	}
	if probes != 2 {
		t.Fatal("environment change reused capability cache", probes)
	}
}

func TestProjectIdentityRejectsChangedObserverEnvironment(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%v", modern), func(t *testing.T) {
			git := gitOrSkip(t)
			root := initRepo(t, git, "https://example.test/acme/repo")
			observer := &IdentityObserver{Run: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				if len(args) >= 4 && args[2] == "var" {
					if modern {
						return []byte(filepath.Join(os.Getenv("HOME"), ".gitconfig") + "\n"), nil
					}
					return nil, projectExitStatusError(129)
				}
				if len(args) >= 4 && args[2] == "rev-parse" && args[3] == "--path-format=absolute" {
					var paths []string
					for _, name := range []string{"HEAD", "config", "config.worktree", "commondir"} {
						paths = append(paths, filepath.Join(root, ".git", name))
					}
					return []byte(strings.Join(paths, "\n") + "\n"), nil
				}
				return ExecRunner(ctx, dir, args...)
			}}
			id := observer.Lookup(t.Context(), root)
			if !id.Known || !ProjectIdentityCurrent(id) {
				t.Fatal("initial identity unavailable", id)
			}
			// No stamped file changes: a different HOME selects a different set
			// of candidate files, including files absent during planning.
			t.Setenv("HOME", t.TempDir())
			if ProjectIdentityCurrent(id) {
				t.Fatal("changed config environment retained planned evidence")
			}
		})
	}
}

func TestProjectIdentityRejectsChangedObserverExecutable(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/repo")
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known {
		t.Fatal(id)
	}
	fakeGitOnPath(t, `exit 129`)
	if ProjectIdentityCurrent(id) {
		t.Fatal("replacement executable retained planned evidence")
	}
}

func TestProjectIdentityTraversalLimitIsBudgetExhaustion(t *testing.T) {
	gitOrSkip(t)
	for _, repository := range []bool{false, true} {
		t.Run(fmt.Sprintf("repository=%v", repository), func(t *testing.T) {
			top := t.TempDir()
			root := top
			for range 65 {
				root = filepath.Join(root, "nested")
			}
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			observer := &IdentityObserver{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if !repository {
					return nil, projectExitStatusError(128)
				}
				if args[2] == "rev-parse" {
					return []byte(top + "\n"), nil
				}
				return []byte("https://example.test/acme/repo\n"), nil
			}, ConfigRun: func(context.Context, string, ...string) ([]byte, error) {
				return []byte("file:" + filepath.Join(top, ".git", "config") + "\x00remote.origin.url\x00"), nil
			}}
			if id := observer.Lookup(t.Context(), root); id.Known || !id.BudgetExhausted {
				t.Fatal("bounded traversal must remain a budget retry", id)
			}
		})
	}
}

// Only an origin Git printed but no repository key can name is a keyless
// checkout (Root set, not Known). A failed origin read may hide any key, so it
// must not look like one: recovery treats keyless proposed roots as non-owning.
func TestProjectIdentityKeylessOnlyForNonportableOrigin(t *testing.T) {
	git := gitOrSkip(t)
	clone := initRepo(t, git, filepath.Join(t.TempDir(), "deleted-worktree"))
	top := ProjectRoot(t.Context(), clone, nil)
	if id := observeProjectIdentity(t.Context(), clone); id.Known || id.Key != "" || id.BudgetExhausted || top == "" || id.Root != top || id.ObservationScope == "" {
		t.Fatalf("local-path origin: %+v", id)
	}
	for _, tc := range []struct {
		name   string
		out    string
		err    error
		root   bool
		budget bool
	}{
		{name: "nonportable", out: "/synthetic/deleted-worktree\n", root: true},
		{name: "config failure", err: projectExitStatusError(128)},
		{name: "failed with output", out: "/synthetic/x\n", err: projectExitStatusError(1)},
		{name: "not installed", err: exec.ErrNotFound},
		{name: "output limit", out: "/synthetic/", err: ErrOutputLimit, budget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] == "rev-parse" {
					return []byte(root + "\n"), nil
				}
				return []byte(tc.out), tc.err
			}
			id := (&IdentityObserver{Run: run}).Lookup(t.Context(), root)
			if id.Known || id.Key != "" || (tc.root && id.Root != root) || (!tc.root && id.Root != "") || id.BudgetExhausted != tc.budget {
				t.Fatalf("%+v", id)
			}
		})
	}
}
