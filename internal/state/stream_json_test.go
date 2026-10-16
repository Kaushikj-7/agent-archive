package state

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// largePending is a pending publication of session "owned" carrying records
// records of size bytes of text each and a source of sourceBytes.
func largePending(records, size, sourceBytes int) PendingPublication {
	text := strings.Repeat("<x&y>é", size/7+1)[:size]
	bundle := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: "owned", NativeSessionID: "native", ProjectID: "project", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, CapturedAt: time.Date(2026, 10, 15, 1, 2, 3, 4, time.UTC)}}
	for i := range records {
		bundle.NativeRecords = append(bundle.NativeRecords, map[string]any{"type": "response_item", "ordinal": float64(i), "payload": map[string]any{"text": text, "z": nil, "list": []any{1.5, " ", true}}})
		bundle.Ordinals = append(bundle.Ordinals, uint64(i))
	}
	source := bytes.Repeat([]byte{0xfb, 0xff, 0x00}, sourceBytes/3+1)[:sourceBytes]
	return PendingPublication{Bundle: bundle, SourceKey: "k", MetadataKey: "m", SourceSHA256: durableRef(source).SHA256, SourceBytes: source, MetadataBytes: []byte(`{"a":"<b>"}`), RequestToken: "token", ReadyAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)}
}

// A streamed encoding is byte for byte the one json.Encoder writes, for a
// pending publication with every kind of field present or empty.
func TestStreamedPendingEncodingIsTheEncodersEncoding(t *testing.T) {
	full := largePending(3, 100, 101)
	full.Catalog = &CatalogPublication{Protocol: 11, ID: "id", ExpectedRevision: "r"}
	full.History = &PendingHistory{Version: 1, Preparing: true, Inputs: []HistoryInput{{RevisionID: "a"}}}
	full.ImportRetired = []archive.SourceReference{{Key: "k", SHA256: "s", CompressedBytes: 1}}
	full.Bundle.History = &archive.SourceHistory{ThreadID: "t", ActiveRolloutID: "r"}
	full.Bundle.NativeText = []archive.TextTranscript{{Format: "f", Content: "c"}}
	empty := PendingPublication{}
	emptySlices := largePending(0, 0, 0)
	emptySlices.Bundle.NativeRecords, emptySlices.SourceBytes = []map[string]any{}, []byte{}
	for name, p := range map[string]PendingPublication{"full": full, "empty": empty, "empty slices": emptySlices, "plain": largePending(5, 1000, 1000)} {
		var want, got bytes.Buffer
		if err := json.NewEncoder(&want).Encode(p); err != nil {
			t.Fatal(err)
		}
		budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
		if err := streamJSON(t.Context(), &got, p, budget.Reservation()); err != nil {
			t.Fatal(name, err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("%s: streamed\n%s\nwant\n%s", name, got.Bytes(), want.Bytes())
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Fatalf("%s: streaming kept %d bytes charged", name, used)
		}
	}
}

// Saving a pending publication charges about one record's encoding, not the
// whole file's: a long thread's pending publication, which carries its whole
// source and records, is saved with read memory far smaller than itself.
func TestSavingAPendingPublicationChargesOneRecordNotTheFile(t *testing.T) {
	s := newTestStore(t)
	pending := largePending(256, 16<<10, 2<<20)
	pending.Bundle.SchemaVersion, pending.Bundle.Ordinals = 2, nil // an ordinary source
	size, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	if err := scoped.SavePending("owned", pending); err != nil {
		t.Fatalf("saving a %d-byte pending publication within 1 MiB: %v", len(size), err)
	}
	if _, peak := budget.Charged(); peak > 512<<10 {
		t.Fatalf("saving charged %d bytes at its peak", peak)
	}
	got, found, err := s.LoadPending("owned")
	if err != nil || !found || len(got.Bundle.NativeRecords) != 256 || !bytes.Equal(got.SourceBytes, pending.SourceBytes) {
		t.Fatalf("saved publication does not read back: found=%v err=%v", found, err)
	}
}
