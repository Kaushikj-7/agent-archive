package local

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRootedAtomicFailureKeepsOriginalAndReportsCleanup(t *testing.T) {
	home := t.TempDir()
	if e := os.Chmod(home, 0700); e != nil {
		t.Fatal(e)
	}
	held, e := OpenRootedHome(home)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = held.Close() }()
	if e = os.WriteFile(filepath.Join(home, "original"), []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	failure := errors.New("synthetic encoding failure")
	if e = RootedAtomicWrite(held.Root, "original", func(w io.Writer) error { _, e := w.Write([]byte("new")); return errors.Join(e, failure) }); !errors.Is(e, failure) {
		t.Fatalf("write: %v", e)
	}
	raw, e := os.ReadFile(filepath.Join(home, "original"))
	if e != nil || string(raw) != "old" {
		t.Fatal("original lost", e)
	}
	entries, e := os.ReadDir(home)
	if e != nil || len(entries) != 1 {
		t.Fatal("failed write retained unexpected allocation", e)
	}
}

func TestRootedConcurrentLockCreation(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, e := OpenRootedHome(home)
			if e != nil {
				errs <- e
				return
			}
			defer func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			}()
			unlock, e := RootedLockWait(h, "hooks.lock", time.Second)
			if e == nil {
				unlock()
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestRootedLockDeadlineAndReplacement(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	unlock, err := RootedLockWait(first, "hooks.lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other, err := OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	}()
	started := time.Now()
	if release, e := RootedLockWait(other, "hooks.lock", 30*time.Millisecond); !errors.Is(e, ErrBusy) {
		if release != nil {
			release()
		}
		t.Fatalf("contention: %v", e)
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline exceeded bounded backoff")
	}
	if err := os.Rename(filepath.Join(home, "hooks.lock"), filepath.Join(home, "held.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "hooks.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Check(); err == nil {
		t.Fatal("replaced lock accepted")
	}
}

func TestRootedHomeRejectsPermissionsChangedDuringScope(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	held, err := OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Chmod(home, 0755); err != nil {
		t.Fatal(err)
	}
	if err := held.Check(); err == nil {
		t.Fatal("unsafe changed home retained authority")
	}
}

func TestRootedCheckedCommitRequiresActualRename(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprint(omit), func(t *testing.T) {
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			held, err := OpenRootedHome(home)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			}()
			err = RootedAtomicWriteChecked(held.Root, "selected.json", func(w io.Writer) error { _, err := w.Write([]byte("selected")); return err }, func(rename func() error) error {
				if omit {
					return nil
				}
				return rename()
			})
			if omit {
				if err == nil {
					t.Fatal("omitted rename reported durable success")
				}
				if _, err = held.Root.Lstat("selected.json"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("omitted commit wrote selection: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(home, "selected.json"))
			if err != nil || string(raw) != "selected" {
				t.Fatal(string(raw), err)
			}
		})
	}
}

func TestRootedCheckedCommitRetainsLateRawError(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	held, err := OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	failure := errors.New("late checked commit failure")
	err = RootedAtomicWriteChecked(held.Root, "selected.json", func(w io.Writer) error { _, err := w.Write([]byte("selected")); return err }, func(rename func() error) error { return errors.Join(rename(), failure) })
	if !errors.Is(err, failure) {
		t.Fatalf("late raw error lost: %v", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(home, "selected.json"))
	if readErr != nil || string(raw) != "selected" {
		t.Fatal(string(raw), readErr)
	}
}
