package backfill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type durableUndoMode string

const (
	durableUndoCorrupt          durableUndoMode = "corrupt"
	durableUndoFuture           durableUndoMode = "future"
	durableUndoSourceOnly       durableUndoMode = "source-only"
	durableUndoOpaque           durableUndoMode = "opaque"
	durableUndoHealthy          durableUndoMode = "healthy-pending"
	durableUndoReceiptExtra     durableUndoMode = "receipt-extra"
	durableUndoReceiptUncleared durableUndoMode = "receipt-uncleared"
)

type durableUndoRemote struct {
	*storagetest.MemoryStore
	deletes int
}

func (s *durableUndoRemote) Delete(ctx context.Context, key string) error {
	s.deletes++
	return s.MemoryStore.Delete(ctx, key)
}

func TestUndoRefusesProtectedRecoveryBeforeRemoteDeletion(t *testing.T) {
	for _, mode := range []durableUndoMode{durableUndoCorrupt, durableUndoFuture, durableUndoSourceOnly, durableUndoOpaque, durableUndoHealthy, durableUndoReceiptExtra, durableUndoReceiptUncleared} {
		t.Run(string(mode), func(t *testing.T) {
			f := newUndoFixture(t)
			if err := config.Save(f.home, config.Config{MachineID: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			if err := config.WithDurableStorage(f.home, func(config.DurableStorageGuard) error { return nil }); err != nil {
				t.Fatal(err)
			}
			batch := f.batch("2026-09-23-1", fixedNow)
			reg := f.register("imported", "/work/p", batch.ID, fixedNow)
			path := filepath.Join(f.home, "pending", reg.ArchiveSessionID+".json")
			raw := []byte("{original")
			sum := sha256.Sum256(raw)
			ref := archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(raw)}
			switch mode {
			case durableUndoFuture:
				raw = []byte(`{"history":{"version":2}}`)
			case durableUndoSourceOnly:
				stage, err := f.store.StagePendingSource(reg.ArchiveSessionID, ref, raw)
				if err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(f.store.SessionDir(reg.ArchiveSessionID), "pending-sources", stage.Name)
			case durableUndoOpaque:
				path = filepath.Join(f.home, "publication-evidence", reg.ArchiveSessionID, "opaque")
			case durableUndoHealthy:
				if err := f.store.SavePending(reg.ArchiveSessionID, state.PendingPublication{SourceKey: ref.Key, SourceSHA256: ref.SHA256, SourceBytes: raw, MetadataKey: "metadata", MetadataBytes: []byte(`{}`), Attempted: true}); err != nil {
					t.Fatal(err)
				}
			case durableUndoReceiptExtra, durableUndoReceiptUncleared:
				path = filepath.Join(f.home, "generation-recovery", reg.ArchiveSessionID+".json")
				suffix := `,"future":null}`
				if mode == durableUndoReceiptUncleared {
					suffix = `,"request":{}}`
				}
				raw = []byte(`{"version":1,"key":{"Agent":"claude","NativeID":"native"},"previous":"` + reg.ArchiveSessionID + `","next":"next","complete":true` + suffix)
			case durableUndoCorrupt:
			}
			if mode != durableUndoSourceOnly && mode != durableUndoHealthy {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			regPath := filepath.Join(f.home, "registrations", reg.ArchiveSessionID+".json")
			registration, err := os.ReadFile(regPath)
			if err != nil {
				t.Fatal(err)
			}
			batchFile := batchPath(f.home, batch.ID)
			batchBytes, err := os.ReadFile(batchFile)
			if err != nil {
				t.Fatal(err)
			}
			remote := &durableUndoRemote{MemoryStore: storagetest.NewMemoryStore()}
			remoteRaw := []byte("remote-original")
			remoteSum := sha256.Sum256(remoteRaw)
			key := "sessions/claude/" + reg.ArchiveSessionID + "/source." + hex.EncodeToString(remoteSum[:]) + ".jsonl.gz"
			if err := remote.Put(t.Context(), key, remoteRaw); err != nil {
				t.Fatal(err)
			}
			plan := UndoPlan{Sessions: []UndoSession{{Registration: reg, InCurrentDestination: true}}}
			result := plan.Remove(t.Context(), f.store, remote, fixedNow)
			if mode == durableUndoHealthy {
				if len(result.Failed) != 0 || len(result.Deleted) != 1 || remote.deletes == 0 {
					t.Fatalf("healthy imported undo refused: %+v deletes=%d", result, remote.deletes)
				}
				return
			}
			if !errors.Is(result.Failed[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) || len(result.Deleted)+len(result.Forgotten) != 0 || remote.deletes != 0 {
				t.Errorf("protected undo escaped: %+v deletes=%d", result, remote.deletes)
			}
			if got, err := remote.Get(t.Context(), key); err != nil || string(got) != "remote-original" {
				t.Errorf("remote original changed: %v", err)
			}
			for p, want := range map[string][]byte{path: original, regPath: registration, batchFile: batchBytes} {
				if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, want) {
					t.Errorf("local original changed %s: %v", p, err)
				}
			}
		})
	}
}
