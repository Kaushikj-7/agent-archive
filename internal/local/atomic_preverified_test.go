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

type preverifiedWitnessMode string

const (
	preverifiedValid                  preverifiedWitnessMode = "valid"
	preverifiedCancel                 preverifiedWitnessMode = "cancel"
	preverifiedReplace                preverifiedWitnessMode = "replace"
	preverifiedSameInodeRestoredMtime preverifiedWitnessMode = "same-inode-restored-mtime"
	preverifiedOmitted                preverifiedWitnessMode = "omitted"
)

type foreignReplacementMode string

const (
	foreignFixtureReplacement              foreignReplacementMode = "replacement"
	foreignFixtureParentSubstitution       foreignReplacementMode = "parent-substitution"
	foreignFixtureAfterSuccessReappearance foreignReplacementMode = "after-success-reappearance"
	foreignFixtureSameInodeRewrite         foreignReplacementMode = "same-inode-rewrite"
)

func TestAtomicWritePreverifiedWitnessStrongIdentityAndLifetime(t *testing.T) {
	for _, mode := range []preverifiedWitnessMode{preverifiedValid, preverifiedCancel, preverifiedReplace, preverifiedSameInodeRestoredMtime, preverifiedOmitted} {
		t.Run(string(mode), func(t *testing.T) {
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
				if mode == preverifiedOmitted {
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
				case preverifiedValid:
				case preverifiedCancel:
					cancel()
				case preverifiedReplace:
					if err := root.Remove(w.state.name); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(rootPath, w.state.name), []byte("selected"), 0600); err != nil {
						t.Fatal(err)
					}
				case preverifiedSameInodeRestoredMtime:
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
			if (err == nil) != (mode == preverifiedValid && supported) {
				t.Fatal("preverified result", mode, err)
			}
			if supported && !restored {
				t.Fatal("cleanup scope not restored")
			}
			if saved != nil && saved() == nil {
				t.Fatal("expired rename succeeded")
			}
			if mode != preverifiedValid || !supported {
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
	spec := &witnessedAtomicCommit{
		ctx:     t.Context(),
		scratch: make([]byte, 1024),
		hash:    sha256.New(),
		cleanup: func() error { return nil },
		preverified: func(_ *AtomicWriteWitness, verify func() (func() error, error)) error {
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
		},
		beforeDirSync: func() {
			unlock, err := NamedLockWait(home, "hooks.lock", 0)
			if err != nil {
				t.Error("policy held before directory sync", err)
				return
			}
			unlock()
			synced = true
		},
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
	for _, mode := range []foreignReplacementMode{foreignFixtureReplacement, foreignFixtureParentSubstitution, foreignFixtureAfterSuccessReappearance, foreignFixtureSameInodeRewrite} {
		t.Run(string(mode), func(t *testing.T) {
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
				case foreignFixtureReplacement:
					if err := root.Remove(foreignName); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(home, foreignName), []byte("foreign!"), 0600); err != nil {
						t.Fatal(err)
					}
					return rename()
				case foreignFixtureParentSubstitution:
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
				case foreignFixtureAfterSuccessReappearance:
					if err := rename(); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(home, foreignName), []byte("foreign!"), 0600)
				case foreignFixtureSameInodeRewrite:
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
			if (err == nil) != (mode == foreignFixtureAfterSuccessReappearance) {
				t.Fatal("selection result", mode, err)
			}
			raw, readErr := os.ReadFile(filepath.Join(home, foreignName))
			if readErr != nil || string(raw) != "foreign!" {
				t.Fatal("cleanup deleted or changed foreign replacement", readErr)
			}
			if mode == foreignFixtureParentSubstitution {
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
