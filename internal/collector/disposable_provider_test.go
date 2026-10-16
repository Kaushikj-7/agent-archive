package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

// Embedding the concrete store preserves its bounded, versioned and conditional
// capabilities; only the acknowledgement of an actual successful Put is lost.
type uncertainDisposableCommit struct {
	*providertest.DisposableS3
	key  string
	lost bool
}

func (s *uncertainDisposableCommit) Put(ctx context.Context, key string, body []byte) error {
	if err := s.DisposableS3.Put(ctx, key, body); err != nil {
		return err
	}
	if key == s.key && !s.lost {
		s.lost = true
		return errors.New("injected lost metadata acknowledgement")
	}
	return nil
}

func TestDisposableProviderReadySelectionRecoversExactCommit(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	store := newTestStore(t)
	cfg, _, err := config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	if err = config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
	reg := registration(t, path)
	if err = store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	key := "sessions/codex/" + reg.ArchiveSessionID + "/metadata.json"
	uncertain := &uncertainDisposableCommit{DisposableS3: remote, key: key}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-provider", Now: func() time.Time { return at }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	result, err := Run(t.Context(), store, uncertain, opts)
	if err != nil || !uncertain.lost || len(result.Errors) == 0 {
		t.Fatal("lost acknowledgement not retained", result, err)
	}
	pending, found, err := store.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || pending.JournalVersion != 2 || pending.Selection == nil {
		t.Fatal("missing current selecting authority", found, err)
	}
	proof := *pending.Selection
	exact := bytes.Clone(pending.MetadataBytes)
	raw, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(raw, exact) {
		t.Fatal("fault did not follow actual commit", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	store, err = openTestStore(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err = store.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || *pending.Selection != proof || !bytes.Equal(pending.MetadataBytes, exact) {
		t.Fatal("restart changed exact authority", err)
	}
	result, err = Run(t.Context(), store, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("exact committed replay failed", result, err)
	}
	raw, err = remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(raw, exact) {
		t.Fatal("replay replaced selected bytes", err)
	}
	peer := remote.IndependentOwner(t)
	metadata := fetchMetadata(t, peer, reg.Harness.Name, reg.ArchiveSessionID)
	bundle, err := reader.LoadSource(t.Context(), peer, metadata, reader.Limits{})
	if err != nil || len(bundle.NativeRecords) == 0 {
		t.Fatal("independent retained read after native loss failed", err)
	}
	if _, found, err = store.LoadPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("acknowledged pending retained", found, err)
	}
}

func TestDisposableProviderStagedGroupSurvivesNativeLoss(t *testing.T) {
	t.Run("committed", func(t *testing.T) { disposableStagedGroup(t, false) })
	t.Run("newer-parent-request", func(t *testing.T) { disposableStagedGroup(t, true) })
}

func disposableStagedGroup(t *testing.T, loseAcknowledgement bool) {
	t.Helper()
	remote := providertest.NewDisposableS3(t)
	store, parent, child, opts := stagedUnadmittedGroupFixture(t)
	for _, path := range []string{parent.TranscriptPath, child.TranscriptPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	guard, err := local.LockCollectorGuard(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	opts.CollectorGuard = guard
	var selected []byte
	if !loseAcknowledgement {
		result, runErr := Run(t.Context(), store, remote, opts)
		defer result.CloseStagedResults()
		if runErr != nil || len(result.Errors) != 0 || len(result.Published) != 2 {
			t.Fatal("staged parent/child publication failed", result, runErr)
		}
	} else {
		uncertain := &uncertainDisposableCommit{DisposableS3: remote, key: "sessions/claude/" + parent.ArchiveSessionID + "/metadata.json"}
		result, runErr := Run(t.Context(), store, uncertain, opts)
		result.CloseStagedResults()
		if runErr != nil || !uncertain.lost || len(result.Errors) == 0 {
			t.Fatal("staged uncertain acknowledgement was not retained", result, runErr)
		}
		pending, found, loadErr := store.LoadPending(parent.ArchiveSessionID)
		if loadErr != nil || !found || pending.JournalVersion != 3 || pending.StagedSelection == nil || pending.StagedSelection.StageSHA256 != parent.StagedImportSHA256 {
			t.Fatal("staged publication lost current stage3 authority", found, loadErr)
		}
		proof := *pending.StagedSelection
		selected = bytes.Clone(pending.MetadataBytes)
		request, requested, requestErr := store.LoadRequest(parent.ArchiveSessionID)
		if requestErr != nil || !requested || request.Token == proof.CoveredToken || len(request.HookEvidence) == 0 {
			t.Fatal("child publication did not retain newer parent evidence", requested, requestErr)
		}
		newerToken := request.Token
		store, loadErr = openTestStore(store.Home())
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		result, runErr = Run(t.Context(), store, remote, opts)
		defer result.CloseStagedResults()
		// The child notification is genuine newer work. The protected stage cannot
		// acknowledge it using the old selecting proof or discard either obligation.
		if runErr != nil || !errors.Is(result.Errors[parent.ArchiveSessionID], state.ErrReadySelectionFrozen) {
			t.Fatal("newer request did not hold frozen parent selection", result, runErr)
		}
		for id, issue := range result.Errors {
			if id != parent.ArchiveSessionID || !errors.Is(issue, state.ErrReadySelectionFrozen) {
				t.Fatal("unexpected staged failure", id, issue)
			}
		}
		retained, found, loadErr := store.LoadPending(parent.ArchiveSessionID)
		if loadErr != nil || !found || retained.StagedSelection == nil || *retained.StagedSelection != proof || !bytes.Equal(retained.MetadataBytes, selected) {
			t.Fatal("refusal changed immutable staged selection", found, loadErr)
		}
		request, requested, requestErr = store.LoadRequest(parent.ArchiveSessionID)
		if requestErr != nil || !requested || request.Token != newerToken {
			t.Fatal("refusal lost newer parent request", requested, requestErr)
		}
		if _, found, loadErr = store.LoadImportStage(t.Context(), parent); loadErr != nil || !found {
			t.Fatal("refusal lost parent stage", found, loadErr)
		}
		raw, readErr := remote.Get(t.Context(), pending.MetadataKey)
		if readErr != nil || !bytes.Equal(raw, selected) {
			t.Fatal("refusal replaced selected provider bytes", readErr)
		}
	}
	peer := remote.IndependentOwner(t)
	for _, reg := range []struct{ id, harness string }{{parent.ArchiveSessionID, parent.Harness.Name}, {child.ArchiveSessionID, child.Harness.Name}} {
		metadata := fetchMetadata(t, peer, reg.harness, reg.id)
		bundle, readErr := reader.LoadSource(t.Context(), peer, metadata, reader.Limits{})
		if readErr != nil || len(bundle.NativeRecords) == 0 {
			t.Fatal("retained staged source unreadable", readErr)
		}
		if _, found, loadErr := store.LoadPending(reg.id); loadErr != nil || found != (loseAcknowledgement && reg.id == parent.ArchiveSessionID) {
			t.Fatal("acknowledged staged journal retained", found, loadErr)
		}
	}
}
