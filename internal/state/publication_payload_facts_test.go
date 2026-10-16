package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func preparationSHA(a PreparationAuthority) string { return preparationSHAWithFacts(a, nil) }

func TestPublicationPayloadInvocationPreservesDigestsAndChecksDistinctBytes(t *testing.T) {
	p := publicationFixture(t, publicationThread, time.Now())
	p.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(32 << 10)
	facts, end, err := newPayloadDigestFacts(t.Context(), budget, len(p.Sources)+len(p.Preparation.Inputs))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := payloadSetSHAWithFacts(p.Sources, facts), payloadSetSHA(p.Sources); got != want {
		t.Fatal("payload digest changed", got, want)
	}
	if got, want := preparationSHAWithFacts(*p.Preparation, facts), preparationSHA(*p.Preparation); got != want {
		t.Fatal("preparation digest changed", got, want)
	}
	if err = p.validatePublicationEnvelopeWithFacts(facts); err != nil {
		t.Fatal(err)
	}
	copySource := p.Sources[0]
	copySource.Payload.Inline = bytes.Clone(copySource.Payload.Inline)
	if got := payloadInlineSHA(copySource, facts); got != copySource.Reference.SHA256 {
		t.Fatal("distinct exact copy differs", got)
	}
	copySource.Payload.Inline[0] ^= 1
	var metadata archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	if err = validatePublicationPayloadWithFacts(copySource, metadata, "destination", "admission", facts); err == nil {
		t.Fatal("unequal copied bytes accepted")
	}
	end()
	end()
	if facts.facts != nil {
		t.Fatal("borrowed bodies retained")
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("facts loan leaked", used)
	}
	// A new invocation must hash even the SAME externally mutated slice afresh.
	mutated := p.Sources[0]
	mutated.Payload.Inline[0] ^= 1
	next, release, err := newPayloadDigestFacts(t.Context(), budget, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err = validatePublicationPayloadWithFacts(mutated, metadata, "destination", "admission", next); err == nil {
		t.Fatal("cross-call same-slice mutation accepted")
	}
}

func TestPublicationPayloadInvocationPressureAndCancellation(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(1024)
	if _, _, err := newPayloadDigestFacts(t.Context(), budget, 1); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("failed reserve leaked", used)
	}
	budget = agentapi.NewNativeReadBudget(32 << 10)
	ctx, cancel := context.WithCancel(t.Context())
	facts, end, err := newPayloadDigestFacts(ctx, budget, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := PublicationSource{Payload: PublicationPayload{Kind: PublicationInline, Inline: bytes.Repeat([]byte("x"), 1<<20)}}
	if got := payloadInlineSHA(source, facts); got != publicationSHA256(source.Payload.Inline) {
		t.Fatal("actual hash differs")
	}
	cancel()
	if got := payloadInlineSHA(source, facts); got != "" || !errors.Is(facts.result(nil), context.Canceled) {
		t.Fatal("canceled reuse", got, facts.err)
	}
	end()
	end()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("canceled facts leaked", used)
	}
}

func TestPublicationBudgetedFactoryMatchesDefaultAndOwnsScratch(t *testing.T) {
	fixture := func() PendingPublication {
		p := publicationFixture(t, publicationThread, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
		p.History = &PendingHistory{Version: 1}
		return p
	}
	prior := PublicationPredecessor{State: PredecessorAbsent}
	original := fixture()
	want, err := PreparePublicationV2(original, prior, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(32 << 10)
	got, err := PreparePublicationV2Budgeted(t.Context(), budget, fixture(), prior, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	a, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("budgeted factory changed the selecting wire")
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("factory scratch retained", used)
	}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		capacity := int64(1024)
		if canceled {
			capacity = 32 << 10
			cancel()
		}
		limited := agentapi.NewNativeReadBudget(capacity)
		refused, err := PreparePublicationV2Budgeted(ctx, limited, fixture(), prior, "destination", "admission", "policy", PublicationCapture)
		cancel()
		expected := agentapi.ErrReadBudget
		if canceled {
			expected = context.Canceled
		}
		if !errors.Is(err, expected) || refused.Commit != nil {
			t.Fatal("factory refusal", canceled, err, refused.Commit)
		}
		if used, _ := limited.Charged(); used != 0 {
			t.Fatal("refusal scratch leaked", used)
		}
	}
}

func TestPublicationPayloadFactsKeepCapturedReservationOwner(t *testing.T) {
	t.Parallel()
	budget := agentapi.NewNativeReadBudget(32 << 10)
	endFirst := budget.BeginScope(4096)
	owner := budget.Reservation()
	endFirst()
	endSecond := budget.BeginScope(4096)
	defer endSecond()
	second := budget.Reservation()
	if !second.Reserve(4096) {
		t.Fatal("second scope setup failed")
	}
	defer second.Release(4096)
	facts, release, err := newPayloadDigestFacts(t.Context(), budget, 1, owner)
	if err != nil {
		t.Fatalf("facts charged to full later scope: %v", err)
	}
	source := PublicationSource{Payload: PublicationPayload{Kind: PublicationInline, Inline: []byte("synthetic")}}
	if got := payloadInlineSHA(source, facts); got != publicationSHA256(source.Payload.Inline) {
		t.Fatal("wrong digest", got)
	}
	release()
	release()
	if second.Available() != 0 {
		t.Fatal("delayed facts release credited later scope", second.Available())
	}
}
