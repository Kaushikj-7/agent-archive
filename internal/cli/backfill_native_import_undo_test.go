package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestBackfillNativeImportUndoPublicCommandPreservesConfigurationAndStorage(t *testing.T) {
	t.Parallel()
	for _, yes := range []bool{false, true} {
		name := "terminal-required"
		if yes {
			name = "confirmed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newBackfillFixture(t)
			cfg, _, err := config.Load(f.data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := state.Open(f.data); err != nil {
				t.Fatal(err)
			}
			f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				t.Fatal("membership undo accessed cloud storage")
				return nil, nil
			}
			at := backfillNow.UTC()
			batch := backfill.Batch{ID: "2026-09-23-1", StartedAt: at, DestinationID: cfg.DestinationID(), Filters: backfill.BatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}}
			// Existing closed intent contains no first-owner authority. This command
			// must stop it without native reads, object deletion or setup changes.
			scope := archive.NativeImportBatchScope{BatchID: batch.ID, StartedAtUTC: at.Format(time.RFC3339Nano), DestinationID: batch.DestinationID, Filters: archive.ImportBatchFilters(batch.Filters), ProjectIDs: []string{cfg.Archive.Projects[0].ProjectID}, Selected: []archive.NativeImportBatchSelection{{Kind: archive.NativeImportOrdinaryRoot, NativeID: "synthetic-undo", OriginalTUTC: at.Add(-time.Hour).Format(time.RFC3339Nano), ProjectID: cfg.Archive.Projects[0].ProjectID, RootFingerprint: strings.Repeat("a", 64), RootRevision: "one", BindingSHA256: strings.Repeat("b", 64), Source: archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1234, SHA256: strings.Repeat("d", 64)}}}}
			digest, err := archive.NativeImportBatchScopeDigest(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			batch.NativeImport = &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("c", 32), ScopeSHA256: digest, Scope: scope}
			path := filepath.Join(f.data, "imports", batch.ID+".json")
			if err := local.Write(path, batch); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(f.data, "config.json")
			configBefore, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{batch.ID}
			if yes {
				args = append(args, "--yes")
			}
			out, stderr, code := f.undoRun(t, nil, false, args...)
			if yes && (code != 0 || !strings.Contains(out, "Detached 0 import memberships")) {
				t.Fatal(code, out, stderr)
			}
			if !yes && (code != 1 || !strings.Contains(stderr, "needs a terminal")) {
				t.Fatal(code, out, stderr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !yes && !bytes.Equal(before, after) {
				t.Fatal("unconfirmed undo changed control")
			}
			if yes {
				batches, err := backfill.LoadBatches(f.data)
				if err != nil || len(batches) != 1 || batches[0].UndoneAt == nil {
					t.Fatal("confirmed batch not stopped", err)
				}
			}
			configAfter, err := os.ReadFile(configPath)
			if err != nil || !bytes.Equal(configBefore, configAfter) {
				t.Fatal("membership undo changed configuration", err)
			}
		})
	}
}
