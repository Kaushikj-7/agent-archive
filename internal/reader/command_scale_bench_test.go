package reader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// BenchmarkReaderScale includes cache opening in every measured operation.
// Fixture preparation, warmup, mutation and resetting the cache are excluded.
func BenchmarkReaderScale(b *testing.B) {
	for _, c := range storagetest.ScaleCases() {
		b.Run(c.Name(), func(b *testing.B) { benchmarkReaderCase(b, c) })
	}
}

func BenchmarkReaderLatency(b *testing.B) {
	for _, state := range []string{"cold", "warm", "changed"} {
		c := storagetest.BenchCase{Sessions: 80, Limit: 50, CacheState: state, Delay: time.Millisecond}
		b.Run(c.Name(), func(b *testing.B) { benchmarkReaderCase(b, c) })
	}
}

func benchmarkReaderCase(b *testing.B, c storagetest.BenchCase) {
	b.Helper()
	b.StopTimer()
	ctx := context.Background()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, c.Sessions, 0)
	store := storagetest.NewMeasuredStore(mem, c.Delay)
	home := b.TempDir()
	var opens time.Duration
	b.ReportAllocs()
	for range b.N {
		if c.CacheState == "changed" {
			mem = storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, c.Sessions, 0)
			store.MemoryStore = mem
		}
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			b.Fatal(err)
		}
		if c.CacheState != "cold" {
			cache, err := OpenMetadataCache(home)
			if err != nil {
				b.Fatal(err)
			}
			if _, err = ListRecent(ctx, mem, "sessions", Filter{}, c.Limit, ListOptions{Cache: cache}); err != nil {
				b.Fatal(err)
			}
		}
		if c.CacheState == "changed" {
			storagetest.SeedArchive(b, mem, c.Sessions, 1)
		}
		b.StartTimer()
		start := time.Now()
		cache, err := OpenMetadataCache(home)
		opens += time.Since(start)
		if err != nil {
			b.Fatal(err)
		}
		result, err := ListRecent(ctx, store, "sessions", Filter{}, c.Limit, ListOptions{Cache: cache})
		if err != nil {
			b.Fatal(err)
		}
		want := c.Sessions
		if c.Limit > 0 {
			want = min(want, c.Limit)
		}
		if len(result.Sessions) != want || !result.Complete {
			b.Fatal("incomplete synthetic listing")
		}
		b.StopTimer()
	}
	b.ReportMetric(float64(opens.Nanoseconds())/float64(b.N), "cacheOpen-ns/op")
	storagetest.ReportReadMetrics(b, store.Metrics())
}

// BenchmarkReaderExhaustiveNoCache validates large fixture cardinality and
// measures exhaustive remote/decode cost without filesystem cache teardown.
// It supplements rather than replaces the scale matrix's cache cases.
func BenchmarkReaderExhaustiveNoCache(b *testing.B) {
	if os.Getenv("AGENT_ARCHIVE_PERF") != "1" {
		b.Skip("set AGENT_ARCHIVE_PERF=1 for large fixtures")
	}
	for _, size := range []int{10000, 50000} {
		c := storagetest.BenchCase{Sessions: size, Limit: 0, CacheState: "disabled"}
		b.Run(c.Name(), func(b *testing.B) {
			b.StopTimer()
			mem := storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, size, 0)
			store := storagetest.NewMeasuredStore(mem, 0)
			b.ReportAllocs()
			b.StartTimer()
			for range b.N {
				result, err := ListRecent(context.Background(), store, "sessions", Filter{}, 0, ListOptions{})
				if err != nil {
					b.Fatal(err)
				}
				if len(result.Sessions) != size || !result.Complete {
					b.Fatal("incomplete synthetic exhaustive listing")
				}
			}
			b.StopTimer()
			storagetest.ReportReadMetrics(b, store.Metrics())
		})
	}
}
