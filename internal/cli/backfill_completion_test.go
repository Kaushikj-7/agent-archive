package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestBackfillInterruptedUnknownRetainedChildrenDoNotPrintUploaded(t *testing.T) {
	fixture := prepareUnregisteredChildRetry(t)
	must(t, os.RemoveAll(filepath.Join(fixture.f.userHome, ".claude")))
	assertRetryNativeDeleted(t, fixture.f.userHome)
	batch, _ := loadBatch(t, fixture.f.data)
	pending := upload{home: fixture.f.data, batch: batch.ID}
	must(t, pending.refresh())
	if pending.pendingCount() != 2 {
		t.Fatal("valid retained-child census did not precede interruption")
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
	signals := make(chan os.Signal, 1)
	fixture.f.env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	out := &notifyingWriter{wrote: make(chan struct{})}
	const notice = "Stopping after the current session; press Ctrl-C again to quit."
	fixture.f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if !corrupted {
			for id := range fixture.children {
				must(t, os.WriteFile(filepath.Join(fixture.f.data, "subagent-candidates", id+".json"), broken, 0600))
			}
			corrupted = true
			// The main command is inside this callback, so its next output can
			// only come from the real watch handling the injected first Ctrl-C.
			out.mu.Lock()
			out.n = 1
			out.mu.Unlock()
			signals <- os.Interrupt
			select {
			case <-out.wrote:
			case <-time.After(5 * time.Second):
				t.Fatal("real upload watch did not acknowledge injected Ctrl-C")
			}
			if !strings.Contains(out.String(), notice) {
				t.Fatal("interruption acknowledgement was not the real watch notice")
			}
		}
		return fixture.bucket, nil
	}
	fixture.f.env.IsTerminal = func(any) bool { return false }
	var stderr bytes.Buffer
	code := Run(append([]string{"backfill"}, fixture.args...), nil, out, &stderr, fixture.f.env)
	checkUnknownRetainedRetryBoundary(t, fixture, before, remoteBefore, broken, corrupted)
	if err := pending.refresh(); err == nil || pending.pendingCount() != 0 {
		t.Fatal("actual interrupted selected-child census was not unknown with zero provisional count", err, pending.pendingCount())
	}
	t.Log("actual Ctrl-C acknowledged by real watch; parent remotely read back; native input gone; two exact child stages retained; child registrations=0; unknown zero census; remote unchanged")
	if code != 0 || strings.Contains(out.String(), "Uploaded ") || !strings.Contains(out.String(), "Import publication is unfinished.") {
		t.Error("interrupted unknown retained children reported successful upload", code, stderr.String(), out.String())
	}
}

func TestImportCompletionKeepsUnknownCensusAheadOfStopAndPause(t *testing.T) {
	t.Parallel()
	unknown := errors.New("synthetic selected child census unavailable")
	for _, test := range []struct {
		name        string
		pending     int
		err         error
		interrupted bool
		want        string
	}{
		{"unknown", 0, unknown, false, "Import publication is unfinished."},
		{"interrupted unknown", 0, unknown, true, "Import publication is unfinished."},
		{"paused unknown", 0, errors.Join(errPaused, unknown), false, "Import publication is unfinished."},
		{"known interrupted", 1, unknown, true, "Stopped. The remaining 1 subagent transcript"},
		{"known paused", 1, errPaused, false, "Collection is paused. The remaining 1 subagent transcript"},
		{"settled interrupted", 0, nil, true, "Uploaded 1 session"},
		{"settled", 0, nil, false, "Uploaded 1 session"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			u := upload{total: 1, childrenPending: test.pending}
			var out bytes.Buffer
			u.printCompletion(&out, test.err, test.interrupted)
			if !strings.HasPrefix(out.String(), test.want) {
				t.Errorf("completion = %q; want prefix %q", out.String(), test.want)
			}
		})
	}
}

func TestBackfillTypedPreparedStageResumesWithNativeInputPresent(t *testing.T) {
	f, bucket := newImportFixture(t)
	cfg := mustLoadConfig(t, f.data)
	cfg.DurableStorageProtection, cfg.PublicationCompositionProtection = true, true
	cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true
	must(t, config.Save(f.data, cfg))
	interrupted := errors.New("synthetic typed preparation interruption")
	f.env.backfillCheckpoint = func(step string) error {
		if step == "staged" {
			return interrupted
		}
		return nil
	}
	args := []string{"--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--until", "2026-09-20"}
	out, stderr, code := f.importRun(t, nil, false, args...)
	if code != 1 || !strings.Contains(stderr, interrupted.Error()) {
		t.Fatal("typed stage-before-admission boundary not reached", code, stderr, out)
	}
	store := state.OpenReadOnly(f.data)
	stages, _, closeStages, err := store.ImportStagesOwned(t.Context(), "", 50)
	defer closeStages()
	must(t, err)
	if len(stages) != 1 || !stages[0].TypedSelection {
		t.Fatal("exact typed preparation missing")
	}
	selected := stages[0]
	if _, found, err := store.LoadRegistration(selected.Registration.ArchiveSessionID); err != nil || found {
		t.Fatal("prepared owner already admitted", found, err)
	}
	if _, err := os.Stat(selected.Registration.TranscriptPath); err != nil {
		t.Fatal("native input missing before recovered admission", err)
	}
	planned := 0
	f.env.observeBackfillPlan = func(plan backfill.Plan) { planned = len(plan.Imported()) }
	f.env.backfillCheckpoint = nil
	out, stderr, code = f.importRun(t, nil, false, args...)
	if code != 0 || planned != 1 {
		t.Fatal("typed recovery with stale planned candidate failed", code, planned, stderr, out)
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 1 || len(children) != 0 || parents[0].StagedImportSHA256 != selected.SHA256 {
		t.Fatal("recovery changed typed logical owner")
	}
	published, err := store.LoadPublishedState(parents[0].ArchiveSessionID)
	must(t, err)
	metadata, found, err := published.LastPublishedMetadata()
	must(t, err)
	if !found {
		t.Fatal("recovered typed owner not published")
	}
	bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
	must(t, err)
	if bundle.NativeSessionID != selected.Registration.NativeSessionID || !bundle.Capture.CapturedAt.Equal(selected.Registration.AdmittedAt) {
		t.Fatal("recovered typed readback changed original provenance")
	}
	if _, found, err := store.LoadImportStage(t.Context(), parents[0]); err != nil || found {
		t.Fatal("recovered typed selection not settled", found, err)
	}
	t.Log("actual typed stage before admission; native input present; one stale planned candidate; recovered owner published and read back with exact original stage digest and capture age")
}
