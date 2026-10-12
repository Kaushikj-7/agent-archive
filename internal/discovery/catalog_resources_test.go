package discovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestCatalogReadAndWritePressurePreserveCheckpointAndRelease(t *testing.T) {
	store, _, _, root := fixture(t)
	c := catalog{Version: catalogVersion, Roots: []string{root}, Queue: []directory{{Root: root, Path: "sessions", Offset: 7}}, Coverage: newCoverage([]string{root})}
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	if err := local.WriteCompact(path, c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	l := &CodexRolloutLookup{readBudget: agentapi.NewNativeReadBudget(2*int64(len(raw)) - 1)}
	var restored catalog
	if err := l.readCatalog(t.Context(), path, &restored); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("catalog decoded without both input owners: %v", err)
	}
	if used, _ := l.readBudget.Charged(); used != 0 {
		t.Fatalf("refusal leaked %d", used)
	}
	l.readBudget = agentapi.NewNativeReadBudget(1 << 20)
	reservation := l.readBudget.Reservation()
	if err := l.readCatalog(t.Context(), path, &restored); err != nil {
		t.Fatal(err)
	}
	if used, _ := l.readBudget.Charged(); used != int64(len(raw)) || len(restored.Queue) != 1 || restored.Queue[0].Offset != 7 {
		t.Fatalf("decoded checkpoint ownership lost: charge=%d queue=%v", used, restored.Queue)
	}
	pressure := l.readBudget.Available()
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if err := l.writeCatalog(t.Context(), path, restored); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("catalog encoded under exhausted shared budget: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("refusal changed durable checkpoint", err)
	}
	reservation.Release(pressure)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := l.readBudget.Charged(); used != 0 {
		t.Fatalf("catalog owner leaked %d", used)
	}
}

func TestRequestedCoverageCannotAllocateWithExhaustedSharedLedger(t *testing.T) {
	store, _, _, root := fixture(t)
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	reservation := l.readBudget.Reservation()
	defer func() { _ = l.Close() }()
	if !l.coverage.request("wanted") || l.catalogCharge == 0 {
		t.Fatal("requested facts were not charged")
	}
	pressure := l.readBudget.Available()
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if l.coverage.request("fresh") {
		t.Fatal("new request allocated under exhausted ledger")
	}
	l.Observe(SourceDescriptor{Root: root, Locator: filepath.Join(root, "sessions", "wanted.jsonl")}, Fingerprint{}, &codexmeta.CodexIdentity{ThreadID: "wanted", RolloutID: "physical"})
	if len(l.coverage.Requests["wanted"].Candidates) != 0 || len(l.observations) != 0 {
		t.Fatal("native facts accumulated under exhausted ledger")
	}
	reservation.Release(pressure)
	if !l.coverage.request("fresh") {
		t.Fatal("independent request cannot progress after pressure ends")
	}
}
