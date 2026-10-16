package state

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestNewPreparationUsesExactCurrentHistoryStage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	stage, err := s.StagePublicationSource(p.Bundle.ArchiveSessionID, p.SourceReference(), p.SourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	p.History = &PendingHistory{Version: 1, Sources: []PendingSource{stage}}
	p, err = PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sources[0].Payload.Kind != PublicationHistoryStage || p.Preparation.Inputs[0].Payload.Kind != PublicationHistoryStage {
		t.Fatal("new preparation duplicated staged current bytes")
	}
	if err := s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LoadPending(p.Bundle.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if len(got.SourceBytes) != 0 || got.Sources[0].Payload.Kind != PublicationHistoryStage {
		t.Fatal("readback reconstructed duplicate inline authority")
	}
	actual, err := s.ReadPendingSource(p.Bundle.ArchiveSessionID, stage)
	if err != nil || !bytes.Equal(actual, p.SourceBytes) {
		t.Fatal("stage changed", err)
	}
}

func TestFrozenInlinePreparationKeepsItsExactPayloadAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	p.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(p.Preparation)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.StagePublicationSource(p.Bundle.ArchiveSessionID, p.SourceReference(), p.SourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	p.History.Sources = []PendingSource{stage}
	if err := p.ValidatePublication(); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(p.Preparation)
	if err != nil || !bytes.Equal(before, after) || p.Sources[0].Payload.Kind != PublicationInline {
		t.Fatal("frozen inline preparation rebound to stage", err)
	}
}
