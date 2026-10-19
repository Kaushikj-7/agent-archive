package gitremote

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

type symlinkOriginSpelling string

const (
	symlinkOriginAbsolute symlinkOriginSpelling = "absolute"
	symlinkOriginRelative symlinkOriginSpelling = "relative"
	symlinkOriginFileURL  symlinkOriginSpelling = "file_url"
)

func TestLocalOriginChainPreservesSymlinkParentTraversal(t *testing.T) {
	for _, spelling := range []symlinkOriginSpelling{symlinkOriginAbsolute, symlinkOriginRelative, symlinkOriginFileURL} {
		t.Run(string(spelling), func(t *testing.T) {
			git := gitOrSkip(t)
			base := t.TempDir()
			for _, name := range []string{"actual/child", "actual/repo", "other/child", "other/repo", "links/repo", "checkout"} {
				if err := os.MkdirAll(filepath.Join(base, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			actual, lexical, other, root := filepath.Join(base, "actual", "repo"), filepath.Join(base, "links", "repo"), filepath.Join(base, "other", "repo"), filepath.Join(base, "checkout")
			for repo, remote := range map[string]string{actual: "https://example.test/acme/repo", lexical: "https://example.test/acme/unrelated", other: "https://example.test/acme/other", root: ""} {
				chainGit(t, t.Context(), git, repo, "init", "-q")
				chainGit(t, t.Context(), git, repo, "config", "remote.origin.url", remote)
			}
			link := filepath.Join(base, "links", "link")
			if err := os.Symlink(filepath.Join(base, "actual", "child"), link); err != nil {
				t.Fatal(err)
			}
			origin := base + "/links/link/../repo"
			path := origin
			switch spelling {
			case symlinkOriginAbsolute:
			case symlinkOriginRelative:
				origin = "../links/link/../repo"
				path = ProjectRoot(t.Context(), root, nil) + "/" + origin
			case symlinkOriginFileURL:
				origin = "file://" + origin
			}
			chainGit(t, t.Context(), git, root, "config", "remote.origin.url", origin)
			// Native Git follows the symlink before .., so the lexical sibling
			// cannot prove this clone unrelated to actual.
			raw, err := ExecRunner(t.Context(), path, "-C", path, "config", "--get", "remote.origin.url")
			if err != nil || string(raw) != "https://example.test/acme/repo\n" {
				t.Fatalf("native Git fixture: %q %v", raw, err)
			}
			id := observeProjectIdentity(t.Context(), root)
			key := archive.RepoKey("https://example.test/acme/repo")
			if !id.Known || id.Key != key || !identityEvidenceCurrent(id) {
				t.Fatalf("local chain disagrees with actual Git target: %+v", id)
			}
			if !slices.ContainsFunc(id.Dependencies, func(dep sourcefacts.RepositoryDependency) bool { return dep.Path == path }) {
				t.Fatal("original alias spelling is absent from renewal dependencies")
			}
			resolve := func(path string) string {
				if canonical, err := filepath.EvalSymlinks(path); err == nil {
					return canonical
				}
				return filepath.Clean(path)
			}
			r := sourcefacts.NewRecoveryResolver([]archive.ProjectActivation{{Root: actual, Included: true}, {Root: root, Included: true}}, nil, resolve, observeProjectIdentity, nil)
			r.Validate = ProjectIdentityCurrent
			if _, outcome := r.Recover(t.Context(), filepath.Join(base, "gone"), key); outcome != sourcefacts.RecoveryAmbiguous {
				t.Fatalf("physical local clone lost its competition: %s", outcome)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(base, "other", "child"), link); err != nil {
				t.Fatal(err)
			}
			if ProjectIdentityCurrent(id) {
				t.Fatal("retargeted symlink left planned repository evidence current")
			}
			if fresh := observeProjectIdentity(t.Context(), root); !fresh.Known || fresh.Key != archive.RepoKey("https://example.test/acme/other") {
				t.Fatal("fresh observation did not follow the retargeted path", fresh)
			}
		})
	}
}

func TestLocalOriginChainRenewsSymlinkParentInclude(t *testing.T) {
	git := gitOrSkip(t)
	base := t.TempDir()
	for _, name := range []string{"actual/child", "actual/includes", "links/includes"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "links", "link")
	if err := os.Symlink(filepath.Join(base, "actual", "child"), link); err != nil {
		t.Fatal(err)
	}
	actual, lexical := filepath.Join(base, "actual", "remote.cfg"), filepath.Join(base, "links", "remote.cfg")
	for _, path := range []string{actual, lexical} {
		if err := os.WriteFile(path, []byte("[remote \"origin\"]\n url = https://example.test/acme/repo\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{"actual", "links"} {
		if err := os.WriteFile(filepath.Join(base, directory, "includes", "parent.cfg"), []byte("[include]\n path = ../remote.cfg\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	terminal := initRepo(t, git, "")
	chainGit(t, t.Context(), git, terminal, "config", "include.path", base+"/links/link/../includes/parent.cfg")
	root := initRepo(t, git, terminal)
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || !identityEvidenceCurrent(id) {
		t.Fatal("initial included target evidence unavailable", id)
	}
	if err := os.WriteFile(actual, []byte("[remote \"origin\"]\n url = https://example.test/acme/other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ProjectIdentityCurrent(id) {
		t.Fatal("changed actual include stayed current through lexical sibling stamps")
	}
}
