package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/retention"
)

func enableSearchForTest(t *testing.T, home string) config.Config {
	t.Helper()
	cfg, found, err := config.Load(home)
	must(t, err)
	if !found {
		t.Fatal("missing test config")
	}
	must(t, config.SetSearchIndexing(&cfg, true))
	must(t, config.Save(home, cfg))
	cfg, _, err = config.Load(home)
	must(t, err)
	return cfg
}

func TestCollectionEntryPointsDispatchQualifiedHintsAfterEveryInnerDefer(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"sync", "_collect"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			home, env, _ := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
			cfg := enableSearchForTest(t, home)
			reg := theRegistration(t, home)
			called := 0
			env.sessionChangeConsumers = []sessionChangeConsumer{{sink: changeSinkFunc(func(_ context.Context, batch SessionChangeBatch) error {
				called++
				if batch.DestinationID != cfg.DestinationID() || len(batch.Sessions) != 1 || batch.Sessions[0].Harness != reg.Harness.Name || batch.Sessions[0].ArchiveSessionID != reg.ArchiveSessionID {
					t.Fatal("wrong captured publication facts", batch)
				}
				release, err := local.Lock(home)
				must(t, err)
				release()
				for _, name := range []string{"hooks.lock", "search-worker.lock"} {
					release, err := local.NamedLock(home, name)
					must(t, err)
					release()
				}
				if _, err := os.Stat(filepath.Join(home, machineRegistrationFile)); err != nil {
					t.Fatal("hint preceded deferred machine registration", err)
				}
				return errors.New("sink must not affect capture")
			})}}
			var out, diagnostics bytes.Buffer
			if code := Run([]string{command}, nil, &out, &diagnostics, env); code != 0 {
				t.Fatal("sink changed capture exit", code, diagnostics.String())
			}
			if called != 1 {
				t.Fatal("missing post-pass hint", called)
			}
			if strings.Contains(diagnostics.String(), "sink must not affect capture") {
				t.Fatal("consumer error leaked into capture error")
			}
		})
	}
}

func TestPartialCollectionErrorsStillDispatchCandidatesAfterLockRelease(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads protected fixture files")
	}
	for _, failure := range []string{"verification", "retention", "health"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			home, env, _ := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
			enableSearchForTest(t, home)
			reg := theRegistration(t, home)
			if failure == "health" {
				must(t, os.Mkdir(filepath.Join(home, "storage-health.json"), 0o700))
			}
			if failure == "retention" {
				env.sweepClock = func(*retention.Options) { must(t, os.Chmod(filepath.Join(home, "registrations"), 0)) }
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "registrations"), 0o700) })
			}
			called := 0
			env.sessionChangeConsumers = []sessionChangeConsumer{{sink: changeSinkFunc(func(_ context.Context, batch SessionChangeBatch) error {
				called++
				release, err := local.Lock(home)
				must(t, err)
				release()
				if len(batch.Sessions) != 1 || batch.Sessions[0].ArchiveSessionID != reg.ArchiveSessionID {
					t.Fatal(batch)
				}
				return errors.New("independent sink failure")
			})}}
			var progressCallback func(collector.Progress)
			if failure == "verification" {
				path := filepath.Join(home, "published", reg.ArchiveSessionID+".json")
				progressCallback = func(progress collector.Progress) {
					if progress.Published {
						must(t, os.Chmod(path, 0))
					}
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			}
			result, err := runPass(env, false, passOptions{progress: progressCallback})
			defer result.CloseStagedResults()
			if err == nil || strings.Contains(err.Error(), "independent sink") {
				t.Fatal("original partial error lost", err)
			}
			if len(result.Published) != 1 || called != 1 {
				t.Fatal("partial-error candidate dropped", result.Published, called, err)
			}
		})
	}
}

func TestCapturedDestinationRemainsOriginalAfterUnlockedConfigChange(t *testing.T) {
	t.Parallel()
	home, env, _ := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	original := enableSearchForTest(t, home)
	var captured passChanges
	result, err := runPassLocked(env, false, passOptions{}, &captured)
	defer result.CloseStagedResults()
	must(t, err)
	release, err := local.Lock(home)
	must(t, err)
	current := original
	current.Storage.Bucket = "new-destination"
	must(t, config.Save(home, current))
	release()
	seen := 0
	consumers := []sessionChangeConsumer{{sink: changeSinkFunc(func(_ context.Context, batch SessionChangeBatch) error {
		seen++
		if batch.DestinationID != original.DestinationID() {
			t.Fatal("batch redirected after unlock", batch)
		}
		return nil
	})}}
	dispatchSessionChanges(t.Context(), captured.destination, result.PublicationCandidates, consumers)
	if seen != 1 || captured.namespace == "" || captured.home != home {
		t.Fatal("captured pass context missing", seen, captured)
	}
}

func TestBackfillUsesSamePostLockSessionChangeBoundary(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	enableSearchForTest(t, f.data)
	batches := 0
	identities := 0
	f.env.sessionChangeConsumers = []sessionChangeConsumer{{sink: changeSinkFunc(func(_ context.Context, batch SessionChangeBatch) error {
		release, err := local.Lock(f.data)
		must(t, err)
		release()
		batches++
		identities += len(batch.Sessions)
		return errors.New("backfill consumer unavailable")
	})}}
	out, diagnostics, code := f.importRun(t, strings.NewReader("y\n"), true)
	if code != 0 || batches == 0 || identities == 0 {
		t.Fatal("backfill hints missing or changed capture", code, batches, identities, out, diagnostics)
	}
}
