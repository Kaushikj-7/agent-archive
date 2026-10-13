package catalog

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestScopedHeadRefusesBeforePublicationWrites(t *testing.T) {
	w, store := fixture(t)
	raw := []byte(`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}}`)
	if err := store.Put(t.Context(), HeadKey, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Head(t.Context()); !errors.Is(err, errScopedPickerUnavailable) {
		t.Fatal(err)
	}
	if _, err := w.PutImmutable(t.Context(), KindMetadata, []byte(`{}`)); !errors.Is(err, errScopedPickerUnavailable) {
		t.Fatal(err)
	}
	s := &Store{ObjectStore: store, Writer: w}
	if _, _, err := s.BeginJournalPublication(t.Context(), "synthetic", nil, local.CatalogJournal{}, nil); !errors.Is(err, errScopedPickerUnavailable) {
		t.Fatal(err)
	}
	key := "sessions/claude/synthetic/source.jsonl.gz"
	if err := s.Put(t.Context(), key, []byte("synthetic")); !errors.Is(err, errScopedPickerUnavailable) {
		t.Fatal(err)
	}
	for _, prefix := range []string{"catalog-v4/metadata/", "catalog-v4/nodes/", "sessions/", CoordinatorKey} {
		objects, err := store.List(t.Context(), prefix)
		if err != nil || len(objects) != 0 {
			t.Fatalf("writes before refusal: %s %+v %v", prefix, objects, err)
		}
	}
	if _, err := w.Commit(t.Context(), CatalogMutation{ID: "synthetic", SessionKey: "sessions/claude/synthetic/metadata.json"}); !errors.Is(err, errScopedPickerUnavailable) {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), CoordinatorKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestHeadProtocolRejectsMixedAndUnknownDescriptors(t *testing.T) {
	for _, raw := range []string{
		`{"Protocol":10,"Schema":4,"Picker":null}`,
		`{"Protocol":10,"Schema":4,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}}`,
		`{"Protocol":11,"Schema":4}`,
		`{"Protocol":10,"Schema":5}`,
		`{"Protocol":11,"Schema":5}`,
		`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":2,"Capabilities":["scoped-picker-v1"]}}`,
		`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":[]}}`,
		`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1","unknown"]}}`,
		`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["unknown"]}}`,
		`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"],"Complete":true}}`,
		`{"Protocol":12,"Schema":6}`,
		`{"Protocol":10,"Schema":4,"Unknown":true}`,
		`{"Protocol":10,"Schema":4} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			h := CatalogHead{Generation: 42}
			if err := json.Unmarshal([]byte(raw), &h); err == nil {
				t.Fatal("accepted invalid head")
			}
			if h.Generation != 42 {
				t.Fatal("failed decoding changed receiver")
			}
		})
	}
}

func TestLegacyHeadCodecRetainsExactWire(t *testing.T) {
	h := CatalogHead{Protocol: 10, Schema: 4, Generation: 1, Epoch: "synthetic"}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var next CatalogHead
	if err = json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(next)
	if err != nil || string(again) != string(raw) {
		t.Fatalf("legacy head changed: %s %s %v", raw, again, err)
	}
}

func TestScopedCoordinatorRefusesSourceAndIndexAdmissionWithLegacyHead(t *testing.T) {
	for _, raw := range []string{
		`{"Protocol":11,"Generation":1,"Mode":"candidate","Owners":{}}`,
		`{"Protocol":11,"Generation":1,"Mode":"candidate","Owners":{},"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}}`,
		`{"Protocol":10,"Generation":1,"Mode":"candidate","Owners":{},"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			w, store := fixture(t)
			if err := store.Put(t.Context(), CoordinatorKey, []byte(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.PutImmutable(t.Context(), KindMetadata, []byte(`{}`)); err == nil {
				t.Fatal("immutable write admitted")
			}
			s := &Store{ObjectStore: store, Writer: w}
			if err := s.Put(t.Context(), "sessions/claude/synthetic/source.jsonl.gz", []byte("synthetic")); err == nil {
				t.Fatal("source write admitted")
			}
			for _, prefix := range []string{"sessions/", "catalog-v4/metadata/", "catalog-v4/nodes/"} {
				objects, err := store.List(t.Context(), prefix)
				if err != nil || len(objects) != 0 {
					t.Fatalf("write before coordinator refusal: %s %v %v", prefix, objects, err)
				}
			}
		})
	}
}
