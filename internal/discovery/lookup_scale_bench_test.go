package discovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

type measuredAdapter struct {
	SourceAdapter
	enumerations int
	entries      int
	headers      int
}

func (a *measuredAdapter) Enumerate(ctx context.Context, root, path string, cookie int64) (SourceBatch, error) {
	a.enumerations++
	batch, err := a.SourceAdapter.Enumerate(ctx, root, path, cookie)
	a.entries += len(batch.Entries)
	return batch, err
}

func (a *measuredAdapter) Inspect(ctx context.Context, source SourceDescriptor) Observation {
	a.headers++
	return a.SourceAdapter.Inspect(ctx, source)
}

// One measured observation at each size. Files are real synthetic old native
// rollouts; the requested tail is beyond the prunable cache at larger sizes.
// Native index rows and filesystem entries scale together. Reported bytes are
// index actual reads/writes and header logical records, separately labelled.
func BenchmarkRequestedLookupScale(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			store, cfg, at, root := fixture(b)
			db := hintDatabase(b, root, true)
			tx, err := db.BeginTx(b.Context(), nil)
			if err != nil {
				b.Fatal(err)
			}
			stmt, err := tx.PrepareContext(b.Context(), "INSERT INTO threads(id,rollout_path,created_at_ms,updated_at_ms) VALUES(?,?,?,?)")
			if err != nil {
				b.Fatal(err)
			}
			wanted := ""
			for n := range count {
				id := writeRollout(b, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), n+1, "sessions")
				path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
				if _, err := stmt.ExecContext(b.Context(), id, path, at.Unix(), at.Unix()); err != nil {
					b.Fatal(err)
				}
				wanted = id
			}
			defer func() { _ = stmt.Close() }()
			defer func() { _ = stmt.Close() }()
			if err := stmt.Close(); err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			if _, err := db.ExecContext(b.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				b.Fatal(err)
			}
			adapter := &measuredAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			started := time.Now()
			passes := 0
			headerBytes := int64(0)
			headerReadBytes, headerReadCalls := int64(0), int64(0)
			catalogBytes := int64(0)
			catalogWrites := 0
			maxCharge := int64(0)
			maxHeap := uint64(0)
			b.ResetTimer()
			for {
				l, err := NewCodexRolloutLookup(b.Context(), store, []string{root})
				if err != nil {
					b.Fatal(err)
				}
				h, err := runWithAdapters(b.Context(), store, cfg, Options{Now: func() time.Time { return at }, Rollouts: l}, []SourceAdapter{adapter})
				if err != nil {
					b.Fatal(err)
				}
				checkpoint, err := os.Stat(filepath.Join(store.Home(), "discovery-catalog.json"))
				if err != nil {
					b.Fatal(err)
				}
				catalogBytes += checkpoint.Size()
				catalogWrites++
				headerBytes += h.Bytes
				headerReadBytes += h.NativeReadBytes
				headerReadCalls += h.NativeReadOperations
				passes++
				complete := !h.Pending
				if complete {
					set, err := l.Thread(b.Context(), wanted)
					if err != nil || set.Current == nil {
						b.Fatal(set, err)
					}
					if err := l.Check(b.Context(), wanted, set.Revision); err != nil {
						b.Fatal(err)
					}
					view := l.indexes[root]
					m := view.snapshot.metrics
					b.ReportMetric(float64(m.NativeBytes), "index-read-B")
					b.ReportMetric(float64(m.PrivateBytes), "index-write-B")
					b.ReportMetric(float64(m.NativeOpens), "index-opens")
					b.ReportMetric(float64(m.NativeReads), "index-reads")
					b.ReportMetric(float64(m.PrivateWrites), "index-writes")
					b.ReportMetric(float64(l.queries), "sql-statements")
				}
				_, peak := l.readBudget.Charged()
				maxCharge = max(maxCharge, peak)
				closeWrites := l.dirty
				if err := l.Close(); err != nil {
					b.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(store.Home(), "discovery-catalog.json"))
				if err != nil {
					b.Fatal(err)
				}
				if closeWrites {
					catalogBytes += info.Size()
					catalogWrites++
				}
				runtime.ReadMemStats(&after)
				maxHeap = max(maxHeap, after.HeapAlloc)
				if complete {
					break
				}
				if passes > count/100+100 {
					b.Fatal("no convergence")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(passes), "passes")
			b.ReportMetric(float64(time.Since(started).Milliseconds()), "convergence-ms")
			b.ReportMetric(float64(adapter.enumerations), "directory-batches")
			b.ReportMetric(float64(adapter.entries), "entry-fingerprints")
			b.ReportMetric(float64(adapter.headers), "header-probes")
			b.ReportMetric(float64(headerBytes), "header-record-B")
			b.ReportMetric(float64(headerReadBytes), "header-native-read-B")
			b.ReportMetric(float64(headerReadCalls), "header-native-reads")
			b.ReportMetric(float64(catalogBytes), "catalog-written-B")
			b.ReportMetric(float64(catalogWrites), "catalog-writes")
			b.ReportMetric(float64(maxCharge), "charged-peak-B")
			b.ReportMetric(float64(maxHeap), "sampled-heap-B")
			b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc), "allocated-B")
		})
	}
}
