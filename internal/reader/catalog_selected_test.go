package reader

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type opaqueCatalogFixture struct{ *storagetest.MemoryStore }

func (s *opaqueCatalogFixture) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	objects, err := s.MemoryStore.List(ctx, prefix)
	for i := range objects {
		objects[i].ETag = "opaque/" + objects[i].ETag
	}
	return objects, err
}

func (s *opaqueCatalogFixture) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	raw, etag, err := s.MemoryStore.GetVersioned(ctx, key)
	return raw, "opaque/" + etag, err
}

func TestSelectedCatalogCacheUsesVerifiedOpaqueRevisionAndRefusesDamage(t *testing.T) {
	_, legacy := remoteReaderFixture(t, 8)
	source := storagetest.NewMeasuredStore(&opaqueCatalogFixture{legacy}, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(t.Context(), cache, source, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	headers, err := DiscoverCatalogHeaders(t.Context(), source, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(t.Context(), headers); err != nil {
		t.Fatal(err)
	}
	page, err := c.Query(t.Context(), CatalogQuery{})
	if err != nil || len(page.Rows) == 0 {
		t.Fatal(err, page)
	}
	row := page.Rows[0]
	source.Reset()
	got, found, err := c.ReadCachedMetadata(t.Context(), row.Key)
	if err != nil || !found {
		t.Fatal("opaque verified cache refused", found, err)
	}
	raw, err := legacy.Get(t.Context(), row.Key)
	if err != nil {
		t.Fatal(err)
	}
	var want archive.Metadata
	if err = json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Metadata, want) || got.Key != row.Key || source.Metrics().Gets != 0 {
		t.Fatal("selected cache authority/read count", got, source.Metrics())
	}
	cache.putVerified(row.Key, row.ETag, []byte(`{"schema_version":1,"session_id":"damaged"}`))
	if _, found, err = c.ReadCachedMetadata(t.Context(), row.Key); err != nil || found {
		t.Fatal("corrupt cached body became authority", found, err)
	}
	if source.Metrics().Gets != 0 {
		t.Fatal("cache miss performed remote read")
	}
	if _, err = ReadMetadata(t.Context(), source, row.Key); err != nil || source.Metrics().Gets != 1 {
		t.Fatal("cache corruption selected fallback", err, source.Metrics())
	}
	cache.putVerified(row.Key, row.ETag, raw)
	if _, err = c.db.ExecContext(t.Context(), "UPDATE sessions SET lowerid='damaged' WHERE key=?", row.Key); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.ReadCachedMetadata(t.Context(), row.Key); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatal("damaged tuple became body authority", err)
	}
}

func TestSelectedRemoteCacheRequiresPinnedViewAndCurrentGeneration(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 8)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := catalog.WithReadView(t.Context())
	if err = c.RefreshRemote(ctx); err != nil {
		t.Fatal(err)
	}
	selected, err := SelectMetadata(ctx, remote, "sessions", MetadataQuery{Limit: 1}, ListOptions{Cache: cache})
	if err != nil || len(selected.Sessions) != 1 {
		t.Fatal(err, selected)
	}
	metadata := selected.Sessions[0]
	key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := c.ReadCachedMetadata(ctx, key); err != nil || !found || !reflect.DeepEqual(got.Metadata, metadata) {
		t.Fatal("pinned body cache refused", found, err)
	}
	if _, _, err = c.ReadCachedMetadata(catalog.WithReadView(t.Context()), key); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatal("another view reused body authority", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = c.ReadCachedMetadata(canceled, key); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read returned body", err)
	}
	if err = remote.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	// The old captured view remains authoritative until another connection
	// replaces the local catalog's successful generation.
	if _, found, err := c.ReadCachedMetadata(ctx, key); err != nil || !found {
		t.Fatal("selected body silently switched roots", found, err)
	}
	replacement, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()
	if err = replacement.RefreshRemote(catalog.WithReadView(t.Context())); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.ReadCachedMetadata(ctx, key); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatal("replacement generation reused stale body", err)
	}
}
