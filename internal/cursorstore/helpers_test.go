package cursorstore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// openWriter opens path as Cursor does, with Cursor's tables.
func openWriter(tb testing.TB, path string, wal bool) *sql.DB {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	stmts := []string{`CREATE TABLE IF NOT EXISTS ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`}
	if wal {
		stmts = append([]string{`PRAGMA journal_mode=WAL`}, stmts...)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			tb.Fatal(err)
		}
	}
	return db
}

// writeDB creates or updates a database at path and closes it, so SQLite
// checkpoints and removes its side files: Cursor closed.
func writeDB(tb testing.TB, path string, wal bool, rows map[string]any) {
	tb.Helper()
	db := openWriter(tb, path, wal)
	for k, v := range rows {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, v); err != nil {
			tb.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		tb.Fatal(err)
	}
}

// closeOrFail closes c, failing the test if that fails. It suits a defer.
func closeOrFail(tb testing.TB, c io.Closer) {
	tb.Helper()
	if err := c.Close(); err != nil {
		tb.Error(err)
	}
}

// chat returns the composerData value of a chat with the given message IDs.
func chat(id string, lastUpdatedAt int64, bubbles ...string) string {
	headers := make([]map[string]any, len(bubbles))
	for i, b := range bubbles {
		headers[i] = map[string]any{"bubbleId": b, "type": 1 + i%2}
	}
	v := map[string]any{"_v": 18, "composerId": id, "fullConversationHeadersOnly": headers}
	if lastUpdatedAt != 0 {
		v["lastUpdatedAt"] = lastUpdatedAt
	}
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func bubble(text string) string {
	data, _ := json.Marshal(map[string]any{"_v": 3, "type": 1, "text": text})
	return string(data)
}

type fileState struct {
	size    int64
	mode    os.FileMode
	modTime time.Time
	sum     [32]byte
}

// snapshotDir records every file in dir, and dir itself, byte for byte.
func snapshotDir(tb testing.TB, dir string) map[string]fileState {
	tb.Helper()
	out := map[string]fileState{}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		tb.Fatal(err)
	}
	out["."] = fileState{mode: info.Mode(), modTime: info.ModTime()}
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			tb.Fatal(err)
		}
		var sum [32]byte
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				tb.Fatal(err)
			}
			sum = sha256.Sum256(data)
		}
		out[e.Name()] = fileState{size: info.Size(), mode: info.Mode(), modTime: info.ModTime(), sum: sum}
	}
	return out
}

func assertUnchanged(tb testing.TB, dir string, before map[string]fileState) {
	tb.Helper()
	after := snapshotDir(tb, dir)
	for name, a := range after {
		if b, ok := before[name]; !ok {
			tb.Errorf("%s was created", name)
		} else if !reflect.DeepEqual(a, b) {
			tb.Errorf("%s changed: %+v -> %+v", name, b, a)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			tb.Errorf("%s was removed", name)
		}
	}
	if tb.Failed() {
		tb.FailNow()
	}
}

// useTempSnapshots points the snapshots' temporary directory, and so
// SnapshotRoot, at a directory of the test's own, and returns the snapshot
// root there.
func useTempSnapshots(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SnapshotTempDirForTesting = dir
	t.Cleanup(func() { SnapshotTempDirForTesting = "" })
	root := snapshotRootPath()
	if filepath.Dir(root) != filepath.Clean(dir) {
		t.Fatalf("snapshot root %s is not under %s", root, dir)
	}
	return root
}

// assertEmpty fails unless dir holds nothing: no snapshot was left behind.
func assertEmpty(tb testing.TB, dir string) {
	tb.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		tb.Fatal(err)
	}
	for _, e := range entries {
		tb.Errorf("left behind in the scratch directory: %s", e.Name())
	}
	if tb.Failed() {
		tb.FailNow()
	}
}

// writer is another process standing in for Cursor. It is a separate
// process because SQLite shares one shared-memory mapping between the
// connections of a process, so an in-process writer would not show whether
// the reader leaves Cursor's -shm file alone.
type writer struct {
	t       testing.TB
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies *bufio.Scanner
}

// writerOp is what a writerCommand asks the writer process to do.
type writerOp string

const (
	writerPut        writerOp = "put"
	writerDelete     writerOp = "delete"
	writerBegin      writerOp = "begin"
	writerCommit     writerOp = "commit"
	writerCheckpoint writerOp = "checkpoint"
	writerExclusive  writerOp = "exclusive"
	writerNormal     writerOp = "normal"
)

// writerCommand is one line the writer process runs.
type writerCommand struct {
	Op    writerOp `json:"op"`
	Key   string   `json:"key,omitempty"`
	Value string   `json:"value,omitempty"`
}

func startWriter(tb testing.TB, path string) *writer {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestWriterProcess$")
	cmd.Env = append(os.Environ(), "CURSORSTORE_WRITER_DB="+path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		tb.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		tb.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	w := &writer{t: tb, cmd: cmd, stdin: stdin, replies: bufio.NewScanner(stdout)}
	tb.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			tb.Error(err)
		}
		_ = cmd.Wait() // the writer exits once stdin closes; its status is not under test
	})
	return w
}

func (w *writer) do(commands ...writerCommand) {
	w.t.Helper()
	for _, c := range commands {
		line, _ := json.Marshal(c)
		if _, err := fmt.Fprintln(w.stdin, string(line)); err != nil {
			w.t.Fatal(err)
		}
		if !w.replies.Scan() || w.replies.Text() != "ok" {
			w.t.Fatalf("the writer did not do %+v: %q", c, w.replies.Text())
		}
	}
}

func (w *writer) put(rows map[string]string) {
	w.t.Helper()
	for k, v := range rows {
		w.do(writerCommand{Op: writerPut, Key: k, Value: v})
	}
}

// TestWriterProcess is the writer process of writer: a WAL database that is
// never checkpointed unless asked, one command per line of stdin until stdin
// closes.
func TestWriterProcess(t *testing.T) {
	path := os.Getenv("CURSORSTORE_WRITER_DB")
	if path == "" {
		t.Skip("run by startWriter")
	}
	db := openWriter(t, path, true)
	defer closeOrFail(t, db)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, conn)
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 1<<24)
	for in.Scan() {
		var c writerCommand
		if err := json.Unmarshal(in.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		var err error
		switch c.Op {
		case writerBegin:
			_, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`)
		case writerCommit:
			_, err = conn.ExecContext(ctx, `COMMIT`)
		case writerCheckpoint:
			_, err = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		case writerPut:
			_, err = conn.ExecContext(ctx, `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, c.Key, c.Value)
		case writerDelete:
			_, err = conn.ExecContext(ctx, `DELETE FROM cursorDiskKV WHERE key = ?`, c.Key)
		case writerExclusive:
			// Holds the database's exclusive lock from the next write on,
			// so every reader in another process is busy.
			if _, err = conn.ExecContext(ctx, `PRAGMA locking_mode=EXCLUSIVE`); err == nil {
				_, err = conn.ExecContext(ctx, `INSERT INTO ItemTable (key, value) VALUES ('lock', 'held')`)
			}
		case writerNormal:
			// Releases the exclusive lock at the next access.
			if _, err = conn.ExecContext(ctx, `PRAGMA locking_mode=NORMAL`); err == nil {
				var n int
				err = conn.QueryRowContext(ctx, `SELECT count(*) FROM ItemTable`).Scan(&n)
			}
		default:
			err = fmt.Errorf("unknown op %q", c.Op)
		}
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("ok")
	}
	if err := in.Err(); err != nil {
		t.Fatal(err)
	}
}

// mustWrite writes a test file, reporting a failure to the test.
func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Error(err)
	}
}
