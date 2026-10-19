package reader

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func BenchmarkCacheOpen(b *testing.B) {
	for _, count := range []int{0, 800} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			home := b.TempDir()
			cache, err := OpenMetadataCache(home)
			if err != nil {
				b.Fatal(err)
			}
			for i := range count {
				dir, _ := cache.keyDir(fmt.Sprintf("sessions/codex/%08d/metadata.json", i))
				if err := os.Mkdir(dir, 0700); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := OpenMetadataCache(home); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Live eviction uses synthetic keys without making physical directory fixtures;
// inventory and deletion costs are deliberately separate from this measurement.
func BenchmarkCacheLiveEviction(b *testing.B) {
	for _, count := range []int{800, 10000, 50000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			cache, err := OpenMetadataCache(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			known := make([]string, count)
			listed := make([]storage.Object, count)
			for i := range count {
				known[i] = fmt.Sprintf("sessions/codex/%08d/metadata.json", i)
				listed[i] = storage.Object{Key: known[i], ETag: "warm"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				cache.evictUnlisted(known, listed)
			}
		})
	}
}

func BenchmarkCacheInventory(b *testing.B) {
	cache, err := OpenMetadataCache(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := range 800 {
		dir, _ := cache.keyDir(fmt.Sprintf("sessions/codex/%08d/metadata.json", i))
		if err := os.Mkdir(dir, 0700); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if len(cache.keys("sessions/")) != 800 {
			b.Fatal("inventory omitted a key")
		}
	}
}

// Each iteration populates a fresh cache with 50k synthetic sidecars. Directory
// enumeration is observed independently of CPU profiles so filesystem wait time
// cannot hide an accidental prune on the cold path.
func BenchmarkCacheColdWrites(b *testing.B) {
	const count = 50000
	keys := make([]string, count)
	for i := range count {
		keys[i] = fmt.Sprintf("sessions/codex/%08d/metadata.json", i)
	}
	data := []byte(`{"version":1}`)
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		cache, err := OpenMetadataCache(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		cache.readDir = func(string) ([]os.DirEntry, error) {
			b.Fatal("cold write enumerated a key directory")
			return nil, nil
		}
		b.StartTimer()
		for _, key := range keys {
			cache.putVerified(key, "synthetic-revision", data)
		}
		b.StopTimer()
		for _, key := range keys {
			if _, ok := cache.get(key, "synthetic-revision"); !ok {
				b.Fatalf("cold write did not cache %q", key)
			}
		}
		b.StartTimer()
	}
	b.ReportMetric(count, "sidecars/op")
}
