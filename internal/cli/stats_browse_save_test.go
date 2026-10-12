package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lastRowOf is the last row of frame i of run, without color codes.
func lastRowOf(run screenRun, i int) string {
	rows := run.frames[i]
	return ansiEscape.ReplaceAllString(rows[len(rows)-1], "")
}

// h asks for a file name, saves the redacted page there without replacing
// anything, and says where it went: on the screen, and once more on the normal
// screen after it is left.
func TestStatsScreenSavesTheRedactedPage(t *testing.T) {
	t.Parallel()
	dir := shortDisplayFixtureDir(t)
	run := runScreen(t, screenOptions{width: 120, height: 30, dir: dir}, "h", "out.html", "\r", "d", "q")
	path := filepath.Join(dir, "out.html")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	if !strings.Contains(page, "<html") || !strings.Contains(page, "project A") || strings.Contains(page, "proj-api") || strings.Contains(page, "agent-archive/") {
		t.Errorf("the page is not the redacted page:\n%.600s", page)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, err %v; want 0600", info.Mode(), err)
	}
	// The prompt, the message, and the key bar again after the next key.
	if got := lastRowOf(run, 1); !strings.HasPrefix(got, "Save redacted HTML as (Enter: agent-archive-stats-2026-09-29.html, Esc cancels): ") {
		t.Errorf("prompt %q", got)
	}
	if got := lastRowOf(run, 2); !strings.HasPrefix(got, "Save redacted HTML as") || !strings.HasSuffix(strings.TrimRight(got, " "), "out.html") {
		t.Errorf("typed %q", got)
	}
	if got := lastRowOf(run, 3); got != "Saved "+path+" (names replaced)" {
		t.Errorf("message %q", got)
	}
	if got := lastRowOf(run, 4); !strings.Contains(got, "q quit") {
		t.Errorf("the message stayed past the next key: %q", got)
	}
	if !strings.HasSuffix(run.out.String(), leaveAltScreenSequence+"Wrote "+path+"\n") {
		t.Errorf("the path is not printed after leaving: %q", run.out.String()[max(len(run.out.String())-200, 0):])
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, ".agent-archive-stats-*")); len(tmp) != 0 {
		t.Errorf("temporary files left: %v", tmp)
	}
}

// An empty answer is the default name, today's date, in the current folder,
// and the page is the window on show.
func TestStatsScreenSavesUnderTheDefaultName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runScreen(t, screenOptions{width: 100, height: 30, dir: dir}, "w", "w", "h", "\r", "q")
	data, err := os.ReadFile(filepath.Join(dir, "agent-archive-stats-2026-09-29.html"))
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	runScreen(t, screenOptions{width: 100, height: 30, dir: other}, "h", "\r", "q")
	thirty, err := os.ReadFile(filepath.Join(other, "agent-archive-stats-2026-09-29.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == string(thirty) {
		t.Error("the page saved from the 7 day window is the 30 day page")
	}
}

// A name of spaces, or with spaces around it, is the name without them: the
// screen never makes a file called " ".
func TestStatsScreenSaveIgnoresSurroundingSpaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runScreen(t, screenOptions{width: 100, height: 30, dir: dir}, "h", "  ", "\r", "h", " named.html ", "\r", "q")
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"agent-archive-stats-2026-09-29.html", "named.html"}) {
		t.Errorf("saved as %q", names)
	}
}

// Nothing is ever replaced: an existing file, whatever it is, keeps its
// contents, and the message says to type another name (there is no --force).
func TestStatsScreenNeverReplacesAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.html"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "link.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := runScreen(t, screenOptions{width: 200, height: 30, dir: dir},
		"h", "keep.html", "\r", "h", "link.html", "\r", "h", "folder", "\r", "h", "nope/x.html", "\r", "h", "folder/", "\r", "q")
	for name, want := range map[string]string{"keep.html": "keep", "target.txt": "target"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
	messages := map[int]string{3: "already exists", 6: "symbolic link", 9: "is a directory", 12: "does not exist", 15: "is a directory"}
	for i, want := range messages {
		got := lastRowOf(run, i)
		if !strings.HasPrefix(got, "Not saved: ") || !strings.Contains(got, want) || strings.Contains(got, "--force") {
			t.Errorf("frame %d: %q, want it to say %q", i, got, want)
		}
	}
	if got := lastRowOf(run, 3); !strings.Contains(got, "type another name") {
		t.Errorf("no advice: %q", got)
	}
	if strings.Contains(run.out.String(), "Wrote ") {
		t.Error("a file that was not written is reported as written")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 4 {
		t.Errorf("the folder has %d entries, want the 4 it started with", len(entries))
	}
}

// Esc cancels the prompt and writes nothing; Backspace edits the name; keys
// that are not characters do nothing at the prompt; the name's end stays in
// view on a narrow terminal; and the prompt is one row at every width.
func TestStatsScreenSavePrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	long := strings.Repeat("abcdefghij", 10)
	run := runScreen(t, screenOptions{width: 50, height: 12, dir: dir},
		"h", "abc\x7f", "\x1b[A\x1b[6~\x1b[D", long, "\x1b", "d", "q")
	if got := lastRowOf(run, 2); !strings.HasSuffix(got, "ab") || !strings.HasPrefix(got, "Save as (Enter: default name): ") {
		t.Errorf("after Backspace: %q", got)
	}
	if got := lastRowOf(run, 3); !strings.HasSuffix(got, "ab") {
		t.Errorf("arrows edited the name: %q", got)
	}
	if got := lastRowOf(run, 4); !strings.Contains(got, "…") || !strings.HasSuffix(got, "hij") || visibleWidth(got) > 50 {
		t.Errorf("a long name: %q", got)
	}
	if got := lastRowOf(run, 5); got != "Not saved." {
		t.Errorf("Esc: %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a cancelled prompt wrote %v", entries)
	}
	if got := lastRowOf(run, 6); !strings.Contains(got, "q quit") {
		t.Errorf("after the message: %q", got)
	}
	for _, width := range []int{20, 30, 40, 50, 60, 80, 100} {
		r := runScreen(t, screenOptions{width: width, height: 6, dir: dir}, "h", long, "q")
		for i, rows := range r.frames {
			if w := visibleWidth(rows[len(rows)-1]); w > width || len(rows) != 6 {
				t.Errorf("width %d frame %d: last row is %d columns, %d rows", width, i, w, len(rows))
			}
		}
	}
	// Ctrl-D cancels too, and a resize keeps what is typed.
	fake := newFakeKeys("h", "ab", string(fakeResize), "\x04", "q")
	r := runScreen(t, screenOptions{width: 80, height: 10, dir: dir, fake: fake})
	if got := lastRowOf(r, 3); !strings.HasSuffix(got, "ab") {
		t.Errorf("a resize lost the name: %q", got)
	}
	if got := lastRowOf(r, 4); got != "Not saved." {
		t.Errorf("Ctrl-D: %q", got)
	}
}

// ~/ is the home folder, and an absolute path is used as it is.
func TestStatsScreenSavesToAHomeOrAbsolutePath(t *testing.T) {
	t.Parallel()
	home, elsewhere, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	runScreen(t, screenOptions{width: 200, height: 30, dir: cwd, tweak: func(e *Env) {
		e.LookupEnv = func(name string) (string, bool) { return home, name == "HOME" }
	}}, "h", "~/home.html", "\r", "h", filepath.Join(elsewhere, "abs.html"), "\r", "q")
	for _, path := range []string{filepath.Join(home, "home.html"), filepath.Join(elsewhere, "abs.html")} {
		if _, err := os.Stat(path); err != nil {
			t.Error(err)
		}
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("the current folder got %v", entries)
	}
}
