package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This explicitly arranges an already-owned receipt. Admission and typed release
// are exercised separately by the collector integration, not fabricated here.
func undoNativeImportOwner(t *testing.T, f *undoFixture, batch Batch) archive.SessionRegistration {
	t.Helper()
	checkpoint := archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1234, SHA256: strings.Repeat("d", 64)}
	at := batch.StartedAt.Add(-time.Hour)
	binding := &archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: "ordinary-id", NativeCreatedAt: at, Cwd: "/work", Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &checkpoint}
	digest, err := archive.NativeImportBindingDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &archive.NativeImport{Contract: "muse-historical-import-v1", Confirmation: archive.NativeImportConfirmation{BatchID: batch.ID, BatchInstance: strings.Repeat("a", 32), ScopeSHA256: strings.Repeat("b", 64), DestinationID: "dest", ProjectID: "project", RootFingerprint: strings.Repeat("c", 64), SelectedBindingSHA256: digest, SelectedSource: checkpoint}, Membership: archive.NativeImportMembership{ReceiptID: strings.Repeat("e", 32), State: "attached", Effects: []string{"registration"}}}
	reg := archive.SessionRegistration{NativeImport: receipt, NativeBinding: binding, ArchiveSessionID: strings.Repeat("1", 32), NativeSessionID: binding.NativeID, ProjectID: "project", ProjectRoot: "/work", Harness: archive.Harness{Name: "muse"}, TranscriptPath: binding.Path, SessionStartedAt: at, RegisteredAt: batch.StartedAt, AdmittedAt: batch.StartedAt, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(batch.ID), DestinationID: "dest"}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(filepath.Join(f.home, "registrations", reg.ArchiveSessionID+".json"), reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestNativeImportUndoPlansPreserveCurrentOwner(t *testing.T) {
	for _, mode := range []string{"attached", "detached", "future"} {
		t.Run(mode, func(t *testing.T) {
			f := newUndoFixture(t)
			cfg := config.Config{NativeImportProtection: true, DurableStorageProtection: true, PublicationCompositionProtection: true, ImportStagingProtection: true, StagedSelectionProtection: true}
			if err := config.Save(f.home, cfg); err != nil {
				t.Fatal(err)
			}
			batch := f.batch("2026-10-17-1", fixedNow)
			reg := undoNativeImportOwner(t, f, batch)
			path := filepath.Join(f.home, "registrations", reg.ArchiveSessionID+".json")
			if mode == "detached" {
				reg.NativeImport.Membership.State = "detached"
				if err := local.Write(path, reg); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "future" {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.Replace(raw, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v9"), 1)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "detached" {
				if _, err := PlanUndo(Environment{}, f.store, cfg, []Batch{batch}, batch, ""); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
					t.Fatalf("normal plan: %v", err)
				}
			}
			for _, current := range []bool{true, false} {
				stale := reg
				stale.NativeImport = nil
				remote := &durableUndoRemote{MemoryStore: storagetest.NewMemoryStore()}
				p := UndoPlan{Sessions: []UndoSession{{Registration: stale, InCurrentDestination: current}}}
				result := p.Remove(t.Context(), f.store, remote, fixedNow)
				if result.Failed[reg.ArchiveSessionID] == nil || len(result.Deleted)+len(result.Forgotten) != 0 || remote.deletes != 0 {
					t.Fatalf("direct plan escaped: %+v deletes=%d", result, remote.deletes)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("owner changed", err)
			}
			var control map[string]json.RawMessage
			if err := json.Unmarshal(after, &control); err != nil {
				t.Fatal(err)
			}
			if _, ok := control["native_import"]; !ok {
				t.Fatal("receipt lost")
			}
		})
	}
}
