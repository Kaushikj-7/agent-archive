package reader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type cacheFailureStore struct {
	storage.ObjectStore
	beforeGet func()
	listError error
}

type cacheDiscoveryFailure string

const (
	cacheBodyError           cacheDiscoveryFailure = "body error"
	cacheBodyCancellation    cacheDiscoveryFailure = "body cancellation"
	cacheIncompleteDiscovery cacheDiscoveryFailure = "incomplete discovery"
)

func (s cacheFailureStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.beforeGet != nil {
		s.beforeGet()
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s cacheFailureStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	objects, err := s.ObjectStore.List(ctx, prefix)
	if s.listError != nil {
		return objects[:len(objects)/2], s.listError
	}
	return objects, err
}

func TestCacheDeletionEvictionUsesCompleteHeadersDespiteBodyFailure(t *testing.T) {
	for _, failure := range []cacheDiscoveryFailure{cacheBodyError, cacheBodyCancellation, cacheIncompleteDiscovery} {
		t.Run(string(failure), func(t *testing.T) {
			store := newCountingStore()
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			deleted := putSession(t, store, "codex", "deleted", baseTime)
			outside := putSession(t, store, "claude", "outside", baseTime)
			opts := ListOptions{Cache: cache}
			if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, opts); err != nil {
				t.Fatal(err)
			}
			validator := listedETag(t, store, outside)
			if err := store.Delete(context.Background(), deleted); err != nil {
				t.Fatal(err)
			}
			live := putSession(t, store, "codex", "live", baseTime)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			outage := errors.New("synthetic outage")
			failing := cacheFailureStore{ObjectStore: store}
			wantError := outage
			switch failure {
			case cacheBodyError:
				store.failGet[live] = outage
			case cacheBodyCancellation:
				failing.beforeGet = cancel
				wantError = context.Canceled
			case cacheIncompleteDiscovery:
				failing.listError = outage
			}
			if _, err := ListMetadataWithOptions(ctx, failing, "sessions", Filter{Harness: "codex"}, opts); !errors.Is(err, wantError) {
				t.Fatalf("listing error = %v, want %v", err, wantError)
			}
			dir, _ := cache.keyDir(deleted)
			_, err = os.Stat(dir)
			if failure == cacheIncompleteDiscovery {
				if err != nil {
					t.Fatalf("failed discovery evicted an unproven deletion: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("complete headers retained deleted key after %s: %v", failure, err)
			}
			if _, ok := cache.get(outside, validator); !ok {
				t.Fatal("scoped failure evicted another harness's cache")
			}
		})
	}
}

func maintenanceFixture(t *testing.T, count int) (string, []string) {
	t.Helper()
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	dirs := make([]string, count)
	old := time.Now().Add(-2 * staleCacheTempAge)
	for i := range count {
		dirs[i], _ = cache.keyDir(fmt.Sprintf("sessions/codex/%04d/metadata.json", i))
		if err := os.Mkdir(dirs[i], 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dirs[i], ".pending-stale")
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return home, dirs
}

func TestCacheOpeningLeavesAllSessionDirectoriesUntouched(t *testing.T) {
	home, dirs := maintenanceFixture(t, 200)
	start := time.Now()
	for range 20 {
		if _, err := OpenMetadataCache(home); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("20 opens with %d entries: %s", len(dirs), time.Since(start))
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, ".pending-stale")); err != nil {
			t.Fatalf("opening visited session directory: %v", err)
		}
	}
}

func TestCacheMaintenanceBoundsVisitsAndEventuallyCompletes(t *testing.T) {
	home, dirs := maintenanceFixture(t, 200)
	visited := make(map[string]int)
	for pass := range 4 {
		cache, err := OpenMetadataCache(home)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		cache.readDir = func(path string) ([]os.DirEntry, error) {
			if path != cache.dir {
				visited[path]++
				count++
			}
			return os.ReadDir(path)
		}
		cache.maintain(context.Background())
		cache.maintain(context.Background())
		want := 64
		if pass == 3 {
			want = 8
		}
		if count != want {
			t.Fatalf("pass %d visited %d directories, want %d", pass, count, want)
		}
		state := cache.maintenanceState()
		if pass == 3 && (state.Cursor != "" || state.LastSweep.IsZero()) {
			t.Fatalf("no completed sweep: %+v", state)
		}
	}
	for _, dir := range dirs {
		if visited[dir] != 1 {
			t.Fatalf("directory visited %d times: %s", visited[dir], dir)
		}
		if _, err := os.Stat(filepath.Join(dir, ".pending-stale")); !os.IsNotExist(err) {
			t.Fatalf("stale temporary survived full sweep: %v", err)
		}
	}
}

func TestCacheMaintenanceResumesAfterCancellationAndMissingCursor(t *testing.T) {
	home, dirs := maintenanceFixture(t, 100)
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	cache.readDir = func(path string) ([]os.DirEntry, error) {
		if path != cache.dir {
			count++
			if count == 5 {
				cancel()
			}
		}
		return os.ReadDir(path)
	}
	cache.maintain(ctx)
	if count != 5 || cache.maintenanceState().Cursor != filepath.Base(dirs[4]) {
		t.Fatalf("interrupted cursor count=%d state=%+v", count, cache.maintenanceState())
	}
	if err := os.RemoveAll(dirs[4]); err != nil {
		t.Fatal(err)
	}
	cache, err = OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	cache.maintain(context.Background())
	if cache.maintenanceState().Cursor != filepath.Base(dirs[68]) {
		t.Fatalf("missing cursor restarted sweep: %+v", cache.maintenanceState())
	}
}

func TestCacheLiveEvictionDoesNotVisitSessionDirectories(t *testing.T) {
	home, dirs := maintenanceFixture(t, 200)
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	known := cache.keys("sessions/")
	listed := make([]storage.Object, len(known))
	for i, key := range known {
		listed[i] = storage.Object{Key: key, ETag: "changed"}
	}
	cache.readDir = func(string) ([]os.DirEntry, error) {
		t.Fatal("eviction opened a live child directory")
		return nil, nil
	}
	cache.evictUnlisted(known, listed)
	// Exact-key deletion still removes the absent entry, while another key
	// with the same session prefix remains present.
	cache.evictUnlisted(known, listed[1:])
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("absent key retained: %v", err)
	}
	if _, err := os.Stat(dirs[1]); err != nil {
		t.Fatalf("live key evicted: %v", err)
	}
}

func TestConcurrentCacheMaintenanceLeavesValidPrivateState(t *testing.T) {
	home, _ := maintenanceFixture(t, 100)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			cache, err := OpenMetadataCache(home)
			if err != nil {
				t.Error(err)
				return
			}
			cache.maintain(context.Background())
		})
	}
	wg.Wait()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	state := cache.maintenanceState()
	if state.Cursor == "" && state.LastSweep.IsZero() {
		t.Fatal("checkpoint lost after concurrent opens")
	}
	info, err := os.Lstat(filepath.Join(cache.dir, maintenanceFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("checkpoint not private: %v %v", info, err)
	}
}

func TestCacheMaintenanceRefusesUnsafeDirectoriesAndState(t *testing.T) {
	home, dirs := maintenanceFixture(t, 3)
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	marker := filepath.Join(target, ".pending-stale")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleCacheTempAge)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dirs[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dirs[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dirs[1], 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(marker, filepath.Join(cache.dir, maintenanceFile)); err != nil {
		t.Fatal(err)
	}
	cache.maintain(context.Background())
	for _, path := range []string{marker, filepath.Join(dirs[1], ".pending-stale")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unsafe path cleaned: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dirs[2], ".pending-stale")); !os.IsNotExist(err) {
		t.Fatal("bad state prevented valid directory cleanup")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("maintenance state followed symlink")
	}
}

func TestCacheOpeningRefusesSymlinkRoots(t *testing.T) {
	for _, subdir := range []string{"cache", "cache/metadata"} {
		t.Run(subdir, func(t *testing.T) {
			home, target := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(home, subdir)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(home, subdir)); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenMetadataCache(home); err == nil {
				t.Fatal("symlink root accepted")
			}
		})
	}
}

func TestCacheMaintenanceTreatsCorruptCheckpointAsMiss(t *testing.T) {
	for _, data := range []string{"partial JSON", `{"cursor":"../../outside"}`, `{"cursor":"not-a-key"}`} {
		t.Run(data, func(t *testing.T) {
			home, dirs := maintenanceFixture(t, 1)
			cache, err := OpenMetadataCache(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache.dir, maintenanceFile), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cache.maintain(context.Background())
			if _, err := os.Stat(filepath.Join(dirs[0], ".pending-stale")); !os.IsNotExist(err) {
				t.Fatal("corrupt checkpoint blocked cleanup")
			}
		})
	}
}

func TestCancelledCacheMaintenanceDoesNoWork(t *testing.T) {
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache.readDir = func(string) ([]os.DirEntry, error) {
		t.Fatal("cancelled maintenance inventoried cache")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache.maintain(ctx)
	if _, err := os.Stat(filepath.Join(cache.dir, maintenanceFile)); !os.IsNotExist(err) {
		t.Fatal("cancelled maintenance wrote checkpoint")
	}
}

func TestChangedCacheKeyPrunesOnlyItsOldValidators(t *testing.T) {
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sessions/codex/changed/metadata.json"
	other := "sessions/codex/other/metadata.json"
	cache.putVerified(key, "old", []byte(`{"version":1}`))
	cache.putVerified(other, "old", []byte(`{"version":1}`))
	cache.putVerified(key, "new", []byte(`{"version":2}`))
	if _, ok := cache.get(key, "old"); ok {
		t.Fatal("changed key kept old validator")
	}
	if _, ok := cache.get(key, "new"); !ok {
		t.Fatal("changed key lost new validator")
	}
	if _, ok := cache.get(other, "old"); !ok {
		t.Fatal("changed key pruned another session")
	}
}

func TestCacheMaintenancePrunesUnselectedValidators(t *testing.T) {
	home, dirs := maintenanceFixture(t, 1)
	for _, name := range []string{"old.json", "new.json"} {
		if err := os.WriteFile(filepath.Join(dirs[0], name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dirs[0], "old.json"), old, old); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	cache.maintain(context.Background())
	if _, err := os.Stat(filepath.Join(dirs[0], "old.json")); !os.IsNotExist(err) {
		t.Fatal("old validator retained")
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "new.json")); err != nil {
		t.Fatal("latest validator removed")
	}
}
