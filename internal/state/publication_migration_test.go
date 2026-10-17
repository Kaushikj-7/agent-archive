package state

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
	"time"
)

func TestOrdinaryMigrationRetainsPriorBodyForFullPublishedReplay(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	next := publicationFixture(t, publicationThread, at)
	oldBundle := next.Bundle
	oldBundle.SchemaVersion, oldBundle.History, oldBundle.Ordinals = archive.SourceSchemaVersion, nil, nil
	packed, err := archive.BuildCompressedSource(oldBundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(oldBundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	old, err := archive.BuildMetadataWithAnalysis(oldBundle, archive.Analysis{}, nil, "synthetic-machine", at, at, ref, archive.ParserInfo{Name: "codex", Version: "synthetic-v1"})
	if err != nil {
		t.Fatal(err)
	}
	old.Title = "sk-abcdefghijklmnopqrstuv"
	oldBody, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	binding := &archive.CodexSourceBinding{Version: 1, NativeThreadID: publicationThread, NativeCreatedAt: at, Cwd: "/synthetic", PhysicalRolloutID: publicationThread, Path: "/synthetic/absent.jsonl"}
	reg := archive.SessionRegistration{ArchiveSessionID: old.SessionID, NativeSessionID: old.NativeSessionID, ProjectID: old.ProjectID, DestinationID: "destination"}
	proof, err := ValidateOrdinaryHistoryMigration(t.Context(), oldBody, next.MetadataBytes, oldBundle, next.Bundle, reg, binding, binding, codex.Filter{}, "admission", "policy", agentapi.NewNativeReadBudget(128<<20))
	if err != nil {
		t.Fatal(err)
	}
	next.History = &PendingHistory{Version: 1}
	next, err = next.WithOrdinaryMigration(proof)
	if err != nil {
		t.Fatal(err)
	}
	next, err = PreparePublicationV2(next, PublicationPredecessor{State: PredecessorPresent, Body: oldBody, Bundle: oldBundle}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateWireParity(t, publicationWire(next))
	published, err := s.LoadPublishedState(old.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(next, at); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.LoadPublishedState(old.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateWireParity(t, reloaded.state)
	if !bytes.Equal(reloaded.state.Preparation.Migration.PreviousMetadata, oldBody) {
		t.Fatal("prior body lost after published replacement")
	}
	corrupted := reloaded.state
	body := bytes.Clone(corrupted.Preparation.Migration.PreviousMetadata)
	body[0] ^= 1
	receipt := *corrupted.Preparation.Migration
	receipt.PreviousMetadata = body
	authority := *corrupted.Preparation
	authority.Migration = &receipt
	corrupted.Preparation = &authority
	if err = corrupted.validateSelectingPublished(); err == nil {
		t.Fatal("tampered previous body accepted")
	}
	receipt.PreviousMetadata = nil
	if err = corrupted.validateSelectingPublished(); err == nil {
		t.Fatal("missing previous body accepted")
	}
}
