package storagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
)

// BenchCase specifies synthetic archive size, selection and cache preparation.
type BenchCase struct {
	Sessions   int
	Limit      int
	CacheState string
	Delay      time.Duration
}

// Name is a stable, content-free benchmark label.
func (c BenchCase) Name() string {
	return fmt.Sprintf("sessions=%d/limit=%d/cache=%s", c.Sessions, c.Limit, c.CacheState)
}

// ScaleCases covers the full local cost matrix without artificial latency.
func ScaleCases() []BenchCase {
	var cases []BenchCase
	for _, size := range []int{800, 10000, 50000} {
		if size > 800 && os.Getenv("AGENT_ARCHIVE_PERF") != "1" {
			continue
		}
		for _, limit := range []int{1, 50, 0} {
			for _, state := range []string{"cold", "warm", "changed"} {
				cases = append(cases, BenchCase{Sessions: size, Limit: limit, CacheState: state})
			}
		}
	}
	return cases
}

// BenchmarkTime pins synthetic capture times independently of the host clock.
var BenchmarkTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// SeedArchive publishes deterministic synthetic sidecars, one source header per
// session and current revision summaries. It never stores a transcript.
func SeedArchive(tb testing.TB, store *MemoryStore, count int, generation int) {
	tb.Helper()
	ctx := context.Background()
	hash := fmt.Sprintf("%064x", 1)
	for i := range count {
		id := fmt.Sprintf("%08x%024x", i+1, i+1)
		key, err := archive.MetadataObjectKey("codex", id)
		if err != nil {
			tb.Fatal(err)
		}
		sourceKey := fmt.Sprintf("sessions/codex/%s/source.%s.jsonl.gz", id, hash)
		m := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, NativeSessionID: id, MachineID: "bench", ProjectID: fmt.Sprintf("project-%d", i%100), Harness: archive.Harness{Name: "codex"}, CapturedAt: BenchmarkTime.Add(time.Duration(i) * time.Second), Title: fmt.Sprintf("synthetic-%d", generation), SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: hash, CompressedBytes: 1}}
		data, err := json.Marshal(m)
		if err != nil {
			tb.Fatal(err)
		}
		if err = store.Put(ctx, key, data); err != nil {
			tb.Fatal(err)
		}
		if generation == 0 {
			if err = store.Put(ctx, sourceKey, []byte{0}); err != nil {
				tb.Fatal(err)
			}
		}
		_, etag, err := store.GetVersioned(ctx, key)
		if err != nil {
			tb.Fatal(err)
		}
		revision, err := listingindex.NewRevision(key, data, etag)
		if err != nil {
			tb.Fatal(err)
		}
		if err = listingindex.PutRevision(ctx, store, revision); err != nil {
			tb.Fatal(err)
		}
	}
}

// ReportReadMetrics reports per-operation requests and bytes and observed peak.
func ReportReadMetrics(b *testing.B, m ReadMetrics) {
	b.Helper()
	n := float64(b.N)
	b.ReportMetric(float64(m.Lists)/n, "LIST/op")
	b.ReportMetric(float64(m.Gets)/n, "GET/op")
	b.ReportMetric(float64(m.Bytes)/n, "GET-B/op")
	b.ReportMetric(float64(m.ListBytes)/n, "LIST-header-B/op")
	b.ReportMetric(float64(m.PeakReads), "peak-reads")
}
