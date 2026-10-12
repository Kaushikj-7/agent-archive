package reader

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestRemoteSQLContinuationRejectsChangedHeadWithoutLocalRefresh(t *testing.T) {
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
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	q := CatalogQuery{Words: []string{"résumé"}, Metadata: MetadataQuery{Limit: 1}}
	page, err := c.Query(t.Context(), q)
	if err != nil || page.Next == "" || len(page.Rows) != 1 {
		t.Fatal("first SQL page", page, err)
	}
	if err = remote.DeleteSession(t.Context(), page.Rows[0].Key); err != nil {
		t.Fatal(err)
	}
	q.Cursor = page.Next
	if _, err = c.Query(t.Context(), q); !errors.Is(err, catalog.ErrStaleCursor) && !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatal("SQL continuation accepted changed remote root", err)
	}
}

func TestRemoteRefreshEvictsOnlyProvenDeletedBodyCache(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "delta", true: "rebuild"}[rebuild], func(t *testing.T) {
			remote, legacy := remoteReaderFixture(t, 8)
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			page, err := c.Query(t.Context(), CatalogQuery{})
			if err != nil || len(page.Rows) < 2 {
				t.Fatal(err)
			}
			deleted, live := page.Rows[0], page.Rows[1]
			for _, row := range []CatalogRow{deleted, live} {
				raw, e := legacy.Get(t.Context(), row.Key)
				if e != nil {
					t.Fatal(e)
				}
				cache.putVerified(row.Key, row.ETag, raw)
			}
			goneDir, ok := cache.keyDir(deleted.Key)
			if !ok {
				t.Fatal("uncacheable fixture")
			}
			liveDir, _ := cache.keyDir(live.Key)
			otherKey := "other/claude/private/metadata.json"
			cache.putVerified(otherKey, "private", []byte(`{}`))
			otherDir, _ := cache.keyDir(otherKey)
			if err = remote.DeleteSession(t.Context(), deleted.Key); err != nil {
				t.Fatal(err)
			}
			if rebuild {
				if _, err = c.db.ExecContext(t.Context(), "DELETE FROM remote_root"); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(goneDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("verified deletion retained body directory", err)
			}
			if _, err = os.Stat(liveDir); err != nil {
				t.Fatal("live body evicted", err)
			}
			if _, err = os.Stat(otherDir); err != nil {
				t.Fatal("unrelated prefix evicted", err)
			}
		})
	}
}

func TestRemoteSQLCursorBindsRefreshedRequestAndCancellation(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 8)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err = first.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	q := CatalogQuery{Metadata: MetadataQuery{Limit: 1}}
	page, err := first.Query(t.Context(), q)
	if err != nil || page.Next == "" {
		t.Fatal(page, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = first.Query(canceled, q); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled query accepted", err)
	}
	second, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err = second.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	q.Cursor = page.Next
	if _, err = second.Query(t.Context(), q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatal("identical-root renewed request accepted old cursor", err)
	}
	if err = first.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Query(t.Context(), q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatal("same-handle refresh accepted old request", err)
	}
}

type cacheMismatchWriteStore struct {
	*catalog.Store
	writes int
}

func (s *cacheMismatchWriteStore) Put(ctx context.Context, key string, raw []byte) error {
	s.writes++
	return s.Store.Put(ctx, key, raw)
}

func (s *cacheMismatchWriteStore) PutConditional(ctx context.Context, key string, raw []byte, c storage.PutCondition) (string, error) {
	s.writes++
	return s.Store.PutConditional(ctx, key, raw, c)
}

func TestRemoteCatalogRefusesUnrelatedBodyCacheBeforeWork(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 8)
	bound, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attempts := &cacheMismatchWriteStore{Store: remote}
	measured := storagetest.NewMeasuredStore(attempts, 0)
	if c, err := OpenSessionCatalog(t.Context(), bound, measured, ListOptions{Cache: other}); err == nil {
		_ = c.Close()
		t.Fatal("unrelated cache accepted")
	}
	if counts := measured.Metrics(); counts.Gets != 0 || counts.Lists != 0 || attempts.writes != 0 {
		t.Fatal("cache mismatch touched provider", counts)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(bound.dir), "catalog")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cache mismatch created index", err)
	}
	entries, err := os.ReadDir(other.dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("other cache changed", entries, err)
	}
	// Legacy callers retain their independent cache arrangement.
	_, legacy := remoteReaderFixture(t, 1)
	c, err := OpenSessionCatalog(t.Context(), bound, legacy, ListOptions{Cache: other})
	if err != nil {
		t.Fatal("legacy cache arrangement changed", err)
	}
	_ = c.Close()
}

func TestRemoteCompleteRefreshRetriesSkippedEvictionAndPreservesRecreatedLiveKey(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-root-retry", true: "recreated-live"}[recreate], func(t *testing.T) {
			remote, legacy := remoteReaderFixture(t, 8)
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			page, err := c.Query(t.Context(), CatalogQuery{})
			if err != nil || len(page.Rows) == 0 {
				t.Fatal(err)
			}
			row := page.Rows[0]
			raw, err := legacy.Get(t.Context(), row.Key)
			if err != nil {
				t.Fatal(err)
			}
			cache.putVerified(row.Key, row.ETag, raw)
			dir, _ := cache.keyDir(row.Key)
			if err = remote.DeleteSession(t.Context(), row.Key); err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(dir, 0500); err != nil {
				t.Fatal(err)
			}
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(dir); err != nil {
				t.Fatal("unsafe directory should be left untouched", err)
			}
			page, err = c.Query(t.Context(), CatalogQuery{})
			if err != nil || page.Total != 7 {
				t.Fatal("SQL deletion not committed", page.Total, err)
			}
			root, generation := c.remoteSnapshot.Root(), c.viewGeneration
			if err = os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if recreate {
				var metadata archive.Metadata
				if err = json.Unmarshal(raw, &metadata); err != nil {
					t.Fatal(err)
				}
				ref, err := remote.Writer.PutImmutable(t.Context(), catalog.KindMetadata, raw)
				if err != nil {
					t.Fatal(err)
				}
				_, revision, err := remote.Writer.Find(t.Context(), row.Key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = remote.Writer.Commit(t.Context(), catalog.CatalogMutation{ID: "private-recreated-cache", SessionKey: row.Key, ExpectedRevision: revision, Next: &catalog.CatalogEntry{Metadata: ref, Summary: metadata}}); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(dir)
			if recreate {
				if err != nil {
					t.Fatal("recreated live cache deleted by old absence", err)
				}
			} else {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("same-root proof failed to retry cache eviction", err)
				}
				if c.remoteSnapshot.Root() != root || c.viewGeneration != generation {
					t.Fatal("retry did not use unchanged-root empty delta")
				}
			}
		})
	}
}
