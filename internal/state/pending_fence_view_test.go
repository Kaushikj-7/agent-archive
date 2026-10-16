package state

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// largePendingJSON is a pending publication of session "owned" carrying a
// sourceBytes-byte source and as many bytes of bundle records, with catalog
// work when commit is not empty.
func largePendingJSON(t *testing.T, sourceBytes int, commit string) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(`{"request_token":"owned",`)
	if commit != "" {
		b.WriteString(`"commit":` + commit + `,`)
	}
	b.WriteString(`"bundle":{"schema_version":2,"native_records":[`)
	text := strings.Repeat("x", 1<<10)
	for i := range sourceBytes >> 10 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"response_item","payload":{"text":"` + text + `\"quoted\"\\"}}`)
	}
	b.WriteString(`],"archive_session_id":"owned"},"source_bytes":"`)
	b.WriteString(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, sourceBytes)))
	b.WriteString(`","metadata_bytes":"e30=","ready_at":"2026-10-15T00:00:00Z"}`)
	if !json.Valid(b.Bytes()) {
		t.Fatal("fixture is not JSON")
	}
	return b.Bytes()
}

// The pending fences read only what they check (the proofs and the catalog
// commit a pending publication carries), so their charge is a read buffer,
// not twice the pending file: a long thread's pending publication, which
// carries its whole source and records, no longer needs read memory for a
// second copy of itself to be saved or replaced.
func TestPendingFenceChargesABufferNotThePendingFile(t *testing.T) {
	raw := largePendingJSON(t, 4<<20, "")
	for _, check := range []catalogFenceCheck{catalogFenceSave, catalogFenceRemoval} {
		t.Run(string(check), func(t *testing.T) {
			s := newTestStore(t)
			if err := local.WriteBytes(s.pendingPath("owned"), raw); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(256 << 10)
			scoped, closeScope := s.WithReadBudget(t.Context(), budget)
			defer closeScope()
			if err := checkCatalogFence(t, scoped, check, PendingPublication{}); err != nil {
				t.Fatalf("fence over a %d-byte pending file: %v", len(raw), err)
			}
			used, peak := budget.Charged()
			if used != 0 || peak > pendingFenceBuffer+(4<<10) {
				t.Fatalf("fence charged %d at peak (%d still held) for a %d-byte pending file", peak, used, len(raw))
			}
		})
	}
}

// Catalog work a large pending publication carries is still seen: its
// removal is refused, and replacing it reads the whole publication to
// compare with.
func TestPendingFenceStillSeesRecoveredCatalogWork(t *testing.T) {
	commit := `{"protocol":11,"id":"mutation","expected_revision":"r1","recovery":{"protocol":11,"owner":"` + strings.Repeat("ab", 16) + `","origin":"` + strings.Repeat("cd", 32) + `","destination":"d","session_id":"owned","mutation_id":"mutation","expected_revision":"r1","sha256":"` + strings.Repeat("ef", 32) + `"}}`
	s := newTestStore(t)
	if err := local.WriteBytes(s.pendingPath("owned"), largePendingJSON(t, 64<<10, commit)); err != nil {
		t.Fatal(err)
	}
	if err := s.checkCatalogPendingRemoval("owned"); !errors.Is(err, ErrCatalogJournalFrozen) {
		t.Fatalf("removal of recovered catalog work: %v", err)
	}
	if err := s.checkCatalogPendingSave(s.durableContext(), "owned", PendingPublication{}); !errors.Is(err, ErrCatalogJournalFrozen) {
		t.Fatalf("replacement of recovered catalog work by other work: %v", err)
	}
}

// The fence view reads exactly what decoding the whole publication would
// put in its fields, whatever the members' order, spelling (a field name
// matches ignoring case, as encoding/json matches it) or escapes, and
// refuses a publication that is not JSON or that a whole decode refuses for
// carrying a future writer's fields.
func TestPendingFenceViewMatchesTheWholeDecode(t *testing.T) {
	raw := []byte(`{"source_bytes":"AAAA","Bundle":{"native_records":[{"a":[1,2.5e3,-3,true,null,{"b":"\"}"}]}],"archive_session_id":"owned","x":{}},` +
		` "Commit" : {"protocol":11,"id":"m\"id","expected_revision":"r"},"metadata_bytes":"e30=","ready_at":"2026-10-15T00:00:00Z"}`)
	var whole PendingPublication
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	if err := local.WriteBytes(s.pendingPath("owned"), raw); err != nil {
		t.Fatal(err)
	}
	view, found, err := s.pendingFenceView("owned")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	want := PendingPublication{JournalVersion: whole.JournalVersion, Selection: whole.Selection, StagedSelection: whole.StagedSelection, CursorSelection: whole.CursorSelection, Catalog: whole.Catalog, Bundle: archive.SourceBundle{ArchiveSessionID: whole.Bundle.ArchiveSessionID}}
	if !reflect.DeepEqual(view, want) || view.Bundle.ArchiveSessionID != "owned" || view.Catalog == nil || view.Catalog.ID != `m"id` {
		t.Fatalf("view %+v, want %+v", view, want)
	}
	for _, broken := range []string{`{"bundle":{},"phase":{"step":1}}`, `{"commit":{"id":"x"}`, `{"commit" {"id":"x"}}`, `{"a":"unterminated}`, `{"a":1} trailing`, `["not an object"]`} {
		if err := local.WriteBytes(s.pendingPath("owned"), []byte(broken)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.pendingFenceView("owned"); err == nil {
			t.Fatalf("read a fence view of %q", broken)
		}
	}
}
