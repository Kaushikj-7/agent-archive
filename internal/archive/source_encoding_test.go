package archive

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

// encodeSourceMarshalReference is the exact pre-Encoder implementation. Keep it
// independent of production encoding for byte and allocation comparisons.
func encodeSourceMarshalReference(w io.Writer, bundle SourceBundle) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	header := sourceHeader(bundle)
	write := func(value any) error {
		line, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode source line: %w", err)
		}
		if len(line) > MaxSourceLineBytes {
			return fmt.Errorf("source line of %d bytes exceeds the %d byte line limit", len(line), MaxSourceLineBytes)
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	}
	if err := write(header); err != nil {
		return err
	}
	for i, record := range bundle.NativeRecords {
		var ordinal *uint64
		if bundle.History != nil {
			ordinal = &bundle.Ordinals[i]
		}
		if err := write(nativeRecordLine{Kind: SourceLineNativeRecord, Record: record, Ordinal: ordinal}); err != nil {
			return err
		}
	}
	for _, text := range bundle.NativeText {
		if err := write(nativeTextLine{Kind: SourceLineNativeText, Format: text.Format, Content: text.Content}); err != nil {
			return err
		}
	}
	for _, evidence := range bundle.SupplementalEvidence {
		if err := write(evidenceLine{Kind: SourceLineSupplementalEvidence, Evidence: evidence}); err != nil {
			return err
		}
	}
	return nil
}

func sourceEncodingFixture(records int, content string) SourceBundle {
	bundle := retainedHistoryFixture()
	bundle.SchemaVersion = SourceSchemaVersion
	bundle.History = nil
	bundle.Ordinals = nil
	bundle.NativeRecords = make([]map[string]any, records)
	for i := range bundle.NativeRecords {
		bundle.NativeRecords[i] = map[string]any{
			"z": content, "a": i, "nested": map[string]any{"unicode": "雪☃\u2028\u2029", "html": "<tag>&"},
			"number": json.Number("9007199254740993"), "raw": json.RawMessage(`{ "html": "<>&", "unicode": "雪" }`), "null": nil, "array": []any{true, "\n\t\\\"", -0.125},
		}
	}
	return bundle
}

func TestSourceEncodingMatchesMarshalBytes(t *testing.T) {
	for _, schema := range []int{SourceSchemaVersion, HistorySourceSchemaVersion} {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			bundle := sourceEncodingFixture(3, "synthetic\ncontent\xff")
			bundle.PreviousGenerationID = "previous"
			bundle.NativeChild = true
			bundle.ParentSessionID = "parent"
			bundle.LinkedSessions = []LinkedSessionReference{{SessionID: "linked", Relationship: "related", ObservedAt: bundle.Capture.CapturedAt}}
			bundle.SupplementalEvidence = []SupplementalEvidence{{Kind: EvidenceKindSkillSnapshot, ObservedAt: bundle.Capture.CapturedAt, Provenance: "synthetic", Payload: map[string]any{"z": "<>&雪", "a": []any{json.Number("1e9"), nil}}}}
			if schema == HistorySourceSchemaVersion {
				history := retainedHistoryFixture()
				bundle.SchemaVersion = schema
				bundle.History = history.History
				bundle.Ordinals = history.Ordinals
				bundle.NativeRecords = bundle.NativeRecords[:2]
			} else {
				bundle.NativeText = []TextTranscript{{Format: "text", Content: "<>&\u2028\u2029\n雪"}}
			}
			var want, got bytes.Buffer
			if err := encodeSourceMarshalReference(&want, bundle); err != nil {
				t.Fatal(err)
			}
			if err := EncodeSource(&got, bundle); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("schema %d changed JSONL bytes", schema)
			}
			var compressed bytes.Buffer
			gz, err := gzip.NewWriterLevel(&compressed, gzip.DefaultCompression)
			if err != nil {
				t.Fatal(err)
			}
			gz.ModTime = time.Unix(1, 0).UTC()
			gz.OS = 255
			if err := encodeSourceMarshalReference(gz, bundle); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			actual, err := BuildCompressedSource(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual.Bytes, compressed.Bytes()) {
				t.Fatalf("schema %d changed compressed bytes", schema)
			}
		})
	}
}

func TestSourceEncodingRepresentativeBundlesMatchMarshal(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		records int
		content string
	}{
		{"small", 2, "synthetic ordinary text"},
		{"many_records", 1024, "synthetic ordinary text"},
		{"long_line", 1, strings.Repeat("synthetic<>&雪\n", 16384)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			bundle := sourceEncodingFixture(fixture.records, fixture.content)
			var want, got bytes.Buffer
			if err := encodeSourceMarshalReference(&want, bundle); err != nil {
				t.Fatal(err)
			}
			if err := EncodeSource(&got, bundle); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want.Bytes(), got.Bytes()) {
				t.Fatal("representative JSONL bytes changed")
			}
		})
	}
}

func TestSourceEncodingValidationWritesNothing(t *testing.T) {
	for _, change := range []func(*SourceBundle){
		func(b *SourceBundle) { b.SchemaVersion = 1 },
		func(b *SourceBundle) { b.ArchiveSessionID = "" },
		func(b *SourceBundle) { b.Capture.AdapterName = "" },
		func(b *SourceBundle) { b.Ordinals[1] = b.Ordinals[0] },
		func(b *SourceBundle) { b.History.Spans[0].EndRecord++ },
		func(b *SourceBundle) { b.History = nil },
	} {
		bundle := retainedHistoryFixture()
		change(&bundle)
		var want, got bytes.Buffer
		oldErr := encodeSourceMarshalReference(&want, bundle)
		newErr := EncodeSource(&got, bundle)
		if oldErr == nil || newErr == nil || oldErr.Error() != newErr.Error() {
			t.Fatalf("validation errors: old %v, new %v", oldErr, newErr)
		}
		if want.Len() != 0 || got.Len() != 0 {
			t.Fatal("invalid bundle published bytes")
		}
	}
}

func TestSourceEncodingSerializationFailurePreservesPrefix(t *testing.T) {
	for _, invalid := range []any{math.NaN(), make(chan int), json.RawMessage("{"), json.Number("invalid")} {
		bundle := sourceEncodingFixture(2, "synthetic")
		bundle.NativeRecords[1]["bad"] = invalid
		var want, got bytes.Buffer
		oldErr := encodeSourceMarshalReference(&want, bundle)
		newErr := EncodeSource(&got, bundle)
		if oldErr == nil || newErr == nil || oldErr.Error() != newErr.Error() {
			t.Fatalf("serialization errors: old %v, new %v", oldErr, newErr)
		}
		if !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Fatal("serialization failure changed completed prefix")
		}
	}
}

type sourceEncodingFailureWriter struct {
	bytes.Buffer
	call, failAt int
	err          error
}

func (w *sourceEncodingFailureWriter) Write(p []byte) (int, error) {
	w.call++
	if w.call == w.failAt {
		n, _ := w.Buffer.Write(p[:len(p)/2])
		return n, w.err
	}
	return w.Buffer.Write(p)
}

func TestSourceEncodingWriterFailurePreservesBoundaries(t *testing.T) {
	bundle := sourceEncodingFixture(2, "synthetic")
	sentinel := errors.New("synthetic write failure")
	for call := 1; call <= 6; call++ {
		want := &sourceEncodingFailureWriter{failAt: call, err: sentinel}
		got := &sourceEncodingFailureWriter{failAt: call, err: sentinel}
		if err := encodeSourceMarshalReference(want, bundle); err != sentinel {
			t.Fatalf("reference: %v", err)
		}
		if err := EncodeSource(got, bundle); err != sentinel {
			t.Fatalf("writer error wrapped or lost: %v", err)
		}
		if want.call != got.call || !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Fatalf("write %d changed error boundary", call)
		}
	}
}

func TestSourceEncodingRejectsShortWrites(t *testing.T) {
	bundle := sourceEncodingFixture(2, "synthetic")
	for call := 1; call <= 6; call++ {
		old := &sourceEncodingFailureWriter{failAt: call}
		if err := encodeSourceMarshalReference(old, bundle); err != nil {
			t.Fatalf("old encoder unexpectedly rejects short write: %v", err)
		}
		got := &sourceEncodingFailureWriter{failAt: call}
		if err := EncodeSource(got, bundle); err != io.ErrShortWrite {
			t.Fatalf("write %d: got %v", call, err)
		}
		if got.call != call {
			t.Fatalf("continued after short write %d", call)
		}
	}
}

func TestSourceLineLimitExcludesNewline(t *testing.T) {
	// A direct guard check reaches the exact 65 MiB boundary without also
	// allocating a transcript and encoding/json's escaped representation.
	line := make([]byte, MaxSourceLineBytes+2)
	line[MaxSourceLineBytes] = '\n'
	w := sourceLineWriter{w: io.Discard}
	if n, err := w.Write(line[:MaxSourceLineBytes+1]); err != nil || n != MaxSourceLineBytes+1 {
		t.Fatalf("exact limit rejected: %d, %v", n, err)
	}
	var output bytes.Buffer
	w = sourceLineWriter{w: &output}
	if _, err := w.Write(line); err == nil || output.Len() != 0 {
		t.Fatalf("oversized line reached destination: %v, %d bytes", err, output.Len())
	}
}

func TestSourceEncodingReducesRecordAllocations(t *testing.T) {
	bundle := sourceEncodingFixture(64, "synthetic ordinary text")
	measure := func(encode func(io.Writer, SourceBundle) error) float64 {
		return testing.AllocsPerRun(20, func() {
			if err := encode(io.Discard, bundle); err != nil {
				t.Fatal(err)
			}
		})
	}
	old := measure(encodeSourceMarshalReference)
	current := measure(EncodeSource)
	if current >= old {
		t.Fatalf("record encoding allocations did not decline: reference %.0f, current %.0f", old, current)
	}
}

func BenchmarkSourceEncoding(b *testing.B) {
	for _, fixture := range []struct {
		name    string
		records int
		content string
	}{
		{"small", 2, "synthetic ordinary text"},
		{"many_records", 1024, "synthetic ordinary text"},
		{"long_line", 1, strings.Repeat("synthetic<>&雪\n", 16384)},
	} {
		bundle := sourceEncodingFixture(fixture.records, fixture.content)
		for _, implementation := range []struct {
			name   string
			encode func(io.Writer, SourceBundle) error
		}{
			{"marshal_reference", encodeSourceMarshalReference}, {"encoder", EncodeSource},
		} {
			b.Run(fixture.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if err := implementation.encode(io.Discard, bundle); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
