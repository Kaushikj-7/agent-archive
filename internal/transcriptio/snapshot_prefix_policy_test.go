package transcriptio_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type prefixLocatorMutation string

const (
	prefixLocatorUnchanged    prefixLocatorMutation = "unchanged"
	prefixLocatorAppend       prefixLocatorMutation = "append"
	prefixLocatorFinalSymlink prefixLocatorMutation = "final-symlink"
)

func TestSnapshotPrefixPreservesStrictRootLocator(t *testing.T) {
	for _, mutation := range []prefixLocatorMutation{prefixLocatorUnchanged, prefixLocatorAppend, prefixLocatorFinalSymlink} {
		t.Run(string(mutation), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "session.jsonl")
			data := []byte("{\"role\":\"user\"}\n")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := transcriptio.Open(sourcefacts.RootOpener{Root: root}, path, transcriptio.OpenPolicy{Root: root, RejectSymlinks: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			digest := sha256.Sum256(data)
			if err := s.CheckPrefix(context.Background(), int64(len(data)), digest); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case prefixLocatorUnchanged:
				// Preserve the original locator and captured bytes.
			case prefixLocatorAppend:
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := f.WriteString("{}\n")
				closeErr := f.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					t.Fatal(err)
				}
			case prefixLocatorFinalSymlink:
				moved := filepath.Join(root, "retained.jsonl")
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, path); err != nil {
					t.Fatal(err)
				}
			}
			err = s.CheckPrefix(context.Background(), int64(len(data)), digest)
			if mutation == prefixLocatorFinalSymlink {
				if !errors.Is(err, transcriptio.ErrChanged) {
					t.Fatalf("new strict locator symlink: got %v, want ErrChanged", err)
				}
			} else if err != nil {
				t.Fatalf("stable captured prefix: %v", err)
			}
		})
	}
}

func TestSnapshotPrefixPreservesExplicitSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.jsonl")
	link := filepath.Join(root, "explicit.jsonl")
	data := []byte("{}\n")
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s, err := transcriptio.Open(transcriptio.OS{}, link, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.CheckPrefix(context.Background(), int64(len(data)), sha256.Sum256(data)); err != nil {
		t.Fatalf("explicit symlink: %v", err)
	}
}
