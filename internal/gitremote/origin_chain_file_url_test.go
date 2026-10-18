package gitremote

import (
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestLocalOriginChainAcceptsGitLocalhostCaseAndRenewsRecovery(t *testing.T) {
	for _, host := range []string{"", "localhost", "LOCALHOST", "LocalHost"} {
		t.Run("host="+host, func(t *testing.T) {
			git := gitOrSkip(t)
			remote := "https://example.test/acme/repo"
			target := initRepo(t, git, "https://example.test/acme/other")
			origin := "file://" + host + target
			// Git's file transport must accept this spelling against a real
			// synthetic repository, without contacting any network service.
			chainGit(t, t.Context(), git, target, "ls-remote", origin)
			clone := initRepo(t, git, origin)
			destination := initRepo(t, git, remote)
			id := observeProjectIdentity(t.Context(), clone)
			if !id.Known || id.Key != archive.RepoKey("https://example.test/acme/other") || id.Root != ProjectRoot(t.Context(), clone, nil) || !identityEvidenceCurrent(id) {
				t.Fatalf("Git-supported local URL did not establish current identity: %+v", id)
			}
			resolve := func(path string) string {
				if canonical, err := filepath.EvalSymlinks(path); err == nil {
					return canonical
				}
				return filepath.Clean(path)
			}
			r := sourcefacts.NewRecoveryResolver([]archive.ProjectActivation{{Root: destination, Included: true}, {Root: clone, Included: true}}, nil, resolve, observeProjectIdentity, nil)
			r.Validate = ProjectIdentityCurrent
			gone := filepath.Join(t.TempDir(), "gone")
			proof, outcome := r.Recover(t.Context(), gone, archive.RepoKey(remote))
			if outcome != "" || proof.Root != destination || !r.Current(proof) {
				t.Fatalf("resolved unrelated chain blocked current recovery: %+v %s", proof, outcome)
			}
			chainGit(t, t.Context(), git, target, "config", "remote.origin.url", remote)
			if ProjectIdentityCurrent(id) || r.Current(proof) {
				t.Fatal("changed local URL target retained recovery authority")
			}
			fresh := sourcefacts.NewRecoveryResolver(r.Projects, nil, resolve, observeProjectIdentity, nil)
			fresh.Validate = ProjectIdentityCurrent
			if _, outcome := fresh.Recover(t.Context(), gone, archive.RepoKey(remote)); outcome != sourcefacts.RecoveryAmbiguous {
				t.Fatalf("same-key physical clone lost its competition: %s", outcome)
			}
		})
	}
}

func TestLocalOriginFileURLKeepsAuthorityRestrictionsAndPathSpelling(t *testing.T) {
	path := "/synthetic/links/link/../repo"
	for _, host := range []string{"", "localhost", "LOCALHOST", "LocalHost"} {
		if got, ok := localOriginPath("/synthetic/checkout", "file://"+host+path); !ok || got != path {
			t.Fatalf("host %q changed filesystem traversal: %q %v", host, got, ok)
		}
	}
	for _, value := range []string{
		"file://remote.test" + path,
		"file://localhost." + path,
		"file://localhoſt" + path,
		"file://LOCALHOſT" + path,
		"file://127.0.0.1" + path,
		"file://[::1]" + path,
		"file://[localhost]" + path,
		"file://LOCALHOST:80" + path,
		"file://LOCALHOST:" + path,
		"file://user@LOCALHOST" + path,
		"file://LOCALHOST" + path + "?query=1",
		"file://LOCALHOST" + path + "#fragment",
		"FILE://LOCALHOST" + path,
		"ftp://LOCALHOST" + path,
	} {
		if got, ok := localOriginPath("/synthetic/checkout", value); ok {
			t.Fatalf("unsupported authority or URL became a local path: %q -> %q", value, got)
		}
	}
}
