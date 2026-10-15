package reader

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A bundle written as schema 1 (one gzip-compressed JSON document) passes the
// size and hash checks, since those cover whatever bytes were stored, and is
// then refused with an error that names its schema version.
func TestLoadSourceRefusesSchemaOneBundles(t *testing.T) {
	metadata, bundle, store := fixture(t)
	bundle.SchemaVersion = 1
	document, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(document); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	legacyKey := strings.TrimSuffix(metadata.SourceBundle.Key, ".jsonl.gz") + ".json.gz"
	if err := store.Put(context.Background(), legacyKey, compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	metadata.SourceBundle = archive.SourceReference{Key: legacyKey, SHA256: storage.SHA256Hex(compressed.Bytes()), CompressedBytes: compressed.Len()}
	_, err = LoadSource(context.Background(), store, metadata, Limits{})
	if err == nil || !strings.Contains(err.Error(), "unsupported source schema version 1") || !IsUnsupportedSource(err) {
		t.Fatalf("schema 1 bundle error = %v", err)
	}
}

// A current bundle loads through the streaming decoder, and the uncompressed
// read limit still applies to it.
func TestLoadSourceStreamsAndKeepsTheUncompressedLimit(t *testing.T) {
	metadata, want, store := fixture(t)
	got, err := LoadSource(context.Background(), store, metadata, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("loaded bundle differs:\n%s\n%s", gotJSON, wantJSON)
	}
	if !strings.HasSuffix(metadata.SourceBundle.Key, ".jsonl.gz") {
		t.Fatalf("source key = %q", metadata.SourceBundle.Key)
	}
	if _, err := LoadSource(context.Background(), store, metadata, Limits{MaxUncompressedBytes: 32}); err == nil || !strings.Contains(err.Error(), "uncompressed read limit") {
		t.Fatalf("uncompressed limit error = %v", err)
	}
}

func TestLoadHistorySourceValidatesActiveRevisionAndReferences(t *testing.T) {
	t.Parallel()
	metadata, bundle, store := fixture(t)
	const thread = "11111111-1111-4111-8111-111111111111"
	const revision = "22222222-2222-4222-8222-222222222222"
	bundle.SchemaVersion = archive.HistorySourceSchemaVersion
	bundle.NativeSessionID = thread
	bundle.Ordinals = []uint64{0, 1}
	bundle.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: revision, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: revision, EndRecord: 2, EndOrdinal: 2}}}
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.NativeSessionID = thread
	metadata.History = &archive.RevisionHistory{CurrentRevision: revision}
	metadata.SourceBundle = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	if err := store.Put(t.Context(), key, packed.Bytes); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), "sessions/codex/session-1/metadata.json", raw); err != nil {
		t.Fatal(err)
	}
	selected, err := ReadMetadata(t.Context(), store, "sessions/codex/session-1/metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadSource(t.Context(), store, selected, Limits{})
	if err != nil || got.History == nil || got.History.ActiveRolloutID != revision {
		t.Fatalf("history readback: %+v %v", got.History, err)
	}
	selected.History.CurrentRevision = thread
	if _, err := LoadSource(t.Context(), store, selected, Limits{}); err == nil {
		t.Fatal("wrong active revision accepted")
	}
	selected.History.CurrentRevision = revision
	selected.History.Preserved = []archive.RevisionReference{{RevisionID: thread, CapturedAt: selected.CapturedAt, Source: archive.SourceReference{Key: "sessions/codex/foreign/source." + packed.SHA256 + ".jsonl.gz", SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}}
	if _, err := LoadSource(t.Context(), store, selected, Limits{}); err == nil {
		t.Fatal("foreign preserved source reference accepted")
	}
}

func TestLoadRevisionUsesIndependentFormatAndFilterProvenance(t *testing.T) {
	t.Parallel()
	for _, schema := range []int{archive.SourceSchemaVersion, archive.HistorySourceSchemaVersion} {
		t.Run(strconv.Itoa(schema), func(t *testing.T) {
			t.Parallel()
			metadata, bundle, store := fixture(t)
			const thread = "11111111-1111-4111-8111-111111111111"
			const active = "22222222-2222-4222-8222-222222222222"
			const preserved = "33333333-3333-4333-8333-333333333333"
			bundle.NativeSessionID = thread
			bundle.Capture.CapturedAt = bundle.Capture.CapturedAt.Add(-time.Hour)
			bundle.Capture.FilterVersion = "14"
			bundle.Capture.Harness.Version = "0.150.0"
			bundle.Capture.Harness.Mode = "older-mode"
			bundle.SchemaVersion = schema
			if schema == archive.HistorySourceSchemaVersion {
				bundle.Ordinals = []uint64{0, 1}
				bundle.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: preserved, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: preserved, EndRecord: 2, EndOrdinal: 2}}}
			}
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
			metadata.Harness.Version = "0.160.0"
			metadata.Harness.Mode = "current-mode"
			metadata.History = &archive.RevisionHistory{CurrentRevision: active, Preserved: []archive.RevisionReference{{RevisionID: preserved, CapturedAt: bundle.Capture.CapturedAt, Source: archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}}}
			for _, explicit := range []bool{false, true} {
				if explicit {
					metadata.History.Preserved[0].SourceSchemaVersion = schema
					metadata.History.Preserved[0].FilterVersion = "14"
				}
				got, err := LoadRevision(t.Context(), store, metadata, preserved, Limits{})
				if err != nil || got.Capture.FilterVersion != "14" || got.SchemaVersion != schema {
					t.Fatalf("mixed legacy/explicit provenance: %+v %v", got.Capture, err)
				}
			}
			metadata.History.Preserved[0].FilterVersion = "15"
			if _, err := LoadRevision(t.Context(), store, metadata, preserved, Limits{}); err == nil {
				t.Fatal("false privacy provenance accepted")
			}
			metadata.History.Preserved[0].FilterVersion = "14"
			metadata.History.Preserved[0].SourceSchemaVersion = 5 - schema
			if _, err := LoadRevision(t.Context(), store, metadata, preserved, Limits{}); err == nil {
				t.Fatal("false format provenance accepted")
			}
			current := metadata
			current.History = &archive.RevisionHistory{CurrentRevision: preserved}
			current.SourceBundle = metadata.History.Preserved[0].Source
			current.CapturedAt = bundle.Capture.CapturedAt
			current.FilterVersion = bundle.Capture.FilterVersion
			if _, err := DecodeReferencedSource(t.Context(), current, packed.Bytes, Limits{}); err == nil {
				t.Fatal("active producer mismatch accepted")
			}
			current.Harness = bundle.Capture.Harness
			if _, err := DecodeReferencedSource(t.Context(), current, packed.Bytes, Limits{}); err != nil {
				t.Fatal("matching active producer refused", err)
			}

		})
	}
}
