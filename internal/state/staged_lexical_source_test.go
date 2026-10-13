package state

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestStagedLexicalSourceFactRefusesMutationAndKeepsFullDefault(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	_, _, stage, err := s.stagedContext(t.Context(), p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := s.validateStagedSourceForFactory(t.Context(), stage, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.validateStagedSourceForFactory(t.Context(), stage, p, fact); err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.SourceBytes = append([]byte(nil), p.SourceBytes...)
	changed.SourceBytes[0] ^= 1
	changed.SourceSHA256 = readySHA(changed.SourceBytes)
	if _, err = s.validateStagedSourceForFactory(t.Context(), stage, changed, fact); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("fact accepted changed source: %v", err)
	}
	if err = s.validateExactStagedSource(t.Context(), stage, changed); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("full default accepted noncanonical source: %v", err)
	}
	changedBundle := p
	changedBundle.Bundle.Capture.AdapterVersion += "-changed"
	if _, err = s.validateStagedSourceForFactory(t.Context(), stage, changedBundle, fact); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("fact accepted changed bundle: %v", err)
	}
	altered := *fact
	altered.stageSHA = "other"
	if _, err = s.validateStagedSourceForFactory(t.Context(), stage, p, &altered); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("fact accepted changed stage: %v", err)
	}
	altered = *fact
	altered.policy.Skill = "other"
	if _, err = s.validateStagedSourceForFactory(t.Context(), stage, p, &altered); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("fact accepted changed policy: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.validateStagedSourceForFactory(ctx, stage, p, fact); !errors.Is(err, context.Canceled) {
		t.Fatalf("fact lost cancellation: %v", err)
	}
}

func TestStagedLexicalSourceFactStillReservesSecondScratch(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	_, _, stage, err := s.stagedContext(t.Context(), p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := s.validateStagedSourceForFactory(t.Context(), stage, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(2 << 20)
	scoped, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	if _, err = scoped.validateStagedSourceForFactory(t.Context(), stage, p, fact); err != nil {
		t.Fatal(err)
	}
	pressure := budget.Reservation()
	if !pressure.Reserve(1536 << 10) {
		t.Fatal("pressure reservation")
	}
	defer pressure.Release(1536 << 10)
	before := budget.Available()
	if _, err = scoped.validateStagedSourceForFactory(t.Context(), stage, p, fact); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("fact skipped second scratch refusal: %v", err)
	}
	if budget.Available() != before {
		t.Fatal("refusal leaked charge")
	}
}

func TestStagedLexicalFactoryRechecksCoveredRequest(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	_, _, stage, err := s.stagedContext(t.Context(), p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := s.validateStagedSourceForFactory(t.Context(), stage, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finishStagedSelectionFactory(t.Context(), p, fact); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveRequest(p.Bundle.ArchiveSessionID, "new-owner", p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	before := stagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID)
	if err = s.finishStagedSelectionFactory(t.Context(), p, fact); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("factory accepted newer request: %v", err)
	}
	assertStagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID, before)
}
