package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestBackfillProtectedGroupsRetainCollectorOwnership(t *testing.T) {
	f, bucket := newImportFixture(t)
	must(t, os.RemoveAll(filepath.Join(f.userHome, ".claude", "projects")))
	project := filepath.Join(f.userHome, "agent-archive")
	for i := range 9 {
		id := fmt.Sprintf("synthetic-protected-%02d", i)
		record := map[string]any{"type": "assistant", "sessionId": id, "cwd": project, "timestamp": "2026-09-20T17:00:00Z", "message": map[string]any{"role": "assistant", "content": "Synthetic protected group evidence."}}
		body, err := json.Marshal(record)
		must(t, err)
		f.write(t, filepath.Join(".claude", "projects", "synthetic-protected", id+".jsonl"), string(body)+"\n")
	}
	cfg := mustLoadConfig(t, f.data)
	cfg.ImportedHarnesses = []string{"claude"}
	cfg.DurableStorageProtection, cfg.PublicationCompositionProtection = true, true
	cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true
	must(t, config.Save(f.data, cfg))
	cfg = mustLoadConfig(t, f.data)
	if !cfg.SupportsStagedSelection() {
		t.Fatal("fixture did not require typed legacy publication ownership")
	}
	store := state.OpenReadOnly(f.data)
	staged, drained := 0, false
	f.env.backfillCheckpoint = func(step string) error {
		if step != "staged" {
			return nil
		}
		staged++
		if staged != 9 {
			return nil
		}
		parents, children := importRegistrations(t, f.data, firstImport)
		if len(parents) != 8 || len(children) != 0 {
			t.Fatal("owned group was not registered before continuing", len(parents), len(children))
		}
		for _, reg := range parents {
			published, err := store.LoadPublishedState(reg.ArchiveSessionID)
			must(t, err)
			if !published.Summary().Published {
				t.Fatal("typed group did not publish under the registration guard")
			}
			if _, found, err := store.LoadImportStage(t.Context(), reg); err != nil || found {
				t.Fatal("typed group was not exactly settled before continuing", found, err)
			}
		}
		drained = true
		return nil
	}
	beforeNative := snapshotTree(t, f.userHome)
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "claude", "--project", project)
	if code != 0 || !drained || staged != 9 {
		t.Fatal("protected owned groups did not complete", code, drained, staged, stderr, out)
	}
	if beforeNative != snapshotTree(t, f.userHome) {
		t.Fatal("protected import changed native input")
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 9 || len(children) != 0 {
		t.Fatal("protected import changed logical ownership", len(parents), len(children))
	}
	for _, reg := range parents {
		assertProtectedImportReadback(t, store, bucket, reg)
	}

	if _, found := readCollectorLockRecord(f.data); found {
		t.Fatal("collector holder record leaked")
	}
	unlock, err := local.Lock(f.data)
	must(t, err)
	unlock()
}

func assertProtectedImportReadback(t *testing.T, store *state.Store, bucket storage.ObjectStore, reg archive.SessionRegistration) {
	t.Helper()
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	must(t, err)
	metadata, found, err := published.LastPublishedMetadata()
	must(t, err)
	if !found {
		t.Fatal("protected publication missing")
	}
	bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
	must(t, err)
	raw, err := json.Marshal(bundle.NativeRecords)
	must(t, err)
	if bundle.NativeSessionID != reg.NativeSessionID || !bundle.Capture.CapturedAt.Equal(reg.AdmittedAt) || !strings.Contains(string(raw), "Synthetic protected group evidence.") {
		t.Fatal("protected publication changed selected native evidence")
	}
	if _, found, err := store.LoadImportStage(t.Context(), reg); err != nil || found {
		t.Fatal("verified protected stage retained", found, err)
	}
	if _, found, err := store.LoadPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("verified protected publication pending", found, err)
	}
}
