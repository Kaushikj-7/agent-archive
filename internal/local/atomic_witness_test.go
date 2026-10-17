package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteWitnessExactIdentityDigestAndLifetime(t *testing.T) {
	for _, mode := range []string{"valid", "omit", "error", "replace", "rename", "symlink", "rewrite", "other-root"} {
		t.Run(mode, func(t *testing.T) {
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
			var saved *AtomicWriteWitness
			failure := errors.New("synthetic callback error")
			err = RootedAtomicWriteWitnessed(t.Context(), held.Root, "selected.json", func(w io.Writer) error { _, err := w.Write([]byte("selected")); return err }, make([]byte, 1024), func(witness *AtomicWriteWitness, rename func() error) error {
				saved = witness
				if err := witness.Check(held.Root); err != nil {
					t.Fatal(err)
				}
				info, err := held.Root.Lstat(witness.state.name)
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := witness.Match(held.Root, witness.state.name, info); err != nil || !ok {
					t.Fatal(ok, err)
				}
				if ok, err := witness.Match(held.Root, ".pending-other", info); err != nil || ok {
					t.Fatal("other name excused", err)
				}
				switch mode {
				case "omit":
					return nil
				case "error":
					return failure
				case "replace", "symlink":
					if err := held.Root.Remove(witness.state.name); err != nil {
						t.Fatal(err)
					}
					if mode == "symlink" {
						if err := held.Root.Symlink("outside", witness.state.name); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(filepath.Join(home, witness.state.name), []byte("selected"), 0600); err != nil {
						t.Fatal(err)
					}
				case "rename":
					if err := held.Root.Rename(witness.state.name, "moved"); err != nil {
						t.Fatal(err)
					}
				case "rewrite":
					if err := os.WriteFile(filepath.Join(home, witness.state.name), []byte("replaced"), 0600); err != nil {
						t.Fatal(err)
					}
					// Restore the timestamp and size on the same inode: only the
					// actual writer digest detects this substitution.
					if err := os.Chtimes(filepath.Join(home, witness.state.name), info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "other-root":
					other, err := os.OpenRoot(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := other.Close(); err != nil {
							t.Error(err)
						}
					}()
					if witness.Check(other) == nil {
						t.Fatal("different root excused")
					}
					return failure
				}
				return rename()
			})
			if saved == nil || saved.Check(held.Root) == nil {
				t.Fatal("witness escaped callback")
			}
			if mode != "valid" {
				if err == nil {
					t.Fatal("changed temporary committed")
				}
				if _, e := os.Stat(filepath.Join(home, "selected.json")); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("failure selected output", e)
				}
				if mode == "error" && !errors.Is(err, failure) {
					t.Fatal("callback error lost", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(home, "selected.json"))
			if err != nil || !bytes.Equal(raw, []byte("selected")) {
				t.Fatal("selected bytes", err)
			}
		})
	}
}

func TestAtomicWriteWitnessZeroAndCanceledRefuse(t *testing.T) {
	var zero AtomicWriteWitness
	if zero.Check(nil) == nil {
		t.Fatal("zero witness accepted")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	err = RootedAtomicWriteWitnessed(ctx, root, "selected.json", func(w io.Writer) error { _, e := w.Write([]byte("selected")); return e }, make([]byte, 1024), func(_ *AtomicWriteWitness, rename func() error) error { cancel(); return rename() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled witness renamed", err)
	}
}
