package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This preserves the original PR382 synthetic latency harness. Its historical
// discard writers do not validate output and do not qualify a matched current
// performance comparison. Existing BenchmarkCommandReads remains separate.
func BenchmarkShowLatency(b *testing.B) {
	for _, tc := range []struct {
		name     string
		query    string
		children int
	}{
		{name: "exact", query: "00000001000000000000000000000001"},
		{name: "short-ID", query: "00000001"},
		{name: "parent-50", query: "00000001000000000000000000000001", children: 50},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.StopTimer()
			mem := storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, 80, 0)
			if tc.children > 0 {
				seedBenchmarkParent(b, mem, tc.children)
			}
			store := storagetest.NewMeasuredStore(mem, time.Millisecond)
			home := b.TempDir()
			env := benchmarkEnv(b, home, store)
			b.ReportAllocs()
			for range b.N {
				if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				code := Run([]string{"show", tc.query, "--json"}, nil, io.Discard, io.Discard, env)
				b.StopTimer()
				if code != 0 {
					b.Fatalf("synthetic show exit %d", code)
				}
			}
			storagetest.ReportReadMetrics(b, store.Metrics())
		})
	}
}
