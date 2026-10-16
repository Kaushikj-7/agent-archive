package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestCatalogRetiresAbsentSummaryDespiteUnrelatedDecodeFailure(t *testing.T) {
	ctx := t.Context()
	store := newCountingStore()
	absent := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	broken := putSession(t, store, "codex", fmt.Sprintf("%032x", 2), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(ctx, absent); err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, broken, []byte("invalid metadata")); err != nil {
		t.Fatal(err)
	}
	headers, err = DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = c.Refresh(ctx, headers); err == nil {
			t.Fatal("invalid metadata accepted")
		}
		var retained int
		if err = c.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE key=?", absent).Scan(&retained); err != nil {
			t.Fatal(err)
		}
		if retained != 0 {
			t.Fatalf("known-absent private summary retained: %d", retained)
		}
		page, e := c.Query(ctx, CatalogQuery{})
		if e != nil || page.Complete || len(page.Rows) != 0 {
			t.Fatalf("failed refresh published partial ready view: %+v %v", page, e)
		}
	}
}

// Opaque validators are meaningful only within one provider destination. This
// deliberately uses equal validators and canonical keys for distinct stores.
type catalogOpaqueStore struct{ *countingStore }

func (s *catalogOpaqueStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	body, err := s.Get(ctx, key)
	return body, "opaque-catalog-revision", err
}

func TestCatalogNamespacesDistinctStoresWithEqualOpaqueValidators(t *testing.T) {
	ctx := t.Context()
	a := &catalogOpaqueStore{newCountingStore()}
	b := &catalogOpaqueStore{newCountingStore()}
	key := putSession(t, a.countingStore, "codex", fmt.Sprintf("%032x", 1), baseTime)
	putSession(t, b.countingStore, "codex", fmt.Sprintf("%032x", 1), baseTime)
	body, err := b.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Title = "distinct second destination"
	body, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := OpenSessionCatalog(ctx, cache, a, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	headers := HeaderSnapshot{CanonicalComplete: true, Canonical: []storage.Object{{Key: key, ETag: "opaque-catalog-revision"}}}
	if err = first.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSessionCatalog(ctx, cache, b, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err = second.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	page, err := second.Query(ctx, CatalogQuery{})
	if err != nil || !page.Complete || len(page.Rows) != 1 || page.Rows[0].Summary.Title != metadata.Title {
		t.Fatalf("cross-destination reuse: %+v %v", page, err)
	}
	if _, err = first.Query(ctx, CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("old destination view remains valid: %v", err)
	}
	b.reset()
	if err = second.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	warmGets := len(b.gets)
	b.mu.Unlock()
	if warmGets != 0 {
		t.Fatalf("warm unchanged namespace read %d bodies", warmGets)
	}
	page, err = second.Query(ctx, CatalogQuery{})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Summary.Title != metadata.Title {
		t.Fatalf("warm second destination: %+v %v", page, err)
	}
}

func TestCatalogIncompleteDiscoveryCannotRetirePrivateRows(t *testing.T) {
	ctx := t.Context()
	store := newCountingStore()
	key := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []HeaderSnapshot{{}, {Canonical: headers.Canonical}} {
		if err = c.Refresh(ctx, snapshot); err == nil {
			t.Fatal("incomplete authority accepted")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = c.Refresh(canceled, HeaderSnapshot{CanonicalComplete: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	var retained int
	if err = c.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE key=?", key).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("unproven deletion committed: count=%d err=%v", retained, err)
	}
}
