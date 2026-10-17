package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func confirmedNativeBatchFixture(t *testing.T) Batch {
	t.Helper()
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	b := Batch{ID: "2026-10-17-1", StartedAt: at, DestinationID: "destination", Filters: BatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}}
	scope := archive.NativeImportBatchScope{BatchID: b.ID, StartedAtUTC: at.Format(time.RFC3339Nano), DestinationID: b.DestinationID, Filters: archive.ImportBatchFilters(b.Filters), ProjectIDs: []string{"project"}, Selected: []archive.NativeImportBatchSelection{{Kind: "ordinary-root", NativeID: "session", OriginalTUTC: at.Add(-time.Hour).Format(time.RFC3339Nano), ProjectID: "project", RootFingerprint: strings.Repeat("a", 64), RootRevision: "one", BindingSHA256: strings.Repeat("b", 64), Source: archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1234, SHA256: strings.Repeat("d", 64)}}}}
	digest, err := archive.NativeImportBatchScopeDigest(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	b.NativeImport = &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("c", 32), ScopeSHA256: digest, Scope: scope}
	return b
}

func TestNativeImportBatchOrdinaryWireUnchanged(t *testing.T) {
	t.Parallel()
	b := Batch{ID: "2026-10-17-1", StartedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), DestinationID: "destination", Filters: BatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"2026-10-17-1","started_at":"2026-10-17T12:00:00Z","filters":{"harnesses":["muse"],"project_ids":[],"include_home":false,"include_temp":false,"include_removed":false},"destination_id":"destination","projects_added":null,"apps_added":null,"sessions":null,"subagents":null}`
	if string(raw) != want {
		t.Fatalf("ordinary wire changed: %s", raw)
	}
	home := t.TempDir()
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(batchPath(home, b.ID))
	if err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(indented, '\n')) {
		t.Fatal("ordinary durable encoding changed")
	}
}

func TestNativeImportBatchGenericSavePreservesAuthorityAndUndo(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	b := confirmedNativeBatchFixture(t)
	if err := SaveBatch(home, b); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
		t.Fatal("generic save minted confirmation", err)
	}
	// Explicitly arrange an existing confirmed control, not a producer proof.
	if err := local.Write(batchPath(home, b.ID), b); err != nil {
		t.Fatal(err)
	}
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(batchPath(home, b.ID))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Batch){
		"drop":        func(b *Batch) { b.NativeImport = nil },
		"destination": func(b *Batch) { b.DestinationID += "other" },
		"started":     func(b *Batch) { b.StartedAt = b.StartedAt.Add(time.Second) },
		"scope": func(b *Batch) {
			copy := *b.NativeImport
			copy.ScopeSHA256 = strings.Repeat("f", 64)
			b.NativeImport = &copy
		},
		"instance": func(b *Batch) {
			copy := *b.NativeImport
			copy.Instance = strings.Repeat("f", 32)
			b.NativeImport = &copy
		},
	} {
		t.Run(name, func(t *testing.T) {
			next := b
			change(&next)
			if err := SaveBatch(home, next); err == nil {
				t.Fatal("authority changed")
			}
			got, _ := os.ReadFile(batchPath(home, b.ID))
			if !bytes.Equal(got, original) {
				t.Fatal("refused write changed control")
			}
		})
	}
	undone := b.StartedAt.Add(time.Hour)
	b.UndoneAt = &undone
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(batchPath(home, b.ID))
	b.UndoneAt = nil
	if err := SaveBatch(home, b); err == nil {
		t.Fatal("undo cleared")
	}
	got, _ := os.ReadFile(batchPath(home, b.ID))
	if !bytes.Equal(got, before) {
		t.Fatal("stale save erased undo")
	}
}

func TestNativeImportBatchUnreadableAuthorityPreventsFallback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	b := confirmedNativeBatchFixture(t)
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"outer-filter-future":       bytes.Replace(raw, []byte(`"filters":{`), []byte(`"filters":{"future":true,`), 1),
		"outer-filter-alias":        bytes.Replace(raw, []byte(`"include_home":`), []byte(`"Include_Home":`), 1),
		"outer-filter-duplicate":    bytes.Replace(raw, []byte(`"include_home":`), []byte(`"include_home":true,"include_home":`), 1),
		"future":                    bytes.Replace(raw, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v2"), 1),
		"malformed-before-selector": append([]byte(`{"id":!,"native_import":`), raw...),
		"escaped-selector":          []byte(`{"id":!,"native\u005fimport":{}}`),
		"null":                      bytes.Replace(raw, []byte(`"native_import":{`), []byte(`"native_import":null,"ignored":{`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(home, "imports"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(batchPath(home, b.ID), bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadBatches(home); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
				t.Fatal("private control not classified", err)
			}
			if _, err := OpenBatch(home, nil, b.Filters, b.DestinationID, b.StartedAt); !errors.Is(err, ErrUnreadableImport) {
				t.Fatal("unreadable control fell back", err)
			}
			if err := SaveBatch(home, Batch{ID: b.ID}); err == nil {
				t.Fatal("generic save replaced unreadable authority")
			}
			got, _ := os.ReadFile(batchPath(home, b.ID))
			if !bytes.Equal(got, bad) {
				t.Fatal("unreadable control changed")
			}
		})
	}
}

func TestNativeImportBatchOrdinarySelectorLikeStringsRemainOrdinary(t *testing.T) {
	t.Parallel()
	b := Batch{ID: "2026-10-17-1", StartedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Filters: BatchFilters{Harnesses: []string{"native_import"}, ProjectIDs: []string{"native_import"}}, DestinationID: "native_import"}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Batch
	if err := decodeBatch(raw, &decoded); err != nil {
		t.Fatal("ordinary values acquired private selector semantics", err)
	}
	if decoded.NativeImport != nil || decoded.DestinationID != b.DestinationID {
		t.Fatal("ordinary value changed")
	}
	home := t.TempDir()
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	batches, err := LoadBatches(home)
	if err != nil || len(batches) != 1 || batches[0].NativeImport != nil {
		t.Fatal("ordinary persisted batch changed domain", err)
	}
}
