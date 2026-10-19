package gitremote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

type colonOriginSpelling string

const (
	colonOriginAbsolute colonOriginSpelling = "absolute"
	colonOriginExplicit colonOriginSpelling = "explicit_relative"
	colonOriginNested   colonOriginSpelling = "slash_before_colon"
	colonOriginDouble   colonOriginSpelling = "double_colon"
)

func TestLocalOriginChainAcceptsUnambiguousColonPaths(t *testing.T) {
	for _, spelling := range []colonOriginSpelling{colonOriginAbsolute, colonOriginExplicit, colonOriginNested, colonOriginDouble} {
		t.Run(string(spelling), func(t *testing.T) {
			git := gitOrSkip(t)
			root := initRepo(t, git, "")
			name := "mirror:backup.git"
			if spelling == colonOriginDouble {
				name = "mirror::backup.git"
			}
			directory := root
			if spelling == colonOriginNested || spelling == colonOriginDouble {
				directory = filepath.Join(root, "mirrors")
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(directory, name)
			if err := os.Rename(initRepo(t, git, "https://example.test/acme/repo"), target); err != nil {
				t.Fatal(err)
			}
			origin := target
			switch spelling {
			case colonOriginAbsolute:
			case colonOriginExplicit:
				origin = "./" + name
			case colonOriginNested, colonOriginDouble:
				origin = "mirrors/" + name
			}
			// Only native local transports are used; an empty synthetic repository
			// is sufficient to establish Git accepts the path spelling.
			chainGit(t, t.Context(), git, root, "ls-remote", origin)
			chainGit(t, t.Context(), git, root, "config", "remote.origin.url", origin)
			id := observeProjectIdentity(t.Context(), root)
			if !id.Known || id.Key != archive.RepoKey("https://example.test/acme/repo") || !identityEvidenceCurrent(id) {
				t.Fatalf("Git-supported colon path was rejected: %+v", id)
			}
			chainGit(t, t.Context(), git, target, "config", "remote.origin.url", "https://example.test/acme/other")
			if ProjectIdentityCurrent(id) {
				t.Fatal("changed colon-path target retained identity")
			}
		})
	}
}

func TestLocalOriginColonPathsKeepRemoteClassification(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "")
	// A local directory with an SCP-shaped name must not override Git's
	// transport classification or supply unrelated local ownership evidence.
	if err := os.Rename(initRepo(t, git, "https://example.test/acme/other"), filepath.Join(root, "host:repo")); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"host:repo", "git@example.test:acme/repo", "helper::repo", "ssh://example.test/acme/repo", "FILE://localhost/repo", "~user/mirror:repo"} {
		if path, ok := localOriginPath(root, origin); ok {
			t.Fatalf("remote or unsupported reference became local: %q -> %q", origin, path)
		}
	}
	chainGit(t, t.Context(), git, root, "config", "remote.origin.url", "host:repo")
	id := observeProjectIdentity(t.Context(), root)
	if !id.Known || id.Key != archive.RepoKey("host:repo") {
		t.Fatalf("portable SCP normalization changed: %+v", id)
	}
	chainGit(t, t.Context(), git, root, "config", "remote.origin.url", "helper::repo")
	if id := observeProjectIdentity(t.Context(), root); id.Known || id.Failure != sourcefacts.RecoveryOriginUnsupported {
		t.Fatalf("remote helper was followed: %+v", id)
	}
}
