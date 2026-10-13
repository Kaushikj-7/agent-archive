package backfill

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStagedBatchReconcileRetainsResultOwnerAcrossCopy(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("actual stage producer: %v %v", result, err)
	}
	budget := agentapi.NewNativeReadBudget(2 << 20)
	store, closeStore := r.Store.WithReadBudget(t.Context(), budget)
	defer closeStore()
	batch := Batch{ID: r.Batch, StartedAt: r.AdmittedAt, DestinationID: r.DestinationID}
	defer func() {
		if closer, ok := any(&batch).(interface{ CloseStagedResults() }); ok {
			closer.CloseStagedResults()
		}
	}()
	before := budget.Available()
	if err = batch.Reconcile(store); err != nil {
		t.Fatal(err)
	}
	if len(batch.StagedSessions) != 1 || batch.StagedSessions[0] != result.Sessions[0] {
		t.Fatalf("retained staged IDs: %v", batch.StagedSessions)
	}
	retained := int64(len(batch.StagedSessions[0]))
	if before-budget.Available() != retained {
		t.Fatalf("result escaped page owner: got %d want %d", before-budget.Available(), retained)
	}
	alias := batch
	if err = SaveBatch(r.Home, alias); err != nil {
		t.Fatal(err)
	}
	if err = batch.Reconcile(store); err != nil {
		t.Fatal(err)
	}
	if before-budget.Available() != retained {
		t.Fatal("repeated reconciliation lost or duplicated receiving ownership")
	}
	// All alias uses end before the central result release. This cleanup-only
	// interface allows exact1010 to compile the identical regression source.
	if closer, ok := any(&batch).(interface{ CloseStagedResults() }); ok {
		closer.CloseStagedResults()
	}
	if budget.Available() != before {
		t.Fatal("result release did not return its captured charge")
	}
}

func TestStagedUndoRefusesBeforeChangingAuthority(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("stage producer: %v %v", result, err)
	}
	batch := Batch{ID: r.Batch, StartedAt: r.AdmittedAt, DestinationID: r.DestinationID}
	paths := []string{filepath.Join(r.Home, "config.json"), filepath.Join(r.Home, "imports", r.Batch+".json"), filepath.Join(r.Home, "registrations", result.Sessions[0]+".json"), filepath.Join(r.Home, "sessions", result.Sessions[0], "import-stage", "stage.json")}
	before := make(map[string][]byte)
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	env := Environment{Sources: testSources, Home: r.Home, Now: func() time.Time { return r.AdmittedAt }}
	if _, err = PlanUndo(env, r.Store, cfg, []Batch{batch}, batch, ""); !errors.Is(err, state.ErrReadySelectionFrozen) {
		t.Fatalf("unsupported typed undo: %v", err)
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before[path], after) {
			t.Fatalf("typed undo changed authority: %s %v", path, err)
		}
	}
}
