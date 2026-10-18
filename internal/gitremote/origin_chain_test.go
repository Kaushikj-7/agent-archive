package gitremote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func chainGit(t *testing.T, git, root string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), git, append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthetic Git %v: %s: %v", args, out, err)
	}
}

func TestLocalOriginChainResolvesEveryBranchAndRetainsPhysicalRoot(t *testing.T) {
	git := gitOrSkip(t)
	remote := "https://user:synthetic-secret@example.test/acme/other.git"
	terminal := initRepo(t, git, remote)
	middle := initRepo(t, git, "file://"+terminal)
	root := initRepo(t, git, middle)
	chainGit(t, git, root, "config", "--add", "remote.origin.url", remote)
	chainGit(t, git, root, "config", "--add", "remote.origin.url", "file://localhost"+terminal)
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || id.Key != archive.RepoKey(remote) || id.Root != ProjectRoot(t.Context(), root, nil) || !identityEvidenceCurrent(id) {
		t.Fatalf("incomplete chain: %+v", id)
	}
	for _, clone := range []string{root, middle, terminal} {
		if !slices.ContainsFunc(id.Dependencies, func(d sourcefacts.RepositoryDependency) bool { return d.Path == filepath.Join(clone, ".git", "config") }) {
			t.Fatalf("missing traversed config dependency for %s", filepath.Base(clone))
		}
	}
	raw, err := json.Marshal(id)
	if err != nil || strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "https://") {
		t.Fatal("raw remote entered identity evidence")
	}
	chainGit(t, git, terminal, "config", "remote.origin.url", "https://example.test/acme/repo")
	if ProjectIdentityCurrent(id) {
		t.Fatal("changed terminal kept the planned identity current")
	}
}

func TestLocalOriginChainSupportsRelativeAndBareTargets(t *testing.T) {
	git := gitOrSkip(t)
	base := t.TempDir()
	bare := filepath.Join(base, "mirror.git")
	if err := os.Mkdir(bare, 0700); err != nil {
		t.Fatal(err)
	}
	chainGit(t, git, bare, "init", "--bare", "-q")
	chainGit(t, git, bare, "config", "remote.origin.url", "https://example.test/acme/repo")
	root := filepath.Join(base, "checkout")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	chainGit(t, git, root, "init", "-q")
	chainGit(t, git, root, "config", "remote.origin.url", "../mirror.git")
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || id.Key != archive.RepoKey("https://example.test/acme/repo") || !identityEvidenceCurrent(id) {
		t.Fatal(id)
	}
	chainGit(t, git, bare, "config", "remote.origin.url", "https://example.test/acme/other")
	if ProjectIdentityCurrent(id) {
		t.Fatal("bare target change was not retained as a dependency")
	}
}

func TestLocalOriginChainFailuresRemainUncertain(t *testing.T) {
	for _, kind := range []string{"cycle", "missing", "unreadable", "keyless_terminal", "conflicting_branches", "portable_and_missing", "unsupported", "subdirectory", "limit"} {
		t.Run(kind, func(t *testing.T) {
			git := gitOrSkip(t)
			root := initRepo(t, git, "")
			target := initRepo(t, git, "https://example.test/acme/repo")
			want := sourcefacts.RecoveryOriginMissing
			switch kind {
			case "cycle":
				chainGit(t, git, target, "config", "remote.origin.url", root)
				want = sourcefacts.RecoveryOriginCycle
			case "missing", "portable_and_missing":
				target = filepath.Join(t.TempDir(), "missing")
			case "unreadable":
				config := filepath.Join(target, ".git", "config")
				if err := os.Chmod(config, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(config, 0600) })
				want = sourcefacts.RecoveryOriginUnreadable
			case "keyless_terminal":
				chainGit(t, git, target, "config", "--unset-all", "remote.origin.url")
				want = sourcefacts.RecoveryOriginUnresolved
			case "conflicting_branches":
				other := initRepo(t, git, "https://example.test/acme/other")
				chainGit(t, git, root, "config", "--add", "remote.origin.url", other)
				want = sourcefacts.RecoveryOriginConflict
			case "unsupported":
				target = "helper::synthetic"
				want = sourcefacts.RecoveryOriginUnsupported
			case "subdirectory":
				target = filepath.Join(target, "nested")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				want = sourcefacts.RecoveryOriginUnsupported
			case "limit":
				for range maxOriginRepositories {
					target = initRepo(t, git, target)
				}
				want = sourcefacts.RecoveryOriginLimit
			}
			chainGit(t, git, root, "config", "--add", "remote.origin.url", target)
			if kind == "portable_and_missing" {
				chainGit(t, git, root, "config", "--add", "remote.origin.url", "https://example.test/acme/repo")
			}
			id := observeProjectIdentity(t.Context(), root)
			if id.Known || id.Key != "" || id.Failure != want || id.BudgetExhausted {
				t.Fatalf("want %s: %+v", want, id)
			}
		})
	}
}

func TestLocalOriginChainRenewsRewritesAndRejectsMidObservationChange(t *testing.T) {
	git := gitOrSkip(t)
	target := initRepo(t, git, "https://example.test/acme/repo")
	root := initRepo(t, git, "mirror:repo")
	chainGit(t, git, root, "config", "url."+target+".insteadOf", "mirror:repo")
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || id.Key != archive.RepoKey("https://example.test/acme/repo") {
		t.Fatal(id)
	}
	chainGit(t, git, root, "config", "url."+target+".insteadOf", "different:")
	if ProjectIdentityCurrent(id) {
		t.Fatal("changed rewrite retained identity")
	}
	chainGit(t, git, root, "config", "remote.origin.url", target)
	reads := 0
	observer := &IdentityObserver{Run: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		raw, err := ExecRunner(ctx, dir, args...)
		if dir == target && slices.Contains(args, "--get-all") {
			reads++
			if reads == 1 {
				chainGit(t, git, target, "config", "remote.origin.url", "https://example.test/acme/other")
			}
		}
		return raw, err
	}}
	if changed := observer.Lookup(t.Context(), root); changed.Known || changed.Failure != sourcefacts.RecoveryOriginChanged {
		t.Fatal(changed)
	}
}

func TestLocalOriginReadFailureCannotBeHiddenByPortableValue(t *testing.T) {
	git := gitOrSkip(t)
	target := initRepo(t, git, "https://example.test/acme/repo")
	root := initRepo(t, git, target)
	chainGit(t, git, root, "config", "--add", "remote.origin.url", "https://example.test/acme/repo")
	observer := &IdentityObserver{Run: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if dir == target && slices.Contains(args, "--get-all") {
			return nil, errors.New("synthetic target failure")
		}
		return ExecRunner(ctx, dir, args...)
	}}
	if id := observer.Lookup(t.Context(), root); id.Known || id.Failure != sourcefacts.RecoveryOriginUnreadable {
		t.Fatal(id)
	}
}

func FuzzLocalOriginPath(f *testing.F) {
	for _, value := range []string{"/mirror/repo", "../mirror.git", "file:///mirror/repo", "file://localhost/mirror/repo", "file://remote.test/mirror", "helper::repo", "ssh://host.test/repo", "\x00"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		path, ok := localOriginPath("/synthetic/checkout", value)
		if ok && (!filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n")) {
			t.Fatalf("invalid local path: %q", path)
		}
		if strings.Contains(value, "://") && !strings.HasPrefix(value, "file://") && ok {
			t.Fatal("network reference accepted as a local path")
		}
	})
}

func TestProjectIdentityRejectsObservationFromBeforeLocalChainResolution(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/repo")
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known {
		t.Fatal(id)
	}
	executable, err := realLocator.find()
	if err != nil {
		t.Fatal(err)
	}
	stamp, ok := repositoryStamp(executable)
	if !ok {
		t.Fatal("executable stamp unavailable")
	}
	oldScope := sha256.Sum256([]byte(stamp + "\x00" + executable + "\x00" + strings.Join(environment(os.Environ()), "\x00")))
	id.ObservationScope = hex.EncodeToString(oldScope[:])
	if ProjectIdentityCurrent(id) {
		t.Fatal("pre-chain cached positive identity retained authority")
	}
}
