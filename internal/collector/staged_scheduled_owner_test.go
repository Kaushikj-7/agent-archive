package collector

import (
	"bytes"
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
func TestStagedRunScheduledReleaseRespectsCapturedPassBudget(t *testing.T) {
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
	defer guard.Release()
	opts.CollectorGuard = guard
	budget := agentapi.NewNativeReadBudget(128 << 20)
	pressure := budget.Reservation()
	if !pressure.Reserve((128 << 20) - 512) {
		t.Fatal("captured independent pressure")
	}
	defer pressure.Release((128 << 20) - 512)
	opts.CodexRollouts = recoveryBudgetLookup{budget: budget}
	pendingPath := filepath.Join(store.Home(), "pending", reg.ArchiveSessionID+".json")
	stagePath := filepath.Join(store.Home(), "sessions", reg.ArchiveSessionID, "import-stage", "stage.json")
	beforePending, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStage, err := os.ReadFile(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Run(t.Context(), store, remote, opts); err != nil {
		t.Fatal(err)
	}
	current, found, err := store.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || current.Token != request.Token {
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
