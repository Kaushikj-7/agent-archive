package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type durableFixtureMode string

const (
	durableFixtureMissing            durableFixtureMode = "missing"
	durableFixtureCorrupt            durableFixtureMode = "corrupt"
	durableFixtureFuture             durableFixtureMode = "future"
	durableFixtureKnown              durableFixtureMode = "known"
	durableFixtureHeader             durableFixtureMode = "header"
	durableFixtureHeaderAfterSummary durableFixtureMode = "header-after-summary"
)

func durableRef(data []byte) archive.SourceReference {
	sum := sha256.Sum256(data)
	return archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(data)}
}

func sparseDurable(t *testing.T, path string, n int64) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(n); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestDurableQuotaRefusesBeforeHistoryAllocation(t *testing.T) {
	s := newTestStore(t)
	sparseDurable(t, filepath.Join(s.home, "pending", "other.json"), durableStorageQuota/2)
	data := []byte("synthetic stage")
	if _, e := s.StagePendingSource("session", durableRef(data), data); !errors.Is(e, ErrDurableStorageCapacity) {
		t.Fatalf("quota: %v", e)
	}
	if _, e := os.Stat(filepath.Join(s.home, "sessions", "session")); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("allocated stage root: %v", e)
	}
	cfg, _, e := config.Load(s.home)
	if e != nil || !cfg.DurableStorageProtection || cfg.SchemaVersion != 7 {
		t.Fatalf("durable gate: %+v %v", cfg, e)
	}
}

func TestDurableQuotaChargesPhysicalHistoryDuplicatesAndTemps(t *testing.T) {
	s := newTestStore(t)
	data := []byte("synthetic")
	for _, id := range []string{"one", "two"} {
		if _, e := s.StagePendingSource(id, durableRef(data), data); e != nil {
			t.Fatal(e)
		}
	}
	sparseDurable(t, filepath.Join(s.home, "sessions", "one", "pending-sources", ".pending-123"), 7)
	e := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { _ = q.Close() }()
		u, e := q.usageContext(t.Context())
		if e != nil {
			return e
		}
		if u.physical != 2*int64(len(data))+7 || u.charged != 2*u.physical {
			t.Fatalf("usage: %+v", u)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}

func TestDurableAtomicShrinkGrowthAndInterruptedCopies(t *testing.T) {
	s := newTestStore(t)
	sparseDurable(t, filepath.Join(s.home, "pending", "old.json"), 100)
	e := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { _ = q.Close() }()
		for _, size := range []int{25, 150, 150} {
			data := make([]byte, size)
			if e = q.writeContext(t.Context(), "pending/old.json", int64(size), func(w io.Writer) error { _, e := w.Write(data); return e }); e != nil {
				return e
			}
			_ = data
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}

func TestSourceOnlyDurableWorkSurvivesWithoutRegistration(t *testing.T) {
	s := newTestStore(t)
	data := []byte("sole original input")
	if _, e := s.StagePendingSource("orphan", durableRef(data), data); e != nil {
		t.Fatal(e)
	}
	reopened := OpenReadOnly(s.home)
	if owed, e := reopened.HasPending("orphan"); !owed || e != nil {
		t.Fatalf("pending %v %v", owed, e)
	}
	if _, found, e := reopened.ForCollectorPass().LoadPending("orphan"); !found || !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("load %v %v", found, e)
	}
	obligations, e := reopened.DurableStorageObligations()
	if e != nil || len(obligations) != 1 || obligations[0].SessionID != "orphan" {
		t.Fatalf("obligations %+v %v", obligations, e)
	}
	if e = s.SweepPendingSources("orphan"); !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("sweep: %v", e)
	}
	if e = s.RemovePending("orphan"); !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("cleanup: %v", e)
	}
}

func TestOpaqueEvidenceProtectsHistoryAndCorruptPending(t *testing.T) {
	s := newTestStore(t)
	id := "session"
	data := []byte("original")
	stage, e := s.StagePendingSource(id, durableRef(data), data)
	if e != nil {
		t.Fatal(e)
	}
	sparseDurable(t, filepath.Join(s.home, "publication-evidence", id, "journal-orphan.tmp"), 11)
	if e = os.WriteFile(s.pendingPath(id), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, found, e := s.ForCollectorPass().LoadPending(id); !found || !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("load %v %v", found, e)
	}
	if e = s.SweepPendingSources(id); !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("sweep: %v", e)
	}
	if e = s.RemovePending(id); !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("cleanup: %v", e)
	}
	if _, e = s.ReadPendingSource(id, stage); e != nil {
		t.Fatal("original input lost", e)
	}
	if _, e = os.Stat(s.pendingPath(id)); e != nil {
		t.Fatal("corrupt descriptor lost", e)
	}
}

func TestUnavailableAdmissionGetsNoQuotaCredit(t *testing.T) {
	s := newTestStore(t)
	sparseDurable(t, filepath.Join(s.home, "admission-stages", "orphan.source.gz"), 9)
	data := []byte("new")
	if _, e := s.StagePendingSource("session", durableRef(data), data); !errors.Is(e, ErrDurableStorageRecovery) {
		t.Fatalf("unavailable owner: %v", e)
	}
	if _, e := os.Stat(filepath.Join(s.home, "sessions", "session")); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("allocated after unknown admission: %v", e)
	}
}

func TestDurableWriterHomeSwapCannotWriteOutsideHeldRoot(t *testing.T) {
	s := newTestStore(t)
	outside := t.TempDir()
	old := s.home + "-held"
	s.onLockWait = func(name string) {
		if name != "temporary-quota" {
			return
		}
		if e := os.Rename(s.home, old); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = os.Remove(s.home); _ = os.Rename(old, s.home) })
		if e := os.Symlink(outside, s.home); e != nil {
			t.Fatal(e)
		}
	}
	data := []byte("synthetic")
	if _, e := s.StagePendingSource("session", durableRef(data), data); e == nil {
		t.Fatal("swapped home accepted")
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatalf("outside writes: %v %v", entries, e)
	}
}

func TestDurableSharedStoresSerializeHistoryWrites(t *testing.T) {
	s := newTestStore(t)
	other := OpenReadOnly(s.home)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, store := range []*Store{s, other} {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			data := []byte{byte(i + 1)}
			_, e := store.StagePendingSource("session", durableRef(data), data)
			errs <- e
		}(i, store)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestInterruptedDurableAtomicWriteRemainsCharged(t *testing.T) {
	s := newTestStore(t)
	sparseDurable(t, filepath.Join(s.home, "pending", "old.json"), 10)
	err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { _ = q.Close() }()
		return q.writeContext(t.Context(), "pending/old.json", 5, func(w io.Writer) error {
			if _, e = w.Write([]byte("newer")); e != nil {
				return e
			}
			// Loss of the directory capability models failure after payload allocation:
			// rename and cleanup cannot run, so the interrupted temp must remain owed.
			return q.home.Root.Close()
		})
	})
	if err == nil {
		t.Fatal("closed directory capability allowed commit")
	}
	old, e := os.Stat(filepath.Join(s.home, "pending", "old.json"))
	if e != nil || old.Size() != 10 {
		t.Fatal("original replaced", e)
	}
	entries, e := os.ReadDir(filepath.Join(s.home, "pending"))
	if e != nil || len(entries) != 2 {
		t.Fatalf("interrupted copy not retained: %v %v", entries, e)
	}
	if e = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { _ = q.Close() }()
		usage, e := q.usageContext(t.Context())
		if !errors.Is(e, ErrDurableStorageRecovery) {
			return e
		}
		if usage.physical != 15 || usage.charged != 30 {
			t.Fatalf("restart usage: %+v", usage)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestFuturePrivateAuthorityRefusesWithoutConfig(t *testing.T) {
	for _, key := range []string{"commit", "sources", "journal_version", "phase", "preparation", "progress", "cleanup", "admission_stage"} {
		t.Run("pending-"+key, func(t *testing.T) {
			s := newTestStore(t)
			if e := os.Remove(filepath.Join(s.home, "config.json")); e != nil {
				t.Fatal(e)
			}
			raw := []byte(`{"` + key + `":null}`)
			path := s.pendingPath("foreign")
			if e := os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			_, found, e := s.ForCollectorPass().LoadPending("foreign")
			if !found || !errors.Is(e, ErrDurableStorageRecovery) {
				t.Fatalf("foreign work: %v %v", found, e)
			}
			after, e := os.ReadFile(path)
			if e != nil || string(after) != string(raw) {
				t.Fatal("foreign work discarded", e)
			}
		})
	}
	for _, key := range []string{"commit", "sources", "predecessor_unknown"} {
		t.Run("published-"+key, func(t *testing.T) {
			s := newTestStore(t)
			raw := []byte(`{"` + key + `":null}`)
			path := s.publishedPath("foreign")
			if e := os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			_, e := s.ForCollectorPass().LoadPublishedState("foreign")
			if !errors.Is(e, ErrDurableStorageRecovery) {
				t.Fatalf("foreign work: %v", e)
			}
			bundle, at, status, found, e := s.LoadPublished("foreign")
			if !found || !errors.Is(e, ErrDurableStorageRecovery) || status != "" || !at.IsZero() || bundle.ArchiveSessionID != "" {
				t.Fatalf("wrapper granted authority: %v %s %v", found, status, e)
			}
			bundle, at, found, e = s.LoadLastPublished("foreign")
			if !found || !errors.Is(e, ErrDurableStorageRecovery) || !at.IsZero() || bundle.ArchiveSessionID != "" {
				t.Fatalf("last wrapper granted authority: %v %v", found, e)
			}
			after, e := os.ReadFile(path)
			if e != nil || string(after) != string(raw) {
				t.Fatal("foreign work discarded", e)
			}
		})
	}
}

func TestDurableInspectionCacheInvalidatesAnonymousAndConfigChanges(t *testing.T) {
	s := newTestStore(t)
	for i := range 20 {
		if _, found, err := s.LoadPending(fmt.Sprintf("absent-%d", i)); err != nil || found {
			t.Fatalf("settled absence: %v %v", found, err)
		}
	}
	if s.durableInspection.scans != 1 || s.durableInspection.configLoads != 1 {
		t.Fatalf("repeated full inspection: scans=%d configs=%d", s.durableInspection.scans, s.durableInspection.configLoads)
	}
	for range 20 {
		if found, err := s.HasPending("absent"); found || err != nil {
			t.Fatal(found, err)
		}
	}
	if s.durableInspection.scans != 1 {
		t.Fatal("HasPending enumerated global roots")
	}
	temp := filepath.Join(s.home, "pending", ".pending-123")
	if err := os.WriteFile(temp, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending("absent"); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("anonymous insertion lost: %v %v", found, err)
	}
	if s.durableInspection.scans != 2 {
		t.Fatal("root insertion did not invalidate")
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(temp, past, past); err != nil {
		t.Fatal(err)
	}
	// Reset the directory observation because metadata changes inside a file
	// do not change the directory stamp; this negative result stays owed until
	// a legitimate owner changes the root, never turning absence from age alone.
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temp, past, past); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending("absent"); err != nil || found {
		t.Fatalf("known legacy cleanup class: %v %v", found, err)
	}
	if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error { return g.CheckHome(s.home) }); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending("absent"); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("upgrade reused legacy classification: %v %v", found, err)
	}
	if s.durableInspection.configLoads != 2 {
		t.Fatal("config upgrade did not reload")
	}
	t.Logf("20 direct reads +20 HasPending: initial root enumerations=1, config decodes=1; insertion and upgrade invalidated observations")
}

func TestDurableInspectionCancellationDoesNotBecomeAbsence(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reader, closeRead := s.WithReadBudget(ctx, agentapi.NewNativeReadBudget(1<<20))
	defer closeRead()
	if _, found, err := reader.LoadPending("absent"); !found || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became absence: %v %v", found, err)
	}
	if s.durableInspection.valid || s.durableInspection.configValid {
		t.Fatal("canceled inspection cached absence")
	}
}

func TestDurableInspectionBoundRefusesPartialRootAndUnsafeEntries(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"a", "b"} {
		if err := os.WriteFile(s.pendingPath(id), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	held, err := local.OpenRootedHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	remaining := 1
	_, _, err = s.inspectPendingRoot(held, "pending", &remaining, false)
	if !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("partial inspection accepted: %v", err)
	}
	outside := t.TempDir()
	if err = os.WriteFile(filepath.Join(outside, "original"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.home, generationRecoveryDir)
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckDurableReadRoots(); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("unsafe generation root accepted: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(outside, "original"))
	if err != nil || string(raw) != "outside" {
		t.Fatal("outside modified", err)
	}
}

func TestDurableFutureLabelContextUsesClosedPublishedDecoder(t *testing.T) {
	s := newTestStore(t)
	for _, key := range []string{"commit", "sources", "predecessor_unknown"} {
		raw := []byte(fmt.Sprintf(`{"summary":{},%q:null}`, key))
		if err := os.WriteFile(s.publishedPath("foreign"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		p, _, err := s.LoadLabelPublication("foreign", 8<<20)
		if p != nil || !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatalf("label context accepted %s: %v", key, err)
		}
		after, err := os.ReadFile(s.publishedPath("foreign"))
		if err != nil || string(after) != string(raw) {
			t.Fatal("foreign label state changed", err)
		}
	}
}

func TestDurablePublishedPrefixRequiresCurrentConfigAndRefusesFutureHeader(t *testing.T) {
	for _, mode := range []durableFixtureMode{durableFixtureMissing, durableFixtureCorrupt, durableFixtureFuture, durableFixtureKnown, durableFixtureHeader, durableFixtureHeaderAfterSummary} {
		t.Run(string(mode), func(t *testing.T) {
			s := newTestStore(t)
			raw := `{"summary":{"published":true,"harness":"codex"},"bundle":{}}`
			switch mode {
			case durableFixtureKnown: // Supported positive control keeps its complete configuration.
			case durableFixtureMissing:
				if err := os.Remove(filepath.Join(s.home, "config.json")); err != nil {
					t.Fatal(err)
				}
			case durableFixtureCorrupt:
				if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case durableFixtureFuture, durableFixtureHeaderAfterSummary:
				if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(`{"schema_version":{"version":8,"writer":"publication-composition-v8"},"publication_composition_protection":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case durableFixtureHeader:
				raw = `{"publication_version":2,"summary":{"published":true},"commit":null}`
			}
			if mode == durableFixtureHeaderAfterSummary {
				raw = `{"summary":{"published":true},"publication_version":2}`
			}
			if err := os.WriteFile(s.publishedPath("foreign"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			summary, found, err := s.LoadPublishedSummary("foreign")
			_, _, labelErr := s.LabelRevision("foreign")
			if mode == durableFixtureKnown {
				if err != nil || !found || !summary.Published || labelErr != nil {
					t.Fatal(summary, found, err, labelErr)
				}
				for range 10 {
					if _, _, err := s.LoadPublishedSummary("foreign"); err != nil {
						t.Fatal(err)
					}
				}
				if s.durableInspection.configLoads != 1 {
					t.Fatal("prefix repeatedly decoded current config")
				}
			} else if !found || !errors.Is(err, ErrDurableStorageRecovery) || summary.Published || !errors.Is(labelErr, ErrDurableStorageRecovery) {
				t.Fatalf("prefix gained authority: %+v %v %v label=%v", summary, found, err, labelErr)
			}
			if _, absent, err := s.LoadPublishedSummary("absent"); absent || err != nil {
				t.Fatalf("proven absence changed: %v %v", absent, err)
			}
			after, err := os.ReadFile(s.publishedPath("foreign"))
			if err != nil || string(after) != raw {
				t.Fatal("refusal changed published file", err)
			}
		})
	}
}

func TestDurableQuotaRefusesConfigReplacementBeforeStageAllocation(t *testing.T) {
	s := newTestStore(t)
	s.onLockWait = func(name string) {
		if name != "temporary-quota" {
			return
		}
		raw, err := os.ReadFile(filepath.Join(s.home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(s.home, "replacement.json")
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p, filepath.Join(s.home, "config.json")); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("original")
	if _, err := s.StagePendingSource("session", durableRef(data), data); err == nil {
		t.Fatal("replaced config granted quota write")
	}
	if _, err := os.Stat(filepath.Join(s.home, "sessions", "session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("allocated stage despite replaced config", err)
	}
}

func TestDurableCorruptPendingWithoutSourcesIsRetainedAndOwed(t *testing.T) {
	s := newTestStore(t)
	if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error { return g.CheckHome(s.home) }); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"bundle":`)
	path := s.pendingPath("corrupt")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, found, err := s.ForCollectorPass().LoadPending("corrupt"); !found || !errors.Is(err, ErrDurableStorageRecovery) || errors.Is(err, ErrQuarantined) {
			t.Fatalf("pending: found=%v err=%v", found, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatalf("protected bytes changed: %q %v", after, err)
	}
	if len(s.QuarantinedFiles()) != 0 {
		t.Fatal("protected corruption quarantined")
	}
	if pending, err := s.HasPending("corrupt"); !pending || err != nil {
		t.Fatalf("pending visibility: %v %v", pending, err)
	}
	if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { _ = q.Close() }()
		usage, err := q.usageContext(t.Context())
		if err != nil {
			return err
		}
		if usage.physical != int64(len(raw)) || usage.charged != 2*int64(len(raw)) {
			t.Fatalf("corruption uncharged: %+v", usage)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDurableLegacyQuarantineIsInactiveOnlyBeforeFloorUpgrade(t *testing.T) {
	s := newTestStore(t)
	name := "session.json.20261007T200000.000000000Z.corrupt"
	path := filepath.Join(s.home, "pending", name)
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending("session"); found || err != nil {
		t.Fatalf("legacy quarantine: %v %v", found, err)
	}
	for _, invalid := range []string{"session.json.20261307T200000.000000000Z.corrupt", "session.json.20261007T200000.000000000Z.corrupt.more", "session.json.20261007T200000Z.corrupt"} {
		if acknowledgedLegacyQuarantine(invalid) {
			t.Fatalf("invalid producer name acknowledged: %s", invalid)
		}
	}
	if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { _ = q.Close() }()
		usage, err := q.usageContext(t.Context())
		if !errors.Is(err, ErrDurableStorageRecovery) || usage.physical != 8 || usage.charged != 16 {
			t.Fatalf("quarantine quota: %+v %v", usage, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending("session"); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("floor upgrade hid quarantine: %v %v", found, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "original" {
		t.Fatal("quarantine bytes lost", err)
	}
}

func TestDurableGlobalGenerationReceiptsAreRevalidatedAndBounded(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, generationRecoveryDir), 0700); err != nil {
		t.Fatal(err)
	}
	path := s.generationRecoveryPath("previous")
	complete := `{"version":1,"key":{"agent":"codex","NativeID":"native"},"previous":"previous","next":"next","complete":true}`
	if err := os.WriteFile(path, []byte(complete), 0600); err != nil {
		t.Fatal(err)
	}
	obligations, err := s.DurableStorageObligations()
	if err != nil || len(obligations) != 0 {
		t.Fatalf("valid complete receipt: %+v %v", obligations, err)
	}
	for _, key := range []string{"registration", "pending", "request"} {
		nullReceipt := strings.TrimSuffix(complete, "}") + `,"` + key + `":null}`
		if err := os.WriteFile(path, []byte(nullReceipt), 0600); err != nil {
			t.Fatal(err)
		}
		obligations, err = s.DurableStorageObligations()
		if err != nil || len(obligations) != 0 {
			t.Fatalf("explicit null %s: %+v %v", key, obligations, err)
		}
		nonnullReceipt := strings.TrimSuffix(complete, "}") + `,"` + key + `":{}}`
		if err := os.WriteFile(path, []byte(nonnullReceipt), 0600); err != nil {
			t.Fatal(err)
		}
		obligations, err = s.DurableStorageObligations()
		if err != nil || len(obligations) != 1 {
			t.Fatalf("nonnull %s not owed: %+v %v", key, obligations, err)
		}
	}
	scans := s.durableInspection.scans
	for _, raw := range []string{strings.Replace(complete, `"complete":true`, `"complete":false`, 1), strings.Replace(complete, `"complete":true`, `"complete":true,"future":null`, 1), `{`, strings.Repeat(" ", int(durableControlBytes)+1)} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		obligations, err = s.DurableStorageObligations()
		if err != nil || len(obligations) != 1 || obligations[0].SessionID != "previous" {
			t.Fatalf("unknown receipt omitted: %+v %v", obligations, err)
		}
	}
	if s.durableInspection.scans != scans {
		t.Fatal("receipt change introduced another root enumeration")
	}
	if err := os.WriteFile(path, []byte(complete), 0600); err != nil {
		t.Fatal(err)
	}
	obligations, err = s.DurableStorageObligations()
	if err != nil || len(obligations) != 0 {
		t.Fatalf("completed replacement not observed: %+v %v", obligations, err)
	}
}

func TestDurableGenerationCensusDoesNotMaterializeUnclearedPayload(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, generationRecoveryDir), 0700); err != nil {
		t.Fatal(err)
	}
	raw := `{"version":1,"key":{"Agent":"codex","NativeID":"native"},"previous":"previous","next":"next","complete":true,"pending":{"history":{"sources":[` + strings.Repeat(`{},`, 18000) + `{}]}}}`
	if len(raw) > int(durableControlBytes) {
		t.Fatal("fixture exceeds receipt bound")
	}
	path := s.generationRecoveryPath("previous")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Close(); err != nil {
			t.Error(err)
		}
	}()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	complete, err := s.completedGenerationReceipt(home, "previous")
	runtime.ReadMemStats(&after)
	if err != nil || complete {
		t.Fatalf("uncleared payload accepted: %v %v", complete, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1024*1024 {
		t.Fatalf("bounded census amplified nested payload: %d bytes", allocated)
	} else {
		t.Logf("%d input bytes; %d allocated bytes", len(raw), allocated)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != raw {
		t.Fatal("census changed owed bytes")
	}
}
