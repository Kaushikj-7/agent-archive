package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type linkedChildPublicationFault struct {
	*storagetest.MemoryStore
	metadataKey string
	attempts    int
}

func (s *linkedChildPublicationFault) Put(ctx context.Context, key string, body []byte) error {
	if key == s.metadataKey {
		s.attempts++
		return errors.New("synthetic linked child metadata unavailable")
	}
	return s.MemoryStore.Put(ctx, key, body)
}

func TestBackfillLinkedChildFailureRemainsPendingUntilVerified(t *testing.T) {
	f, bucket := newImportFixture(t)
	remote := &linkedChildPublicationFault{MemoryStore: bucket}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	f.env.backfillCheckpoint = func(step string) error {
		if step != "uploading" {
			return nil
		}
		candidates, issues, err := state.OpenReadOnly(f.data).ScanSubagentCandidates()
		must(t, err)
		if len(issues) != 0 || len(candidates) != 2 {
			t.Fatal("missing actual selected child cohort", len(candidates), issues)
		}
		remote.metadataKey, err = archive.MetadataObjectKey(candidates[0].Harness.Name, candidates[0].ArchiveSessionID)
		must(t, err)
		return nil
	}
	args := []string{"--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--since", "2026-09-21"}
	out, stderr, code := f.importRun(t, nil, false, args...)
	if remote.metadataKey == "" {
		t.Fatal("child-only publication fault was not armed")
	}
	if remote.attempts == 0 {
		t.Fatal("selected child metadata PUT was never attempted", code, stderr, out)
	}
	t.Logf("child-only metadata PUT failed %d times for the exact selected key", remote.attempts)
	if code != 1 || strings.Contains(out, "Uploaded ") {
		t.Error("unfinished linked child reported completed import", code, stderr, out)
	}
	store := state.OpenReadOnly(f.data)
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 1 || len(children) != 2 {
		t.Fatal("selected logical membership changed", len(parents), len(children))
	}
	failed := 0
	var failedID string
	var frozen state.PendingPublication
	for _, reg := range children {
		key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		must(t, err)
		if key != remote.metadataKey {
			continue
		}
		failed++
		failedID = reg.ArchiveSessionID
		owed, err := store.Outstanding(reg, true)
		must(t, err)
		if owed.Published || !owed.Pending() || !owed.StagedImport || !owed.Upload {
			t.Fatal("fault did not retain an actual unpublished child obligation", owed)
		}
		var found bool
		frozen, found, err = store.LoadPending(reg.ArchiveSessionID)
		must(t, err)
		if !found {
			t.Fatal("failed child lost its frozen publication")
		}
	}
	if failed != 1 {
		t.Fatal("missing exact failed child", failed)
	}
	history, errout, historyCode := f.command(t, "backfill", "history")
	if historyCode != 0 || !strings.Contains(history, "1 subagent transcript waiting") {
		t.Error("history hid the retained linked child", historyCode, errout, history)
	}
	// The same semantic import must retry only retained evidence after native
	// deletion, even though the parent and the other child already published.
	must(t, os.RemoveAll(filepath.Join(f.userHome, ".claude")))
	remote.metadataKey, f.env.backfillCheckpoint = "", nil
	out, stderr, code = f.importRun(t, nil, false, args...)
	if code != 0 {
		t.Fatal("retained linked child retry failed", code, stderr, out)
	}
	for _, reg := range append(parents, children...) {
		published, err := store.LoadPublishedState(reg.ArchiveSessionID)
		must(t, err)
		metadata, found, err := published.LastPublishedMetadata()
		must(t, err)
		if !found {
			t.Error("linked group returned without all verified owners", reg.ArchiveSessionID)
			continue
		}
		if reg.ArchiveSessionID == failedID && !bytes.Equal(frozen.MetadataBytes, published.Metadata()) {
			t.Fatal("retained child retry changed exact pending metadata")
		}
		bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
		must(t, err)
		if bundle.NativeSessionID != reg.NativeSessionID || !bundle.Capture.CapturedAt.Equal(reg.AdmittedAt) {
			t.Fatal("retained retry changed child or parent provenance")
		}
		_, requested, err := store.LoadRequest(reg.ArchiveSessionID)
		must(t, err)
		owed, err := store.Outstanding(reg, requested)
		must(t, err)
		if owed.Pending() {
			t.Error("verified group still owes local settlement", reg.ArchiveSessionID, owed)
		}
	}
}
