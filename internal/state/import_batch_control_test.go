package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

type importLockTestMode string

const (
	lockTestValid     importLockTestMode = "valid"
	lockTestName      importLockTestMode = "name"
	lockTestContent   importLockTestMode = "content"
	lockTestDirectory importLockTestMode = "directory"
	lockTestSymlink   importLockTestMode = "symlink"
)

func TestNativeImportBatchLockSerializesBeforeRequest(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	if err := os.Mkdir(filepath.Join(home, "request-locks"), 0700); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("batch worker did not join after cancellation")
		}
	})
	go func() {
		defer close(finished)
		done <- WithImportBatchLock(ctx, home, "2026-10-17-1", func(held *local.RootedHome) error {
			unlock, err := local.RootedLockWait(held, requestLockName("session"), time.Second)
			if err != nil {
				return err
			}
			defer unlock()
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("batch worker returned before entering callback", err)
	case <-ctx.Done():
		t.Fatal("batch worker never entered callback", ctx.Err())
	}
	acquired := make(chan struct{})
	second := make(chan error, 1)
	secondFinished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-secondFinished:
		case <-time.After(time.Second):
			t.Error("second batch worker did not join after cancellation")
		}
	})
	go func() {
		defer close(secondFinished)
		second <- WithImportBatchLock(ctx, home, "2026-10-17-1", func(*local.RootedHome) error { close(acquired); return nil })
	}()
	select {
	case <-acquired:
		t.Error("batch lock admitted a concurrent final writer")
	case <-time.After(30 * time.Millisecond):
	case err := <-second:
		t.Fatal("second batch worker returned while lock was held", err)
	case <-ctx.Done():
		t.Fatal("batch serialization timed out", ctx.Err())
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("first batch worker did not finish", ctx.Err())
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("second batch worker did not finish", ctx.Err())
	}
}

func TestNativeImportBatchReplacementRechecksAfterPreparation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	if err := local.WriteBytes(filepath.Join(home, "imports", "2026-10-17-1.json"), []byte(`{"undone_at":null}`)); err != nil {
		t.Fatal(err)
	}
	encoded := make(chan struct{})
	allowCommit := make(chan struct{})
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("batch worker did not join after cancellation")
		}
	})
	go func() {
		defer close(finished)
		done <- ReplaceImportBatchControl(ctx, home, "2026-10-17-1", func(w io.Writer) error {
			_, err := w.Write([]byte(`{"undone_at":null,"sessions":["stale"]}`))
			close(encoded)
			select {
			case <-allowCommit:
				return err
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			}
		}, func(raw []byte, found bool) error {
			if !found || !bytes.Equal(raw, []byte(`{"undone_at":null}`)) {
				return ErrReadySelectionFrozen
			}
			return nil
		})
	}()
	select {
	case <-encoded:
	case err := <-done:
		t.Fatal("replacement returned before encoding", err)
	case <-ctx.Done():
		t.Fatal("replacement never encoded", ctx.Err())
	}
	undone := []byte(`{"undone_at":"2026-10-17T12:00:00Z"}`)
	if err := WithImportBatchLock(ctx, home, "2026-10-17-1", func(held *local.RootedHome) error {
		return local.RootedAtomicWriteChecked(held.Root, "imports/2026-10-17-1.json", func(w io.Writer) error { _, err := w.Write(undone); return err }, func(rename func() error) error { return rename() })
	}); err != nil {
		t.Fatal(err)
	}
	close(allowCommit)
	select {
	case err := <-done:
		if !errors.Is(err, ErrReadySelectionFrozen) {
			t.Fatal("stale replacement succeeded", err)
		}
	case <-ctx.Done():
		t.Fatal("replacement did not finish", ctx.Err())
	}
	got, err := os.ReadFile(filepath.Join(home, "imports", "2026-10-17-1.json"))
	if err != nil || !bytes.Equal(got, undone) {
		t.Fatal("undo marker changed", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "imports"))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed replacement leaked temp", err)
	}
}

func TestNativeImportBatchLockInspectionRefusesUnknownContent(t *testing.T) {
	t.Parallel()
	for _, mode := range []importLockTestMode{lockTestValid, lockTestName, lockTestContent, lockTestDirectory, lockTestSymlink} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := WithImportBatchLock(t.Context(), home, "2026-10-17-1", func(*local.RootedHome) error { return nil }); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(home, importLocksDirectory, "2026-10-17-1.lock")
			switch mode {
			case lockTestName:
				if err := os.Rename(name, filepath.Join(home, importLocksDirectory, "future.lock")); err != nil {
					t.Fatal(err)
				}
			case lockTestContent:
				if err := os.WriteFile(name, []byte("authority"), 0600); err != nil {
					t.Fatal(err)
				}
			case lockTestDirectory:
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
			case lockTestSymlink:
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), name); err != nil {
					t.Fatal(err)
				}
			}
			held, err := local.OpenRootedHome(home)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			}()
			remaining := durableEntryLimit
			err = inspectImportLocks(t.Context(), held, &remaining)
			if (err == nil) != (mode == lockTestValid) {
				t.Fatalf("inspection %s: %v", mode, err)
			}
			if mode == lockTestValid && remaining != durableEntryLimit-1 {
				t.Fatal("lock entry was not charged")
			}
		})
	}
}
