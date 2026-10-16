package storagetest

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestMeasuredStoreProviderPagesAndBoundaries(t *testing.T) {
	t.Parallel()
	headers := make([]storage.Object, 2500)
	for i := range headers {
		headers[i] = storage.Object{Key: fmt.Sprintf("sessions/%04d", i), ETag: "etag"}
	}
	for _, tc := range []struct {
		name, after, through  string
		corrected             bool
		returned, pages, keys int64
	}{
		{"historical exact page", "", headers[999].Key, false, 1000, 2, 2000},
		{"corrected exact page", "", headers[999].Key, true, 1000, 1, 1000},
		{"absent boundary", "", headers[999].Key + "x", true, 1000, 2, 2000},
		{"short range", "", headers[9].Key, true, 10, 1, 1000},
		{"final range", headers[1999].Key, "", true, 500, 1, 500},
		{"empty range", headers[2499].Key, "", true, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewMeasuredStore(NewMemoryStore(), 0)
			store.ListingModel = &ListingLatencyModel{Headers: headers, StopAtBoundary: tc.corrected}
			objects, err := store.ListRange(context.Background(), "sessions/", tc.after, tc.through)
			m := store.Metrics()
			if err != nil || int64(len(objects)) != tc.returned || m.Lists != 1 || m.ListPages != tc.pages || m.ListKeys != tc.keys || m.LongestListPages != tc.pages {
				t.Fatalf("objects=%d metrics=%+v err=%v", len(objects), m, err)
			}
			headerBytes := int64(len(headers[0].Key) + len(headers[0].ETag) + 16)
			if m.ListBytes != tc.keys*headerBytes || m.DiscardedListBytes != (tc.keys-tc.returned)*headerBytes {
				t.Fatalf("bytes: %+v", m)
			}
		})
	}
	store := NewMeasuredStore(NewMemoryStore(), 0)
	store.ListingModel = &ListingLatencyModel{Headers: headers}
	if objects, err := store.List(context.Background(), "missing/"); err != nil || len(objects) != 0 || store.Metrics().ListPages != 1 {
		t.Fatal("empty prefix request missing", err, store.Metrics())
	}
}

func TestListingScheduleSharesPrefixBudget(t *testing.T) {
	t.Parallel()
	// Sixteen one-page session ranges, plus two empty listing prefixes, consume
	// two waves together. Separate per-prefix budgets would incorrectly give one.
	pages := make([]int64, 18)
	for i := range pages {
		pages[i] = 1
	}
	if got := ListingSchedule(pages, 16); got != 2 {
		t.Fatal(got)
	}
	// A skewed range has a sequential critical path despite otherwise idle slots.
	pages[0] = 7
	if got := ListingSchedule(pages, 16); got != 7 {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(pages[1:], []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}) {
		t.Fatal("schedule mutated input")
	}
}

func TestReadPathFixtureHasStableOnePercentChanges(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore()
	SeedReadPathArchive(t, mem, 100, 0)
	baseline, err := SnapshotMemoryStore(t.Context(), mem)
	if err != nil {
		t.Fatal(err)
	}
	var changedHeaders int
	for range 2 {
		candidate, err := baseline.Clone(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		SeedReadPathArchive(t, candidate, 100, 1)
		headers, err := candidate.List(t.Context(), "sessions/")
		if err != nil || len(headers) != 280 {
			t.Fatalf("canonical keys=%d err=%v", len(headers), err)
		}
		before, err := mem.List(t.Context(), "sessions/")
		if err != nil {
			t.Fatal(err)
		}
		changed := 0
		for i, h := range headers {
			if h.ETag != before[i].ETag {
				changed++
			}
		}
		if changed != 1 {
			t.Fatalf("changed sidecars=%d", changed)
		}
		hints, err := candidate.List(t.Context(), "listing/v3/")
		if err != nil {
			t.Fatal(err)
		}
		if changedHeaders != 0 && changedHeaders != len(hints) {
			t.Fatalf("hint inventory grew: %d -> %d", changedHeaders, len(hints))
		}
		changedHeaders = len(hints)
	}
}

func TestMeasuredStoreModelsExplicitPageReads(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore()
	if err := mem.Put(t.Context(), "sessions/a", []byte("body")); err != nil {
		t.Fatal(err)
	}
	store := NewMeasuredStore(mem, 0)
	store.ListingModel = &ListingLatencyModel{}
	if page, err := store.ListPage(t.Context(), "sessions/", "", 1000); err != nil || len(page.Objects) != 1 {
		t.Fatal(page, err)
	}
	if m := store.Metrics(); m.ListPages != 1 || m.ListKeys != 1 || m.Lists != 1 || m.ListBytes == 0 {
		t.Fatal(m)
	}
}
