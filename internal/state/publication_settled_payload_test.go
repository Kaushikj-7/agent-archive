package state

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
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

// publishedUsage returns the durable quota census of s's home.
func publishedUsage(t *testing.T, s *Store, id string, n int64) (u durableUsage, additional int64) {
	t.Helper()
	if err := config.WithPublicationComposition(s.home, func(c config.PublicationCompositionGuard) error {
		g, e := c.Storage(s.home)
		if e != nil {
			return e
		}
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() {
			if err := q.Close(); err != nil {
				t.Error(err)
			}
		}()
		if u, e = q.usageContext(t.Context()); e != nil {
			return e
		}
		path := filepath.Join("published", id+".json")
		info, e := q.home.Root.Lstat(path)
		if e != nil {
			return e
		}
		additional, e = q.publishedAdditional(t.Context(), path, info, n, chargedInFull)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return u, additional
}

// A settled source-set state is published cache like every other version:
// only source bytes it still holds inline count against the durable quota, so
// a history of ordinary publications cannot exhaust it and refuse the next.
func TestSettledPublicationChargesOnlyRetainedInlineBytes(t *testing.T) {
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
	// Only the published state is left to count.
	if err = s.RemovePending(id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for a large cached bundle.
	raw = append(raw, bytes.Repeat([]byte(" "), 2<<20)...)
	if err = os.WriteFile(s.publishedPath(id), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if u, additional := publishedUsage(t, s, id, 1024); u.charged != 0 || additional != int64(len(raw))+1024 {
		t.Fatalf("released settled state charged %d (replacement %d); want 0 and %d", u.charged, additional, len(raw)+1024)
	}

	// Earlier builds left the journal's inline bytes in the settled state.
	old := published.state
	old.Payloads = pending.Sources
	old.Preparation = pending.Preparation
	type wireState publishedState
	raw, err = json.Marshal(wireState(old))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, bytes.Repeat([]byte(" "), 2<<20)...)
	if err = os.WriteFile(s.publishedPath(id), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	inline := int64(base64.StdEncoding.EncodedLen(len(pending.Sources[0].Payload.Inline)) + base64.StdEncoding.EncodedLen(len(pending.Preparation.Inputs[0].Payload.Inline)))
	u, additional := publishedUsage(t, s, id, 1024)
	if u.charged != 2*inline || u.physical != int64(len(raw)) {
		t.Fatalf("settled state with inline bytes charged %d of %d physical; want %d", u.charged, u.physical, 2*inline)
	}
	if want := max(int64(len(raw))+1024, 2*1024) - 2*inline; additional != want {
		t.Fatalf("replacement credit: additional %d, want %d", additional, want)
	}
}
