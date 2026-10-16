package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

func assertPrivateWireParity(t *testing.T, value any) {
	t.Helper()
	expected, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	owner := budget.Reservation()
	var actual bytes.Buffer
	if err := jsonwire.Encode(t.Context(), &actual, value, owner); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), append(expected, '\n')) {
		t.Fatalf("%T private bytes differ", value)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("encoder loan leaked", used)
	}
}

func TestPublicationStreamingMatchesActualDurableOwners(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	s := newTestStore(t)
	legacy := publicationFixture(t, publicationThread, at)
	legacy.History = &PendingHistory{Version: 1}
	assertPrivateWireParity(t, legacy)
	if err := s.SavePending(legacy.Bundle.ArchiveSessionID, legacy); err != nil {
		t.Fatal(err)
	}
	ready, err := PreparePublicationV2(legacy, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateWireParity(t, publicationWire(ready))
	preparing := legacy
	preparing.History = &PendingHistory{Version: 1, Preparing: true, FilterVersion: archive.FilterVersion, AdapterVersion: legacy.Bundle.Capture.AdapterVersion, PreparedAt: at}
	preparing, err = PreparePublicationV2(preparing, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateWireParity(t, publicationWire(preparing))
	if err = s.SavePending(ready.Bundle.ArchiveSessionID, ready); err != nil {
		t.Fatal(err)
	}
	var evidence PublicationOriginalEvidence
	raw, err := os.ReadFile(s.home + "/" + evidencePath(ready.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	assertPrivateWireParity(t, evidence)
	published, err := s.LoadPublishedState(ready.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(ready, at); err != nil {
		t.Fatal(err)
	}
	assertPrivateWireParity(t, published.state)
	generationStore, original, generatedAt := generationFixture(t)
	interrupted := errors.New("synthetic crash")
	generationStore.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err = generationStore.BeginGenerationRecovery(original.ArchiveSessionID, generatedAt, generationBuilder(generatedAt)); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	journal, found, err := generationStore.loadGenerationRecovery(t.Context(), original.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	assertPrivateWireParity(t, journal)
	generationStore.onIndexStep = nil
	if err = generationStore.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	journal, found, err = generationStore.loadGenerationRecovery(t.Context(), original.ArchiveSessionID)
	if err != nil || !found || !journal.Complete {
		t.Fatal(found, err)
	}
	assertPrivateWireParity(t, journal)
}

func TestPublicationStreamingInheritedPressureNeverResetsLedger(t *testing.T) {
	s := newTestStore(t)
	budget := agentapi.NewNativeReadBudget(96 << 10)
	owner := budget.Reservation()
	pressure := int64(80 << 10)
	if !owner.Reserve(pressure) {
		t.Fatal("pressure")
	}
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	fixture := publicationFixture(t, publicationThread, time.Now())
	if err := scoped.SavePending(fixture.Bundle.ArchiveSessionID, fixture); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.pendingPath(fixture.Bundle.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial pending", err)
	}
	if used, _ := budget.Charged(); used != pressure {
		t.Fatal("inherited ledger reset/leak", used)
	}
	owner.Release(pressure)
	if err := scoped.SavePending(fixture.Bundle.ArchiveSessionID, fixture); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("successful writer loan leaked", used)
	}
	before, err := os.ReadFile(s.pendingPath(fixture.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = config.WithPublicationComposition(s.home, func(guard config.PublicationCompositionGuard) error {
		storage, err := guard.Storage(s.home)
		if err != nil {
			return err
		}
		return scoped.writeDurableGuard(ctx, storage, "pending/"+fixture.Bundle.ArchiveSessionID+".json", fixture)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.pendingPath(fixture.Bundle.ArchiveSessionID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancel replaced pending", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("cancel writer loan leaked", used)
	}
}
