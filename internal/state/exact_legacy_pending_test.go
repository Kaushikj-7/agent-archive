package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func exactLegacyPendingFixture(t *testing.T, s *Store) (PendingPublication, []byte) {
	t.Helper()
	const id = "owned"
	source := []byte("private synthetic source")
	bundle := archive.SourceBundle{
		SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: id, NativeSessionID: "native-owned", ProjectID: "project",
		Capture:       archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, AdapterName: "synthetic", CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		NativeRecords: []map[string]any{{"content": strings.Repeat("x", 96<<10)}},
	}
	sum := storage.SHA256Hex(source)
	key, err := archive.SourceObjectKey(bundle, sum)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey(bundle.Capture.Harness.Name, id)
	if err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, NativeSessionID: bundle.NativeSessionID, ProjectID: bundle.ProjectID, Harness: bundle.Capture.Harness, CapturedAt: bundle.Capture.CapturedAt, SourceBundle: archive.SourceReference{Key: key, SHA256: sum, CompressedBytes: len(source)}}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingPublication{Bundle: bundle, SourceKey: key, SourceSHA256: sum, SourceBytes: source, MetadataKey: metadataKey, MetadataBytes: data, ReadyAt: bundle.Capture.CapturedAt, Attempted: true}
	if err = s.SavePending(id, pending); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	return pending, raw
}

func TestExactLegacyPendingRemovalFitsOneWireView(t *testing.T) {
	s := newTestStore(t)
	pending, raw := exactLegacyPendingFixture(t, s)
	const scratch = int64(32 << 10)
	const pressure = int64(1)
	budget := agentapi.NewNativeReadBudget(int64(len(raw)) + 2*scratch + pressure)
	if !budget.Reserve(pressure) {
		t.Fatal("independent owner reservation")
	}
	defer budget.Release(pressure)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	if err := scoped.RemoveExactLegacyPending("owned", pending); err != nil {
		t.Fatalf("exact acknowledgement required a second decoded view: %v", err)
	}
	if _, err := os.Lstat(s.pendingPath("owned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact pending not removed: %v", err)
	}
	used, _ := budget.Charged()
	if used != pressure {
		t.Fatalf("acknowledgement changed independent ownership: charged=%d", used)
	}
	if err := scoped.RemoveExactLegacyPending("owned", pending); err != nil {
		t.Fatalf("already absent ordinary transaction: %v", err)
	}
}

func TestExactLegacyPendingRemovalRefusesChangedFullWire(t *testing.T) {
	for _, extra := range []string{`,"future":"authority"`, `,"sources":[]`, `,"commit":null`, `,"commit":{"protocol":11,"id":"foreign","expected_revision":""}`, `,"attempted":false`} {
		t.Run(extra, func(t *testing.T) {
			s := newTestStore(t)
			pending, raw := exactLegacyPendingFixture(t, s)
			changed := append(append([]byte(nil), raw[:len(raw)-2]...), []byte(extra+"}\n")...)
			if err := local.WriteBytes(s.pendingPath("owned"), changed); err != nil {
				t.Fatal(err)
			}
			if err := s.RemoveExactLegacyPending("owned", pending); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatalf("changed/foreign/duplicate pending accepted: %v", err)
			}
			after, err := os.ReadFile(s.pendingPath("owned"))
			if err != nil || !bytes.Equal(after, changed) {
				t.Fatalf("refused wire changed: %v", err)
			}
		})
	}
}

func TestExactLegacyPendingRemovalKeepsNoncanonicalAndSourceBytes(t *testing.T) {
	s := newTestStore(t)
	pending, raw := exactLegacyPendingFixture(t, s)
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{indented.Bytes(), bytes.Replace(raw, []byte(`"source_bytes":"`), []byte(`"source_bytes":"A`), 1)} {
		if err := local.WriteBytes(s.pendingPath("owned"), changed); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveExactLegacyPending("owned", pending); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatalf("nonexact complete wire accepted: %v", err)
		}
		after, err := os.ReadFile(s.pendingPath("owned"))
		if err != nil || !bytes.Equal(after, changed) {
			t.Fatalf("nonexact bytes changed: %v", err)
		}
	}
}

func TestExactLegacyPendingRemovalRefusalPreservesOwnership(t *testing.T) {
	s := newTestStore(t)
	pending, raw := exactLegacyPendingFixture(t, s)
	budget := agentapi.NewNativeReadBudget(1)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	err := scoped.RemoveExactLegacyPending("owned", pending)
	closeScope()
	if !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("allocation refusal: %v", err)
	}
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("refusal leaked %d bytes", used)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	scoped, closeScope = s.WithReadBudget(canceled, budget)
	err = scoped.RemoveExactLegacyPending("owned", pending)
	closeScope()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation refusal: %v", err)
	}
	after, err := os.ReadFile(s.pendingPath("owned"))
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("refusal changed pending: %v", err)
	}
}

func TestExactLegacyPendingRemovalSupportsOwnedMetadataOnly(t *testing.T) {
	s := newTestStore(t)
	pending, _ := exactLegacyPendingFixture(t, s)
	pending.SourceSize = len(pending.SourceBytes)
	pending.SourceBytes = nil
	pending.MetadataOnly = true
	if err := s.SavePending("owned", pending); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveExactLegacyPending("owned", pending); err != nil {
		t.Fatalf("exact metadata-only acknowledgement: %v", err)
	}
}

func TestExactLegacyPendingRemovalRejectsNonlegacyOwner(t *testing.T) {
	s := newTestStore(t)
	pending, raw := exactLegacyPendingFixture(t, s)
	wrong := pending
	wrong.Bundle.ArchiveSessionID = "other"
	if err := s.RemoveExactLegacyPending("owned", wrong); !errors.Is(err, ErrCatalogJournalFrozen) {
		t.Fatalf("other session claimed acknowledgement: %v", err)
	}
	wrong = pending
	wrong.Catalog = &CatalogPublication{Protocol: 10, ID: "foreign", ExpectedRevision: ""}
	if err := s.RemoveExactLegacyPending("owned", wrong); !errors.Is(err, ErrCatalogJournalFrozen) {
		t.Fatalf("catalog work claimed ordinary acknowledgement: %v", err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.MetadataSchemaVersion + 1
	wrong = pending
	var err error
	wrong.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveExactLegacyPending("owned", wrong); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("future metadata claimed legacy acknowledgement: %v", err)
	}
	after, err := os.ReadFile(s.pendingPath("owned"))
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("refused ownership changed pending: %v", err)
	}
}

func TestExactLegacyPendingRemovalRetainsEvidenceAndPrivateMode(t *testing.T) {
	s := newTestStore(t)
	pending, raw := exactLegacyPendingFixture(t, s)
	if err := os.Chmod(s.pendingPath("owned"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveExactLegacyPending("owned", pending); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("nonprivate pending accepted: %v", err)
	}
	if err := os.Chmod(s.pendingPath("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(s.home, "publication-evidence", "owned", "foreign.json")
	if err := local.WriteBytes(evidence, []byte(`{"foreign":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveExactLegacyPending("owned", pending); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("publication evidence discarded: %v", err)
	}
	after, err := os.ReadFile(s.pendingPath("owned"))
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("refused private/evidence guard changed pending: %v", err)
	}
	if _, err := os.Lstat(evidence); err != nil {
		t.Fatalf("foreign publication evidence removed: %v", err)
	}
}
