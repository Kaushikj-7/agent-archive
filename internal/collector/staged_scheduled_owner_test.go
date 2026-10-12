package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Already-committed exact staged publication is the same valid setup in both
// arms. Actual Run must not release it through an independent scheduled ledger
// after ordinary scans refuse the captured pass budget.
func stagedCommittedOwnerFixture(t *testing.T) (*state.Store, state.PendingPublication, Options, *storagetest.MemoryStore) {
	t.Helper()
	store := newTestStore(t)
	cfg, _, err := config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	if err = config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, store.Home(), "parent.jsonl", `{"type":"assistant","sessionId":"parent-native","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n")
	reg := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("confirmed-single"), DestinationID: cfg.DestinationID()}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return at }, AcceptSession: cfg.AcceptSession, SkillEvidence: cfg.EffectiveSkillEvidence(), SkipSessionIndexRecovery: true}
	bundle, preserved, _, err := ReadImportSourceSelection(t.Context(), reg, at, opts)
	if err != nil || len(preserved) != 0 {
		t.Fatalf("retained selection: %v", err)
	}
	reg.StagedImportSHA256, err = store.SaveImportStage(t.Context(), reg, bundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveRequest(reg.ArchiveSessionID, "backfill", at); err != nil {
		t.Fatal(err)
	}
	request, _, err := store.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(t.Context(), nil, "synthetic", bundle, reg, at, opts, func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	p := state.PendingPublication{Bundle: bundle, SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata, SkillEvidence: string(cfg.EffectiveSkillEvidence()), RequestToken: request.Token, ReadyAt: at, Attempted: true}
	p, err = store.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, p, state.ReadyPredecessorAbsent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, at); err != nil {
		t.Fatal(err)
	}
	confirmation := struct {
		ID            string `json:"id"`
		DestinationID string `json:"destination_id"`
	}{reg.ImportBatch.Recorded(), cfg.DestinationID()}
	if err = local.Write(filepath.Join(store.Home(), "imports", reg.ImportBatch.Recorded()+".json"), confirmation); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	if err = remote.Put(t.Context(), p.SourceKey, p.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if err = remote.Put(t.Context(), p.MetadataKey, p.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { guard.Release() })
	opts.CollectorGuard = guard
	return store, p, opts, remote
}

func TestStagedRunScheduledReleaseRespectsCapturedPassBudget(t *testing.T) {
	store, p, opts, remote := stagedCommittedOwnerFixture(t)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	pressure := budget.Reservation()
	pressureApplied := false
	originalNow := opts.Now
	// Run invokes Now after initial admission and generation recovery. Apply
	// independent pressure at that actual boundary to target scheduled release.
	opts.Now = func() time.Time {
		if !pressureApplied {
			if !pressure.Reserve((128 << 20) - 512) {
				t.Fatal("captured scheduled pressure after admission")
			}
			pressureApplied = true
		}
		return originalNow()
	}
	defer func() {
		if pressureApplied {
			pressure.Release((128 << 20) - 512)
		}
	}()
	opts.CodexRollouts = recoveryBudgetLookup{budget: budget}
	pendingPath := filepath.Join(store.Home(), "pending", p.Bundle.ArchiveSessionID+".json")
	stagePath := filepath.Join(store.Home(), "sessions", p.Bundle.ArchiveSessionID, "import-stage", "stage.json")
	beforePending, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStage, err := os.ReadFile(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), store, remote, opts)
	defer closeStagedControlResult(result)
	if !pressureApplied {
		t.Fatal("actual post-admission timing boundary not reached")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Errors["staged-releases"], agentapi.ErrReadBudget) {
		t.Fatalf("missing actual scheduled pressure cause: %v", result.Errors["staged-releases"])
	}
	current, found, err := store.LoadRequest(p.Bundle.ArchiveSessionID)
	if err != nil || !found || current.Token != p.RequestToken {
		t.Fatalf("scheduled release escaped pass owner: %v %v", found, err)
	}
	afterPending, err := os.ReadFile(pendingPath)
	if err != nil || !bytes.Equal(beforePending, afterPending) {
		t.Fatalf("pending lost under pressure: %v", err)
	}
	afterStage, err := os.ReadFile(stagePath)
	if err != nil || !bytes.Equal(beforeStage, afterStage) {
		t.Fatalf("stage lost under pressure: %v", err)
	}
}

func closeStagedControlResult(result Result) {
	if closer, ok := any(result).(interface{ CloseStagedResults() }); ok {
		closer.CloseStagedResults()
	}
}

type stagedDiagnosticFailureStore struct {
	*storagetest.MemoryStore
	source string
	cause  error
}

func (r *stagedDiagnosticFailureStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if key == r.source {
		return nil, r.cause
	}
	return r.MemoryStore.GetLimited(ctx, key, limit)
}
func TestStagedRunDiagnosticOwnerSurvivesAliasesFailuresAndRepeatedPasses(t *testing.T) {
	store, p, opts, remote := stagedCommittedOwnerFixture(t)
	cause := errors.New("controlled bounded readback failure")
	failing := &stagedDiagnosticFailureStore{MemoryStore: remote, source: p.SourceKey, cause: cause}
	budget := agentapi.NewNativeReadBudget(2 << 20)
	opts.CodexRollouts = recoveryBudgetLookup{budget: budget}
	before := budget.Available()
	for range 3 {
		result, err := Run(t.Context(), store, failing, opts)
		if err != nil {
			closeStagedControlResult(result)
			t.Fatal(err)
		}
		alias := result
		id := p.Bundle.ArchiveSessionID
		if !errors.Is(result.Errors[id], cause) || !errors.Is(alias.Errors[id], cause) {
			closeStagedControlResult(result)
			t.Fatal("raw per-owner cause lost")
		}
		if before-budget.Available() != int64(len(id)) {
			closeStagedControlResult(result)
			t.Fatalf("returned diagnostic ID not owned: %d", before-budget.Available())
		}
		current, found, err := store.LoadRequest(id)
		if err != nil || !found || current.Token != p.RequestToken {
			closeStagedControlResult(result)
			t.Fatal("failure changed selecting token", err)
		}
		// Both aliases have completed diagnostic reads before the first close.
		closeStagedControlResult(alias)
		closeStagedControlResult(result)
		if budget.Available() != before {
			t.Fatal("loop iteration retained diagnostic ownership")
		}
	}
}

func TestStagedLegacyResultCloseIsNoop(t *testing.T) {
	cause := errors.New("legacy result cause")
	result := Result{Errors: map[string]error{"legacy": cause}}
	alias := result
	closeStagedControlResult(result)
	closeStagedControlResult(alias)
	if !errors.Is(result.Errors["legacy"], cause) {
		t.Fatal("legacy close changed diagnostics")
	}
}
