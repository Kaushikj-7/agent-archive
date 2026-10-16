package backfill

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Linux support, PR 3: Cursor keeps its data under the XDG config home
// instead of ~/Library/Application Support, and the macOS-only inputs
// (desktop-app folders, TCC-protected folders) are not consulted. Every test
// injects Environment.OS, so both branches run on any OS.

func linuxEnv(tr *tree, xdg string) Environment {
	env := tr.env()
	env.OS = platform.Linux
	env.Getenv = func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return xdg
		}
		return ""
	}
	return env
}

func TestCursorPathsFollowTheOperatingSystem(t *testing.T) {
	t.Parallel()
	home := filepath.FromSlash("/home/me")
	for _, tc := range []struct {
		name    string
		system  platform.OS
		xdg     string
		wantDir string
	}{
		{"darwin", platform.Darwin, "", "/home/me/Library/Application Support/Cursor"},
		{"darwin ignores XDG_CONFIG_HOME", platform.Darwin, "/xdg", "/home/me/Library/Application Support/Cursor"},
		{"linux", platform.Linux, "", "/home/me/.config/Cursor"},
		{"linux with XDG_CONFIG_HOME", platform.Linux, "/xdg", "/xdg/Cursor"},
		{"linux with a relative XDG_CONFIG_HOME", platform.Linux, "xdg", "/home/me/.config/Cursor"},
	} {
		env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home, OS: tc.system, Getenv: func(string) string { return tc.xdg }}
		wantDir := filepath.FromSlash(tc.wantDir)
		if got := filepath.Dir(filepath.Dir(filepath.Dir(env.cursorStateDatabase()))); got != wantDir {
			t.Errorf("%s: data folder %q, want %q", tc.name, got, wantDir)
		}
		if got, want := cursorWorkspaceStorage(env), filepath.Join(wantDir, "User", "workspaceStorage"); got != want {
			t.Errorf("%s: workspaceStorage %q, want %q", tc.name, got, want)
		}
		if got, want := env.cursorStateDatabase(), filepath.Join(wantDir, "User", "globalStorage", "state.vscdb"); got != want {
			t.Errorf("%s: state.vscdb %q, want %q", tc.name, got, want)
		}
	}
}

// On macOS the paths are what they always were, byte for byte, with OS
// unset (the real system) or "darwin".
func TestCursorPathsOnMacOSAreUnchanged(t *testing.T) {
	t.Parallel()
	home := "/Users/me"
	env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home, OS: platform.Darwin}
	if got, want := cursorWorkspaceStorage(env), "/Users/me/Library/Application Support/Cursor/User/workspaceStorage"; got != want {
		t.Errorf("workspaceStorage %q, want %q", got, want)
	}
	if got, want := env.cursorStateDatabase(), "/Users/me/Library/Application Support/Cursor/User/globalStorage/state.vscdb"; got != want {
		t.Errorf("state.vscdb %q, want %q", got, want)
	}
}

// Cursor's workspace folders are read from the Linux location on Linux and
// from the Library one on macOS, whichever has files.
func TestCursorWorkspaceFoldersLinux(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	linuxFolder := tr.path("home/linux-app")
	macFolder := tr.path("home/mac-app")
	xdgFolder := tr.path("home/xdg-app")
	tr.write("home/.config/Cursor/User/workspaceStorage/aaa/workspace.json", `{"folder":"file://`+linuxFolder+`"}`)
	tr.write("home/Library/Application Support/Cursor/User/workspaceStorage/bbb/workspace.json", `{"folder":"file://`+macFolder+`"}`)
	tr.write("xdg/Cursor/User/workspaceStorage/ccc/workspace.json", `{"folder":"file://`+xdgFolder+`"}`)

	for _, tc := range []struct {
		name string
		env  Environment
		want []string
	}{
		{"linux", linuxEnv(tr, ""), []string{linuxFolder}},
		{"linux with XDG_CONFIG_HOME", linuxEnv(tr, tr.path("xdg")), []string{xdgFolder}},
		{"linux with a relative XDG_CONFIG_HOME", linuxEnv(tr, "xdg"), []string{linuxFolder}},
		{"darwin", func() Environment { e := tr.env(); e.OS = platform.Darwin; return e }(), []string{macFolder}},
	} {
		if got := cursorWorkspaceFolders(tc.env); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A Cursor transcript's project slug is resolved through the Linux
// workspaceStorage when OS is Linux, and is unresolved when only the
// Library location has the file (and vice versa on macOS).
func TestCursorSlugResolvesThroughLinuxWorkspaceStorage(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	gone := filepath.Join(tr.home, "gone-app")
	tr.write("home/.config/Cursor/User/workspaceStorage/0f3a/workspace.json", `{"folder":"file://`+gone+`"}`)
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(gone), "agent-transcripts", "chat-ws", "chat-ws.jsonl"), cursorTranscript)

	linux := plan(t, linuxEnv(tr, ""), nil, config.Config{}, Filters{})
	if c := candidate(t, linux, "chat-ws"); c.Skip != "" || c.ProjectRoot != gone || c.ProjectExists {
		t.Fatalf("linux: %+v", c)
	}
	mac := tr.env()
	mac.OS = platform.Darwin
	if c := candidate(t, plan(t, mac, nil, config.Config{}, Filters{}), "chat-ws"); c.Skip != SkipProjectUnknown {
		t.Fatalf("darwin must not read ~/.config/Cursor: %+v", c)
	}
}

// The database chats' workspace.json lookup (cursordb.go) uses the same
// location.
func TestCursorDatabaseWorkspaceJSONOnLinux(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	ws := tr.repo("home/ws")
	tr.write("home/.config/Cursor/User/workspaceStorage/abc123/workspace.json", `{"folder":"file://`+ws+`"}`)
	env := linuxEnv(tr, "")
	chats := []CursorDatabaseChat{{ID: "wsjson", WorkspaceID: "abc123", CreatedAt: fixedNow.Add(-48 * time.Hour)}}
	env.CursorDatabase = fakeCursorDatabase(chats, map[string]cursorstore.Composer{"wsjson": syntheticChat("wsjson", nil, "a")}, nil, nil)
	p := plan(t, env, nil, config.Config{}, Filters{})
	if !p.CursorDatabaseChecked {
		t.Fatal("the database was not checked")
	}
	c := candidate(t, p, "wsjson")
	if c.ProjectRoot != ws {
		t.Fatalf("project %q, want %q (Skip %q)", c.ProjectRoot, ws, c.Skip)
	}
}

// The desktop-app workspace folders are macOS locations: on Linux a session
// there is an ordinary folder, not a scratch project.
func TestDesktopAppWorkspaceFoldersAreMacOSOnly(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	scratch := tr.mkdir("home/Library/Application Support/Claude/scratch-workspaces/a-b/scratch-1")
	codexDir := tr.mkdir("home/Documents/Codex/2026-09-20/plan-trip")
	mac := tr.env()
	mac.OS = platform.Darwin
	linux := linuxEnv(tr, "")

	if got := workspaceFolders(linux); len(got) != 0 {
		t.Fatalf("linux workspace folders: %v", got)
	}
	if got := workspaceFolders(mac); len(got) != 2 {
		t.Fatalf("darwin workspace folders: %v", got)
	}
	rl := newResolver(linux, config.Config{}, Filters{})
	if len(rl.workspaces) != 0 {
		t.Fatalf("linux resolver workspaces: %+v", rl.workspaces)
	}
	for _, cwd := range []string{scratch, codexDir} {
		if got := rl.resolve(cwd); got.kind != ProjectKindDirectory || got.root != cwd || got.skip != "" {
			t.Errorf("linux resolve(%q) = %+v, want a plain folder", cwd, got)
		}
		if got := newResolver(mac, config.Config{}, Filters{}).resolve(cwd); got.kind != ProjectKindScratch {
			t.Errorf("darwin resolve(%q) = %+v, want scratch", cwd, got)
		}
	}
}

// The TCC-protected folders are macOS only: on Linux none is protected, so
// the look inside an added folder reads inside ~/Library, ~/Documents and
// /Volumes like any other folder and keeps nothing out unread.
func TestPrivacyProtectedFoldersAreMacOSOnly(t *testing.T) {
	t.Parallel()
	home := "/home/me"
	id := func(p string) (string, error) { return p, nil }
	if got := privacyProtectedFolders(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home, OS: platform.Linux, EvalSymlinks: id}); len(got) != 0 {
		t.Fatalf("linux protected folders: %v", got)
	}
	if got := privacyProtectedFolders(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home, OS: platform.Darwin, EvalSymlinks: id}); len(got) == 0 {
		t.Fatal("darwin protected folders: none")
	}
	if protectedOutside("/home/me/Documents/x", "/home/me", privacyProtectedFolders(Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home, OS: platform.Linux, EvalSymlinks: id})) {
		t.Fatal("~/Documents is protected on Linux")
	}
}

func TestNestedLookReadsEveryFolderOnLinux(t *testing.T) {
	tr := newTree(t)
	library := tr.mkdir("home/Library")
	repo := tr.repo("home/Library/dev/repo")
	icloud := tr.mkdir("home/Library/Mobile Documents")
	icloudRepo := tr.repo("home/Library/Mobile Documents/com~apple~CloudDocs/repo")
	containers := tr.mkdir("home/Library/Containers")
	containerRepo := tr.repo("home/Library/Containers/com.example/repo")

	env := linuxEnv(tr, "")
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

	r := newResolver(env, config.Config{}, Filters{})
	nested, err := r.findNested(context.Background(), library, nil)
	if err != nil || !nested.Complete {
		t.Fatalf("nested: %+v, %v", nested, err)
	}
	if len(nested.Unchecked) != 0 {
		t.Fatalf("folders kept out unread on Linux: %v", nested.Unchecked)
	}
	if got, want := strings.Join(sorted(nested.KeptOut), "\n"), strings.Join(sorted([]string{repo, icloudRepo, containerRepo}), "\n"); got != want {
		t.Fatalf("kept out %v, want the three repositories", nested.KeptOut)
	}
	if len(touched) == 0 {
		t.Fatal("the folders were never looked in, though nothing is protected on Linux")
	}

	// The same tree on macOS keeps iCloud Drive and containers out unread.
	mac := tr.env()
	mac.OS = platform.Darwin
	nested, err = newResolver(mac, config.Config{}, Filters{}).findNested(context.Background(), library, nil)
	if err != nil || len(nested.Unchecked) != 2 {
		t.Fatalf("darwin: %+v, %v", nested, err)
	}
}

func TestDefaultTempDirsByOS(t *testing.T) {
	t.Parallel()
	mac := []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	linux := []string{"/tmp", "/var/tmp"}
	if got := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Darwin}).DefaultTempDirs(); !reflect.DeepEqual(got, mac) {
		t.Errorf("darwin %v, want %v", got, mac)
	}
	if got := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Linux}).DefaultTempDirs(); !reflect.DeepEqual(got, linux) {
		t.Errorf("linux %v, want %v", got, linux)
	}
	// An unknown system errs toward more temporary directories, so it skips
	// every folder either system would.
	unknown := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Unknown}).DefaultTempDirs()
	for _, dir := range append(slices.Clone(mac), linux...) {
		if !slices.Contains(unknown, dir) {
			t.Errorf("unknown system does not skip %s: %v", dir, unknown)
		}
	}
	// An Environment without TempDirs answers for its own system.
	if got := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Linux}).tempDirs(); !reflect.DeepEqual(got, linux) {
		t.Errorf("linux environment %v", got)
	}
	if got := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Darwin}).tempDirs(); !reflect.DeepEqual(got, mac) {
		t.Errorf("darwin environment %v", got)
	}
	// The result is the caller's to change.
	first := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Linux}).DefaultTempDirs()
	first[0] = "/changed"
	if got := (Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), OS: platform.Linux}).DefaultTempDirs(); !reflect.DeepEqual(got, linux) {
		t.Errorf("a caller's change reached the next answer: %v", got)
	}
}

// An operating system the program does not know is not the Linux layout: it
// has no Cursor database or workspace storage (Cursor reads as not
// installed), no desktop-app workspace folders and no protected folders, and
// nothing is looked for under XDG_CONFIG_HOME.
func TestUnknownSystemFailsClosed(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	xdg := tr.path("xdg")
	tr.write("home/.config/Cursor/User/workspaceStorage/aaa/workspace.json", `{"folder":"file://`+tr.path("home/linux-app")+`"}`)
	tr.write("xdg/Cursor/User/workspaceStorage/ccc/workspace.json", `{"folder":"file://`+tr.path("home/xdg-app")+`"}`)
	env := linuxEnv(tr, xdg)
	env.OS = platform.Unknown

	if got := env.cursorStateDatabase(); got != "" {
		t.Errorf("state.vscdb %q, want none", got)
	}
	if got := cursorWorkspaceStorage(env); got != "" {
		t.Errorf("workspaceStorage %q, want none", got)
	}
	if got := cursorWorkspaceFolders(env); len(got) != 0 {
		t.Errorf("workspace folders %v, want none", got)
	}
	if got := workspaceFolders(env); len(got) != 0 {
		t.Errorf("desktop app workspaces %v, want none", got)
	}
	if got := privacyProtectedFolders(env); len(got) != 0 {
		t.Errorf("protected folders %v, want none", got)
	}
	res, err := CursorDatabaseReaderFor(env)(context.Background())
	// A missing database is checked with no chats: Cursor is not installed.
	if err != nil || !res.Checked || len(res.Chats) != 0 || res.Reason != "" {
		t.Errorf("Cursor's database on an unknown system: %+v, %v; want Cursor not installed", res, err)
	}
}

func TestVarTmpIsATemporaryDirectoryOnLinux(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: tr.home, OS: platform.Linux, EvalSymlinks: func(p string) (string, error) { return p, nil }, Stat: func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }}
	got := newResolver(env, config.Config{}, Filters{}).resolve("/var/tmp/run-1")
	if got.kind != ProjectKindTemporary || got.skip != SkipTemporaryDirectory {
		t.Fatalf("/var/tmp/run-1: %+v", got)
	}
}

// Undo reads a resumed Cursor chat's lastUpdatedAt from the database at the
// Environment's location: the Linux one when OS is Linux, so a database
// under ~/Library is not consulted there.
func TestUndoReadsTheLinuxCursorDatabase(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	admitted := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "s-new", NativeSessionID: "new", Harness: archive.Harness{Name: "cursor"},
		SourceKind: archive.SourceKindCursorSQLite, SourceKey: "new", AdmittedAt: admitted, Origin: archive.SessionOriginImport}
	rows := chatRows("new", map[string]any{"lastUpdatedAt": admitted.Add(time.Hour).UnixMilli()}, "a")
	env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home, OS: platform.Linux, Getenv: func(string) string { return "" }}

	// A database only at the macOS location is not seen on Linux.
	writeCursorDB(t, (Environment{Home: home, OS: platform.Darwin, NativePaths: builtin.NewBuiltins()}).cursorStateDatabase(), false, rows)
	if resumed, unknown, err := resumedSinceImport(env, store, reg, state.Request{}); err != nil || resumed || unknown {
		t.Fatalf("macOS-location database on Linux: resumed %v, unknown %v, err %v", resumed, unknown, err)
	}
	writeCursorDB(t, filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb"), false, rows)
	if resumed, unknown, err := resumedSinceImport(env, store, reg, state.Request{}); err != nil || !resumed || unknown {
		t.Fatalf("Linux-location database: resumed %v, unknown %v, err %v", resumed, unknown, err)
	}
}
