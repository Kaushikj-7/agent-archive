package local

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// caseInsensitive reports whether dir's volume ignores case, as macOS's
// default APFS does; the case tests need one.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestCanonicalPathSpellsEachLocationOnce(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "Data", "Agent-Archive")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "Data"), link); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		target,
		target + "/",
		filepath.Join(dir, "Data", ".", "Agent-Archive"),
		filepath.Join(dir, "Data", "x", "..", "Agent-Archive"),
		filepath.Join(link, "Agent-Archive"),
	} {
		if got := CanonicalPath(spelling); got != target {
			t.Errorf("CanonicalPath(%q) = %q, want %q", spelling, got, target)
		}
		if !SameLocation(spelling, target) {
			t.Errorf("SameLocation(%q, %q) = false", spelling, target)
		}
	}
	// What does not exist yet is kept as written, under the existing
	// part's canonical spelling.
	if got, want := CanonicalPath(filepath.Join(link, "Agent-Archive", "New", "Dir")), filepath.Join(target, "New", "Dir"); got != want {
		t.Errorf("CanonicalPath of a missing tail = %q, want %q", got, want)
	}
	other := filepath.Join(dir, "Data", "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if SameLocation(target, other) || SameLocation(target, "") || SameLocation("", target) {
		t.Error("two different locations compared the same")
	}
	if !SameLocation("", "") {
		t.Error("two empty paths differ")
	}

	if !caseInsensitive(t, dir) {
		t.Log("case-sensitive volume: another case is another directory, so the case checks are skipped")
		return
	}
	for _, spelling := range []string{
		filepath.Join(dir, "data", "agent-archive"),
		filepath.Join(dir, "DATA", "AGENT-ARCHIVE"),
		strings.ToLower(filepath.Join(link, "Agent-Archive")),
	} {
		if got := CanonicalPath(spelling); got != target {
			t.Errorf("CanonicalPath(%q) = %q, want %q", spelling, got, target)
		}
		if !SameLocation(spelling, target) {
			t.Errorf("SameLocation(%q, %q) = false", spelling, target)
		}
	}
}

// Regression: CanonicalPath listed every directory along a path to find each
// component's spelling, so its cost grew with the siblings of every parent;
// with macOS's per-user $TMPDIR holding ~28,000 entries, canonicalizing 129
// candidate roots ran past matchProjects' 5 s deadline. It now spells a
// component without listing its directory, which this test shows by taking
// read permission away: a directory that cannot be listed still yields its
// entries' spellings, in either case where the volume ignores case and
// reports stored names (macOS with cgo).
func TestCanonicalPathDoesNotListDirectories(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(dir, "Sealed")
	target := filepath.Join(sealed, "Agent-Archive")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	insensitive := caseInsensitive(t, dir)
	if err := os.Chmod(sealed, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })
	if _, err := os.ReadDir(sealed); err == nil {
		t.Skip("this user can list a directory without read permission (root?)")
	}
	spellings := []string{target, filepath.Join(sealed, "x", "..", "Agent-Archive")}
	if _, stored := storedName(target); insensitive && !stored {
		t.Log("case-insensitive volume without stored names: another case needs a listing, so the case checks are skipped")
	} else if insensitive {
		spellings = append(spellings, filepath.Join(dir, "sealed", "agent-archive"), filepath.Join(dir, "SEALED", "AGENT-ARCHIVE"))
	}
	for _, spelling := range spellings {
		if got := CanonicalPath(spelling); got != target {
			t.Errorf("CanonicalPath(%q) = %q, want %q", spelling, got, target)
		}
	}
	// A name with no case to fold, and a missing tail, need no listing either.
	if got, want := CanonicalPath(filepath.Join(sealed, "2026", "x")), filepath.Join(sealed, "2026", "x"); got != want {
		t.Errorf("CanonicalPath of a missing tail = %q, want %q", got, want)
	}
}

// BenchmarkCanonicalPathSiblings canonicalizes a path whose parent holds few
// or many entries, spelled as listed and (where the volume ignores case) in
// another case. The time per operation should not grow with the siblings:
//
//	go test ./internal/local -run '^$' -bench CanonicalPathSiblings
func BenchmarkCanonicalPathSiblings(b *testing.B) {
	for _, siblings := range []int{10, 20000} {
		dir, err := filepath.EvalSymlinks(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		for i := range siblings {
			if err := os.WriteFile(filepath.Join(dir, "sibling-"+strconv.Itoa(i)), nil, 0o600); err != nil {
				b.Fatal(err)
			}
		}
		target := filepath.Join(dir, "Project")
		if err := os.Mkdir(target, 0o700); err != nil {
			b.Fatal(err)
		}
		for _, spelling := range []string{target, filepath.Join(dir, "project")} {
			if _, err := os.Stat(spelling); err != nil {
				continue // a case-sensitive volume: another case is another path
			}
			b.Run(strconv.Itoa(siblings)+"/"+filepath.Base(spelling), func(b *testing.B) {
				for b.Loop() {
					if got := CanonicalPath(spelling); got != target {
						b.Fatalf("CanonicalPath(%q) = %q, want %q", spelling, got, target)
					}
				}
			})
		}
	}
}
