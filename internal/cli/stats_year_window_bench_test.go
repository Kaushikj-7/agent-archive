package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Restored original synthetic methodology; no native or matched timing qualification.
func BenchmarkStatsYearWindow(b *testing.B) {
	for _, delay := range []time.Duration{0, time.Millisecond} {
		for _, exhaustive := range []bool{false, true} {
			b.Run(fmt.Sprintf("exhaustive=%v/delay=%s", exhaustive, delay), func(b *testing.B) {
				b.StopTimer()
				mem := storagetest.NewMemoryStore()
				storagetest.SeedArchive(b, mem, 800, 0)
				seedBenchmarkStatsDays(b, mem, 800, 400)
				store := storagetest.NewMeasuredStore(mem, delay)
				filter := reader.Filter{From: statsFetchFrom(storagetest.BenchmarkTime.Add(24*time.Hour), time.UTC, 1)}
				b.ReportAllocs()
				b.StartTimer()
				for range b.N {
					var err error
					if exhaustive {
						_, err = reader.ListRecent(context.Background(), store, "sessions", filter, 0, reader.ListOptions{})
					} else {
						_, err = reader.SelectMetadata(context.Background(), store, "sessions", reader.MetadataQuery{Filter: filter, IncludeRootChildren: true}, reader.ListOptions{})
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				storagetest.ReportReadMetrics(b, store.Metrics())
			})
		}
	}
}

func seedBenchmarkStatsDays(b *testing.B, mem *storagetest.MemoryStore, count, days int) {
	b.Helper()
	ctx := context.Background()
	for i := range count {
		id := fmt.Sprintf("%08x%024x", i+1, i+1)
		key, _ := archive.MetadataObjectKey("codex", id)
		data, err := mem.Get(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			b.Fatal(err)
		}
		m.CapturedAt = storagetest.BenchmarkTime.Add(24*time.Hour - time.Duration(i%days)*24*time.Hour)
		m.StartedAt = m.CapturedAt
		input, output := 1000+i, 200+i%100
		m.Counts = archive.Counts{InputTokens: &input, OutputTokens: &output}
		m.ModelTokens = []archive.ModelTokens{{Model: "gpt-5", InputTokens: &input, OutputTokens: &output}}

		data, err = json.Marshal(m)
		if err != nil {
			b.Fatal(err)
		}
		if err = mem.Put(ctx, key, data); err != nil {
			b.Fatal(err)
		}
		_, etag, err := mem.GetVersioned(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		revision, err := listingindex.NewRevision(key, data, etag)
		if err != nil {
			b.Fatal(err)
		}
		if err = listingindex.PutRevision(ctx, mem, revision); err != nil {
			b.Fatal(err)
		}
	}
}
