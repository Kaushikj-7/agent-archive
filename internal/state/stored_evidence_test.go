package state

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// StoredEvidence returns the evidence of both the pending publication and
// the published state, pending first, and nothing for a session with
// neither.
func TestStoredEvidenceReadsPendingAndPublished(t *testing.T) {
	store, err := openTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	evidence := func(provenance string) []archive.SupplementalEvidence {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindFinalResponse, ObservedAt: at, Provenance: provenance, Payload: map[string]any{"text": "x"}}}
	}
	if got, err := store.StoredEvidence("s1"); err != nil || len(got) != 0 {
		t.Fatalf("nothing stored: %v %v", got, err)
	}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "s1", NativeRecords: []map[string]any{{"type": "user"}}, SupplementalEvidence: evidence("hook:published")}
	p, err := store.LoadPublishedState("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(bundle, at, CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	bundle.SupplementalEvidence = evidence("hook:pending")
	// This read-only partial-reader fixture deliberately contains invalid source
	// facts. It never asserts selecting authority or a successful SavePending.
	if err := local.Write(store.pendingPath("s1"), PendingPublication{Bundle: bundle, SourceKey: "k", MetadataKey: "m", SourceSHA256: "h", SourceBytes: []byte("s"), MetadataBytes: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	got, err := store.StoredEvidence("s1")
	if err != nil || len(got) != 2 || got[0].Provenance != "hook:pending" || got[1].Provenance != "hook:published" {
		t.Fatalf("%+v %v", got, err)
	}
	// Hook evidence a recoverable block deferred counts too.
	if err := local.Write(store.publishedPath("s2"), map[string]any{"status": "blocked", "deferred_hook_evidence": evidence("hook:deferred")}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.StoredEvidence("s2"); err != nil || len(got) != 1 || got[0].Provenance != "hook:deferred" {
		t.Fatalf("deferred: %+v %v", got, err)
	}
	if _, err := store.StoredEvidence("../escape"); err == nil {
		t.Fatal("unsafe ID accepted")
	}
}
