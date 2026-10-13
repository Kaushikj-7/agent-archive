package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
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

func TestOrdinaryPublicationFormatRefusalPreservesAdmissionAndObjects(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, head := range []string{
			`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}}`,
			`{"Protocol":11,"Schema":4}`,
			`{"Protocol":12,"Schema":6}`,
		} {
			t.Run(fmt.Sprintf("retry=%v/%s", retry, head), func(t *testing.T) {
				_, provider := fixture(t)
				store, err := Wrap(provider)
				if err != nil {
					t.Fatal(err)
				}
				source := []byte("synthetic new publication")
				key := "sessions/claude/new/source." + storage.SHA256Hex(source) + ".jsonl.gz"
				metadata, err := json.Marshal(archive.Metadata{SchemaVersion: 1, SessionID: "new", Harness: archive.Harness{Name: "claude"}, SourceBundle: archive.SourceReference{Key: key, SHA256: storage.SHA256Hex(source), CompressedBytes: len(source)}})
				if err != nil {
					t.Fatal(err)
				}
				if retry {
					if _, err := store.BeginPublication(t.Context(), "new", metadata); err != nil {
						t.Fatal(err)
					}
					store.EndPublicationAttempt("new")
				}
				before, beforeErr := provider.Get(t.Context(), CoordinatorKey)
				if beforeErr != nil && !errors.Is(beforeErr, storage.ErrNotFound) {
					t.Fatal(beforeErr)
				}
				if err := provider.Put(t.Context(), HeadKey, []byte(head)); err != nil {
					t.Fatal(err)
				}
				ctx, err := store.BeginPublication(t.Context(), "new", metadata)
				if err == nil {
					// Positively exercise the vulnerable claim rather than merely inspect
					// a decoder error: without the entrypoint gate this writes a source.
					if err := store.Put(ctx, key, source); err != nil {
						t.Fatal(err)
					}
					t.Fatal("unsupported head admitted a publication and source write")
				}
				after, afterErr := provider.Get(t.Context(), CoordinatorKey)
				if string(after) != string(before) || errors.Is(afterErr, storage.ErrNotFound) != errors.Is(beforeErr, storage.ErrNotFound) {
					t.Fatal("refusal changed durable admission", afterErr)
				}
				for _, prefix := range []string{"sessions/", "catalog-v4/metadata/", "catalog-v4/nodes/"} {
					objects, err := provider.List(t.Context(), prefix)
					if err != nil || len(objects) != 0 {
						t.Fatalf("refusal left protected objects: %s %+v %v", prefix, objects, err)
					}
				}
				if store.running["new"] || store.claims["new"] != nil {
					t.Fatal("refusal granted local execution authority")
				}
				// Restoring the old format is only a synthetic positive control: real
				// upgrades must preserve their permanent fence.
				if err := provider.Delete(t.Context(), HeadKey); err != nil {
					t.Fatal(err)
				}
				ctx, err = store.BeginPublication(t.Context(), "new", metadata)
				if err != nil {
					t.Fatal("legacy publication refused", err)
				}
				defer store.EndPublicationAttempt("new")
				if err := store.Put(ctx, key, source); err != nil {
					t.Fatal(err)
				}
				if err := store.Publication("new", "sessions/claude/new/metadata.json", "").Put(ctx, "sessions/claude/new/metadata.json", metadata); err != nil {
					t.Fatal(err)
				}
				if err := store.CompletePublication(ctx, "new", metadata); err != nil {
					t.Fatal(err)
				}
				if _, err := provider.Get(t.Context(), key); err != nil {
					t.Fatal("legacy source missing", err)
				}
			})
		}
	}
}

func TestReservedHeadRefusesFreshAndTransferredJournalWithoutChangingOwner(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		t.Run(fmt.Sprintf("transfer=%v", transfer), func(t *testing.T) {
			store, provider, localStore, pending, guard, _ := journalFixture(t)
			journal := *pending.Catalog.Recovery
			if transfer {
				if _, _, err := store.BeginJournalPublication(t.Context(), journal.MutationID, pending.MetadataBytes, journal, guard); err != nil {
					t.Fatal(err)
				}
				store.EndPublicationAttempt(journal.MutationID)
			}
			before, beforeErr := provider.Get(t.Context(), CoordinatorKey)
			if beforeErr != nil && !errors.Is(beforeErr, storage.ErrNotFound) {
				t.Fatal(beforeErr)
			}
			if err := provider.Put(t.Context(), HeadKey, []byte(`{"Protocol":11,"Schema":5,"Picker":{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}}`)); err != nil {
				t.Fatal(err)
			}
			if _, settled, err := store.BeginJournalPublication(t.Context(), journal.MutationID, pending.MetadataBytes, journal, guard); !errors.Is(err, errScopedPickerUnavailable) || settled {
				t.Fatal("reserved journal admitted", settled, err)
			}
			after, afterErr := provider.Get(t.Context(), CoordinatorKey)
			if string(after) != string(before) || errors.Is(afterErr, storage.ErrNotFound) != errors.Is(beforeErr, storage.ErrNotFound) {
				t.Fatal("refusal changed durable journal admission", afterErr)
			}
			if err := localStore.VerifyCatalogJournal("journal", pending, guard, journal.Destination); err != nil {
				t.Fatal("refusal damaged frozen journal", err)
			}
			if store.running[journal.MutationID] || store.claims[journal.MutationID] != nil {
				t.Fatal("refusal granted journal execution")
			}
		})
	}
}
