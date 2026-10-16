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
