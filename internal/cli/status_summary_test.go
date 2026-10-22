package cli

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestStatusConfigurationIDTracksMusePolicy(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	base := statusConfigurationID(f.cfg)
	for _, change := range []struct {
		name  string
		apply func(*config.Config)
	}{
		{"destination", func(c *config.Config) { c.Storage.Bucket = "other-destination" }},
		{"window", func(c *config.Config) { c.PauseGeneration = "new-window" }},
		{"policy", func(c *config.Config) { c.NativeRoots[0].Revision = "new-root" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			t.Parallel()
			cfg := f.cfg
			cfg.NativeRoots = append(cfg.NativeRoots[:0:0], cfg.NativeRoots...)
			change.apply(&cfg)
			cfg, err := config.ReconcileMuseCapture(cfg, f.cfg, f.observed.Add(time.Minute))
			must(t, err)
			_, err = json.Marshal(cfg)
			must(t, err)
			if got := statusConfigurationID(cfg); got == "" || got == base {
				t.Fatalf("changed %s retained fingerprint %q", change.name, got)
			}
		})
	}
	unchanged := f.cfg
	unchanged.StorageVerifiedAt = f.observed.Add(time.Minute)
	unchanged.DurableStorageProtection = true
	if statusConfigurationID(unchanged) != base {
		t.Fatal("writer protection or observation changed capture identity")
	}
}

func TestStatusConfigurationIDRejectsUnserializablePolicy(t *testing.T) {
	t.Parallel()
	_, env, cfg, summary := overviewFixture(t)
	cfg.DestinationSince = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	id := statusConfigurationID(cfg)
	summary.ConfigurationID = id
	if id != "" || validStatusSummary(summary, id, env.now()) {
		t.Fatal("unserializable policy supplied a usable summary fingerprint")
	}
}

func TestScopedImportUpdatesUploadWithoutRenewingGlobalHealth(t *testing.T) {
	t.Parallel()
	f := newStagedSyncFixture(t)
	cfg := mustLoadConfig(t, f.home)
	previous := statusSummary{Version: 1, ConfigurationID: statusConfigurationID(cfg), ObservedAt: f.env.now().Add(-time.Minute), CompletedAt: f.env.now().Add(-time.Minute), Pending: 7, PendingKnown: true, Problem: statusProblemCollection, Notices: 2}
	must(t, local.Write(filepath.Join(f.home, statusSummaryFile), previous))
	must(t, f.store.SaveStatus(state.Status{LastScanAt: previous.CompletedAt, PendingCount: previous.Pending}))
	result, err := runPass(f.env, false, passOptions{importBatch: f.reg.ImportBatch.Recorded()})
	defer result.CloseStagedResults()
	must(t, err)
	if len(result.Published) != 1 {
		t.Fatalf("published=%d", len(result.Published))
	}
	var summary statusSummary
	must(t, local.Read(filepath.Join(f.home, statusSummaryFile), &summary))
	if !summary.LastPublishedAt.Equal(f.env.now()) {
		t.Fatalf("import upload missing: %+v", summary)
	}
	withoutUpload := summary
	withoutUpload.LastPublishedAt = previous.LastPublishedAt
	withoutUpload.ObservedAt = previous.ObservedAt
	if withoutUpload != previous {
		t.Fatalf("import changed global health: %+v", summary)
	}
	result, err = runOnePass(f.env, true)
	defer result.CloseStagedResults()
	must(t, err)
	if len(result.Published) != 0 {
		t.Fatalf("ordinary pass republished %d sessions", len(result.Published))
	}
	must(t, local.Read(filepath.Join(f.home, statusSummaryFile), &summary))
	if !summary.LastPublishedAt.Equal(f.env.now()) {
		t.Fatalf("ordinary pass lost import upload: %+v", summary)
	}
}

func TestScopedUploadWithoutMatchingSummaryKeepsGlobalHealthUnknown(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		home, env, cfg, previous := overviewFixture(t)
		if missing {
			must(t, os.Remove(filepath.Join(home, statusSummaryFile)))
		} else {
			other := cfg
			other.Storage.Bucket = "other-destination"
			previous.ConfigurationID = statusConfigurationID(other)
			previous.Problem = statusProblemCapture
			previous.VerificationFailures = 4
			must(t, local.Write(filepath.Join(home, statusSummaryFile), previous))
		}
		store, err := state.Open(home)
		must(t, err)
		// Even if legacy status retains a prior destination's upload time,
		// only this pass's observed publication establishes the new timestamp.
		must(t, store.SaveStatus(state.Status{LastScanAt: env.now(), LastPublishedAt: env.now().Add(-time.Hour), PendingCount: 9}))
		w := beginStatusProgress(home, cfg, env)
		w.processed(collector.Progress{Published: true})
		w.finish(cfg, nil, false)
		var summary statusSummary
		must(t, local.Read(filepath.Join(home, statusSummaryFile), &summary))
		if !summary.LastPublishedAt.Equal(env.now()) || !summary.CompletedAt.IsZero() || summary.PendingKnown || summary.Problem != statusProblemNone || summary.VerificationFailures != 0 {
			t.Fatalf("scoped upload inherited global health: %+v", summary)
		}
		view, err := readStatusOverview(env)
		must(t, err)
		if view.state != overviewUnknown || view.uploads != "Last succeeded just now" {
			t.Fatalf("scoped upload established global health: %+v", view)
		}
	}
}

func TestStatusOverviewDistinguishesCollectionFromMaintenance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		holder     string
		collection bool
	}{
		{"sync", true}, {"scheduled collection", true}, {"backfill upload", true}, {"collect", true},
		{"purge apply", false}, {"migrate", false}, {"machine rename", false},
		{"backfill projects", false}, {"backfill undo", false}, {"unknown command", false},
	} {
		t.Run(tc.holder, func(t *testing.T) {
			t.Parallel()
			home, env, cfg, _ := overviewFixture(t)
			unlock, err := lockCollector(home, tc.holder, env.now().Add(-3*time.Hour))
			must(t, err)
			defer unlock()
			// Even matching leftover progress cannot turn maintenance into collection.
			w := beginStatusProgress(home, cfg, env)
			w.phase(statusCollecting)
			view, err := readStatusOverview(env)
			must(t, err)
			if tc.collection {
				if view.state != overviewAttention || !strings.Contains(view.problem, "expected lifetime") {
					t.Fatalf("collection lost overdue warning: %+v", view)
				}
			} else {
				if view.state == overviewProcessing || view.state == overviewAttention || !strings.Contains(view.background, "another operation") {
					t.Fatalf("maintenance reported as collection: %+v", view)
				}
			}
		})
	}
}

func TestStatusIssuesIncludesImportAndTranscriptNotices(t *testing.T) {
	t.Parallel()
	for _, view := range []statusView{
		{State: "Ready", Apps: []appStatus{{Name: "cursor", WaitingForTranscriptSessions: 1, importedWithIssues: 2}}},
		{State: "Ready", importedApps: []appStatus{{Name: "cursor", WaitingForTranscriptSessions: 1, importedWithIssues: 2}}},
	} {
		var out strings.Builder
		printStatusIssues(&out, view, statusScreen{})
		for _, want := range []string{"Cursor", "1 chat has no transcript yet", "2 imported sessions have a capture gap or failed scan"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q: %s", want, &out)
			}
		}
	}
}

func TestBackfillCapacityDrainRecordsItsUploadTime(t *testing.T) {
	t.Parallel()
	f := newStagedSyncFixture(t)
	cfg := mustLoadConfig(t, f.home)
	guard, unlock, err := lockCollectorGuard(f.home, "backfill import", f.env.now(), collectorNeedsGuard(cfg))
	must(t, err)
	defer unlock()
	drain := importCapacityDrain(f.env, io.Discard, f.home, f.store, f.reg.ImportBatch.Recorded(), guard, nil, nil, nil)
	must(t, drain(t.Context()))
	var summary statusSummary
	must(t, local.Read(filepath.Join(f.home, statusSummaryFile), &summary))
	if !summary.LastPublishedAt.Equal(f.env.now()) || !summary.CompletedAt.IsZero() || summary.PendingKnown {
		t.Fatalf("capacity drain upload not recorded as scoped: %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(f.home, statusProgressFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("capacity drain left its progress file: %v", err)
	}
}

func TestUnenumeratedVerificationKeepsPreviousFailureCount(t *testing.T) {
	t.Parallel()
	home, env, cfg, previous := overviewFixture(t)
	previous.VerificationFailures = 3
	must(t, local.Write(filepath.Join(home, statusSummaryFile), previous))
	store, err := state.Open(home)
	must(t, err)
	must(t, os.RemoveAll(filepath.Join(home, "registrations")))
	must(t, os.WriteFile(filepath.Join(home, "registrations"), []byte("not a directory"), 0600))
	verification, err := verifyPublicationsWithin(t.Context(), home, cfg, env, store, storagetest.NewMemoryStore())
	if err == nil {
		t.Fatal("verification enumerated an unreadable registrations directory")
	}
	w := beginStatusProgress(home, cfg, env)
	w.verified(verification)
	w.finish(cfg, err, true)
	var summary statusSummary
	must(t, local.Read(filepath.Join(home, statusSummaryFile), &summary))
	if summary.VerificationFailures != 3 {
		t.Fatalf("failed enumeration reset unresolved failures to %d", summary.VerificationFailures)
	}
}

// A directory listing can succeed while a registration cannot be read. That
// still does not prove that the session's prior verification failure is gone.
func TestPartialVerificationKeepsPreviousFailureCount(t *testing.T) {
	t.Parallel()
	home, env, cfg, previous := overviewFixture(t)
	previous.VerificationFailures = 3
	must(t, local.Write(filepath.Join(home, statusSummaryFile), previous))
	store, err := state.Open(home)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(home, "registrations", "unreadable.json"), []byte("{"), 0600))
	verification, err := verifyPublicationsWithin(t.Context(), home, cfg, env, store, storagetest.NewMemoryStore())
	must(t, err)
	w := beginStatusProgress(home, cfg, env)
	w.verified(verification)
	w.finish(cfg, nil, true)
	var summary statusSummary
	must(t, local.Read(filepath.Join(home, statusSummaryFile), &summary))
	if summary.VerificationFailures != 3 {
		t.Fatalf("partial registration scan reset unresolved failures to %d", summary.VerificationFailures)
	}
}

func TestIncompleteVerificationRecordsKnownFailuresWithoutClearingPreviousOnes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		complete bool
		observed int
		want     int
	}{
		{"incomplete with no observed failures", false, 0, 3},
		{"incomplete with more observed failures", false, 5, 5},
		{"incomplete with fewer observed failures", false, 1, 3},
		{"complete with fewer failures", true, 1, 1},
		{"complete and successful", true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, env, cfg, previous := overviewFixture(t)
			previous.VerificationFailures = 3
			must(t, local.Write(filepath.Join(home, statusSummaryFile), previous))
			w := beginStatusProgress(home, cfg, env)
			w.verified(verificationSummary{Enumerated: tc.complete, UnresolvedFailures: tc.observed})
			w.finish(cfg, nil, true)
			var summary statusSummary
			must(t, local.Read(filepath.Join(home, statusSummaryFile), &summary))
			if summary.VerificationFailures != tc.want {
				t.Fatalf("recorded %d verification failures, want %d", summary.VerificationFailures, tc.want)
			}
		})
	}
}
