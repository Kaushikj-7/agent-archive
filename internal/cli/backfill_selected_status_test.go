package cli

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestBackfillSelectedPublicationPreservesUnrelatedIssues(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	store, err := state.Open(f.data)
	must(t, err)
	original, err := store.LoadRegistrations()
	must(t, err)
	if len(original) != 1 {
		t.Fatal("missing existing unrelated owner", len(original))
	}
	args := []string{"--yes", "--background", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--until", "2026-09-20"}
	out, stderr, code := f.importRun(t, nil, false, args...)
	if code != 0 {
		t.Fatal("prepare selected retained owner", code, stderr, out)
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 1 || len(children) != 0 {
		t.Fatal("unexpected selected cohort", len(parents), len(children))
	}
	selected, unrelated := parents[0].ArchiveSessionID, original[0].ArchiveSessionID
	status := state.Status{LastScanAt: backfillNow.Add(-time.Hour), PendingCount: 2,
		SessionIssues: map[string]string{selected: issueCaptureFailed, unrelated: issueStorageUnavailable},
		IssueCounts:   map[string]int{issueCaptureFailed: 1, issueStorageUnavailable: 1},
	}
	oldSummary := issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 1}, issueStorageUnavailable: {sessions: 1}})
	status.SetLastErrors(oldSummary)
	must(t, store.SaveStatus(status))
	key, err := archive.MetadataObjectKey(parents[0].Harness.Name, selected)
	must(t, err)
	remote := &linkedChildPublicationFault{MemoryStore: bucket, metadataKey: key}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	result, err := runPass(f.env, false, passOptions{importBatch: firstImport})
	defer result.CloseStagedResults()
	must(t, err)
	if remote.attempts == 0 || result.Errors[selected] == nil {
		t.Fatal("selected publication failure was not exercised", result.Errors, remote.attempts)
	}
	after, err := store.LoadStatus()
	must(t, err)
	if !maps.Equal(after.SessionIssues, status.SessionIssues) || !maps.Equal(after.IssueCounts, status.IssueCounts) || after.LastError != oldSummary {
		t.Error("selected failure erased unrelated issue or changed complete summary", after)
	}
	remote.metadataKey = ""
	result, err = runPass(f.env, false, passOptions{importBatch: firstImport})
	defer result.CloseStagedResults()
	must(t, err)
	if len(result.Errors) != 0 {
		t.Fatal("selected retry failed", result.Errors)
	}
	after, err = store.LoadStatus()
	must(t, err)
	wantSummary := issueSummary(map[string]issueTally{issueStorageUnavailable: {sessions: 1}})
	if !maps.Equal(after.SessionIssues, map[string]string{unrelated: issueStorageUnavailable}) || !maps.Equal(after.IssueCounts, map[string]int{issueStorageUnavailable: 1}) || after.LastError != wantSummary || strings.Contains(after.LastError, "failed to capture") {
		t.Error("selected success did not clear only its stale issue and tally", after)
	}
	if !after.LastScanAt.Equal(status.LastScanAt) || after.LastPublishedAt.IsZero() {
		t.Error("scoped publication replaced whole-pass observation or lost actual publication", after)
	}
}

func TestBackfillSelectedStatusCountsPolicyHeldFirstStage(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	prepare := func(project string, dates ...string) string {
		t.Helper()
		args := append([]string{"--yes", "--background", "--harness", "claude", "--project", filepath.Join(f.userHome, project)}, dates...)
		out, stderr, code := f.importRun(t, nil, false, args...)
		if code != 0 {
			t.Fatal("prepare isolated retained cohort", code, stderr, out)
		}
		batches, err := backfill.LoadBatches(f.data)
		must(t, err)
		return batches[len(batches)-1].ID
	}
	heldBatch := prepare("agent-archive", "--until", "2026-09-20")
	selectedBatch := prepare("levenshtein", "--since", "2026-09-20", "--until", "2026-09-20")
	held, heldChildren := importRegistrations(t, f.data, heldBatch)
	selected, selectedChildren := importRegistrations(t, f.data, selectedBatch)
	if len(held) != 1 || len(selected) != 1 || len(heldChildren)+len(selectedChildren) != 0 {
		t.Fatal("unexpected actual batch membership", len(held), len(selected))
	}
	store := state.OpenReadOnly(f.data)
	stagePath := filepath.Join(f.data, "sessions", held[0].ArchiveSessionID, "import-stage", "stage.json")
	beforeStage, err := os.ReadFile(stagePath)
	must(t, err)
	request, found, err := store.LoadRequest(held[0].ArchiveSessionID)
	must(t, err)
	if !found {
		t.Fatal("held first stage has no covered request")
	}
	cfg := mustLoadConfig(t, f.data)
	for i := range cfg.Archive.Projects {
		if cfg.Archive.Projects[i].ProjectID == held[0].ProjectID {
			cfg.Archive.Projects[i].Included = false
		}
	}
	must(t, config.Save(f.data, cfg))
	if cfg.AcceptSession(held[0]) || !cfg.AcceptSession(selected[0]) {
		t.Fatal("fixture did not isolate publication policy")
	}
	result, err := runPass(f.env, false, passOptions{importBatch: selectedBatch})
	defer result.CloseStagedResults()
	must(t, err)
	if len(result.Published) != 1 || result.Published[0] != selected[0].ArchiveSessionID || len(result.Errors) != 0 {
		t.Fatal("eligible selected owner did not actually publish", result)
	}
	published, err := store.LoadPublishedState(selected[0].ArchiveSessionID)
	must(t, err)
	metadata, found, err := published.LastPublishedMetadata()
	must(t, err)
	if !found {
		t.Fatal("selected publication missing")
	}
	bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
	must(t, err)
	if bundle.NativeSessionID != selected[0].NativeSessionID || !bundle.Capture.CapturedAt.Equal(selected[0].AdmittedAt) {
		t.Fatal("selected readback changed identity or capture age")
	}
	afterStage, err := os.ReadFile(stagePath)
	must(t, err)
	afterRequest, found, err := store.LoadRequest(held[0].ArchiveSessionID)
	must(t, err)
	heldKey, err := archive.MetadataObjectKey(held[0].Harness.Name, held[0].ArchiveSessionID)
	must(t, err)
	if _, err := bucket.Get(t.Context(), heldKey); !errors.Is(err, storage.ErrNotFound) || !found || afterRequest.Token != request.Token || !bytes.Equal(beforeStage, afterStage) {
		t.Fatal("counting held evidence changed bytes, request or upload permission", err)
	}
	status, err := store.LoadStatus()
	must(t, err)
	if status.PendingCount != 1 {
		t.Error("current exclusion hid an unsettled first import from global pending", status.PendingCount)
	}
	// Once a stage has settled, ordinary later excluded work keeps the existing
	// policy filtering rather than gaining a new pending/publication obligation.
	for i := range cfg.Archive.Projects {
		cfg.Archive.Projects[i].Included = false
	}
	must(t, config.Save(f.data, cfg))
	writable, err := state.Open(f.data)
	must(t, err)
	must(t, writable.SaveRequest(selected[0].ArchiveSessionID, "synthetic-later-request", backfillNow))
	result, err = runPass(f.env, false, passOptions{importBatch: selectedBatch})
	defer result.CloseStagedResults()
	must(t, err)
	status, err = store.LoadStatus()
	must(t, err)
	if status.PendingCount != 1 || len(result.Published) != 0 {
		t.Error("settled excluded work changed first-stage counting or publication policy", status.PendingCount, result)
	}
}
