package local

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWritePreverifiedWitnessStrongIdentityAndLifetime(t *testing.T) {
	for _, mode := range []string{"valid", "cancel", "replace", "same-inode-restored-mtime", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.Chmod(rootPath, 0700); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			restored := false
			var saved func() error
			err = RootedAtomicWritePreverifiedWitnessed(ctx, root, "selected", func(w io.Writer) error { _, err := w.Write([]byte("selected")); return err }, make([]byte, 1024), func(w *AtomicWriteWitness, verify func() (func() error, error)) error {
				if mode == "omitted" {
					return nil
				}
				rename, err := verify()
				if err != nil {
					return err
				}
				saved = rename
				if _, err := verify(); err == nil {
					t.Fatal("second verification acquired a fresh rename")
				}
				switch mode {
				case "valid":
				case "cancel":
					cancel()
				case "replace":
					if err := root.Remove(w.state.name); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(rootPath, w.state.name), []byte("selected"), 0600); err != nil {
						t.Fatal(err)
					}
				case "same-inode-restored-mtime":
					stamp, err := root.Stat(w.state.name)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(rootPath, w.state.name), []byte("replaced"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(filepath.Join(rootPath, w.state.name), stamp.ModTime(), stamp.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
				return rename()
			}, func() error { restored = true; return nil })
			supported := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
			if (err == nil) != (mode == "valid" && supported) {
				t.Fatal("preverified result", mode, err)
			}
			if supported && !restored {
				t.Fatal("cleanup scope not restored")
			}
			if saved != nil && saved() == nil {
				t.Fatal("expired rename succeeded")
			}
			if mode != "valid" || !supported {
				if _, err := root.Stat("selected"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refusal selected target", err)
				}
			}
		})
	}
}

func TestAtomicWritePreverifiedReleasesPolicyBeforeDirectorySync(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	synced := false
	spec := &witnessedAtomicCommit{ctx: t.Context(), scratch: make([]byte, 1024), hash: sha256.New(), cleanup: func() error { return nil }}
	spec.preverified = func(_ *AtomicWriteWitness, verify func() (func() error, error)) error {
		rename, err := verify()
		if err != nil {
			return err
		}
		unlock, err := NamedLockWait(home, "hooks.lock", 0)
		if err != nil {
			return err
		}
		defer unlock()
		return rename()
	}
	spec.beforeDirSync = func() {
		unlock, err := NamedLockWait(home, "hooks.lock", 0)
		if err != nil {
			t.Error("policy held before directory sync", err)
			return
		}
		unlock()
		synced = true
	}
	err = rootedAtomicWrite(root, "selected", func(w io.Writer) error { _, err := w.Write([]byte("selected")); return err }, nil, spec)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("unsupported strong identity accepted")
		}
		return
	}
	if err != nil || !synced {
		t.Fatal("directory sync path", synced, err)
	}
}

func TestAtomicWritePreverifiedCleanupPreservesForeignReplacement(t *testing.T) {
	for _, mode := range []string{"replacement", "parent-substitution", "after-success-reappearance", "same-inode-rewrite"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(home, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			foreignName := ""
			err = RootedAtomicWritePreverifiedWitnessed(t.Context(), root, "nested/selected", func(w io.Writer) error { _, err := w.Write([]byte("selected")); return err }, make([]byte, 1024), func(w *AtomicWriteWitness, verify func() (func() error, error)) error {
				rename, err := verify()
				if err != nil {
					return err
				}
				foreignName = w.state.name
				switch mode {
				case "replacement":
					if err := root.Remove(foreignName); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(home, foreignName), []byte("foreign!"), 0600); err != nil {
						t.Fatal(err)
					}
					return rename()
				case "parent-substitution":
					if err := root.Rename("nested", "moved"); err != nil {
						t.Fatal(err)
					}
					if err := root.Mkdir("nested", 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(home, foreignName), []byte("foreign!"), 0600); err != nil {
						t.Fatal(err)
					}
					return rename()
				case "after-success-reappearance":
					if err := rename(); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(home, foreignName), []byte("foreign!"), 0600)
				case "same-inode-rewrite":
					info, err := root.Stat(foreignName)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(home, foreignName), []byte("foreign!"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(filepath.Join(home, foreignName), info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
					return rename()
				}
				return errors.New("unknown fixture")
			}, func() error { return nil })
			if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
				if err == nil {
					t.Fatal("unsupported strong identity accepted")
				}
				return
			}
			if (err == nil) != (mode == "after-success-reappearance") {
				t.Fatal("selection result", mode, err)
			}
			raw, readErr := os.ReadFile(filepath.Join(home, foreignName))
			if readErr != nil || string(raw) != "foreign!" {
				t.Fatal("cleanup deleted or changed foreign replacement", readErr)
			}
			if mode == "parent-substitution" {
				own := filepath.Join(home, "moved", filepath.Base(foreignName))
				if raw, err := os.ReadFile(own); err != nil || string(raw) != "selected" {
					t.Fatal("substituted-parent refusal lost original evidence", err)
				}
			}
		})
	}
}

func TestAtomicWritePreverifiedCleanupRecognizesOwnPartialOutput(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	failure := errors.New("synthetic partial write")
	err = RootedAtomicWritePreverifiedWitnessed(t.Context(), root, "selected", func(w io.Writer) error {
		if _, err := w.Write([]byte("partial")); err != nil {
			return err
		}
		return failure
	}, make([]byte, 1024), func(*AtomicWriteWitness, func() (func() error, error)) error {
		t.Fatal("partial output reached commit")
		return nil
	}, func() error { return nil })
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	} else if err == nil {
		t.Fatal("unsupported strong identity accepted")
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("own partial output leaked", entries, err)
	}
}
