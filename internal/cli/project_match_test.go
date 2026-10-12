package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestProjectMatcherDeduplicatesAndDoesNotExpandExclusions(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://user:synthetic-secret@example.test/acme/repo.git")
	var calls atomic.Int32
	env := Env{WorkingDir: func() (string, error) { return alias, nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(ctx context.Context, path string) string {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 250*time.Millisecond {
			t.Error("missing child deadline")
		}
		return key
	}}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: true}}}}
	got := matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
	if got.Incomplete || len(got.Roots[0]) != 1 || calls.Load() != 1 {
		t.Fatalf("got %+v; calls %d", got, calls.Load())
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: filepath.Join(root, "private"), Included: false})
	got = matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
	if len(got.Roots[0]) != 0 {
		t.Fatalf("exclusion broadened: %+v", got)
	}
	command := anotherMachineCommand(cfg, home, env)
	if !strings.Contains(command, "--project-scope") || !strings.Contains(command, key) || strings.Contains(command, "synthetic-secret") || strings.Contains(command, "https://") {
		t.Fatalf("command %q", command)
	}
}

func TestProjectMatcherAmbiguousClonesAndKnownMismatch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	key := archive.RepoKey("git@example.test:acme/repo.git")
	other := archive.RepoKey("git@example.test:acme/other.git")
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	for _, root := range []string{a, b} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := Env{WorkingDir: func() (string, error) { return a, nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(context.Context, string) string { return key }}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: b, Included: true}}}}
	got := matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}, {RepoKey: other, Path: a}})
	if got.Incomplete || len(got.Roots[0]) != 2 || len(got.Roots[1]) != 0 {
		t.Fatalf("got %+v", got)
	}
	env.repoKeyContext = func(context.Context, string) string { return "" }
	got = matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key, Path: a}})
	if len(got.Roots[0]) != 1 {
		t.Fatalf("no-origin path hint: %+v", got)
	}
}

func TestProjectMatcherStopsAtSharedDeadlineAndBoundsGitConcurrency(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := config.Config{}
	for i := range 8 {
		root := filepath.Join(home, string(rune('a'+i)))
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: root, Included: true})
	}
	var active, peak atomic.Int32
	env := Env{WorkingDir: func() (string, error) { return "", nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(ctx context.Context, _ string) string {
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-ctx.Done()
		active.Add(-1)
		return ""
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := matchProjects(ctx, env, home, cfg, []projectMatchRequest{{RepoKey: archive.RepoKey("git@example.test:repo.git")}})
	if !got.Incomplete || peak.Load() > 4 || active.Load() != 0 {
		t.Fatalf("got %+v peak %d active %d", got, peak.Load(), active.Load())
	}
}

func TestSetupYesRepoSelectionAndExplicitPathsAreIndependent(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unique", "ambiguous", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			a, b, explicit := filepath.Join(userHome, "a"), filepath.Join(userHome, "b"), filepath.Join(userHome, "explicit")
			for _, root := range []string{a, b, explicit} {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			env.WorkingDir = func() (string, error) { return a, nil }
			env = withEnvironment(env, map[string]string{})
			key := archive.RepoKey("https://example.test/acme/repo.git")
			env.repoKeyContext = func(ctx context.Context, root string) string {
				if mode == "timeout" {
					<-ctx.Done()
					return ""
				}
				if root == local.CanonicalPath(a) || root == local.CanonicalPath(b) {
					return key
				}
				return ""
			}
			if mode == "ambiguous" {
				cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: b, Included: true}}}}
				if err := config.Save(home, cfg); err != nil {
					t.Fatal(err)
				}
			}
			output := setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--region", "us-east-1", "--project-repo", key, "--project", explicit, "--apps", "claude")
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			included := map[string]bool{}
			for _, p := range cfg.Archive.Projects {
				if p.Included {
					included[p.Root] = true
				}
			}
			if !included[local.CanonicalPath(explicit)] || included[local.CanonicalPath(a)] != (mode == "unique") {
				t.Fatalf("mode %s cfg %+v output %s", mode, cfg, output)
			}
			if mode != "unique" && !strings.Contains(output, "Skipped repository") {
				t.Fatalf("missing diagnostic: %s", output)
			}
		})
	}
}

// The cap is what this test checks, not speed: its candidates live in the
// run's own temporary folder (TestMain), history scanning reads no temporary
// directory of the machine's, and the deadline is far off, so a loaded
// machine cannot turn the cap into a timeout.
func TestProjectMatcherCapsCandidatesAndPreservesPartialMatches(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	key := archive.RepoKey("git@example.test:repo.git")
	cfg := config.Config{}
	for i := range 129 {
		root := filepath.Join(home, strconv.Itoa(i))
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: root, Included: true})
	}
	var calls atomic.Int32
	env := Env{BackfillTempDirs: []string{}, projectMatchTimeout: time.Hour, WorkingDir: func() (string, error) { return "", nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(context.Context, string) string { calls.Add(1); return key }}
	got := matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
	if !got.Capped || !got.Incomplete || got.TimedOut || len(got.Roots[0]) != 128 || calls.Load() != 128 {
		t.Fatalf("got %+v calls %d", got, calls.Load())
	}
}

func TestProjectMatcherFindsRelocatedRepositoryInHistory(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "relocated")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "synthetic")
	if err := os.MkdirAll(history, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"cwd": root})
	if err := os.WriteFile(filepath.Join(history, "synthetic.jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://user:synthetic-secret@example.test/acme/repo.git")
	env := Env{BackfillTempDirs: []string{}, WorkingDir: func() (string, error) { return "", nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(context.Context, string) string { return key }}
	got := matchProjects(context.Background(), env, home, config.Config{}, []projectMatchRequest{{RepoKey: key}})
	if got.Incomplete || len(got.Roots[0]) != 1 || got.Roots[0][0] != local.CanonicalPath(root) {
		t.Fatalf("got %+v", got)
	}
}

func TestSetupYesExplainsRepoSkipsWhenNoProjectIsIncluded(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env = withEnvironment(env, map[string]string{})
	key := archive.RepoKey("git@example.test:missing.git")
	output := setupYes(t, env, "", 1, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--region", "us-east-1", "--apps", "claude", "--project-repo", key)
	if !strings.Contains(output, "Skipped repository "+key+": not found") {
		t.Fatalf("missing skip reason: %s", output)
	}
}

func TestAnotherMachineCommandPreservesSubdirectoryScope(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo", "public")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "repo", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://example.test/acme/repo.git")
	env := Env{repoKeyContext: func(context.Context, string) string { return key }}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: true}}}}
	got := anotherMachineCommand(cfg, home, env)
	if strings.Contains(got, "--project-repo") || !strings.Contains(got, "--project ~/repo/public") {
		t.Fatalf("broadened subdirectory scope: %s", got)
	}
}

func TestProjectMatcherWithholdsInvalidOriginLookupPathFallback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := Env{WorkingDir: func() (string, error) { return "", nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(context.Context, string) string { return "invalid-origin" }}
	got := matchProjects(t.Context(), env, home, config.Config{}, []projectMatchRequest{{RepoKey: archive.RepoKey("https://example.test/repo.git"), Path: home}})
	if !got.Incomplete || len(got.Roots[0]) != 0 {
		t.Fatalf("unverified no-origin fallback: %+v", got)
	}
}

func TestProjectMatcherDoesNotBroadenOverlappingSavedScope(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	child := filepath.Join(root, "nested")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://example.test/repo.git")
	other := archive.RepoKey("https://example.test/nested.git")
	env := Env{WorkingDir: func() (string, error) { return root, nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(_ context.Context, path string) string {
		if path == local.CanonicalPath(child) {
			return other
		}
		return key
	}}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: child, Included: true}}}}
	got := matchProjects(t.Context(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
	if len(got.Roots[0]) != 0 {
		t.Fatalf("broadened saved scope: %+v", got)
	}
}

// A repository key represents its full checkout, even when setup starts below it.
func TestProjectMatcherUsesRepositoryTopLevelWithoutChangingSavedScope(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	child := filepath.Join(root, "pkg")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	remote := "https://example.test/acme/repo.git"
	key := archive.RepoKey(remote)
	for _, saved := range []string{"none", "nested", "root"} {
		t.Run(saved, func(t *testing.T) {
			t.Parallel()
			env := Env{WorkingDir: func() (string, error) { return child, nil }, LookupEnv: func(string) (string, bool) { return "", false }, projectGitRunner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] == "rev-parse" {
					return []byte(root + "\n"), nil
				}
				return []byte(remote + "\n"), nil
			}}
			var projects []archive.ProjectActivation
			if saved != "none" {
				configured := child
				if saved == "root" {
					configured = root
				}
				projects = []archive.ProjectActivation{{Root: configured, Included: true}}
			}
			cfg := config.Config{Archive: archive.Config{Projects: projects}}
			got := matchProjects(t.Context(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
			if got.Incomplete {
				t.Fatalf("incomplete: %+v", got)
			}
			pathOnly := matchProjects(t.Context(), env, home, cfg, []projectMatchRequest{{Path: child}})
			if len(pathOnly.Roots[0]) != 1 || pathOnly.Roots[0][0] != local.CanonicalPath(child) {
				t.Fatalf("changed explicit path scope: %+v", pathOnly)
			}
			if saved == "nested" {
				if len(got.Roots[0]) != 0 {
					t.Fatalf("selected nested inclusion by whole repository key: %+v", got)
				}
			} else if len(got.Roots[0]) != 1 || got.Roots[0][0] != local.CanonicalPath(root) {
				t.Fatalf("narrowed repository: %+v", got)
			}
		})
	}
}
