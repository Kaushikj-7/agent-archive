package storagetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestMemoryStoreClonesKeepBodiesVersionsAndMutationsPrivate(t *testing.T) {
	ctx := t.Context()
	source := NewMemoryStore()
	input := []byte("immutable seed")
	if err := source.Put(ctx, "body", input); err != nil {
		t.Fatal(err)
	}
	input[0] = '!'
	first, err := CloneMemoryStore(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CloneMemoryStore(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == source {
		t.Fatal("clones share stores")
	}
	original, version, err := source.GetCatalogVersion(ctx, "body", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, clone := range []*MemoryStore{first, second} {
		got, v, err := clone.GetCatalogVersion(ctx, "body", 100)
		if err != nil || !bytes.Equal(got, original) || v != version {
			t.Fatal("clone changed bytes or exact version", v, version, err)
		}
		listings, err := clone.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		want, err := source.List(ctx, "")
		if err != nil || !reflect.DeepEqual(listings, want) {
			t.Fatal("clone changed listing validators or timestamps", err)
		}
		getters := []func() ([]byte, error){
			func() ([]byte, error) { return clone.Get(ctx, "body") },
			func() ([]byte, error) { return clone.GetLimited(ctx, "body", 100) },
			func() ([]byte, error) { b, _, e := clone.GetVersioned(ctx, "body"); return b, e },
			func() ([]byte, error) { b, _, e := clone.GetLimitedVersioned(ctx, "body", 100); return b, e },
			func() ([]byte, error) { b, _, e := clone.GetCatalogVersion(ctx, "body", 100); return b, e },
		}
		for _, get := range getters {
			raw, err := get()
			if err != nil {
				t.Fatal(err)
			}
			raw[0] = '!'
		}
	}
	replacement := []byte("private write")
	if _, err := first.PutConditional(ctx, "body", replacement, storage.PutCondition{MatchETag: version.ETag}); err != nil {
		t.Fatal(err)
	}
	replacement[0] = '!'
	if err := first.Put(ctx, "new", []byte("private key")); err != nil {
		t.Fatal(err)
	}
	if err := second.Delete(ctx, "body"); err != nil {
		t.Fatal(err)
	}
	raw, v, err := source.GetCatalogVersion(ctx, "body", 100)
	if err != nil || !bytes.Equal(raw, original) || v != version {
		t.Fatal("mutation changed the source", err)
	}
	if _, err := source.Get(ctx, "new"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("new key escaped its clone", err)
	}
	raw, err = first.Get(ctx, "body")
	if err != nil || string(raw) != "private write" {
		t.Fatal("conditional input or sibling deletion changed a clone", err)
	}
	if _, err := first.PutConditional(ctx, "body", []byte("stale"), storage.PutCondition{MatchETag: version.ETag}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatal("clone lost private CAS state", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if clone, err := CloneMemoryStore(canceled, source); clone != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled clone returned state", err)
	}
	for i := range 4 {
		t.Run(fmt.Sprintf("concurrent-%d", i), func(t *testing.T) {
			t.Parallel()
			private, err := CloneMemoryStore(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if err := private.Put(t.Context(), "body", []byte("private")); err != nil {
				t.Fatal(err)
			}
			raw, err := source.Get(t.Context(), "body")
			if err != nil || !bytes.Equal(raw, original) {
				t.Fatal("concurrent mutation escaped its clone", err)
			}
		})
	}
}

func TestMemorySnapshotSurvivesSourceAndCloneMutation(t *testing.T) {
	ctx := t.Context()
	source := NewMemoryStore()
	if err := source.Put(ctx, "body", []byte("seed")); err != nil {
		t.Fatal(err)
	}
	original, version, err := source.GetCatalogVersion(ctx, "body", 100)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotMemoryStore(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Put(ctx, "body", []byte("new source")); err != nil {
		t.Fatal(err)
	}
	if err := source.Put(ctx, "new", []byte("new source key")); err != nil {
		t.Fatal(err)
	}
	first, err := snapshot.Clone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, v, err := first.GetCatalogVersion(ctx, "body", 100)
	if err != nil || !bytes.Equal(got, original) || v != version {
		t.Fatal("source mutation changed immutable snapshot", err)
	}
	if _, err := first.Get(ctx, "new"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("source key escaped into snapshot", err)
	}
	if err := first.Delete(ctx, "body"); err != nil {
		t.Fatal(err)
	}
	second, err := snapshot.Clone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, v, err = second.GetCatalogVersion(ctx, "body", 100)
	if err != nil || !bytes.Equal(got, original) || v != version {
		t.Fatal("clone mutation changed immutable snapshot", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if clone, err := snapshot.Clone(canceled); clone != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled snapshot clone returned state", err)
	}
	if _, err := SnapshotMemoryStore(canceled, source); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled snapshot ignored context", err)
	}
}
