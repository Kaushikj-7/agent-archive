package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type retainedChildRetryFixture struct {
	f        *backfillFixture
	bucket   *storagetest.MemoryStore
	store    *state.Store
	children map[string]state.ImportStage
	args     []string
}

func prepareUnregisteredChildRetry(t *testing.T) retainedChildRetryFixture {
	t.Helper()
	f, bucket := newImportFixture(t)
	store := state.OpenReadOnly(f.data)
	dir := filepath.Join(f.data, "registrations")
	t.Cleanup(func() { must(t, os.Chmod(dir, 0700)) })
	children := map[string]state.ImportStage{}
	armed := false
	f.env.backfillCheckpoint = func(step string) error {
		if step != "uploading" {
			return nil
		}
		stages, _, err := store.ImportStages(t.Context(), "", 50)
		must(t, err)
		for _, stage := range stages {
			if stage.Registration.ParentSessionID != "" {
				full, found, err := store.LoadImportStage(t.Context(), stage.Registration)
				must(t, err)
				if !found {
					t.Fatal("selected child stage absent at actual write-fault boundary")
				}
				children[stage.Registration.ArchiveSessionID] = full
			}
		}
		armed = true
		return os.Chmod(dir, 0500)
	}
	args := []string{"--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--since", "2026-09-21"}
	out, stderr, code := f.importRun(t, nil, false, args...)
	if !armed || len(children) != 2 || code != 1 || strings.Contains(out, "Uploaded ") {
		t.Fatal("initial confirmed child-write fault did not retain exact obligations", armed, len(children), code, stderr, out)
	}
	checkUnregisteredChildrenRetained(t, f, store, bucket, children, stderr, out)
	must(t, os.Chmod(dir, 0700))
	f.env.backfillCheckpoint = nil
	return retainedChildRetryFixture{f, bucket, store, children, args}
}

func TestBackfillDelayedRetainedChildrenPreserveCaptureAge(t *testing.T) {
	fixture := prepareUnregisteredChildRetry(t)
	must(t, os.RemoveAll(filepath.Join(fixture.f.userHome, ".claude")))
	assertRetryNativeDeleted(t, fixture.f.userHome)
	fixture.f.env.Now = func() time.Time { return backfillNow.Add(24 * time.Hour) }
	out, stderr, code := fixture.f.importRun(t, nil, false, fixture.args...)
	if code != 0 {
		t.Fatal("delayed native-deleted retained retry did not publish", code, stderr, out)
	}
	parents, children := importRegistrations(t, fixture.f.data, firstImport)
	if len(parents) != 1 || len(children) != 2 {
		t.Fatal("delayed retry changed exact logical owners", len(parents), len(children))
	}
	checkRetainedParentReadback(t, fixture, parents[0])
	var refreshed []bool
	for _, child := range children {
		stage, expected := fixture.children[child.ArchiveSessionID]
		if !expected || child.StagedImportSHA256 != stage.SHA256 || !child.AdmittedAt.Equal(stage.Registration.AdmittedAt) || child.NativeSessionID != stage.Registration.NativeSessionID {
			t.Fatal("delayed retry changed frozen child admission or identity")
		}
		bundle := readPublishedChild(t, fixture, child)
		if bundle.NativeSessionID != stage.Bundle.NativeSessionID {
			t.Fatal("delayed retry published another native child")
		}
		refreshed = append(refreshed, !bundle.Capture.CapturedAt.Equal(stage.Bundle.Capture.CapturedAt))
	}
	t.Log("actual native-deleted retained retry advanced synthetic clock 24h; parent and both child remote readbacks complete; exact stage digests, admission instants and native identities preserved")
	for _, changed := range refreshed {
		if changed {
			t.Error("delayed retained retry refreshed child capture age")
		}
	}
}

func TestBackfillUnknownRetainedChildrenDoNotPrintUploaded(t *testing.T) {
	fixture := prepareUnregisteredChildRetry(t)
	must(t, os.RemoveAll(filepath.Join(fixture.f.userHome, ".claude")))
	assertRetryNativeDeleted(t, fixture.f.userHome)
	batch, _ := loadBatch(t, fixture.f.data)
	pending := upload{home: fixture.f.data, batch: batch.ID}
	must(t, pending.refresh())
	if pending.pendingCount() != 2 {
		t.Fatal("valid retained-child census did not precede corruption")
	}
	before := map[string][]byte{}
	for id := range fixture.children {
		raw, err := os.ReadFile(filepath.Join(fixture.f.data, "sessions", id, "import-stage", "stage.json"))
		must(t, err)
		before[id] = raw
	}
	remoteBefore := bucketSnapshot(t, fixture.bucket)
	broken := []byte("{synthetic-unreadable")
	corrupted := false
	fixture.f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		// runPass opens storage after uploadImport's actual initial refresh.
		// The parent's earlier verified publication has already retired its
		// stage, so admission recovery cannot requeue these child candidates.
		if !corrupted {
			for id := range fixture.children {
				must(t, os.WriteFile(filepath.Join(fixture.f.data, "subagent-candidates", id+".json"), broken, 0600))
			}
			corrupted = true
		}
		return fixture.bucket, nil
	}
	out, stderr, code := fixture.f.importRun(t, nil, false, fixture.args...)
	checkUnknownRetainedRetryBoundary(t, fixture, before, remoteBefore, broken, corrupted)
	if code != 1 || strings.Contains(out, "Uploaded ") || !strings.Contains(out, "Import publication is unfinished.") || strings.Contains(out, "\n not uploaded yet.") {
		t.Error("unknown retained children reported successful upload", code, stderr, out)
	}
}

func checkUnknownRetainedRetryBoundary(t *testing.T, fixture retainedChildRetryFixture, before map[string][]byte, remoteBefore string, broken []byte, corrupted bool) {
	t.Helper()
	parents, children := importRegistrations(t, fixture.f.data, firstImport)
	if !corrupted || len(parents) != 1 || len(children) != 0 {
		t.Fatal("actual selected candidate corruption did not isolate unregistered retry")
	}
	checkRetainedParentReadback(t, fixture, parents[0])
	quarantined := fixture.store.QuarantinedFiles()
	if len(quarantined) != 2 {
		t.Fatal("actual collector did not quarantine both selected candidates")
	}
	for _, path := range quarantined {
		raw, err := os.ReadFile(filepath.Join(fixture.f.data, path))
		must(t, err)
		if !bytes.Equal(raw, broken) {
			t.Fatal("actual candidate quarantine changed decode-failure evidence")
		}
	}
	for id, stage := range fixture.children {
		_, found, err := fixture.store.LoadSubagentCandidate(id)
		must(t, err)
		if found {
			t.Fatal("quarantined selected candidate reappeared")
		}
		current, found, err := fixture.store.LoadImportStage(t.Context(), stage.Registration)
		must(t, err)
		if !found || current.SHA256 != stage.SHA256 {
			t.Fatal("quarantine lost exact retained child stage")
		}
		raw, err := os.ReadFile(filepath.Join(fixture.f.data, "sessions", id, "import-stage", "stage.json"))
		must(t, err)
		if !bytes.Equal(before[id], raw) {
			t.Fatal("candidate quarantine rewrote exact retained child stage bytes")
		}
	}
	if remoteBefore != bucketSnapshot(t, fixture.bucket) {
		t.Fatal("failed unknown-child pass changed remote publication")
	}
	t.Log("actual retained retry quarantined 2 selected candidates after valid initial census; parent publication/readback retained; exact child stages=2 child registrations=0")
}

func checkRetainedParentReadback(t *testing.T, fixture retainedChildRetryFixture, parent archive.SessionRegistration) {
	t.Helper()
	bundle := readPublishedChild(t, fixture, parent)
	if bundle.NativeSessionID != parent.NativeSessionID || !bundle.Capture.CapturedAt.Equal(parent.AdmittedAt) {
		t.Fatal("retained parent publication lost source identity or capture age")
	}
}

func readPublishedChild(t *testing.T, fixture retainedChildRetryFixture, child archive.SessionRegistration) archive.SourceBundle {
	t.Helper()
	published, err := fixture.store.LoadPublishedState(child.ArchiveSessionID)
	must(t, err)
	metadata, found, err := published.LastPublishedMetadata()
	must(t, err)
	if !found {
		t.Fatal("required retained owner has no actual published metadata")
	}
	bundle, err := reader.LoadSource(t.Context(), fixture.bucket, metadata, reader.Limits{})
	must(t, err)
	return bundle
}

func assertRetryNativeDeleted(t *testing.T, home string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(home, ".claude"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native files still present for retained retry")
	}
}
