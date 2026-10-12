package cursorstore

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// kill ends the writer without closing its database, as a crash (or a quit
// that leaves the side files) does, and waits until it has exited.
func (w *writer) kill() {
	w.t.Helper()
	if err := w.cmd.Process.Kill(); err != nil {
		w.t.Fatal(err)
	}
	// The error only reports the kill; the cleanup's Wait reports nothing new.
	_, _ = w.cmd.Process.Wait()
}

// settledWriter leaves chatRows in a WAL database whose -wal is empty
// (checkpointed and truncated) beside a -shm, with the writer still running
// unless killed.
func settledWriter(t *testing.T, path string, killed bool) *writer {
	t.Helper()
	w := startWriter(t, path)
	w.put(chatRows())
	w.do(writerCommand{Op: writerCheckpoint})
	if killed {
		w.kill()
	}
	if !emptyFile(path + "-wal") {
		t.Fatal("the -wal is not empty")
	}
	if _, err := os.Lstat(path + "-shm"); err != nil {
		t.Fatal("no -shm", err)
	}
	return w
}

// TestRecoveryReadsSettledEmptyWAL: a WAL database whose -wal is empty holds
// every committed write in the file itself, so recovery reads it immutably
// whether Cursor left -shm beside it or not, and whether Cursor quit or is
// open and idle. Nothing beside it changes and nothing is copied.
func TestRecoveryReadsSettledEmptyWAL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		killed  bool
		dropShm bool
	}{{"quit, -wal and -shm left", true, false}, {"quit, only -wal left", true, true}, {"running, idle", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			root := useTempSnapshots(t)
			path := StateDatabase(t.TempDir())
			settledWriter(t, path, tc.killed)
			if tc.dropShm {
				if err := os.Remove(path + "-shm"); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
			c, err := ReadRecoveryComposer(t.Context(), path, "c", 0, 1<<20, b)
			if err != nil || len(c.Bubbles) != 3 || string(c.Bubbles[0].Value) != bubble("first") || !c.Bubbles[1].Missing {
				t.Fatal(c, err)
			}
			assertUnchanged(t, dir, before)
			assertEmpty(t, root)
		})
	}
}

// TestRecoveryRefusesWALFrames: frames in the -wal, from a running Cursor or
// one that was killed before checkpointing, are never read by recovery
// (immutable would ignore them), and nothing beside the database changes.
func TestRecoveryRefusesWALFrames(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "killed"}[killed], func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			w := settledWriter(t, path, false)
			w.put(map[string]string{"composerData:late": chat("late", 1, "x")})
			if killed {
				w.kill()
			}
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			err := ReadRecovery(t.Context(), path, &recoveryTestBudget{rows: 100, bytes: 1 << 20}, func(context.Context, *sql.DB) error {
				t.Fatal("read a database with WAL frames")
				return nil
			})
			if err == nil || ReasonOf(err) != Locked {
				t.Fatal(err)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestRecoveryRefusesSettledWithoutShmHeader: a settled read compares the
// -shm's wal-index header before and after, so an empty -wal beside a -shm
// whose header cannot be read (shorter than the header, or not a regular
// file) is Locked rather than read without that comparison.
func TestRecoveryRefusesSettledWithoutShmHeader(t *testing.T) {
	for name, replace := range map[string]func(t *testing.T, shm string){
		"short -shm": func(t *testing.T, shm string) {
			t.Helper()
			if err := os.Truncate(shm, shmHeaderSize-1); err != nil {
				t.Fatal(err)
			}
		},
		"-shm is a link": func(t *testing.T, shm string) {
			t.Helper()
			target := filepath.Join(t.TempDir(), "shm")
			data, err := os.ReadFile(shm)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(shm); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, shm); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			settledWriter(t, path, true)
			replace(t, path+"-shm")
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			err := ReadRecovery(t.Context(), path, &recoveryTestBudget{rows: 100, bytes: 1 << 20}, func(context.Context, *sql.DB) error {
				t.Fatal("read a settled database without its -shm header")
				return nil
			})
			if err == nil || ReasonOf(err) != Locked {
				t.Fatal(err)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestRecoverySettledReadInvalidatedByChange: a settled read that sees
// Cursor write, checkpoint, or change its side files while it reads is
// ChangedDuringRead, not a result.
func TestRecoverySettledReadInvalidatedByChange(t *testing.T) {
	for name, tc := range map[string]struct {
		killed bool
		setup  func(t *testing.T, path string)
		change func(t *testing.T, w *writer, path string)
	}{
		"write fills the -wal": {change: func(t *testing.T, w *writer, _ string) {
			t.Helper()
			w.put(map[string]string{"composerData:late": chat("late", 1, "x")})
		}},
		"write checkpointed and truncated": {change: func(t *testing.T, w *writer, _ string) {
			t.Helper()
			w.put(map[string]string{"composerData:late": chat("late", 1, "x")})
			w.do(writerCommand{Op: writerCheckpoint})
		}},
		// Rewriting a value in place leaves the file's size and header as
		// they were (WAL commits change the header's counter only when
		// page 1 changes): only the modification time shows the
		// checkpoint.
		"same-size write checkpointed and truncated": {change: func(t *testing.T, w *writer, path string) {
			t.Helper()
			header, _ := sqliteHeader(path)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			w.put(map[string]string{"bubbleId:c:b1": bubble("FIRST")})
			w.do(writerCommand{Op: writerCheckpoint})
			headerAfter, _ := sqliteHeader(path)
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(header, headerAfter) || after.Size() != info.Size() || !emptyFile(path+"-wal") {
				t.Fatal("the rewrite changed more than the modification time")
			}
		}},
		// A coarse timestamp (one kernel tick) can leave even the
		// modification time as it was: only the -shm's wal-index header
		// shows the write and the checkpoint.
		"same-size write checkpointed within one timestamp tick": {change: func(t *testing.T, w *writer, path string) {
			t.Helper()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			w.put(map[string]string{"bubbleId:c:b1": bubble("FIRST")})
			w.do(writerCommand{Op: writerCheckpoint})
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() || !emptyFile(path+"-wal") {
				t.Fatal("the rewrite left more than the -shm changed")
			}
		}},
		"-shm removed": {killed: true, change: func(t *testing.T, _ *writer, path string) {
			t.Helper()
			if err := os.Remove(path + "-shm"); err != nil {
				t.Fatal(err)
			}
		}},
		"-shm appears beside a stray -wal": {killed: true, setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path + "-shm"); err != nil {
				t.Fatal(err)
			}
		}, change: func(t *testing.T, _ *writer, path string) {
			t.Helper()
			mustWrite(t, path+"-shm", nil)
		}},
		"journal appears": {killed: true, change: func(t *testing.T, _ *writer, path string) {
			t.Helper()
			mustWrite(t, path+"-journal", nil)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			w := settledWriter(t, path, tc.killed)
			// A checkpoint during the read may show only in the file's
			// modification time: set it into the past, so a file system
			// with coarse timestamps still shows one.
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, path)
			}
			called := false
			err := ReadRecovery(t.Context(), path, &recoveryTestBudget{rows: 100, bytes: 1 << 20}, func(ctx context.Context, db *sql.DB) error {
				called = true
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cursorDiskKV`).Scan(&n); err != nil {
					return err
				}
				tc.change(t, w, path)
				return nil
			})
			if !called || err == nil || ReasonOf(err) != ChangedDuringRead {
				t.Fatal(called, err)
			}
		})
	}
}
