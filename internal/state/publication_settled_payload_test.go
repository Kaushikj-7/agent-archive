package state

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// settledPayloadShape counts the inline payloads of a published state file
// that still carry bytes and those whose bytes were released.
func settledPayloadShape(t *testing.T, raw []byte) (inline, released int) {
	t.Helper()
	var wire struct {
		Payloads    []PublicationSource `json:"payloads"`
		Preparation *struct {
			Inputs []PreparationInput `json:"inputs"`
		} `json:"preparation"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	payloads := make([]PublicationPayload, 0, len(wire.Payloads))
	for _, s := range wire.Payloads {
		payloads = append(payloads, s.Payload)
	}
	if wire.Preparation != nil {
		for _, input := range wire.Preparation.Inputs {
			payloads = append(payloads, input.Payload)
		}
	}
	for _, p := range payloads {
		if len(p.Inline) > 0 {
			inline++
		}
		if p.Released {
			released++
		}
	}
	return inline, released
}

func settledPayloadFixture(t *testing.T) (*Store, PendingPublication) {
	t.Helper()
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LoadPending(p.Bundle.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if inline := len(got.Sources[0].Payload.Inline); inline == 0 || got.Preparation == nil || len(got.Preparation.Inputs[0].Payload.Inline) == 0 {
		t.Fatal("fixture journal does not carry inline replay bytes")
	}
	return s, got
}

func TestSettledPublicationKeepsPayloadDigestsNotBytes(t *testing.T) {
	t.Parallel()
	s, pending := settledPayloadFixture(t)
	id := pending.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(pending, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(pending.Sources[0].Payload.Inline) == 0 || len(pending.Preparation.Inputs[0].Payload.Inline) == 0 {
		t.Fatal("settling released the pending journal's replay bytes")
	}
	raw, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if inline, released := settledPayloadShape(t, raw); inline != 0 || released != 2 {
		t.Fatalf("settled state keeps %d inline payloads and released %d; want 0 and 2", inline, released)
	}
	reloaded, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal("released settled state does not load", err)
	}
	if refs, err := reloaded.CommittedSources(); err != nil || len(refs) != 1 || refs[0] != pending.Sources[0].Reference {
		t.Fatal("released settled state lost its committed source", refs, err)
	}
	if reloaded.state.Commit.PayloadSetSHA256 != pending.Commit.PayloadSetSHA256 || reloaded.state.Commit.PreparationSHA256 != pending.Commit.PreparationSHA256 {
		t.Fatal("releasing inline bytes changed the commit digests")
	}
}

func TestSettledPublicationWithInlineBytesLoadsAndReleasesOnWrite(t *testing.T) {
	t.Parallel()
	s, pending := settledPayloadFixture(t)
	id := pending.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(pending, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Rewrite the file as earlier builds did: the same state with the
	// journal's inline bytes in both payloads and preparation.
	old := published.state
	old.Payloads = pending.Sources
	old.Preparation = pending.Preparation
	type wireState publishedState
	raw, err := json.Marshal(wireState(old))
	if err != nil {
		t.Fatal(err)
	}
	if inline, _ := settledPayloadShape(t, raw); inline != 2 {
		t.Fatal("earlier settled shape not reproduced", inline)
	}
	if err = os.WriteFile(s.publishedPath(id), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal("settled state with inline bytes no longer loads", err)
	}
	if err = loaded.Save(pending.Bundle, time.Now(), CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if inline, released := settledPayloadShape(t, raw); inline != 0 || released != 2 {
		t.Fatalf("rewritten state keeps %d inline payloads and released %d; want 0 and 2", inline, released)
	}
	if _, err = s.LoadPublishedState(id); err != nil {
		t.Fatal(err)
	}
}

func TestPendingPublicationRefusesReleasedPayload(t *testing.T) {
	t.Parallel()
	_, pending := settledPayloadFixture(t)
	for name, release := range map[string]func(*PendingPublication){
		"source": func(p *PendingPublication) {
			p.Sources = append([]PublicationSource(nil), p.Sources...)
			p.Sources[0].Payload = PublicationPayload{Kind: PublicationInline, Released: true}
			p.SourceBytes = nil
		},
		"preparation": func(p *PendingPublication) {
			a := *p.Preparation
			a.Inputs = append([]PreparationInput(nil), a.Inputs...)
			a.Inputs[0].Payload = PublicationPayload{Kind: PublicationInline, Released: true}
			p.Preparation = &a
		},
	} {
		p := pending
		release(&p)
		if err := p.ValidatePublication(); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Errorf("%s: pending journal accepted released replay bytes: %v", name, err)
		}
		raw, err := json.Marshal(publicationWire(p))
		if err != nil {
			t.Fatal(err)
		}
		var decoded PendingPublication
		if err := json.Unmarshal(raw, &decoded); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Errorf("%s: pending decoder accepted released replay bytes: %v", name, err)
		}
	}
}
