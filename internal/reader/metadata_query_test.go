package reader

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSelectMetadataUnlimitedDateSelectionMatchesExhaustive(t *testing.T) {
	store := &indexedCountingStore{countingStore: newCountingStore()}
	for i := range 400 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*24*time.Hour))
	}
	if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []Filter{
		{From: baseTime.Add(365 * 24 * time.Hour)},
		{From: baseTime.Add(390 * 24 * time.Hour), To: baseTime.Add(395 * 24 * time.Hour)},
		{From: baseTime.Add(401 * 24 * time.Hour)},
		{Model: "no-such-model"},
	} {
		oracle, err := ListMetadata(t.Context(), store, "sessions", filter)
		if err != nil {
			t.Fatal(err)
		}
		store.reset()
		got, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: filter}, ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Sessions, oracle) {
			t.Fatalf("filter %+v: got %d want %d", filter, len(got.Sessions), len(oracle))
		}
		_, gets := store.counts()
		want := len(oracle)
		if filter.Model != "" {
			want = 400
		}
		if len(gets) != want {
			t.Fatalf("filter %+v: GETs %d want %d", filter, len(gets), want)
		}
	}
}

func TestFindMetadataPrefixUsesFreshCanonicalHeadersAndCandidateBodies(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	first := putSession(t, store, "codex", "abcdef01", baseTime)
	putSession(t, store, "claude", "abcdef02", baseTime)
	putSession(t, store, "codex", "other", baseTime)
	for _, includeText := range []bool{false, true} {
		store.reset()
		var matchers []func(archive.Metadata) bool
		if includeText {
			matchers = append(matchers, func(m archive.Metadata) bool { return m.SessionID == "other" })
		}
		got, err := FindMetadataPrefix(ctx, store, "sessions", "abcdef", Filter{}, ListOptions{}, matchers...)
		if err != nil {
			t.Fatal(err)
		}
		lists, gets := store.counts()
		want := 2
		if includeText {
			want = 3
		}
		if len(got) != want || len(lists) != 1 || len(gets) != want {
			t.Fatalf("matches=%d lists=%d GETs=%d want=%d", len(got), len(lists), len(gets), want)
		}
	}
	if err := store.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := FindMetadataPrefix(ctx, store, "sessions", "abcdef", Filter{}, ListOptions{})
	if err != nil || len(got) != 1 || got[0].Harness.Name != "claude" {
		t.Fatalf("deleted prefix result=%+v err=%v", got, err)
	}
}

func TestMetadataQueryEntryPointsMaintainCacheOnceWithinBound(t *testing.T) {
	for _, prefixFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefixFirst=%v", prefixFirst), func(t *testing.T) {
			home, dirs := maintenanceFixture(t, 200)
			cache, err := OpenMetadataCache(home)
			if err != nil {
				t.Fatal(err)
			}
			visits := 0
			cache.readDir = func(path string) ([]os.DirEntry, error) {
				if path != cache.dir {
					visits++
				}
				return os.ReadDir(path)
			}
			store := newCountingStore()
			// Narrow discovery to another harness so deletion eviction does not
			// remove the synthetic unselected directories under maintenance.
			filter := Filter{Harness: "claude", From: baseTime.Add(time.Hour)}
			opts := ListOptions{Cache: cache}
			selectQuery := func() {
				if _, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: filter}, opts); err != nil {
					t.Fatal(err)
				}
			}
			prefixQuery := func() {
				if _, err := FindMetadataPrefix(t.Context(), store, "sessions", "abcd", filter, opts); err != nil {
					t.Fatal(err)
				}
			}
			if prefixFirst {
				prefixQuery()
			} else {
				selectQuery()
			}
			if visits != 64 || cache.maintenanceState().Cursor != filepath.Base(dirs[63]) {
				t.Fatalf("first query visits=%d state=%+v", visits, cache.maintenanceState())
			}
			selectQuery()
			prefixQuery()
			if _, err := ListRecent(t.Context(), store, "sessions", filter, 0, opts); err != nil {
				t.Fatal(err)
			}
			if visits != 64 {
				t.Fatalf("shared command cache repeated maintenance: visits=%d", visits)
			}
			info, err := os.Lstat(filepath.Join(cache.dir, maintenanceFile))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("checkpoint not private: %v %v", info, err)
			}
			if _, err := os.Stat(filepath.Join(dirs[0], ".pending-stale")); !os.IsNotExist(err) {
				t.Fatal("query retained stale temporary in visited directory")
			}
			if _, err := os.Stat(filepath.Join(dirs[64], ".pending-stale")); err != nil {
				t.Fatal("query exceeded maintenance bound")
			}
		})
	}
}
