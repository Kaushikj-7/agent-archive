package transcriptio

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type bufferMutation string

const (
	bufferMutationUnchanged    bufferMutation = "unchanged"
	bufferMutationRewrite      bufferMutation = "rewrite"
	bufferMutationMode         bufferMutation = "mode"
	bufferMutationReplace      bufferMutation = "replace"
	bufferMutationSymlink      bufferMutation = "symlink"
	bufferMutationAppend       bufferMutation = "append"
	bufferMutationCancelBefore bufferMutation = "cancel-before"
	bufferMutationCancelDuring bufferMutation = "cancel-during"
	bufferMutationReadError    bufferMutation = "read-error"
)

type bufferOpener struct {
	OS
	read  func()
	fault error
	calls *int
}

type bufferFile struct {
	File
	read  func()
	fault error
	calls *int
}

func (o bufferOpener) OpenRegular(path string) (File, error) {
	file, err := o.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return bufferFile{file, o.read, o.fault, o.calls}, nil
}

func (f bufferFile) ReadAt(p []byte, off int64) (int, error) {
	*f.calls++
	if f.fault != nil {
		return 0, f.fault
	}
	n, err := f.File.ReadAt(p, off)
	if *f.calls == 1 && f.read != nil {
		f.read()
	}
	return n, err
}

func TestPrefixBorrowedBufferPreservesReadAndMutationFailures(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 7, 64 << 10} {
		for _, change := range []bufferMutation{bufferMutationUnchanged, bufferMutationRewrite, bufferMutationMode, bufferMutationReplace, bufferMutationSymlink, bufferMutationAppend, bufferMutationCancelBefore, bufferMutationCancelDuring, bufferMutationReadError} {
			t.Run(fmt.Sprintf("%d/%s", size, change), func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "source")
				data := []byte(strings.Repeat("synthetic source\n", 8193))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fault := errors.New("synthetic read failure")
				calls := 0
				opener := bufferOpener{calls: &calls}
				switch change {
				case bufferMutationUnchanged, bufferMutationCancelBefore:
					// No mutation during reading.
				case bufferMutationReadError:
					opener.fault = fault
				case bufferMutationCancelDuring:
					opener.read = cancel
				case bufferMutationRewrite:
					opener.read = func() {
						rewritten := []byte(strings.Repeat("different source\n", 8193))
						if err := os.WriteFile(path, rewritten, 0600); err != nil {
							t.Fatal(err)
						}
					}
				case bufferMutationMode:
					opener.read = func() {
						if err := os.Chmod(path, 0640); err != nil {
							t.Fatal(err)
						}
					}
				case bufferMutationReplace:
					opener.read = func() {
						if err := os.Rename(path, path+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, data, 0600); err != nil {
							t.Fatal(err)
						}
					}
				case bufferMutationSymlink:
					opener.read = func() {
						if err := os.Rename(path, path+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(path+".old", path); err != nil {
							t.Fatal(err)
						}
					}
				case bufferMutationAppend:
					opener.read = func() {
						if err := os.WriteFile(path, append(append([]byte(nil), data...), []byte("appended\n")...), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				snap, err := Open(opener, path, OpenPolicy{Root: filepath.Dir(path), RejectSymlinks: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := snap.Close(); err != nil {
						t.Error(err)
					}
				})
				if change == bufferMutationCancelBefore {
					cancel()
				}
				err = snap.CheckPrefixBuffer(ctx, int64(len(data)), sha256.Sum256(data), make([]byte, size))
				switch change {
				case bufferMutationUnchanged, bufferMutationAppend:
					if err != nil {
						t.Fatal(err)
					}
				case bufferMutationCancelBefore, bufferMutationCancelDuring:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancel: %v", err)
					}
				case bufferMutationReadError:
					if !errors.Is(err, fault) {
						t.Fatalf("read failure: %v", err)
					}
				case bufferMutationRewrite, bufferMutationMode, bufferMutationReplace, bufferMutationSymlink:
					if !errors.Is(err, ErrChanged) {
						t.Fatalf("mutation: %v", err)
					}
				}
			})
		}
	}
}

func TestPrefixBorrowedBufferReducesCallsWithoutSkippingBytes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "source")
	data := []byte(strings.Repeat("x", 128<<10) + "tail")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	snap, err := Open(bufferOpener{calls: &calls}, path, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snap.Close(); err != nil {
			t.Error(err)
		}
	})
	digest := sha256.Sum256(data)
	if err := snap.CheckPrefix(t.Context(), int64(len(data)), digest); err != nil {
		t.Fatal(err)
	}
	original := calls
	calls = 0
	if err := snap.CheckPrefixBuffer(t.Context(), int64(len(data)), digest, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if original != 5 || calls != 3 {
		t.Fatalf("read calls=%d/%d want=5/3", original, calls)
	}
}
