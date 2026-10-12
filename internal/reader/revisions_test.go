package reader

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestRevisionInventoryEmptyAndInvalid(t *testing.T) {
	metadata, _, _ := fixture(t)
	got, err := Revisions(metadata)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no history: %#v %v", got, err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	if _, err := Revisions(metadata); err == nil {
		t.Fatal("accepted missing history")
	}
}

func TestReadRevisionPreservesLegacyProvenanceAndLimits(t *testing.T) {
	metadata, bundle, store := fixture(t)
	const thread = "11111111-1111-4111-8111-111111111111"
	const active = "22222222-2222-4222-8222-222222222222"
	const old = "33333333-3333-4333-8333-333333333333"
	bundle.NativeSessionID = thread
	bundle.Capture.CapturedAt = bundle.Capture.CapturedAt.Add(-time.Hour)
	bundle.Capture.FilterVersion = "14"
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), key, packed.Bytes); err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.NativeSessionID = thread
	metadata.FilterVersion = "15"
	metadata.History = &archive.RevisionHistory{CurrentRevision: active, Preserved: []archive.RevisionReference{{RevisionID: old, CapturedAt: bundle.Capture.CapturedAt, Source: archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}}}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey := "sessions/codex/session-1/metadata.json"
	if err := store.Put(t.Context(), metadataKey, data); err != nil {
		t.Fatal(err)
	}
	selected, got, err := ReadRevision(t.Context(), store, metadataKey, old, Limits{})
	if err != nil || selected.FilterVersion != "14" || !selected.CapturedAt.Equal(bundle.Capture.CapturedAt) || got.Capture.FilterVersion != "14" || selected.SourceBundle.Key != key {
		t.Fatalf("selected=%+v error=%v", selected, err)
	}
	for _, limits := range []Limits{{MaxCompressedBytes: 1}, {MaxUncompressedBytes: 1}} {
		if _, _, err := ReadRevision(t.Context(), store, metadataKey, old, limits); err == nil {
			t.Fatalf("ignored limits %+v", limits)
		}
	}
	if _, _, err := ReadRevision(t.Context(), store, metadataKey, key, Limits{}); err == nil {
		t.Fatal("accepted object key as revision")
	}
	if err := store.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRevision(t.Context(), store, metadataKey, old, Limits{}); err == nil {
		t.Fatal("accepted missing source")
	}
}
