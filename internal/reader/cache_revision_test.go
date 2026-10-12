package reader

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
)

// A normal metadata refresh leaves a complete index and an older local cache.
// Count physical cache reads independently of the successful-result observer.
func TestIndexedRefreshSharesPhysicalBodyBudgetWithWarmHits(t *testing.T) {
	for _, limit := range []int{1, 7} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			ctx := context.Background()
			store := newOpaqueListingStore()
			keys := make([]string, limit)
			for i := range limit {
				keys[i] = putSession(t, store, "codex", strings.Repeat("s", i+1), baseTime)
			}
			if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			cache, err := OpenMetadataCache(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ListRecent(ctx, store, "sessions", Filter{}, limit, ListOptions{Cache: cache}); err != nil {
				t.Fatal(err)
			}
			refreshed := limit/2 + 1
			for _, key := range keys[:refreshed] {
				data, _, err := store.GetVersioned(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				var m archive.Metadata
				if err = json.Unmarshal(data, &m); err != nil {
					t.Fatal(err)
				}
				m.ProjectID = "refreshed-project"
				data, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.Put(ctx, key, data); err != nil {
					t.Fatal(err)
				}
				if err = listingindex.PublishRevision(ctx, store, key, data); err != nil {
					t.Fatal(err)
				}
			}
			var physical atomic.Int64
			cache.readFile = func(path string) ([]byte, error) { physical.Add(1); return os.ReadFile(path) }
			store.reset()
			observed := 0
			scanned := false
			got, err := ListRecent(ctx, store, "sessions", Filter{}, limit, ListOptions{Cache: cache, BodyRead: func(string, bool) { observed++ }, CompatibilityScan: func(string) { scanned = true }})
			_, gets := store.counts()
			if err != nil || scanned || len(got.Sessions) != limit || int(physical.Load()) != limit-refreshed || len(gets) != refreshed || int(physical.Load())+len(gets) != limit || observed != limit {
				t.Fatalf("limit=%d cached=%d GETs=%v observed=%d scan=%v err=%v", limit, physical.Load(), gets, observed, scanned, err)
			}
			if files := cacheFiles(t, home); len(files) != limit {
				t.Fatalf("stale versions retained: %v", files)
			}
			for _, key := range gets {
				if !isMetadataKey(key) {
					t.Fatalf("read source or auxiliary body %q", key)
				}
			}
		})
	}
}

func TestVersionCacheMigratesFlatFilesWithoutReadingAndPreservesLongKeys(t *testing.T) {
	ctx := context.Background()
	store := newOpaqueListingStore()
	id := strings.Repeat("a", maxCacheKeyBytes-len("sessions/codex//metadata.json"))
	key := putSession(t, store, "codex", id, baseTime)
	if len(key) != maxCacheKeyBytes {
		t.Fatalf("key length=%d", len(key))
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(cache.dir, hex.EncodeToString([]byte(key))+".json")
	if err = os.WriteFile(flat, []byte("legacy cache body must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	var physical atomic.Int64
	cache.readFile = func(path string) ([]byte, error) { physical.Add(1); return os.ReadFile(path) }
	store.reset()
	if _, err = ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	_, gets := store.counts()
	if int(physical.Load()) != 0 || len(gets) != 1 {
		t.Fatalf("migration bodies=%d GETs=%v", physical.Load(), gets)
	}
	if _, err = os.Lstat(flat); !os.IsNotExist(err) {
		t.Fatalf("legacy cache not discarded: %v", err)
	}
	store.reset()
	if _, err = ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	_, gets = store.counts()
	if int(physical.Load()) != 1 || len(gets) != 0 {
		t.Fatalf("long-key warm bodies=%d GETs=%v", physical.Load(), gets)
	}
	dir, ok := cache.keyDir(key)
	if !ok {
		t.Fatal("long key refused")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("key directory is not private: %v %v", info, err)
	}
}

func TestVersionCacheRefusesSymlinkKeyDirectory(t *testing.T) {
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sessions/codex/safe/metadata.json"
	dir, _ := cache.keyDir(key)
	target := t.TempDir()
	marker := filepath.Join(target, "keep.json")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	cache.putVerified(key, "opaque/revision", []byte(`{"metadata":"cache"}`))
	if _, ok := cache.get(key, "opaque/revision"); ok {
		t.Fatal("symlink cache was read")
	}
	cache.evictUnlisted([]string{key}, nil)
	if keys := cache.keys("sessions/"); len(keys) != 0 {
		t.Fatalf("symlink indexed: %v", keys)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 1 {
		t.Fatalf("symlink target modified: %v %v", entries, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "keep" {
		t.Fatal("symlink target changed")
	}
}
