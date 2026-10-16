package backfill

import (
	"bytes"
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// PR #53 second review: the look inside an added plain folder must not make
// macOS show a privacy prompt. It never reads inside a protected location
// the added folder is not itself in: /Volumes and the other volume paths,
// and home's Desktop, Documents, Downloads and Library, with iCloud Drive
// and other apps' containers as locations of their own inside Library.
func TestProtectedOutside(t *testing.T) {
	t.Parallel()
	protected := privacyProtectedFolders(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: "/Users/me", OS: platform.Darwin, EvalSymlinks: func(p string) (string, error) { return p, nil }})
	for _, tc := range []struct {
		path string
		root string
		want bool
	}{
		{"/Users/me/code/x", "/Users/me/code", false},
		{"/Users/me/Documents/code/x", "/Users/me/Documents/code", false},
		{"/Users/me/Documents", "/Users/me", true},
		{"/Users/me/Desktop/repo", "/Users", true},
		{"/Users/me/Downloads", "/Users/me", true},
		{"/Users/me/Library/Caches", "/Users/me/Library", false},
		{"/Users/me/Library/Mobile Documents", "/Users/me/Library", true},
		{"/Users/me/Library/Mobile Documents/com~apple~CloudDocs/x", "/Users/me/Library/Mobile Documents/com~apple~CloudDocs", false},
		{"/Users/me/Library/Containers", "/Users/me/Library", true},
		{"/Users/me/Library/Group Containers", "/Users/me/Library", true},
		{"/Volumes", "/", true},
		{"/Volumes/USB", "/Volumes", false},
		{"/Volumes/USB/code/x", "/Volumes/USB/code", false},
		{"/System/Volumes/Data", "/System", true},
		{"/Network/Servers", "/", true},
		{"/net/host", "/", true},
		{"/Users/me/Desktopper", "/Users/me", false},
	} {
		if got := protectedOutside(tc.path, tc.root, protected); got != tc.want {
			t.Errorf("protectedOutside(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

// A home reached through a symlink is protected under both spellings: the
// protected folders are resolved the way the rest of backfill resolves paths
// (Environment.EvalSymlinks), so a project path spelled through the link's
// target is still kept out of Documents.
func TestProtectedFoldersCoverTheResolvedHome(t *testing.T) {
	t.Parallel()
	protected := privacyProtectedFolders(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: "/Users/me", OS: platform.Darwin, EvalSymlinks: func(p string) (string, error) {
		if p == "/Users/me" {
			return "/Volumes/Data/Users/me", nil
		}
		return p, nil
	}})
	for _, folder := range []string{"/Users/me/Documents", "/Volumes/Data/Users/me/Documents", "/Volumes/Data/Users/me/Library/Containers"} {
		if !slices.Contains(protected, folder) {
			t.Errorf("%s is not protected: %v", folder, protected)
		}
	}
	if !protectedOutside("/Volumes/Data/Users/me/Documents/x", "/Volumes/Data/Users/me", protected) {
		t.Error("a path through the resolved home reads inside Documents")
	}
}

// A folder added inside Library is looked in, but iCloud Drive and other
// apps' containers inside it are not: they are kept out whole, and nothing
// inside them is listed or stat'd. The plan says so. A folder added inside a
// protected location is looked in as before, still within the budget.
func TestNestedLookNeverReadsInsideProtectedFolders(t *testing.T) {
	tr := newTree(t)
	library := tr.mkdir("home/Library")
	tr.write("home/"+claudeFile("s", "lib"), claudeTranscript("lib", library, fixedNow.Add(-48*time.Hour)))
	repo := tr.repo("home/Library/dev/repo")
	icloud := tr.mkdir("home/Library/Mobile Documents")
	tr.repo("home/Library/Mobile Documents/com~apple~CloudDocs/repo")
	containers := tr.mkdir("home/Library/Containers")
	tr.repo("home/Library/Containers/com.example/repo")

	env := tr.env()
	var touched []string
	guard := func(path string) {
		for _, p := range []string{icloud, containers} {
			if path != p && local.PathWithin(path, p) {
				touched = append(touched, path)
			}
		}
	}
	env.ReadDir = func(path string) ([]fs.DirEntry, error) { guard(path); return os.ReadDir(path) }
	env.Stat = func(path string) (fs.FileInfo, error) { guard(path); return os.Stat(path) }
	env.Lstat = func(path string) (fs.FileInfo, error) { guard(path); return os.Lstat(path) }
	env.ReadFile = func(path string) ([]byte, error) { guard(path); return os.ReadFile(path) }

	p := plan(t, env, nil, config.Config{}, Filters{})
	if len(touched) > 0 {
		t.Fatalf("read inside a protected folder: %v", touched)
	}
	s := summaryFor(t, p, library)
	want := []string{icloud, containers, repo}
	if !s.NestedComplete || strings.Join(s.KeptOut, "\n") != strings.Join(sorted(want), "\n") {
		t.Fatalf("kept out %v (complete %v), want %v", s.KeptOut, s.NestedComplete, sorted(want))
	}
	if strings.Join(s.KeptOutUnchecked, "\n") != strings.Join(sorted([]string{icloud, containers}), "\n") {
		t.Fatalf("unchecked %v", s.KeptOutUnchecked)
	}
	var out bytes.Buffer
	RenderText(&out, p)
	for _, line := range []string{
		"  3 folders inside it (repositories, app folders, or folders macOS\n  protects) are added as excluded projects",
		"  macOS asks before an app reads ~/Library/Containers and ~/Library/Mobile Documents,\n  so they were not looked in and stay out of capture whole.",
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("plan lacks %q:\n%s", line, out.String())
		}
	}
	var js bytes.Buffer
	if err := RenderJSON(&js, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"kept_out_unchecked": [`) {
		t.Fatalf("json:\n%s", js.String())
	}

	// Added inside a protected location, the folder is looked in, within
	// the budget.
	r := newResolver(tr.env(), config.Config{}, Filters{})
	nested, err := r.findNested(context.Background(), filepath.Join(icloud, "com~apple~CloudDocs"), nil)
	if err != nil || len(nested.KeptOut) != 1 || len(nested.Unchecked) != 0 || !nested.Complete {
		t.Fatalf("inside iCloud Drive: %+v, %v", nested, err)
	}
	saved := nestedScanBudget
	nestedScanBudget = 1
	t.Cleanup(func() { nestedScanBudget = saved })
	tr.mkdir("home/Library/Mobile Documents/com~apple~CloudDocs/a/b")
	if nested, err = r.findNested(context.Background(), filepath.Join(icloud, "com~apple~CloudDocs"), nil); err != nil || nested.Complete {
		t.Fatalf("budget not applied inside a protected folder: %+v, %v", nested, err)
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
