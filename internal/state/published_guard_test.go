package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func legacyGenerationMetadata(t *testing.T, p *Published, at time.Time) (archive.SourceBundle, archive.Metadata, []byte) {
	t.Helper()
	bundle, _, _ := p.LastPublished()
	var metadata archive.Metadata
	if err := json.Unmarshal(p.Metadata(), &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.MetadataDerivedAt = at
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, metadata, raw
}

func legacyGenerationMutation(p *Published, method string, bundle archive.SourceBundle, metadata archive.Metadata, raw []byte, at time.Time) error {
	switch method {
	case "cache":
		return p.CacheMetadata(raw)
	case "save":
		return p.Save(bundle, at, CacheStatusPublished, raw)
	case "publication":
		return p.SavePublication(bundle, at, metadata.SourceBundle, raw)
	default:
		return p.SaveRepublishedMetadata(PendingPublication{MetadataOnly: true, Bundle: bundle, MetadataBytes: raw, SourceKey: metadata.SourceBundle.Key, SourceSHA256: metadata.SourceBundle.SHA256, SourceSize: metadata.SourceBundle.CompressedBytes}, at)
	}
}

func TestLegacyPublishedWritersSerializeWithGenerationRecovery(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"cache", "save", "publication", "republished"} {
		for _, phase := range []string{"generation-fenced", "generation-pending", "generation-active"} {
			t.Run(method+"/"+phase, func(t *testing.T) {
				t.Parallel()
				s, reg, at := generationFixture(t)
				published, err := s.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				original := bytes.Clone(published.Metadata())
				bundle, metadata, changed := legacyGenerationMetadata(t, published, at.Add(time.Hour))
				attempted := false
				var attemptErr error
				s.onIndexStep = func(step string) error {
					if step == phase {
						attempted = true
						attemptErr = legacyGenerationMutation(published, method, bundle, metadata, changed, at)
					}
					return nil
				}
				next, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at))
				if err != nil || next == "" {
					t.Fatalf("consistent transition failed: %q %v", next, err)
				}
				current, err := s.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if !attempted || !errors.Is(attemptErr, local.ErrBusy) || !bytes.Equal(original, current.Metadata()) {
					t.Fatalf("transition accepted changed predecessor: attempted=%v writer=%v", attempted, attemptErr)
				}
				receipt, found, err := s.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID)
				if err != nil || !found || !receipt.Complete {
					t.Fatalf("transition incomplete: %v %v", found, err)
				}
				// The completed generation can still receive legitimate retained metadata
				// maintenance through a fresh view after the transition releases its lock.
				if err := current.CacheMetadata(changed); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLegacyPublishedWriterRefusesStaleView(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"cache", "save", "publication", "republished"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			s, reg, at := generationFixture(t)
			first, err := s.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			stale, err := s.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, newer := legacyGenerationMetadata(t, first, at)
			if err := first.CacheMetadata(newer); err != nil {
				t.Fatal(err)
			}
			bundle, metadata, older := legacyGenerationMetadata(t, stale, at.Add(time.Hour))
			if err := legacyGenerationMutation(stale, method, bundle, metadata, older, at); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatalf("stale view overwrote newer publication: %v", err)
			}
			current, err := s.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil || !bytes.Equal(current.Metadata(), newer) {
				t.Fatal("newer metadata lost", err)
			}
		})
	}
}

func TestLegacyPublishedWriterCancellationAndRootReplacement(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "replace-root"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, reg, at := generationFixture(t)
			read := s
			cancel := func() {}
			if mode == "cancel" {
				var ctx context.Context
				ctx, cancel = context.WithCancel(t.Context())
				var closeRead func()
				read, closeRead = s.WithReadBudget(ctx, nil)
				defer closeRead()
			}
			defer cancel()
			published, err := read.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, changed := legacyGenerationMetadata(t, published, at)
			if mode == "cancel" {
				unlock, err := local.NamedLockWait(s.Home(), "hooks.lock", 0)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				cancel()
				if err := published.CacheMetadata(changed); !errors.Is(err, context.Canceled) {
					t.Fatalf("writer lost cancellation: %v", err)
				}
			} else {
				moved := filepath.Join(t.TempDir(), "original-home")
				if err := os.Rename(s.Home(), moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.Home(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := published.CacheMetadata(changed); !errors.Is(err, ErrDurableStorageRecovery) {
					t.Fatalf("view followed replacement root: %v", err)
				}
			}
		})
	}
}

func TestLegacyPublishedWriterPreservesUnprotectedHomeCompatibility(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"permissions", "symlink", "generation-two"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, reg, at := generationFixture(t)
			if mode == "generation-two" {
				if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(s.Home(), 0755); err != nil {
				t.Fatal(err)
			}
			home := s.Home()
			if mode == "symlink" {
				home = filepath.Join(t.TempDir(), "legacy-home")
				if err := os.Symlink(s.Home(), home); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(s.Home(), "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			read := OpenReadOnly(home)
			published, err := read.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, changed := legacyGenerationMetadata(t, published, at)
			err = published.CacheMetadata(changed)
			if mode == "generation-two" {
				if !errors.Is(err, ErrDurableStorageRecovery) {
					t.Fatalf("protected generation weakened root: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(s.Home(), "config.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("legacy write changed configuration floor", err)
			}
			info, err := os.Stat(s.Home())
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatal("legacy permissions changed", err)
			}
		})
	}
	home := filepath.Join(t.TempDir(), "missing-home")
	published, err := OpenReadOnly(home).LoadPublishedState("missing")
	if err != nil || published.Found() {
		t.Fatalf("read-only missing home changed behavior: %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only missing home created data: %v", err)
	}
}

func TestLegacyPublishedViewsKeepIndependentObservedDigests(t *testing.T) {
	t.Parallel()
	s, reg, at := generationFixture(t)
	first, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, metadata, _ := legacyGenerationMetadata(t, first, at)
	bundle.ArchiveSessionID = "other-session"
	metadata.SessionID = bundle.ArchiveSessionID
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SourceBundle.Key, err = archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SourceBundle.SHA256 = packed.SHA256
	metadata.SourceBundle.CompressedBytes = len(packed.Bytes)
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.LoadPublishedState(bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Save(bundle, at, CacheStatusPublished, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPublishedState(bundle.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	_, _, changed := legacyGenerationMetadata(t, first, at.Add(time.Hour))
	if err := first.CacheMetadata(changed); err != nil {
		t.Fatal("another view replaced original digest", err)
	}
	current, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil || !bytes.Equal(current.Metadata(), changed) {
		t.Fatal("original view lost its own wire identity", err)
	}
}

func TestLegacyPublishedPrivateOpenCannotAdoptReplacementRoot(t *testing.T) {
	t.Parallel()
	s, reg, _ := generationFixture(t)
	published, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := os.ReadFile(s.publishedPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	var opened *local.RootedHome
	directory, err := published.openLegacyPublishedDirectoryUsing(func(home string) (*local.RootedHome, error) {
		if err := os.Rename(home, filepath.Join(t.TempDir(), "original-home")); err != nil {
			return nil, err
		}
		if err := os.Mkdir(home, 0700); err != nil {
			return nil, err
		}
		if err := os.Mkdir(filepath.Join(home, "published"), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(s.publishedPath(reg.ArchiveSessionID), wire, 0600); err != nil {
			return nil, err
		}
		held, e := local.OpenRootedHome(home)
		opened = held
		return held, e
	})
	if err == nil {
		if directory.close != nil {
			if e := directory.close(); e != nil {
				t.Error(e)
			}
		}
		t.Fatal("old observed bytes adopted an identically populated replacement root")
	}
	if !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal(err)
	}
	if opened == nil {
		t.Fatal("actual private open did not run")
	}
	if _, err := opened.Root.Stat("."); err == nil {
		t.Fatal("refused foreign root handle remained open")
	}
}

func TestLegacyPublishedWeakRootRefusesAmbiguousGenerationMarker(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"duplicate", "case-duplicate", "case-only", "downgraded-controls", "null", "future"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, reg, at := generationFixture(t)
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); err != nil {
				t.Fatal(err)
			}
			path := s.generationRecoveryPath(reg.ArchiveSessionID)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			selector := []byte(`"version":2`)
			if !bytes.Contains(raw, selector) {
				t.Fatal("actual version2 receipt missing selector")
			}
			replacements := map[string]string{"duplicate": `"version":2,"version":1`, "case-duplicate": `"version":2,"Version":1`, "case-only": `"Version":1`, "downgraded-controls": `"version":1`, "null": `"version":null`, "future": `"version":99`}
			raw = bytes.Replace(raw, selector, []byte(replacements[mode]), 1)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(s.Home(), 0755); err != nil {
				t.Fatal(err)
			}
			read := OpenReadOnly(s.Home())
			published, err := read.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, changed := legacyGenerationMetadata(t, published, at)
			before, err := os.ReadFile(s.publishedPath(reg.ArchiveSessionID))
			if err != nil {
				t.Fatal(err)
			}
			if err := published.CacheMetadata(changed); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatalf("ambiguous generation marker enabled weak-root cache write: %v", err)
			}
			after, err := os.ReadFile(s.publishedPath(reg.ArchiveSessionID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("ambiguous marker changed published bytes", err)
			}
		})
	}
}
