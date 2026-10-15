package search

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func legacyWorkerFixture(t *testing.T) (archive.Metadata, []byte, *storagetest.MemoryStore) {
	t.Helper()
	m, bundle, objects := verifiedFixture(t, "claude", workerTranscript)
	if historicalVisibilityUnavailable(bundle.Capture.FilterVersion) {
		t.Fatal("fixture requires a supported privacy filter")
	}
	bundle.SchemaVersion = 1
	document, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(document); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	m.SourceBundle = archive.SourceReference{Key: strings.TrimSuffix(m.SourceBundle.Key, ".jsonl.gz") + ".json.gz", SHA256: storage.SHA256Hex(compressed.Bytes()), CompressedBytes: compressed.Len()}
	if err = objects.Put(t.Context(), m.SourceBundle.Key, compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	putMetadata(t, objects, m)
	if err = m.ValidateSourceReference(); err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ReadMetadata(t.Context(), objects, key); err != nil {
		t.Fatal(err)
	}
	return m, compressed.Bytes(), objects
}

func TestWorkerCachesUnsupportedImmutableSourceGap(t *testing.T) {
	t.Parallel()
	metadata, _, objects := legacyWorkerFixture(t)
	dir := t.TempDir()
	s, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	first, err := s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || first.Loaded != 1 || first.Committed != 1 || first.Pending != 0 || first.Failed != 0 || len(first.Gaps) != 1 || first.Gaps[0].Reason != "unsupported_source_schema" {
		t.Fatalf("unsupported source did not settle as a gap: %+v %v", first, err)
	}
	var gaps []byte
	if err = s.db.QueryRowContext(t.Context(), "SELECT gaps FROM search_sessions").Scan(&gaps); err != nil || !bytes.Contains(gaps, []byte("unsupported_source_schema")) {
		t.Fatalf("gap not persisted: %s %v", gaps, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Observe(t.Context(), metadata, parsers, at.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	next, err := s.Run(t.Context(), counted, parsers, at.Add(time.Hour), Budget{})
	if err != nil || next.Loaded != 0 || next.Pending != 0 || next.Failed != 0 || counted.gets != 1 {
		t.Fatalf("unsupported immutable source retried: %+v GETs=%d %v", next, counted.gets, err)
	}
	ref := Reference{Source: Source{Destination: "test/context", SessionID: metadata.SessionID, Reference: metadata.SourceBundle, ExtractorVersion: ExtractorVersion}, End: 4}
	if _, err = ReadPassage(t.Context(), objects, parsers, ref, 0, 4, reader.Limits{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported read classification: %v", err)
	}
	// A newly published supported source must clear the gap and become searchable.
	current, _, currentObjects := verifiedFixture(t, "claude", workerTranscript)
	if err = s.Observe(t.Context(), current, parsers, at.Add(2*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Run(t.Context(), currentObjects, parsers, at.Add(2*time.Hour), Budget{})
	if err != nil || replacement.Loaded != 1 || replacement.Committed != 1 || len(replacement.Gaps) != 0 {
		t.Fatalf("changed source did not replace gap: %+v %v", replacement, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 || len(page.Hits[0].Gaps) != 0 {
		t.Fatalf("supported source not searchable: %+v %v", page, err)
	}
}

type transientFormatObjects struct {
	storage.ObjectStore
	cause error
}

func (o transientFormatObjects) GetLimited(context.Context, string, int64) ([]byte, error) {
	return nil, o.cause
}

type sourceFailureKind string

const (
	failureChecksum              sourceFailureKind = "checksum"
	failureGzip                  sourceFailureKind = "gzip"
	failureMalformedCurrent      sourceFailureKind = "malformed-current"
	failureTransient             sourceFailureKind = "transient"
	failureUnverifiedSchemaError sourceFailureKind = "unverified-schema-error"
)

func TestWorkerDoesNotCacheUnverifiedOrCorruptSourcesAsUnsupported(t *testing.T) {
	t.Parallel()
	for _, mode := range []sourceFailureKind{failureChecksum, failureGzip, failureMalformedCurrent, failureTransient, failureUnverifiedSchemaError} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			metadata, data, objects := legacyWorkerFixture(t)
			var reads storage.ObjectStore = objects
			switch mode {
			case failureChecksum:
				data = append([]byte(nil), data...)
				data[len(data)-1] ^= 1
				if err := objects.Put(t.Context(), metadata.SourceBundle.Key, data); err != nil {
					t.Fatal(err)
				}
			case failureGzip:
				data = append([]byte(nil), data...)
				data[len(data)-8] ^= 1 // Valid declared checksum, invalid gzip trailer.
				metadata.SourceBundle.SHA256 = storage.SHA256Hex(data)
				if err := objects.Put(t.Context(), metadata.SourceBundle.Key, data); err != nil {
					t.Fatal(err)
				}
			case failureMalformedCurrent:
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write([]byte(`{"schema_version":2}`)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				data = compressed.Bytes()
				metadata.SourceBundle.SHA256 = storage.SHA256Hex(data)
				metadata.SourceBundle.CompressedBytes = len(data)
				if err := objects.Put(t.Context(), metadata.SourceBundle.Key, data); err != nil {
					t.Fatal(err)
				}
			case failureTransient:
				reads = transientFormatObjects{ObjectStore: objects, cause: errors.New("temporary read failure")}
			case failureUnverifiedSchemaError:
				reads = transientFormatObjects{ObjectStore: objects, cause: &archive.UnsupportedSourceSchemaError{Version: 1}}
			}
			s := openTest(t)
			parsers := builtin.NewBuiltins()
			if err := s.Observe(t.Context(), metadata, parsers, metadata.CapturedAt, 0); err != nil {
				t.Fatal(err)
			}
			for _, at := range []time.Time{metadata.CapturedAt, metadata.CapturedAt.Add(time.Second)} {
				progress, err := s.Run(t.Context(), reads, parsers, at, Budget{})
				if err != nil || progress.Loaded != 1 || progress.Committed != 0 || progress.Failed != 1 || progress.Pending != 1 || len(progress.Gaps) != 0 {
					t.Fatalf("failure was cached as unsupported: %+v %v", progress, err)
				}
			}
			var failures, blocked int
			if err := s.db.QueryRowContext(t.Context(), "SELECT failures,blocked FROM search_work").Scan(&failures, &blocked); err != nil || failures != 2 || blocked != 0 {
				t.Fatalf("failure did not remain retryable: failures=%d blocked=%d %v", failures, blocked, err)
			}
		})
	}
}
