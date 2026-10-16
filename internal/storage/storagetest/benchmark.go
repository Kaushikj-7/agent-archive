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
	if m.ListPages > 0 {
		b.ReportMetric(float64(m.ListPages)/n, "provider-LIST-pages/op")
		b.ReportMetric(float64(m.ListKeys)/n, "transferred-LIST-keys/op")
		b.ReportMetric(float64(m.DiscardedListBytes)/n, "discarded-LIST-B/op")
		b.ReportMetric(float64(m.LongestListPages), "longest-LIST-pages")
	}
}

// ReadPathSizes keeps the measured 1.8k archive in CI and larger scenarios opt-in.
// Historical ScaleCases and SeedArchive intentionally retain their old controls.
func ReadPathSizes() []int {
	sizes := []int{1800}
	if os.Getenv("AGENT_ARCHIVE_PERF") == "1" {
		sizes = append(sizes, 10000, 25000, 50000)
	}
	return sizes
}

// SeedReadPathArchive creates 2.8 canonical keys/session with uneven source
// history, distinct native identities, mostly unlabeled rows and an unknown
// harness. The first session has a real synthetic source for handoff selection.
// Generation one changes only the oldest one percent, outside the first page.
func SeedReadPathArchive(tb testing.TB, store *MemoryStore, count, generation int) {
	tb.Helper()
	ctx := context.Background()
	for i := range count {
		if generation > 0 && i >= max(1, count/100) {
			continue
		}
		id := fmt.Sprintf("%08x%024x", i+1, i+1)
		harness := "codex"
		if i%20 == 19 {
			harness = "future-agent"
		}
		native := fmt.Sprintf("native-%08x", i+1)
		sourceKey := fmt.Sprintf("sessions/%s/%s/source.%064x.jsonl.gz", harness, id, 1)
		sourceHash := fmt.Sprintf("%064x", 1)
		sourceBytes := 1
		if i == 0 {
			bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: id, NativeSessionID: native, ProjectID: "project-0", Capture: archive.SourceCapture{Harness: archive.Harness{Name: harness}, AdapterName: harness, AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: archive.FilterVersion, CapturedAt: BenchmarkTime}, NativeRecords: []map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Synthetic read path benchmark request"}}}}
			compressed, err := archive.BuildCompressedSource(bundle)
			if err != nil {
				tb.Fatal(err)
			}
			sourceKey, err = archive.SourceObjectKey(bundle, compressed.SHA256)
			if err != nil {
				tb.Fatal(err)
			}
			sourceHash = compressed.SHA256
			sourceBytes = len(compressed.Bytes)
			if generation == 0 {
				if err = store.Put(ctx, sourceKey, compressed.Bytes); err != nil {
					tb.Fatal(err)
				}
			}
		} else if generation == 0 {
			if err := store.Put(ctx, sourceKey, []byte{0}); err != nil {
				tb.Fatal(err)
			}
		}
		// Four historical keys every fifth session yields 0.8 extra keys/session.
		if generation == 0 && i%5 == 0 {
			for j := range 4 {
				key := fmt.Sprintf("sessions/%s/%s/source.%064x.jsonl.gz", harness, id, j+2)
				if err := store.Put(ctx, key, []byte{0}); err != nil {
					tb.Fatal(err)
				}
			}
		}
		version := 0
		if generation > 0 && i < max(1, count/100) {
			version = 1
		}
		projectName := ""
		if i%10 == 0 {
			projectName = "synthetic project"
		}
		source := archive.SourceReference{Key: sourceKey, SHA256: sourceHash, CompressedBytes: sourceBytes}
		m := archive.Metadata{ProjectName: projectName, SchemaVersion: archive.MetadataSchemaVersion, FilterVersion: archive.FilterVersion, SessionID: id, NativeSessionID: native, MachineID: "bench", ProjectID: fmt.Sprintf("project-%d", i%100), Harness: archive.Harness{Name: harness}, CapturedAt: BenchmarkTime.Add(time.Duration(i) * time.Second), Title: fmt.Sprintf("synthetic-%d", version), SourceBundle: source}
		raw, err := json.Marshal(m)
		if err != nil {
			tb.Fatal(err)
		}
		key, err := archive.MetadataObjectKey(harness, id)
		if err != nil {
			tb.Fatal(err)
		}
		if err = store.Put(ctx, key, raw); err != nil {
			tb.Fatal(err)
		}
		_, etag, err := store.GetVersioned(ctx, key)
		if err != nil {
			tb.Fatal(err)
		}
		revision, err := listingindex.NewRevision(key, raw, etag)
		if err != nil {
			tb.Fatal(err)
		}
		if err = listingindex.PutRevision(ctx, store, revision); err != nil {
			tb.Fatal(err)
		}
	}
}
