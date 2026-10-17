package state

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeImportBatchLockSerializesBeforeRequest(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "request-locks"), 0700); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithImportBatchLock(t.Context(), home, "2026-10-17-1", func(held *local.RootedHome) error {
			unlock, err := local.RootedLockWait(held, requestLockName("session"), time.Second)
			if err != nil {
				return err
			}
			defer unlock()
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	acquired := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		second <- WithImportBatchLock(t.Context(), home, "2026-10-17-1", func(*local.RootedHome) error { close(acquired); return nil })
	}()
	select {
	case <-acquired:
		t.Error("batch lock admitted a concurrent final writer")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestNativeImportBatchReplacementRechecksAfterPreparation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := local.WriteBytes(filepath.Join(home, "imports", "2026-10-17-1.json"), []byte(`{"undone_at":null}`)); err != nil {
		t.Fatal(err)
	}
	encoded := make(chan struct{})
	allowCommit := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ReplaceImportBatchControl(t.Context(), home, "2026-10-17-1", func(w io.Writer) error {
			_, err := w.Write([]byte(`{"undone_at":null,"sessions":["stale"]}`))
			close(encoded)
			<-allowCommit
			return err
		}, func(raw []byte, found bool) error {
			if !found || !bytes.Equal(raw, []byte(`{"undone_at":null}`)) {
				return ErrReadySelectionFrozen
			}
			return nil
		})
	}()
	<-encoded
	undone := []byte(`{"undone_at":"2026-10-17T12:00:00Z"}`)
	if err := WithImportBatchLock(t.Context(), home, "2026-10-17-1", func(held *local.RootedHome) error {
		return local.RootedAtomicWriteChecked(held.Root, "imports/2026-10-17-1.json", func(w io.Writer) error { _, err := w.Write(undone); return err }, func(rename func() error) error { return rename() })
	}); err != nil {
		t.Fatal(err)
	}
	close(allowCommit)
	if err := <-done; !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatal("stale replacement succeeded", err)
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
	for _, mode := range []string{"valid", "name", "content", "directory", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			if err := WithImportBatchLock(t.Context(), home, "2026-10-17-1", func(*local.RootedHome) error { return nil }); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(home, importLocksDirectory, "2026-10-17-1.lock")
			switch mode {
			case "name":
				if err := os.Rename(name, filepath.Join(home, importLocksDirectory, "future.lock")); err != nil {
					t.Fatal(err)
				}
			case "content":
				if err := os.WriteFile(name, []byte("authority"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
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
			defer held.Close()
			remaining := durableEntryLimit
			err = inspectImportLocks(t.Context(), held, &remaining)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("inspection %s: %v", mode, err)
			}
			if mode == "valid" && remaining != durableEntryLimit-1 {
				t.Fatal("lock entry was not charged")
			}
		})
	}
}
