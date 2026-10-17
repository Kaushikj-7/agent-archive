package state

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPublicationBlockedExactRestorePreservesSelectingProof(t *testing.T) {
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Now())
	fixture.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = published.SaveBlocked(p.Bundle, time.Now(), BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	if err = published.SavePublication(p.Bundle, time.Now(), p.SourceReference(), p.MetadataBytes); err != nil {
		t.Fatal("exact restore", err)
	}
	published, err = s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil || published.state.PublicationVersion != 5 || published.state.Commit == nil {
		t.Fatal("restart lost selecting proof", err)
	}
	before, err := os.ReadFile(s.publishedPath(p.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"body", "source", "metadata-refresh-source"} {
		t.Run(kind, func(t *testing.T) {
			body := append([]byte(nil), p.MetadataBytes...)
			source := p.SourceReference()
			if kind == "body" {
				body = append(body, ' ')
			} else {
				source.CompressedBytes++
			}
			var e error
			if kind == "metadata-refresh-source" {
				next := p
				next.Commit = nil
				next.SourceSize = source.CompressedBytes
				next.Sources = append([]PublicationSource(nil), p.Sources...)
				next.Sources[0].Reference = source
				if next.SourceReference() == p.SourceReference() {
					t.Fatal("negative fixture did not change effective selecting reference")
				}
				e = published.SaveRepublishedMetadata(next, time.Now())
			} else {
				e = published.SavePublication(p.Bundle, time.Now(), source, body)
			}
			if e == nil {
				t.Fatal("changed selecting authority accepted without committed successor")
			}
			after, e := os.ReadFile(s.publishedPath(p.Bundle.ArchiveSessionID))
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed selecting bytes", e)
			}
		})
	}
	// Persisted snapshot hints cannot claim a different selecting reference either.
	forged := published.state
	snapshot := *forged.LastPublished
	source := *snapshot.Source
	source.CompressedBytes++
	snapshot.Source = &source
	forged.LastPublished = &snapshot
	summary := forged.summary()
	forged.Summary = &summary
	raw, e := json.Marshal(forged)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(s.publishedPath(p.Bundle.ArchiveSessionID), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.LoadPublishedState(p.Bundle.ArchiveSessionID); e == nil {
		t.Fatal("persisted selecting source hint tamper accepted")
	}

}
