package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestKeyRangePlansBalanceActualKeys(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 999, 1000, 1001, 1819, 5152, 16000, 16001, 280000} {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("sessions/unknown/nonhex-%08d", n-i)
		}
		bounds := planKeyRanges(keys)
		if n <= 1000 {
			if len(bounds) != 0 {
				t.Fatalf("%d keys planned %d boundaries", n, len(bounds))
			}
			continue
		}
		if len(bounds) > 15+(n+999)/1000 {
			t.Fatalf("%d keys exceeded hint bound", n)
		}
		slices.Sort(keys)
		counts := make([]int, len(bounds)+1)
		for _, key := range keys {
			i, _ := slices.BinarySearch(bounds, key)
			counts[i]++
		}
		for i, count := range counts {
			if (i < len(counts)-1 && count != 1000) || count < 1 || count > 1000 {
				t.Fatalf("%d keys not split into full provider pages and a tail: %v", n, counts)
			}
		}
	}
}

func TestRangeHintsArePrivateDisposableAndPrefixSpecific(t *testing.T) {
	t.Parallel()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	objects := make([]storage.Object, 1800)
	for i := range objects {
		objects[i].Key = fmt.Sprintf("sessions/custom/%08d", i)
	}
	cache.saveRangeBounds("sessions/", objects)
	bounds, ok := cache.rangeBounds("sessions/")
	if !ok || len(bounds) != 1 {
		t.Fatalf("saved bounds: %d, %v", len(bounds), ok)
	}
	if _, ok := cache.rangeBounds("sessions/custom/"); ok {
		t.Fatal("used another prefix's plan")
	}
	info, err := os.Stat(cache.rangePlanPath("sessions/"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("hint permissions: %v, %v", info, err)
	}
	cache.keys("sessions/")
	if _, ok := cache.rangeBounds("sessions/"); !ok {
		t.Fatal("cache inventory discarded hint")
	}
	for _, plan := range []rangePlan{
		{Prefix: "other/", Keys: 1800, Bounds: bounds},
		{Prefix: "sessions/", Keys: 1, Bounds: make([]string, 17)},
		{Prefix: "sessions/", Keys: 1800, Bounds: []string{"sessions/z", "sessions/a"}},
		{Prefix: "sessions/", Keys: 1800, Bounds: []string{"sessions/a", "sessions/a"}},
		{Prefix: "sessions/", Keys: 1800, Bounds: []string{""}},
		{Prefix: "sessions/", Keys: 1800, Bounds: []string{"other/a"}},
	} {
		data, _ := json.Marshal(plan)
		if err := os.WriteFile(cache.rangePlanPath("sessions/"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := cache.rangeBounds("sessions/"); ok {
			t.Fatalf("accepted invalid plan: %+v", plan)
		}
	}
	if _, ok := (*MetadataCache)(nil).rangeBounds("sessions/"); ok {
		t.Fatal("nil cache has hints")
	}
	entries, err := os.ReadDir(cache.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(filepath.Base(entry.Name()), ".ranges-") {
			t.Fatalf("unexpected file %s", entry.Name())
		}
	}
}

func TestHeaderGroupsShareRangeBudgetAndPreserveCompleteListings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore()}
	prefixes := []string{"sessions/", listingindex.V2Prefix, listingindex.V3Prefix}
	for _, prefix := range prefixes {
		for i := range 6000 {
			if err := store.Put(ctx, fmt.Sprintf("%scustom/nonhex-%05d", prefix, i), []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, complete, err := listHeaderGroups(ctx, store, prefixes[0], nil, true, prefixes[2], cache)
	if err != nil || !complete {
		t.Fatalf("cold listing: complete %v, err %v", complete, err)
	}
	// Delete hint boundaries (including entire ranges), and insert new edge keys.
	for _, prefix := range prefixes {
		for i := range 5400 {
			_ = store.Delete(ctx, fmt.Sprintf("%scustom/nonhex-%05d", prefix, i))
		}
		for _, suffix := range []string{"!first", "custom/nonhex-05400!", "unknown/~last"} {
			_ = store.Put(ctx, prefix+suffix, []byte("new"))
		}
	}
	store.hold = make(chan struct{})
	done := make(chan error, 1)
	var got [3][]storage.Object
	go func() {
		var err error
		got, _, err = listHeaderGroups(ctx, store, prefixes[0], nil, true, prefixes[2], cache)
		done <- err
	}()
	waitInFlight(t, store, rangeConcurrency)
	close(store.hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if store.maxInFlight != rangeConcurrency || len(store.ranges) != 3*6 {
		t.Fatalf("shared budget: peak %d ranges %d", store.maxInFlight, len(store.ranges))
	}
	for i, prefix := range prefixes {
		want, err := store.MemoryStore.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[i], want) {
			t.Fatalf("prefix %s: ranged and sequential objects differ", prefix)
		}
		if len(first[i]) != 6000 {
			t.Fatalf("cold prefix %s incomplete", prefix)
		}
	}
	// A complete shrunken listing replaces stale hints with a sequential plan.
	for _, prefix := range prefixes {
		if bounds, ok := cache.rangeBounds(prefix); !ok || len(bounds) != 0 {
			t.Fatalf("stale plan not replaced: %s", prefix)
		}
	}
}

func TestFailedListingKeepsLastCompleteRangeHint(t *testing.T) {
	t.Parallel()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	objects := make([]storage.Object, 1800)
	for i := range objects {
		objects[i].Key = fmt.Sprintf("sessions/custom/%08d", i)
	}
	cache.saveRangeBounds("sessions/", objects)
	before, err := os.ReadFile(cache.rangePlanPath("sessions/"))
	if err != nil {
		t.Fatal(err)
	}
	bounds, _ := cache.rangeBounds("sessions/")
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore(), failAt: bounds[0], failErr: os.ErrPermission}
	if _, err := listObjects(context.Background(), store, "sessions/", nil, cache); err == nil {
		t.Fatal("expected listing error")
	}
	after, err := os.ReadFile(cache.rangePlanPath("sessions/"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatal("failed listing overwrote complete hint")
	}
}
