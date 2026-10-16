package state

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestLocalBudgetRefusalPreservesPendingAndIndependentProgress(t *testing.T) {
	s := newTestStore(t).ForCollectorPass()
	raw := []byte(`{"request_token":"newer-request","source_bytes":"cHJlc2VydmVk"}`)
	path := s.pendingPath("owed")
	if err := local.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(2 * int64(len(raw)))
	reservation := budget.Reservation()
	pressure := int64(1)
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure reservation")
	}
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	if _, _, err := scoped.LoadPending("owed"); !agentapi.HasFailure(err, agentapi.Limit) || errors.Is(err, ErrQuarantined) {
		t.Fatalf("refusal %v", err)
	}
	closeScope()
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatalf("journal changed: %s %v", after, err)
	}
	if len(s.QuarantinedFiles()) != 0 {
		t.Fatal("valid work quarantined")
	}
	reservation.Release(pressure)
	scoped, closeScope = s.WithReadBudget(t.Context(), budget)
	p, found, err := scoped.LoadPending("owed")
	if err != nil || !found || p.RequestToken != "newer-request" {
		t.Fatalf("retry %#v %t %v", p, found, err)
	}
	used, _ := budget.Charged()
	if used != int64(len(raw)) {
		t.Fatalf("decoded lifetime charged %d", used)
	}
	closeScope()
	closeScope()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("close leaked %d", used)
	}
}

func TestLocalBudgetCancelAndEncodingRefusalRelease(t *testing.T) {
	s := newTestStore(t).ForCollectorPass()
	path := s.pendingPath("cancel")
	if err := local.WriteBytes(path, []byte(`{"request_token":"owed"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeScope := s.WithReadBudget(ctx, budget)
	if _, _, err := scoped.LoadPending("cancel"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeScope()
	scoped, closeScope = s.WithReadBudget(t.Context(), budget)
	root, err := os.OpenRoot(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := scoped.writeCompactAt(root, filepath.Join("pending", "unsupported.json"), make(chan int)); !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.pendingPath("unsupported")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	closeScope()
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("refusal leaked %d", used)
	}

}

func TestAcknowledgedMetadataDecodeReservesIndependentOwner(t *testing.T) {
	s := newTestStore(t)
	budget := agentapi.NewNativeReadBudget(1)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "session-1", NativeSessionID: "11111111-1111-4111-8111-111111111111", ProjectID: "project-1", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, CapturedAt: at, FilterVersion: archive.FilterVersion}}
	ref := archive.SourceReference{Key: "sessions/codex/session-1/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 10}
	metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID, ProjectID: bundle.ProjectID, Harness: bundle.Capture.Harness, CapturedAt: at, FilterVersion: archive.FilterVersion, SourceBundle: ref}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	// The enclosing published state already belongs to the caller. Decoding the
	// sidecar adds an independent metadata owner that cannot borrow that charge.
	p := &Published{id: bundle.ArchiveSessionID, store: scoped, found: true, state: publishedState{Bundle: bundle, Status: CacheStatusPublished, MetadataBytes: raw, LastPublished: &publishedSnapshot{SameAsBundle: true, Source: &ref}}}
	if _, _, err := p.LastPublishedMetadata(); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("uncharged independent metadata decode: %v", err)
	}
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("refusal leaked %d", used)
	}
	// Returning the unrelated pressure permits a useful exact owner, held until
	// the scoped state consumer closes, including cancellation/error release.
	budget = agentapi.NewNativeReadBudget(int64(len(raw)))
	scoped, closeScope = s.WithReadBudget(t.Context(), budget)
	p.store = scoped
	got, found, err := p.LastPublishedMetadata()
	if err != nil || !found || got.SessionID != bundle.ArchiveSessionID {
		t.Fatal(got.SessionID, found, err)
	}
	used, _ = budget.Charged()
	if used != int64(len(raw)) {
		t.Fatalf("decoded metadata lifetime %d", used)
	}
	closeScope()
	closeScope()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("metadata close leaked %d", used)
	}
}

func TestEphemeralMetadataValidationAndSummaryReserveBeforeDecode(t *testing.T) {
	s := newTestStore(t)
	raw := []byte(`{"schema_version":2,"history":{"current_revision":"11111111-1111-4111-8111-111111111111"}}`)
	budget := agentapi.NewNativeReadBudget(int64(len(raw) - 1))
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	if _, err := scoped.summaryBudgeted(publishedState{MetadataBytes: raw}); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("summary decoded without reservation", err)
	}
	pending := PendingPublication{History: &PendingHistory{Version: 1}, MetadataBytes: raw}
	if err := pending.ValidateHistoryBudgeted("session-1", budget); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("journal decoded without reservation", err)
	}
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("ephemeral refusal leaked %d", used)
	}
	budget = agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeScope = s.WithReadBudget(t.Context(), budget)
	summary, err := scoped.summaryBudgeted(publishedState{MetadataBytes: raw})
	if err != nil || summary.SourceSetComplete {
		t.Fatal(summary, err)
	}
	// Invalid envelopes still use the prior summary fallback, but no decoder
	// owner or temporary digest buffer remains after the operation.
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("ephemeral summary owner leaked %d", used)
	}
	if err := pending.ValidateHistoryBudgeted("session-1", budget); err == nil || errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("invalid journal accepted or mislabeled", err)
	}
	closeScope()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("ephemeral validation leaked %d", used)
	}
}
