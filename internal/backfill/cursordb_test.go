package backfill

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// composerJSON builds a composerData value with headers message headers;
// extra fields are merged in.
func composerJSON(id string, headers int, extra map[string]any) string {
	hs := make([]map[string]any, headers)
	for i := range hs {
		hs[i] = map[string]any{"bubbleId": "m", "type": 1}
	}
	v := map[string]any{"_v": 18, "composerId": id, "fullConversationHeadersOnly": hs}
	for k, x := range extra {
		if x == nil {
			delete(v, k)
		} else {
			v[k] = x
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func millis(t time.Time) int64 { return t.UnixMilli() }

// writeCursorDB creates a synthetic state.vscdb at path, in WAL mode when
// wal is set, and closes it, so SQLite checkpoints and removes its side
// files.
func writeCursorDB(t *testing.T, path string, wal bool, rows map[string]any) {
	t.Helper()
	db := openCursorWriter(t, path, wal)
	insertCursorRows(t, db, rows)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// openCursorWriter opens path as Cursor does, with Cursor's table.
func openCursorWriter(t *testing.T, path string, wal bool) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	stmts := []string{`CREATE TABLE IF NOT EXISTS ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`}
	if wal {
		stmts = append([]string{`PRAGMA journal_mode=WAL`}, stmts...)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// closeAtEnd closes c once the test and its subtests finish, and fails the
// test if closing fails.
func closeAtEnd(t *testing.T, c io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func insertCursorRows(t *testing.T, db execer, rows map[string]any) {
	t.Helper()
	for k, v := range rows {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

type fileState struct {
	size    int64
	mode    os.FileMode
	modTime time.Time
	sum     [32]byte
}

// snapshotDir records every file in dir, and dir itself, byte for byte.
func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	out["."] = fileState{mode: info.Mode(), modTime: info.ModTime()}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		var sum [32]byte
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			sum = sha256.Sum256(data)
		}
		out[e.Name()] = fileState{size: info.Size(), mode: info.Mode(), modTime: info.ModTime(), sum: sum}
	}
	return out
}

func assertUnchanged(t *testing.T, dir string, before map[string]fileState) {
	t.Helper()
	after := snapshotDir(t, dir)
	for name, a := range after {
		if b, ok := before[name]; !ok {
			t.Errorf("%s was created", name)
		} else if !reflect.DeepEqual(a, b) {
			t.Errorf("%s changed: %+v -> %+v", name, b, a)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("%s was removed", name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

func readCursor(t *testing.T, home string) CursorDatabaseResult {
	t.Helper()
	res, err := CursorDatabaseReader(home)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func chatIDs(res CursorDatabaseResult) []string {
	var ids []string
	for _, c := range res.Chats {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestCursorDatabaseReader reads a database with no side files, as Cursor
// leaves it when closed, in both journal modes.
func TestCursorDatabaseReader(t *testing.T) {
	sept10 := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	rows := map[string]any{
		"composerData:a": composerJSON("a", 2, map[string]any{"createdAt": millis(sept10)}),
		// Stored as a BLOB, as Cursor does.
		"composerData:b": []byte(composerJSON("b", 1, map[string]any{
			"createdAt":           millis(sept10.AddDate(0, 0, 11)),
			"workspaceIdentifier": map[string]any{"id": "w", "uri": map[string]any{"$mid": 1, "scheme": "file", "fsPath": "/work/site", "path": "/work/site", "external": "file:///work/site"}},
		})),
		"composerData:c": composerJSON("c", 3, map[string]any{"workspaceIdentifier": map[string]any{"uri": "file:///work/other%20dir"}}),
		"composerData:d": composerJSON("d", 3, map[string]any{"workspaceIdentifier": map[string]any{"uri": map[string]any{"scheme": "vscode-remote", "path": "/srv/x"}}}),
		// workspaceIdentifier in shapes the count does not need.
		"composerData:ws-empty":  composerJSON("ws-empty", 1, map[string]any{"workspaceIdentifier": ""}),
		"composerData:ws-list":   composerJSON("ws-list", 1, map[string]any{"workspaceIdentifier": []any{}}),
		"composerData:ws-no-uri": composerJSON("ws-no-uri", 1, map[string]any{"workspaceIdentifier": map[string]any{"id": "x", "uri": 7}}),
		// An older chat keeps its messages inline, without headers.
		"composerData:old": composerJSON("old", 0, map[string]any{"_v": 2, "fullConversationHeadersOnly": nil, "conversation": []any{map[string]any{"type": 1}}}),
		// The parent is counted; its subagents are part of it, whether a
		// draft or a chat with a file names them.
		"composerData:parent": composerJSON("parent", 2, map[string]any{"subagentComposerIds": []string{"sub1"}}),
		"composerData:sub1":   composerJSON("sub1", 2, nil),
		"composerData:sub2":   composerJSON("sub2", 2, nil),
		"composerData:draft":  composerJSON("draft", 2, map[string]any{"isDraft": true, "subagentComposerIds": []string{"sub2"}}),
		// Not counted: no messages at all, and a value that is NULL.
		"composerData:empty":     composerJSON("empty", 0, nil),
		"composerData:old-empty": composerJSON("old-empty", 0, map[string]any{"_v": 2, "fullConversationHeadersOnly": nil, "conversation": []any{}}),
		"composerData:null":      nil,
		// Counted by the reader; the plan leaves out chats with a file. The
		// one without a composerId is known by its key.
		"composerData:k1": composerJSON("k1", 4, nil),
		"composerData:k2": `{"fullConversationHeadersOnly":[{}]}`,
		// Other rows are never read.
		"bubbleId:a:1":  `{"type":1,"text":"never read"}`,
		"composerDataX": `not json`,
	}
	for _, wal := range []bool{false, true} {
		t.Run(fmt.Sprintf("wal=%v", wal), func(t *testing.T) {
			home := t.TempDir()
			path := CursorStateDatabase(home)
			writeCursorDB(t, path, wal, rows)
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			if len(before) != 2 {
				t.Fatalf("side files before the read: %v", before)
			}

			res := readCursor(t, home)
			if !res.Checked || res.Reason != "" {
				t.Fatalf("checked %v, reason %q", res.Checked, res.Reason)
			}
			want := []string{"a", "b", "c", "d", "k1", "k2", "old", "parent", "ws-empty", "ws-list", "ws-no-uri"}
			if got := chatIDs(res); !reflect.DeepEqual(got, want) {
				t.Fatalf("chats %v, want %v", got, want)
			}
			byID := map[string]CursorDatabaseChat{}
			for _, c := range res.Chats {
				byID[c.ID] = c
			}
			for id, w := range map[string]CursorDatabaseChat{
				"a": {ID: "a", KeyID: "a", CreatedAt: sept10},
				"b": {ID: "b", KeyID: "b", CreatedAt: sept10.AddDate(0, 0, 11), Folder: "/work/site", WorkspaceID: "w", WorkspaceIdentifier: true},
				"c": {ID: "c", KeyID: "c", Folder: "/work/other dir", WorkspaceIdentifier: true},
				// A remote or unknown reference resolves to no folder but is recorded.
				"d":         {ID: "d", KeyID: "d", WorkspaceIdentifier: true},
				"ws-empty":  {ID: "ws-empty", KeyID: "ws-empty", WorkspaceIdentifier: true},
				"ws-list":   {ID: "ws-list", KeyID: "ws-list", WorkspaceIdentifier: true},
				"ws-no-uri": {ID: "ws-no-uri", KeyID: "ws-no-uri", WorkspaceID: "x", WorkspaceIdentifier: true},
				"k2":        {ID: "k2", KeyID: "k2"},
			} {
				if got := byID[id]; !reflect.DeepEqual(got, w) {
					t.Errorf("%s: %+v, want %+v", id, got, w)
				}
			}
			// Subagent chats, listed under the chat that names them.
			if want := map[string][]string{"parent": {"sub1"}, "draft": {"sub2"}}; !reflect.DeepEqual(res.Subagents, want) {
				t.Errorf("subagents %v, want %v", res.Subagents, want)
			}
			if res.ReadSnapshot == nil || res.Close == nil {
				t.Fatal("a checked result can't read its chats")
			}
			if err := res.Close(); err != nil {
				t.Fatal(err)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestCursorDatabaseReaderNewerFormat: rows with a newer _v are still read
// when the fields the count needs decode, and are counted for the plan.
func TestCursorDatabaseReaderNewerFormat(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeCursorDB(t, CursorStateDatabase(home), false, map[string]any{
		"composerData:known": composerJSON("known", 1, nil),
		"composerData:newer": composerJSON("newer", 1, map[string]any{"_v": 18 + 1, "someNewField": map[string]any{"x": 1}}),
		"composerData:draft": composerJSON("draft", 1, map[string]any{"_v": 18 + 7, "isDraft": true}),
	})
	res := readCursor(t, home)
	if !res.Checked || res.NewerFormat != 2 || !reflect.DeepEqual(chatIDs(res), []string{"known", "newer"}) {
		t.Fatalf("%+v", res)
	}
}

// TestCursorDatabaseReaderSymlink follows a symlinked database to find its
// side files: read beside the link, a live WAL would be missed and the
// chats only it holds silently left out.
func TestCursorDatabaseReaderSymlink(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere", "state.vscdb")
	w := startCursorWriter(t, target, true)
	w.do("a", "checkpoint", "b", "c")
	link := CursorStateDatabase(home)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dir, linkDir := filepath.Dir(target), filepath.Dir(link)
	before, linkBefore := snapshotDir(t, dir), snapshotDir(t, linkDir)
	if res := readCursor(t, home); !res.Checked || !reflect.DeepEqual(chatIDs(res), []string{"a", "b", "c"}) {
		t.Fatalf("%+v", res)
	}
	assertUnchanged(t, dir, before)
	assertUnchanged(t, linkDir, linkBefore)

	// A link to nothing is no database.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing.vscdb"), link); err != nil {
		t.Fatal(err)
	}
	if res := readCursor(t, home); !res.Checked || len(res.Chats) != 0 {
		t.Fatalf("dangling link: %+v", res)
	}
}

type queryIndexHost struct {
	db *sql.DB
	t  *testing.T
}

func (h queryIndexHost) Query(ctx context.Context, query string, visit func(agentapi.DatabaseRecord) error) error {
	rows, err := h.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			h.t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	if got := strings.Join(plan, "; "); !strings.Contains(got, "USING INDEX") || !strings.Contains(got, "key>? AND key<?") {
		h.t.Fatalf("query plan %q does not search the key index", got)
	}
	return (databaseCatalogHost{h.db}).Query(ctx, query, visit)
}

func TestCursorDatabaseQueryUsesIndex(t *testing.T) {
	t.Parallel()
	db := openCursorWriter(t, filepath.Join(t.TempDir(), "state.vscdb"), false)
	closeAtEnd(t, db)
	provider, _ := builtin.NewBuiltins().LookupDatabaseCatalog("cursor")
	if _, err := provider.InspectCatalog(t.Context(), queryIndexHost{db, t}); err != nil {
		t.Fatal(err)
	}
}

func TestCursorDatabaseReaderMissing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if res := readCursor(t, home); !res.Checked || len(res.Chats) != 0 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading a missing database created something: %v", err)
	}
}

func TestCursorDatabaseReaderNotChecked(t *testing.T) {
	good := map[string]any{"composerData:a": composerJSON("a", 1, nil)}
	value := func(v string) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			t.Helper()
			writeCursorDB(t, path, false, map[string]any{"composerData:a": composerJSON("a", 1, nil), "composerData:b": v})
		}
	}
	for name, tc := range map[string]struct {
		setup  func(t *testing.T, path string)
		reason CursorUncheckedReason
	}{
		"garbage": {func(t *testing.T, path string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 100), 0o644); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedUnreadable},
		"truncated": {func(t *testing.T, path string) {
			t.Helper()
			writeCursorDB(t, path, false, good)
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedUnreadable},
		"directory": {func(t *testing.T, path string) {
			t.Helper()
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedUnreadable},
		"no table": {func(t *testing.T, path string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `CREATE TABLE ItemTable (key TEXT, value BLOB)`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedUnknownFormat},
		"not json":               {value("not json"), CursorUncheckedUnknownFormat},
		"not an object":          {value(`[1,2]`), CursorUncheckedUnknownFormat},
		"isDraft not a bool":     {value(`{"composerId":"b","isDraft":"no","fullConversationHeadersOnly":[{}]}`), CursorUncheckedUnknownFormat},
		"headers not a list":     {value(`{"composerId":"b","fullConversationHeadersOnly":{"n":3}}`), CursorUncheckedUnknownFormat},
		"conversation not alist": {value(`{"composerId":"b","conversation":"hi"}`), CursorUncheckedUnknownFormat},
		"newer _v, bad isDraft":  {value(composerJSON("b", 1, map[string]any{"_v": 18 + 1, "isDraft": 1})), CursorUncheckedUnknownFormat},
		"_v not a number":        {value(composerJSON("b", 1, map[string]any{"_v": "3"})), CursorUncheckedUnknownFormat},
		"_v zero":                {value(composerJSON("b", 1, map[string]any{"_v": 0})), CursorUncheckedUnknownFormat},
		// A WAL database with one side file and not the other can't be
		// read without SQLite creating the missing one. (An empty -wal
		// alone is the exception; see TestCursorDatabaseReaderStrayWAL.)
		"wal without shm": {func(t *testing.T, path string) {
			t.Helper()
			writeCursorDB(t, path, true, good)
			if err := os.WriteFile(path+"-wal", []byte("frames"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedUnreadable},
		"shm without wal": {func(t *testing.T, path string) {
			t.Helper()
			writeCursorDB(t, path, true, good)
			if err := os.WriteFile(path+"-shm", nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedUnreadable},
		// A rollback journal is a write in progress, or a hot journal only a
		// writer may roll back.
		"journal": {func(t *testing.T, path string) {
			t.Helper()
			writeCursorDB(t, path, false, good)
			if err := os.WriteFile(path+"-journal", []byte("journal"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, CursorUncheckedLocked},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := CursorStateDatabase(home)
			tc.setup(t, path)
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			res := readCursor(t, home)
			if res.Checked || len(res.Chats) != 0 || res.Reason != tc.reason {
				t.Fatalf("checked %v, %d chats, reason %q, want %q", res.Checked, len(res.Chats), res.Reason, tc.reason)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestCursorDatabaseReaderStrayWAL: the empty -wal SQLite can leave when
// Cursor quits just as a reader opens the database has no frames to replay,
// so the database is counted as closed, and left exactly as it was, rather
// than being unreadable until Cursor next runs.
func TestCursorDatabaseReaderStrayWAL(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := CursorStateDatabase(home)
	writeCursorDB(t, path, true, map[string]any{"composerData:a": composerJSON("a", 1, nil)})
	if err := os.WriteFile(path+"-wal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	if res := readCursor(t, home); !res.Checked || !reflect.DeepEqual(chatIDs(res), []string{"a"}) {
		t.Fatalf("%+v", res)
	}
	assertUnchanged(t, dir, before)
}

// TestCursorDatabaseReaderChangedDuringRead: with Cursor closed the database
// is read immutable, so a write that lands during the read is caught
// afterwards and the read is not trusted.
func TestCursorDatabaseReaderChangedDuringRead(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, path string){
		"modified": func(t *testing.T, path string) {
			t.Helper()
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		},
		"grown": func(t *testing.T, path string) {
			t.Helper()
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(make([]byte, 4096)); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		},
		"replaced": func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(path)
			// Holding the old file open keeps its inode from being reused
			// for the new one, as Linux otherwise does at once; a byte-
			// identical file on the same inode is not a change at all.
			old, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = old.Close() }()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		// Same size, time, and inode; only SQLite's change counter moved.
		"header": func(t *testing.T, path string) {
			t.Helper()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte{0xff}, 27); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"cursor started": func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path+"-wal", nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := CursorStateDatabase(home)
			writeCursorDB(t, path, true, map[string]any{"composerData:a": composerJSON("a", 1, nil)})
			res := readCursorDatabase(context.Background(), builtin.NewBuiltins(), path, cursorstore.Options{AfterImmutableRead: func(p string) { change(t, p) }})
			if res.Checked || res.Reason != CursorUncheckedChangedDuringRead {
				t.Fatalf("checked %v, reason %q", res.Checked, res.Reason)
			}
		})
	}
}

// cursorWriter is another process standing in for Cursor. It is a separate
// process because SQLite shares one shared-memory mapping between the
// connections of a process, so an in-process writer would not show whether
// the reader leaves Cursor's -shm file alone, and because a crashed writer
// is a killed process.
type cursorWriter struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies *bufio.Scanner
}

func startCursorWriter(t *testing.T, path string, wal bool) *cursorWriter {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not t.Context(): the writer is stopped by closing its stdin in the
	// cleanup, which runs after that context is cancelled.
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestCursorWriterProcess$")
	cmd.Env = append(os.Environ(), "BACKFILL_CURSOR_WRITER_DB="+path, fmt.Sprintf("BACKFILL_CURSOR_WRITER_WAL=%v", wal))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &cursorWriter{t: t, cmd: cmd, stdin: stdin, replies: bufio.NewScanner(stdout)}
	t.Cleanup(func() {
		// Closing stdin ends the writer. Either may fail after kill, which
		// has already stopped it.
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return w
}

// do sends commands: "begin", "checkpoint", or a chat ID to insert.
func (w *cursorWriter) do(commands ...string) {
	w.t.Helper()
	for _, c := range commands {
		if _, err := fmt.Fprintln(w.stdin, c); err != nil {
			w.t.Fatal(err)
		}
		if !w.replies.Scan() || w.replies.Text() != "ok" {
			w.t.Fatalf("the writer did not do %s: %q", c, w.replies.Text())
		}
	}
}

// kill ends the writer without closing its database, as a crash does.
func (w *cursorWriter) kill() {
	w.t.Helper()
	if err := w.cmd.Process.Kill(); err != nil {
		w.t.Fatal(err)
	}
	// The error only reports the kill.
	_ = w.cmd.Wait()
}

// TestCursorWriterProcess is the writer process of cursorWriter: it never
// checkpoints, and runs one command per line of stdin until stdin closes.
func TestCursorWriterProcess(t *testing.T) {
	t.Parallel()
	path := os.Getenv("BACKFILL_CURSOR_WRITER_DB")
	if path == "" {
		t.Skip("run by startCursorWriter")
	}
	db := openCursorWriter(t, path, os.Getenv("BACKFILL_CURSOR_WRITER_WAL") == "true")
	closeAtEnd(t, db)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, conn)
	if _, err := conn.ExecContext(context.Background(), `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch c := in.Text(); c {
		case "begin":
			if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
				t.Fatal(err)
			}
		case "checkpoint":
			if _, err := conn.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
		case "full:" + strings.TrimPrefix(c, "full:"):
			// A whole chat the composer filter accepts.
			for k, v := range chatRows(strings.TrimPrefix(c, "full:"), nil, "hi", "Hello.") {
				if _, err := conn.ExecContext(context.Background(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, v); err != nil {
					t.Fatal(err)
				}
			}
		default:
			if _, err := conn.ExecContext(context.Background(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "composerData:"+c, composerJSON(c, 1, nil)); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println("ok")
	}
	if err := in.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestCursorDatabaseReaderLive reads a WAL database in place while Cursor
// holds it open with writes not yet checkpointed.
func TestCursorDatabaseReaderLive(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := CursorStateDatabase(home)
	w := startCursorWriter(t, path, true)
	w.do("a", "b")

	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	if _, ok := before["state.vscdb-wal"]; !ok {
		t.Fatal("the writer has no -wal file")
	}
	if res := readCursor(t, home); !res.Checked || !reflect.DeepEqual(chatIDs(res), []string{"a", "b"}) {
		t.Fatalf("%+v", res)
	}
	assertUnchanged(t, dir, before)

	// Cursor keeps writing after the read.
	w.do("c")
	if res := readCursor(t, home); !res.Checked || len(res.Chats) != 3 {
		t.Fatalf("after a write: %+v", res)
	}
}

// TestCursorDatabaseReaderStaleSideFiles reads, in place, the -wal and -shm a
// killed Cursor left behind, including the writes only the -wal holds.
func TestCursorDatabaseReaderStaleSideFiles(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := CursorStateDatabase(home)
	w := startCursorWriter(t, path, true)
	w.do("a", "b")
	w.kill()

	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	if _, ok := before["state.vscdb-shm"]; !ok {
		t.Fatal("the killed writer left no -shm file")
	}
	if res := readCursor(t, home); !res.Checked || !reflect.DeepEqual(chatIDs(res), []string{"a", "b"}) {
		t.Fatalf("%+v", res)
	}
	assertUnchanged(t, dir, before)
}

// TestCursorRecoveryReaderSettledSideFiles: the -wal and -shm a quit Cursor
// left behind after checkpointing everything (the -wal empty) don't make the
// recovery inventory locked: the catalog and each chat are read, and nothing
// beside the database changes. Frames left in the -wal still lock it.
func TestCursorRecoveryReaderSettledSideFiles(t *testing.T) {
	t.Parallel()
	for _, frames := range []bool{false, true} {
		t.Run(fmt.Sprintf("frames=%v", frames), func(t *testing.T) {
			t.Parallel()
			env := newTree(t).env()
			path := env.cursorStateDatabase()
			w := startCursorWriter(t, path, true)
			w.do("full:a", "checkpoint")
			if frames {
				w.do("full:b")
			}
			w.kill()
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			if _, ok := before["state.vscdb-shm"]; !ok {
				t.Fatal("the killed writer left no -shm file")
			}
			if wal, ok := before["state.vscdb-wal"]; !ok || (wal.size == 0) == frames {
				t.Fatalf("-wal %+v present %v", wal, ok)
			}
			res, err := CursorRecoveryDatabaseReaderFor(env)(t.Context(), cursorRecoveryRows, cursorRecoveryBytes)
			if err != nil {
				t.Fatal(err)
			}
			if frames {
				if res.Checked || res.Reason != CursorUncheckedLocked {
					t.Fatalf("checked %v, reason %q", res.Checked, res.Reason)
				}
				assertUnchanged(t, dir, before)
				return
			}
			if !res.Checked || !reflect.DeepEqual(chatIDs(res), []string{"a"}) {
				t.Fatalf("%+v", res)
			}
			c, snap, err := res.ReadRecoverySnapshot(t.Context(), "a", &cursorRecoveryReadBudget{remaining: cursorRecoveryBytes, rows: cursorRecoveryRecords})
			if err != nil || len(c.Composer) == 0 || len(c.Bubbles) != 2 {
				t.Fatal(c, err)
			}
			if err := errors.Join(snap.Close(), res.Close()); err != nil {
				t.Fatal(err)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestCursorDatabaseReaderJournal: a rollback-journal database with a write
// in progress, and the hot journal a killed writer leaves, are not checked.
func TestCursorDatabaseReaderJournal(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(fmt.Sprintf("killed=%v", killed), func(t *testing.T) {
			home := t.TempDir()
			path := CursorStateDatabase(home)
			writeCursorDB(t, path, false, map[string]any{"composerData:a": composerJSON("a", 1, nil)})
			w := startCursorWriter(t, path, false)
			w.do("begin", "b")
			if killed {
				w.kill()
			}
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			if _, ok := before["state.vscdb-journal"]; !ok {
				t.Fatal("the writer has no -journal file")
			}
			if res := readCursor(t, home); res.Checked || res.Reason != CursorUncheckedLocked {
				t.Fatalf("checked %v, reason %q", res.Checked, res.Reason)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

func TestCursorDatabaseReaderCancelled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeCursorDB(t, CursorStateDatabase(home), false, map[string]any{"composerData:a": composerJSON("a", 1, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CursorDatabaseReader(home)(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// TestCursorDatabasePlan runs the real reader under BuildPlan, with Cursor
// closed. A chat with a transcript on disk is the file's session, the
// archive's reasons come first, and the filters apply as they do to file
// sessions: a chat without the field a filter needs does not match it.
// Nothing next to the database changes.
func TestCursorDatabasePlan(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	other := tr.repo("home/other")
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(site), "agent-transcripts", "k1", "k1.jsonl"), cursorTranscript)
	uri := func(dir string) map[string]any {
		return map[string]any{"uri": map[string]any{"$mid": 1, "scheme": "file", "fsPath": dir, "path": dir}}
	}
	sept := func(day int) int64 { return millis(time.Date(2026, 9, day, 18, 0, 0, 0, time.UTC)) }
	path := CursorStateDatabase(tr.home)
	writeCursorDB(t, path, true, mergeRows(
		chatRows("k1", map[string]any{"createdAt": sept(20), "workspaceIdentifier": uri(site)}, "a", "b", "c"),
		chatRows("early", map[string]any{"createdAt": sept(2), "workspaceIdentifier": uri(site)}, "a"),
		chatRows("site", map[string]any{"createdAt": sept(21), "workspaceIdentifier": uri(site)}, "a"),
		chatRows("other", map[string]any{"createdAt": sept(21), "workspaceIdentifier": uri(other)}, "a"),
		chatRows("bare", map[string]any{"createdAt": nil}, "a"),
		chatRows("archived", map[string]any{"createdAt": sept(21), "workspaceIdentifier": uri(site)}, "a"),
		chatRows("draft", map[string]any{"isDraft": true, "createdAt": sept(21)}, "a"),
	))
	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	env := tr.env()
	env.CursorDatabase = CursorDatabaseReader(tr.home)
	st := states{"archived": SkipAlreadyArchived}

	// "bare" names no workspace, so it has no project (and no start).
	for _, tc := range []struct {
		name      string
		filters   Filters
		imported  int
		filtered  int
		noProject int
	}{
		{"no filters", Filters{}, 3, 0, 1},
		{"since", Filters{Since: "2026-09-10"}, 2, 2, 0},
		{"until", Filters{Until: "2026-09-10"}, 1, 3, 0},
		{"project", Filters{Projects: []string{site}}, 2, 2, 0},
		{"project and since", Filters{Projects: []string{site}, Since: "2026-09-10"}, 1, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan(t, env, st, config.Config{}, tc.filters)
			want := map[SkipReason]int{"": tc.imported, SkipFilteredOut: tc.filtered, SkipProjectUnknown: tc.noProject, SkipAlreadyArchived: 1}
			for reason, n := range want {
				if n == 0 {
					delete(want, reason)
				}
			}
			if got := databaseOutcomes(p); !p.CursorDatabaseChecked || !reflect.DeepEqual(got, want) {
				t.Fatalf("checked %v, outcomes %v, want %v", p.CursorDatabaseChecked, got, want)
			}
			for _, c := range databaseCandidates(p) {
				//lint:ignore LV1001 native session IDs are external Cursor chat IDs, not a closed set
				if c.NativeSessionID == "k1" || c.NativeSessionID == "draft" {
					t.Fatalf("%s planned from the database", c.NativeSessionID)
				}
			}
			if p.Found() != len(p.Candidates) {
				t.Fatalf("found %d of %d candidates", p.Found(), len(p.Candidates))
			}
			var out strings.Builder
			RenderText(&out, p)
			if strings.Contains(out.String(), "were not checked") {
				t.Fatalf("a checked database is reported unchecked:\n%s", out.String())
			}
		})
	}

	// --harness without cursor does not open the database at all.
	p := plan(t, env, st, config.Config{}, Filters{Harnesses: []string{"codex"}})
	if p.CursorDatabaseChecked || len(databaseCandidates(p)) != 0 || p.Found() != len(p.Candidates) {
		t.Fatalf("harness codex: %+v, skipped %v", p, p.Skipped())
	}
	var out strings.Builder
	RenderText(&out, p)
	if strings.Contains(out.String(), "were not checked") {
		t.Fatalf("an unchecked line for a harness filtered out:\n%s", out.String())
	}
	assertUnchanged(t, dir, before)

	// A database that can't be read is not checked, says why, and the plan
	// still succeeds.
	if err := os.WriteFile(path, []byte("SQLite format 3\x00 but not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	p = plan(t, env, st, config.Config{}, Filters{})
	if p.CursorDatabaseChecked || len(databaseCandidates(p)) != 0 || p.CursorDatabaseUnchecked != CursorUncheckedUnreadable {
		t.Fatalf("checked %v, %d from the database, reason %q", p.CursorDatabaseChecked, len(databaseCandidates(p)), p.CursorDatabaseUnchecked)
	}
	out.Reset()
	RenderText(&out, p)
	if !strings.Contains(out.String(), "were not checked:\n      the database could not be read safely.") {
		t.Fatalf("no unchecked line:\n%s", out.String())
	}
	out.Reset()
	if err := RenderJSON(&out, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"cursor_database_unchecked_reason": "unreadable"`) {
		t.Fatalf("no reason in the JSON plan:\n%s", out.String())
	}
}

// With any part of Cursor's transcript store unreadable, a chat with a
// transcript can't be told apart from a database-only one, so the database
// is not counted at all, rather than counting every chat as database-only.
func TestCursorDatabaseSkippedWhenTranscriptsUnreadable(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	slugDir := filepath.Join(tr.home, ".cursor", "projects", cursorSlug(site))
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(site), "agent-transcripts", "k1", "k1.jsonl"), cursorTranscript)
	writeCursorDB(t, CursorStateDatabase(tr.home), true, map[string]any{
		"composerData:k1": composerJSON("k1", 3, nil),
		"composerData:k2": composerJSON("k2", 1, nil),
	})
	for _, tc := range []struct {
		name       string
		unreadable string
	}{
		{"store", filepath.Join(tr.home, ".cursor", "projects")},
		{"one project's transcripts", filepath.Join(slugDir, "agent-transcripts")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tr.env()
			env.ReadDir = func(dir string) ([]fs.DirEntry, error) {
				if dir == tc.unreadable {
					return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
				}
				return os.ReadDir(dir)
			}
			env.CursorDatabase = func(context.Context) (CursorDatabaseResult, error) {
				t.Fatal("the database was read although Cursor's transcripts were incomplete")
				return CursorDatabaseResult{}, nil
			}
			p := plan(t, env, states{}, config.Config{}, Filters{})
			if p.CursorDatabaseChecked || p.CursorDatabaseUnchecked != CursorUncheckedTranscriptsUnreadable || len(databaseCandidates(p)) != 0 {
				t.Fatalf("checked %v, reason %q, %d from the database", p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, len(databaseCandidates(p)))
			}
		})
	}
	// Unaffected when every folder is readable.
	env := tr.env()
	env.CursorDatabase = CursorDatabaseReader(tr.home)
	if p := plan(t, env, states{}, config.Config{}, Filters{}); !p.CursorDatabaseChecked || len(databaseCandidates(p)) != 1 {
		t.Fatalf("readable store: checked %v, %d from the database", p.CursorDatabaseChecked, len(databaseCandidates(p)))
	}
}

// TestCursorDatabasePlanLive plans with Cursor running: the chats are read
// from one snapshot in the per-user temporary directory, which is gone when
// the plan is made, and nothing next to the database changes.
func TestCursorDatabasePlanLive(t *testing.T) {
	temp := t.TempDir()
	previous := cursorstore.SnapshotTempDirForTesting
	cursorstore.SnapshotTempDirForTesting = temp
	t.Cleanup(func() { cursorstore.SnapshotTempDirForTesting = previous })
	tr := newTree(t)
	path := CursorStateDatabase(tr.home)
	w := startCursorWriter(t, path, true)
	w.do("full:one", "full:two")
	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	env := tr.env()
	env.CursorDatabase = CursorDatabaseReader(tr.home)
	p := plan(t, env, states{}, config.Config{}, Filters{})
	got := databaseCandidates(p)
	if !p.CursorDatabaseChecked || len(got) != 2 {
		t.Fatalf("checked %v (%q), %+v", p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, got)
	}
	for _, c := range got {
		// Read whole (it has a size) and filtered; it names no workspace.
		if c.Bytes == 0 || c.Skip != SkipProjectUnknown {
			t.Fatalf("%+v", c)
		}
	}
	assertUnchanged(t, dir, before)
	entries, err := os.ReadDir(filepath.Join(temp, fmt.Sprintf("agent-archive-cursor-%d", os.Getuid())))
	if err != nil || len(entries) != 0 {
		t.Fatalf("snapshots left after the plan: %v %v", entries, err)
	}
}
