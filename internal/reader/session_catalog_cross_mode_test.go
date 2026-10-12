package reader

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
)

type catalogCrossModeIdentity string

const (
	catalogCrossModeSameKey      catalogCrossModeIdentity = "same_key"
	catalogCrossModeDifferentKey catalogCrossModeIdentity = "different_key"
)

// The private SQLite cache is shared by the two functional reader modes.
// Equal row counts and valid self-checksums do not establish remote provenance.
// The remote fixture qualifies only its private synthetic store, never S3/R2.
func TestCatalogRemoteLegacyRemoteRebuildsUnchangedRoot(t *testing.T) {
	for _, identity := range []catalogCrossModeIdentity{catalogCrossModeSameKey, catalogCrossModeDifferentKey} {
		t.Run(string(identity), func(t *testing.T) {
			remote, legacy := remoteReaderFixture(t, 1)
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			first, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = first.Close() }()
			if err = first.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			original, err := first.Query(t.Context(), CatalogQuery{})
			if err != nil || !original.Complete || original.Total != 1 || len(original.Rows) != 1 {
				t.Fatalf("initial remote universe: %+v %v", original, err)
			}
			root := first.remoteSnapshot.Root()
			key := original.Rows[0].Key
			body, err := legacy.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err = json.Unmarshal(body, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.Title = "different legacy destination"
			if identity == catalogCrossModeDifferentKey {
				if err = legacy.Delete(t.Context(), key); err != nil {
					t.Fatal(err)
				}
				metadata.SessionID = "legacy-other"
				key, err = archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
				if err != nil {
					t.Fatal(err)
				}
			}
			body, err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err = legacy.Put(t.Context(), key, body); err != nil {
				t.Fatal(err)
			}
			middle, err := OpenSessionCatalog(t.Context(), cache, legacy, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = middle.Close() }()
			if err = middle.Refresh(t.Context(), HeaderSnapshot{}); err == nil {
				t.Fatal("incomplete canonical discovery accepted")
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if err = middle.Refresh(canceled, HeaderSnapshot{CanonicalComplete: true}); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled canonical discovery: %v", err)
			}
			var retained int
			if err = first.db.QueryRowContext(t.Context(), "SELECT count(*) FROM remote_root WHERE id=1").Scan(&retained); err != nil || retained != 1 {
				t.Fatalf("refused refresh changed remote provenance: %d %v", retained, err)
			}
			headers, err := DiscoverCatalogHeaders(t.Context(), legacy, ListOptions{Cache: cache})
			if err != nil {
				t.Fatal(err)
			}
			if err = middle.Refresh(t.Context(), headers); err != nil {
				t.Fatal(err)
			}
			legacyPage, err := middle.Query(t.Context(), CatalogQuery{})
			if err != nil || !legacyPage.Complete || legacyPage.Total != 1 || len(legacyPage.Rows) != 1 || legacyPage.Rows[0].Key != key || legacyPage.Rows[0].Summary.Title != metadata.Title {
				t.Fatalf("legacy equal-count universe: %+v %v", legacyPage, err)
			}
			if _, err = first.Query(t.Context(), CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
				t.Fatalf("old remote handle retained authority: %v", err)
			}
			var bodyReads int
			last, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{BodyRead: func(string, bool) { bodyReads++ }})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = last.Close() }()
			// A required-root request is reuse-only: it cannot silently repair a
			// legacy universe merely because its count equals the remote count.
			if reused, err := last.refreshRemote(t.Context(), &root); err != nil || reused {
				t.Fatalf("legacy rows authorized unchanged-root reuse: reused=%v err=%v", reused, err)
			}
			if err = last.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, err := last.Query(t.Context(), CatalogQuery{})
			if err != nil || !current.Complete || current.Total != 1 || len(current.Rows) != 1 || current.Rows[0].Key != original.Rows[0].Key || current.Rows[0].ETag != original.Rows[0].ETag || current.Rows[0].Summary.Title != original.Rows[0].Summary.Title {
				t.Fatalf("legacy rows exposed under unchanged remote root: %+v %v", current, err)
			}
			if last.remoteSnapshot.Root() != root {
				t.Fatal("fixture remote root changed")
			}
			if _, err = middle.Query(t.Context(), CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
				t.Fatalf("old legacy handle retained authority: %v", err)
			}
			bodyReads = 0
			if err = last.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			if bodyReads != 0 {
				t.Fatalf("unchanged remote refresh read %d metadata bodies", bodyReads)
			}
			var persisted catalog.ObjectRef
			var raw []byte
			if err = last.db.QueryRowContext(t.Context(), "SELECT root FROM remote_root WHERE id=1").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &persisted); err != nil || persisted != root {
				t.Fatalf("rebuilt universe lacks exact remote provenance: %+v %v", persisted, err)
			}
		})
	}
}
