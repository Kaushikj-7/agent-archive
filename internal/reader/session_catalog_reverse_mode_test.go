package reader

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Revisions are opaque and scoped to a destination: equality across stores
// does not prove that their metadata bodies are interchangeable.
type catalogReverseOpaqueStore struct {
	*countingStore
	revision string
}

func (s *catalogReverseOpaqueStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	body, err := s.Get(ctx, key)
	return body, s.revision, err
}

// This fixture qualifies only its private synthetic remote store, never S3/R2.
func TestCatalogLegacyRemoteLegacyRebuildsEqualOpaqueRevision(t *testing.T) {
	remote, legacy := remoteReaderFixture(t, 1)
	const key = "sessions/claude/session-0000/metadata.json"
	snapshot, err := catalog.OpenSnapshot(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := snapshot.Find(t.Context(), key)
	if err != nil || entry == nil || entry.Revision == "" {
		t.Fatalf("remote fixture revision: %+v %v", entry, err)
	}
	body, err := legacy.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	remoteTitle := metadata.Title
	metadata.Title = "different canonical destination"
	body, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	canonical := &catalogReverseOpaqueStore{countingStore: newCountingStore(), revision: entry.Revision}
	if err = canonical.Put(t.Context(), key, body); err != nil {
		t.Fatal(err)
	}
	headers := HeaderSnapshot{CanonicalComplete: true, Canonical: []storage.Object{{Key: key, ETag: entry.Revision}}}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := OpenSessionCatalog(t.Context(), cache, canonical, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err = first.Refresh(t.Context(), headers); err != nil {
		t.Fatal(err)
	}
	initial, err := first.Query(t.Context(), CatalogQuery{})
	if err != nil || !initial.Complete || initial.Total != 1 || len(initial.Rows) != 1 || initial.Rows[0].Key != key || initial.Rows[0].ETag != entry.Revision || initial.Rows[0].Summary.Title != metadata.Title {
		t.Fatalf("initial canonical universe: %+v %v", initial, err)
	}
	var initialNamespace string
	if err = first.db.QueryRowContext(t.Context(), "SELECT namespace FROM catalog_state WHERE id=1").Scan(&initialNamespace); err != nil || initialNamespace == "" {
		t.Fatalf("initial canonical provenance: %q %v", initialNamespace, err)
	}
	middle, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = middle.Close() }()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = middle.RefreshRemote(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled remote refresh: %v", err)
	}
	var retainedNamespace string
	if err = first.db.QueryRowContext(t.Context(), "SELECT namespace FROM catalog_state WHERE id=1").Scan(&retainedNamespace); err != nil || retainedNamespace != initialNamespace {
		t.Fatalf("refused refresh changed canonical provenance: %q %v", retainedNamespace, err)
	}
	if _, err = first.Query(t.Context(), CatalogQuery{}); err != nil {
		t.Fatalf("refused refresh invalidated canonical handle: %v", err)
	}
	if err = middle.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	remotePage, err := middle.Query(t.Context(), CatalogQuery{})
	if err != nil || !remotePage.Complete || remotePage.Total != 1 || len(remotePage.Rows) != 1 || remotePage.Rows[0].Key != key || remotePage.Rows[0].ETag != entry.Revision || remotePage.Rows[0].Summary.Title != remoteTitle {
		t.Fatalf("remote equal-revision universe: %+v %v", remotePage, err)
	}
	if _, err = first.Query(t.Context(), CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("old canonical handle retained authority: %v", err)
	}
	last, err := OpenSessionCatalog(t.Context(), cache, canonical, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = last.Close() }()
	if err = last.Refresh(t.Context(), headers); err != nil {
		t.Fatal(err)
	}
	current, err := last.Query(t.Context(), CatalogQuery{})
	if err != nil || !current.Complete || current.Total != 1 || len(current.Rows) != 1 || current.Rows[0].Key != key || current.Rows[0].ETag != entry.Revision || current.Rows[0].Summary.Title != metadata.Title {
		t.Fatalf("remote rows exposed under equal canonical revision: %+v %v", current, err)
	}
	if _, err = middle.Query(t.Context(), CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("old remote handle retained authority: %v", err)
	}
	canonical.reset()
	if err = last.Refresh(t.Context(), headers); err != nil {
		t.Fatal(err)
	}
	canonical.mu.Lock()
	warmGets := len(canonical.gets)
	canonical.mu.Unlock()
	if warmGets != 0 {
		t.Fatalf("unchanged canonical refresh read %d metadata bodies", warmGets)
	}
	warm, err := last.Query(t.Context(), CatalogQuery{})
	if err != nil || !warm.Complete || warm.Total != 1 || len(warm.Rows) != 1 || warm.Rows[0].Key != key || warm.Rows[0].ETag != entry.Revision || warm.Rows[0].Summary.Title != metadata.Title {
		t.Fatalf("warm canonical universe: %+v %v", warm, err)
	}
}
