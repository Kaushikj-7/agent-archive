package gitremote

import (
	"os/exec"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Every remote.origin.url value is read: one that normalizes is the identity
// whatever its position, the checkout is keyless only when no value
// normalizes, and values naming different repositories are unknown.
func TestProjectKeyReadsEveryOriginValue(t *testing.T) {
	t.Parallel()
	repo := archive.RepoKey("https://example.test/acme/repo")
	for _, tc := range []struct {
		name        string
		out         string
		key         string
		known       bool
		nonportable bool
	}{
		{name: "single", out: "https://example.test/acme/repo\x00", key: repo, known: true},
		{name: "single unterminated", out: "https://example.test/acme/repo\n", key: repo, known: true},
		{name: "single local", out: "/srv/mirror\x00", nonportable: true},
		{name: "empty value", out: "\x00", known: true},
		{name: "real then local", out: "https://example.test/acme/repo\x00/srv/mirror\x00", key: repo, known: true},
		{name: "local then real", out: "/srv/mirror\x00https://example.test/acme/repo\x00", key: repo, known: true},
		{name: "empty then local", out: "\x00/srv/mirror\x00", nonportable: true},
		{name: "every value local", out: "/srv/mirror\x00file:///srv/other\x00", nonportable: true},
		{name: "same repository twice", out: "https://example.test/acme/repo.git\x00git@example.test:acme/repo.git\x00", key: repo, known: true},
		{name: "different repositories", out: "https://example.test/acme/repo\x00/srv/mirror\x00https://example.test/acme/other\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{out: tc.out}
			root := t.TempDir()
			key, known, nonportable := projectKey(t.Context(), root, fake.run)
			if key != tc.key || known != tc.known || nonportable != tc.nonportable {
				t.Fatalf("got %q known %t nonportable %t, want %q %t %t", key, known, nonportable, tc.key, tc.known, tc.nonportable)
			}
			if publicKey, publicKnown := ProjectKey(t.Context(), root, fake.run); publicKey != tc.key || publicKnown != tc.known {
				t.Fatalf("ProjectKey = %q %t", publicKey, publicKnown)
			}
			want := []string{"-C", root, "config", "-z", "--get-all", "remote.origin.url"}
			if len(fake.calls) == 0 || !slices.Equal(fake.calls[0], want) {
				t.Fatalf("git ran as %v, want %v", fake.calls, want)
			}
		})
	}
}

// A real checkout whose last origin URL is a local path still has the key of
// its other origin URL, and its identity is not keyless.
func TestProjectKeyWithLocalLastOriginValue(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/repo.git")
	cmd := exec.CommandContext(t.Context(), git, "-C", root, "config", "--add", "remote.origin.url", "/srv/mirror")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := archive.RepoKey("https://example.test/acme/repo.git")
	if key, known := ProjectKey(t.Context(), root, nil); !known || key != want {
		t.Fatalf("ProjectKey = %q %t", key, known)
	}
	if _, _, nonportable := projectKey(t.Context(), root, nil); nonportable {
		t.Fatal("checkout with a real origin classified keyless")
	}
	if id := observeProjectIdentity(t.Context(), root); !id.Known || id.Key != want {
		t.Fatalf("identity %+v", id)
	}
}
