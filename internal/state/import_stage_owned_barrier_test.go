package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Model the collector's final cleanup ports with a real committed selection and
// exact pending journal. Remote closing proof itself is covered by CLI tests.
func ownedImportReleaseCommittedFixture(t *testing.T) (*Store, archive.SessionRegistration, PendingPublication, []byte) {
	t.Helper()
	s, reg, bundle := ownedImportReleaseFixture(t, "original selection")
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, archive.Analysis{}, nil, "synthetic-machine", bundle.Capture.CapturedAt, bundle.Capture.CapturedAt, ref, archive.ParserInfo{Name: "test", Version: "test-v1"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingPublication{Bundle: bundle, SourceKey: key, SourceSHA256: ref.SHA256, SourceBytes: packed.Bytes, MetadataKey: metadataKey, MetadataBytes: raw}
	pending, err = PreparePublicationV2(pending, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending(reg.ArchiveSessionID, pending); err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(pending, bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	return s, reg, pending, raw
}

func TestOwnedImportReleaseRetryRequiresDirectoryDurabilityBeforeJournalRemoval(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"stage-directory", "session-parent"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			s, reg, pending, before := ownedImportReleaseCommittedFixture(t)
			stageDirectory := filepath.Dir(filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID)))
			sessionParent := filepath.Dir(stageDirectory)
			faultPath := stageDirectory
			if phase == "session-parent" {
				faultPath = sessionParent
			}
			fault := errors.New("synthetic held-directory sync failure")
			complete := func(barrier func(*os.Root) error) error {
				if err := s.removeOwnedImportStageUsing(reg, barrier); err != nil {
					return err
				}
				return s.RemoveExactSourceSetPending(reg.ArchiveSessionID, pending)
			}
			for range 2 {
				failed := false
				err := complete(func(held *os.Root) error {
					if filepath.Clean(held.Name()) == faultPath {
						failed = true
						return fault
					}
					return syncDurableParent(held)
				})
				if !failed || !errors.Is(err, fault) {
					t.Fatal("uncertain unlink skipped its retry durability barrier", failed, err)
				}
				after, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("failed durability barrier lost frozen pending journal", err)
				}
			}
			barriers := 0
			if err := complete(func(held *os.Root) error {
				after, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("journal completed before actual directory barrier", err)
				}
				barriers++
				return syncDurableParent(held)
			}); err != nil {
				t.Fatal(err)
			}
			if barriers == 0 {
				t.Fatal("retry completed without a held-directory barrier")
			}
			if found, err := s.HasPending(reg.ArchiveSessionID); err != nil || found {
				t.Fatal("durable release did not permit exact journal completion", found, err)
			}
		})
	}
}

func TestOwnedImportReleaseBarrierRefusesChangedRootAndCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"session-parent", "archive-home", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, reg, _, before := ownedImportReleaseCommittedFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			read, closeRead := s.WithReadBudget(ctx, nil)
			defer closeRead()
			pendingPath := s.pendingPath(reg.ArchiveSessionID)
			stageDirectory := filepath.Join(s.Home(), "sessions", reg.ArchiveSessionID, "import-stage")
			changed := false
			err := read.removeOwnedImportStageUsing(reg, func(held *os.Root) error {
				if err := syncDurableParent(held); err != nil {
					return err
				}
				if changed || filepath.Clean(held.Name()) != stageDirectory {
					return nil
				}
				changed = true
				switch mode {
				case "cancel":
					cancel()
				case "session-parent":
					parent := filepath.Dir(stageDirectory)
					if err := os.Rename(parent, filepath.Join(t.TempDir(), "held-session")); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(stageDirectory, 0700); err != nil {
						t.Fatal(err)
					}
				case "archive-home":
					moved := filepath.Join(t.TempDir(), "held-home")
					if err := os.Rename(s.Home(), moved); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(s.Home(), 0700); err != nil {
						t.Fatal(err)
					}
					pendingPath = filepath.Join(moved, "pending", reg.ArchiveSessionID+".json")
				}
				return nil
			})
			if !changed || err == nil || mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("release accepted a changed scope after its barrier", changed, err)
			}
			after, err := os.ReadFile(pendingPath)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatal("refused release lost original pending bytes", err)
			}
			if mode == "session-parent" {
				if info, err := os.Stat(stageDirectory); err != nil || !info.IsDir() {
					t.Fatal("old handle removed the replacement empty import directory", err)
				}
			}
		})
	}
}
