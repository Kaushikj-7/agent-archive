package reader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type opaqueListingStore struct {
	*indexedCountingStore
	change func(string)
}

func (s *opaqueListingStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	objects, err := s.indexedCountingStore.List(ctx, prefix)
	for i := range objects {
		objects[i].ETag = "opaque/revision:" + objects[i].ETag
	}
	return objects, err
}

func (s *opaqueListingStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	info.ETag = "opaque/revision:" + info.ETag
	return info, err
}

func (s *opaqueListingStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	if s.change != nil {
		s.change(key)
	}
	data, etag, err := s.indexedCountingStore.GetVersioned(ctx, key)
	return data, "opaque/revision:" + etag, err
}

func newOpaqueListingStore() *opaqueListingStore {
	return &opaqueListingStore{indexedCountingStore: &indexedCountingStore{countingStore: newCountingStore()}}
}

// The body budget counts local cache opens as well as remote responses. Doubling
// archive size changes header work, never the selected-body work.
func TestListRevisionBodyBudgetColdWarmAndExhaustiveOracle(t *testing.T) {
	t.Parallel()
	sizes := []int{10000, 20000}
	if testing.Short() {
		// Still double the archive beyond a provider page and two timestamp
		// cycles, so selected bodies remain a small subset with tied dates.
		sizes = []int{1600, 3200}
	}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := newOpaqueListingStore()
			for i := range size {
				// Timestamp ties and backfills deliberately disagree with identity order.
				key := putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i%701)*time.Second))
				data, etag, err := store.GetVersioned(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				revision, err := listingindex.NewRevision(key, data, etag)
				if err != nil {
					t.Fatal(err)
				}
				if err = listingindex.PutRevision(ctx, store, revision); err != nil {
					t.Fatal(err)
				}
			}
			oracle, err := ListMetadataWithOptions(ctx, store, "sessions", Filter{}, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var physicalCacheReads atomic.Int64
			cache.readFile = func(path string) ([]byte, error) {
				physicalCacheReads.Add(1)
				return os.ReadFile(path)
			}
			for _, warm := range []bool{false, true} {
				physicalCacheReads.Store(0)
				store.reset()
				bodies, cached := 0, 0
				result, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{Cache: cache, BodyRead: func(_ string, hit bool) {
					bodies++
					if hit {
						cached++
					}
				}})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result.Sessions, oracle[:50]) || !result.Complete || result.TotalMatched != size {
					t.Fatal("indexed results disagree with exhaustive oracle")
				}
				_, gets := store.counts()
				wantGets := 50
				wantCached := 0
				if warm {
					wantGets = 0
					wantCached = 50
				}
				if bodies != 50 || cached != wantCached || len(gets) != wantGets || int(physicalCacheReads.Load())+len(gets) != 50 {
					t.Fatalf("warm=%v bodies=%d cached=%d physical cache reads=%d GETs=%d", warm, bodies, cached, physicalCacheReads.Load(), len(gets))
				}
				for _, key := range gets {
					if !isMetadataKey(key) {
						t.Fatalf("list downloaded source or index body: %s", key)
					}
				}
			}
		})
	}
}

func TestListRevisionCoverageDetectsMixedWritersAndStaleEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newOpaqueListingStore()
	first := putSession(t, store, "codex", "first", baseTime)
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", "old-writer", baseTime.Add(time.Hour))
	scans := 0
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scans++ }})
	if err != nil || scans != 1 || result.Sessions[0].SessionID != "old-writer" || result.TotalMatched != 2 {
		t.Fatalf("mixed writer omitted: scans=%d err=%v", scans, err)
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", "first", baseTime.Add(2*time.Hour))
	result, err = ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{})
	if err != nil || result.Sessions[0].SessionID != "first" {
		t.Fatal("stale revision concealed rewrite")
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	store.change = func(key string) {
		store.change = nil
		if key == first {
			putSession(t, store, "codex", "first", baseTime.Add(3*time.Hour))
		}
	}
	if _, err = ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{}); err == nil || !strings.Contains(err.Error(), "incomplete listing") {
		t.Fatalf("concurrent rewrite accepted: %v", err)
	}
}

func TestListRevisionActivityAndChildSummariesMatchExhaustive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newOpaqueListingStore()
	for i := range 7 {
		key := putSession(t, store, "codex", fmt.Sprintf("s%d", i), baseTime.Add(time.Duration(i)*time.Hour))
		data, err := store.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			m.ParentSessionID = "s3"
		}
		if i == 3 {
			ended := baseTime.Add(10 * time.Hour)
			m.EndedAt = &ended
		}
		data, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{ActivityOrder: true, TopLevelOnly: true}
	bounded, err := ListRecent(ctx, store, "sessions", Filter{}, 2, opts)
	if err != nil {
		t.Fatal(err)
	}
	full, err := ListRecent(ctx, store, "sessions", Filter{}, 0, opts)
	if err != nil {
		t.Fatal(err)
	}
	unlimited, err := SelectMetadata(ctx, store, "sessions", MetadataQuery{Order: ActivityOrder, TopLevelOnly: true}, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unlimited, full) {
		t.Fatalf("unlimited activity/child summary differs: got=%+v want=%+v", unlimited, full)
	}
	if !reflect.DeepEqual(bounded.Sessions, full.Sessions[:2]) || bounded.TotalMatched != 4 || bounded.Hidden != 3 || bounded.Children["codex/s3"] != 3 {
		t.Fatalf("summary parity failed: total=%d hidden=%d children=%v", bounded.TotalMatched, bounded.Hidden, bounded.Children)
	}
}

// A writer that passed its validator check must not delete an entry created
// after that check by another completed publication.
type cleanupInterleaveStore struct {
	*storagetest.MemoryStore
	afterStat func()
}

func (s *cleanupInterleaveStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	if s.afterStat != nil {
		callback := s.afterStat
		s.afterStat = nil
		callback()
	}
	return info, err
}

func TestListingCleanupPreservesConcurrentCompletedPublication(t *testing.T) {
	ctx := context.Background()
	store := &cleanupInterleaveStore{MemoryStore: storagetest.NewMemoryStore()}
	key := putSession(t, store, "codex", "concurrent", baseTime)
	old, _, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	store.afterStat = func() {
		prior, _, err := store.GetVersioned(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		var metadata archive.Metadata
		if err := json.Unmarshal(prior, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata.CapturedAt = baseTime.Add(time.Hour)
		replacement, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, replacement); err != nil {
			t.Fatal(err)
		}
		current, _, err := store.GetVersioned(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = listingindex.PublishRevision(ctx, store.MemoryStore, key, current); err != nil {
			t.Fatal(err)
		}
	}
	if err := listingindex.PublishRevision(ctx, store, key, old); err != nil {
		t.Fatal(err)
	}
	scanned := false
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }})
	if err != nil || scanned || len(result.Sessions) != 1 || !result.Sessions[0].CapturedAt.Equal(baseTime.Add(time.Hour)) {
		t.Fatalf("completed concurrent index lost: scanned=%v result=%+v err=%v", scanned, result, err)
	}
}

func TestListSelectedDeletionAndCorruptionDoNotRefill(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%v", deleted), func(t *testing.T) {
			ctx := context.Background()
			store := newOpaqueListingStore()
			putSession(t, store, "codex", "older", baseTime)
			newest := putSession(t, store, "codex", "newest", baseTime.Add(time.Hour))
			if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
				t.Fatal(err)
			}
			store.reset()
			store.change = func(key string) {
				store.change = nil
				if key != newest {
					t.Fatalf("wrong selection: %q", key)
				}
				var err error
				if deleted {
					err = store.Delete(ctx, key)
				} else {
					err = store.Put(ctx, key, []byte("corrupt"))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{}); err == nil || !strings.Contains(err.Error(), "incomplete listing") {
				t.Fatalf("mutation accepted: %v", err)
			}
			_, gets := store.counts()
			if len(gets) != 1 || gets[0] != newest {
				t.Fatalf("refilled selection: %v", gets)
			}
		})
	}
}

func TestListUnknownSchemaFallsBackAndReportsSkipped(t *testing.T) {
	ctx := context.Background()
	store := newOpaqueListingStore()
	putSession(t, store, "codex", "supported", baseTime)
	key := putSession(t, store, "codex", "future", baseTime.Add(time.Hour))
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	data, _, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion++
	data, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	scanned, skipped := false, 0
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }, Skipped: func(s SkippedSidecar) {
		skipped++
		if s.Key != key {
			t.Errorf("wrong skipped identity: %q", s.Key)
		}
	}})
	if err != nil || !scanned || skipped != 1 || result.TotalMatched != 1 || result.Sessions[0].SessionID != "supported" {
		t.Fatalf("unsupported schema hidden silently: scanned=%v skipped=%d result=%+v err=%v", scanned, skipped, result, err)
	}
}

func TestListCacheEvictionKeepsUnselectedPresentRows(t *testing.T) {
	ctx := context.Background()
	store := newOpaqueListingStore()
	keys := []string{putSession(t, store, "codex", "old", baseTime), putSession(t, store, "codex", "new", baseTime.Add(time.Hour))}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListRecent(ctx, store, "sessions", Filter{}, 2, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	bodies := 0
	store.reset()
	if _, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{Cache: cache, BodyRead: func(string, bool) { bodies++ }}); err != nil {
		t.Fatal(err)
	}
	_, gets := store.counts()
	if bodies != 1 || len(gets) != 0 {
		t.Fatalf("selection reopened bodies: bodies=%d gets=%v", bodies, gets)
	}
	for _, key := range keys {
		_, validator, err := store.GetVersioned(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cache.get(key, validator); !ok {
			t.Fatalf("unselected live row evicted: %s", key)
		}
	}
}

func TestListingCleanupPreservesReactivatedValidator(t *testing.T) {
	ctx := context.Background()
	store := &cleanupInterleaveStore{MemoryStore: storagetest.NewMemoryStore()}
	key := putSession(t, store, "codex", "reactivated", baseTime.Add(time.Hour))
	b, _, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(ctx, store.MemoryStore, key, b); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", "reactivated", baseTime)
	a, _, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	store.afterStat = func() {
		// MemoryStore uses content-derived validators like ordinary S3 PUTs:
		// restoring identical bytes legitimately restores the same validator.
		if err := store.Put(ctx, key, b); err != nil {
			t.Fatal(err)
		}
		if err := listingindex.PublishRevision(ctx, store.MemoryStore, key, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := listingindex.PublishRevision(ctx, store, key, a); err != nil {
		t.Fatal(err)
	}
	scanned := false
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }})
	if err != nil || scanned || len(result.Sessions) != 1 || !result.Sessions[0].CapturedAt.Equal(baseTime.Add(time.Hour)) {
		t.Fatalf("reactivated index lost: scanned=%v result=%+v err=%v", scanned, result, err)
	}
}

func TestListingRepeatedPublicationAndBoundedCleanup(t *testing.T) {
	ctx := context.Background()
	store := newOpaqueListingStore()
	key := putSession(t, store, "codex", "repeated", baseTime)
	data, validator, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		r, err := listingindex.NewRevision(key, data, validator)
		if err != nil {
			t.Fatal(err)
		}
		if err := listingindex.PutRevision(ctx, store, r); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct immutable publication identities for the same bytes must not
	// be mistaken for conflicting discovery claims.
	scanned := false
	if _, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }}); err != nil || scanned {
		t.Fatalf("equal summaries conflict: scanned=%v err=%v", scanned, err)
	}
	store.reset()
	if err := listingindex.PublishRevision(ctx, store, key, data); err == nil || !strings.Contains(err.Error(), "cleanup remains pending") {
		t.Fatalf("unbounded cleanup: %v", err)
	}
	_, cleanupGets := store.counts()
	if len(cleanupGets) != 1 || cleanupGets[0] != key {
		t.Fatalf("cleanup reads=%v, want exactly one metadata response and no auxiliary bodies", cleanupGets)
	}
	hints, err := store.List(ctx, listingindex.V3Prefix)
	if err != nil || len(hints) != 9 {
		t.Fatalf("cleanup did not stop after 32: hints=%d err=%v", len(hints), err)
	}
	if err := listingindex.PublishRevision(ctx, store, key, data); err != nil {
		t.Fatal(err)
	}
	hints, err = store.List(ctx, listingindex.V3Prefix)
	if err != nil || len(hints) != 1 {
		t.Fatalf("cleanup did not converge: hints=%d err=%v", len(hints), err)
	}
	var summary listingindex.Revision
	if summary, err = listingindex.ParseRevision(hints[0].Key); err != nil {
		t.Fatal(err)
	}
	// A supported, canonical encoded entry with the same provider validator
	// but a changed scope summary must still force the exhaustive reader.
	summary.ProjectID = "conflicting-project"
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(summary.Key, "/")
	parts[len(parts)-1] = base64.RawURLEncoding.EncodeToString(encoded)
	if err := store.Put(ctx, strings.Join(parts, "/"), nil); err != nil {
		t.Fatal(err)
	}
	scanned = false
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }})
	if err != nil || !scanned || len(result.Sessions) != 1 {
		t.Fatalf("conflicting summary accepted: scanned=%v err=%v", scanned, err)
	}
}

type sameRevisionCleanupStore struct {
	*storagetest.MemoryStore
	lists     atomic.Int32
	stats     atomic.Int32
	listReady chan struct{}
	statReady chan struct{}
}

func (s *sameRevisionCleanupStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if strings.HasPrefix(prefix, listingindex.V3Prefix) {
		if s.lists.Add(1) == 2 {
			close(s.listReady)
		}
		<-s.listReady
	}
	return s.MemoryStore.List(ctx, prefix)
}

func (s *sameRevisionCleanupStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	if s.stats.Add(1) == 2 {
		close(s.statReady)
	}
	<-s.statReady
	return info, err
}

func TestListingConcurrentEquivalentCleanupKeepsCoverage(t *testing.T) {
	ctx := context.Background()
	store := &sameRevisionCleanupStore{MemoryStore: storagetest.NewMemoryStore(), listReady: make(chan struct{}), statReady: make(chan struct{})}
	key := putSession(t, store, "codex", "equivalent", baseTime)
	data, _, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- listingindex.PublishRevision(ctx, store, key, data) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	scanned := false
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }})
	if err != nil || scanned || len(result.Sessions) != 1 {
		t.Fatalf("equivalent cleanups removed all coverage: scanned=%v err=%v", scanned, err)
	}
}

func TestListingRepairReusesSummaryWithFreshIdentity(t *testing.T) {
	ctx := context.Background()
	store := newOpaqueListingStore()
	key := putSession(t, store, "codex", "retry", baseTime)
	data, validator, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	r, err := listingindex.NewRevision(key, data, validator)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := listingindex.RepairRevision(ctx, store, r); err != nil {
			t.Fatal(err)
		}
		hints, err := store.List(ctx, listingindex.V3Prefix)
		if err != nil || len(hints) != 1 || hints[0].Key == r.Key {
			t.Fatalf("repair reused stale identity: hints=%v err=%v", hints, err)
		}
	}
	scanned := false
	if _, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }}); err != nil || scanned {
		t.Fatalf("retried repair lost coverage: scanned=%v err=%v", scanned, err)
	}
}
