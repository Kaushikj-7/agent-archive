package reader

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type headerDiscoveryMode string

const (
	headerSerial   headerDiscoveryMode = "serial"
	headerParallel headerDiscoveryMode = "parallel"
)

// BenchmarkHeaderDiscovery measures provider delay separately from body reads.
// Header bytes estimate key/validator payload, excluding provider wire overhead.
func BenchmarkHeaderDiscovery(b *testing.B) {
	for _, size := range []int{80, 800} {
		for _, mode := range []headerDiscoveryMode{headerSerial, headerParallel} {
			b.Run(strconv.Itoa(size)+"/"+string(mode), func(b *testing.B) {
				benchmarkHeaderDiscovery(b, size, mode)
			})
		}
	}
}

func benchmarkHeaderDiscovery(b *testing.B, size int, mode headerDiscoveryMode) {
	b.Helper()
	b.StopTimer()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, size, 0)
	store := storagetest.NewMeasuredStore(mem, time.Millisecond)
	ctx := context.Background()
	b.ReportAllocs()
	b.StartTimer()
	for range b.N {
		if mode == headerParallel {
			if _, err := discoverHeaders(ctx, store, "sessions", Filter{}, nil); err != nil {
				b.Fatal(err)
			}
		} else {
			var groups [3][]storage.Object
			for i, prefix := range []string{"sessions", listingindex.V2Prefix, listingindex.V3Prefix} {
				var err error
				groups[i], err = store.List(ctx, prefix)
				if err != nil {
					b.Fatal(err)
				}
			}
			if snapshot := headerSnapshotFromGroups(groups); snapshot.incompleteReason != "" {
				b.Fatal(snapshot.incompleteReason)
			}
		}
	}
	b.StopTimer()
	storagetest.ReportReadMetrics(b, store.Metrics())
}

// BenchmarkHeaderDiscoveryRanges exposes the additional requests made when a
// warm canonical inventory splits the scan, while body reads remain excluded.
func BenchmarkHeaderDiscoveryRanges(b *testing.B) {
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, 800, 0)
	cache, err := OpenMetadataCache(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := ListMetadataWithOptions(context.Background(), mem, "sessions", Filter{}, ListOptions{Cache: cache}); err != nil {
		b.Fatal(err)
	}
	store := storagetest.NewMeasuredStore(mem, time.Millisecond)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := discoverHeaders(context.Background(), store, "sessions", Filter{}, cache); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	storagetest.ReportReadMetrics(b, store.Metrics())
}
