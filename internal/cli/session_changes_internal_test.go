package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/collector"
)

type changeSinkFunc func(context.Context, SessionChangeBatch) error

func (f changeSinkFunc) Hint(ctx context.Context, batch SessionChangeBatch) error {
	return f(ctx, batch)
}

func TestSessionChangeFanoutCapsDeduplicatesAndIsolatesConsumerSlices(t *testing.T) {
	t.Parallel()
	candidates := []collector.PublicationCandidate{{Harness: "codex", ArchiveSessionID: "one"}, {Harness: "codex", ArchiveSessionID: "one"}, {}}
	for i := 0; i < 300; i++ {
		candidates = append(candidates, collector.PublicationCandidate{Harness: "claude", ArchiveSessionID: fmt.Sprintf("session-%d", i)})
	}
	diagnosed := 0
	called := 0
	first := changeSinkFunc(func(_ context.Context, batch SessionChangeBatch) error {
		called++
		if len(batch.Sessions) != sessionChangeLimit || batch.DestinationID != "original" {
			t.Fatal(batch)
		}
		batch.Sessions[0].ArchiveSessionID = "mutated"
		return errors.New("consumer failure")
	})
	second := changeSinkFunc(func(_ context.Context, batch SessionChangeBatch) error {
		called++
		if len(batch.Sessions) != sessionChangeLimit || batch.Sessions[0].ArchiveSessionID != "one" {
			t.Fatal("consumer mutated another slice", batch)
		}
		batch.Sessions[0].ArchiveSessionID = "second"
		return nil
	})
	dispatchSessionChanges(t.Context(), "original", candidates, []sessionChangeConsumer{{sink: first, diagnostic: func(error) { diagnosed++ }}, {sink: second}})
	if called != 2 || diagnosed != 1 || candidates[0].ArchiveSessionID != "one" {
		t.Fatal(called, diagnosed, candidates[0])
	}
}

func TestSessionChangeSlowConsumerLeavesSecondReservedChance(t *testing.T) {
	t.Parallel()
	secondCalled := false
	first := changeSinkFunc(func(ctx context.Context, _ SessionChangeBatch) error { <-ctx.Done(); return ctx.Err() })
	second := changeSinkFunc(func(ctx context.Context, _ SessionChangeBatch) error {
		secondCalled = true
		if err := ctx.Err(); err != nil {
			t.Fatal("first consumer consumed second reservation", err)
		}
		return nil
	})
	dispatchSessionChanges(t.Context(), "original", []collector.PublicationCandidate{{Harness: "codex", ArchiveSessionID: "one"}}, []sessionChangeConsumer{{sink: first}, {sink: second}})
	if !secondCalled {
		t.Fatal("second consumer starved")
	}
}

func TestSessionChangeEmptyAndCancelledBatchesDoNotInvokeConsumers(t *testing.T) {
	t.Parallel()
	consumer := sessionChangeConsumer{sink: changeSinkFunc(func(context.Context, SessionChangeBatch) error { t.Fatal("invoked consumer"); return nil })}
	dispatchSessionChanges(t.Context(), "", []collector.PublicationCandidate{{Harness: "codex", ArchiveSessionID: "one"}}, []sessionChangeConsumer{consumer})
	dispatchSessionChanges(t.Context(), "original", nil, []sessionChangeConsumer{consumer})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dispatchSessionChanges(ctx, "original", []collector.PublicationCandidate{{Harness: "codex", ArchiveSessionID: "one"}}, []sessionChangeConsumer{consumer})
}
