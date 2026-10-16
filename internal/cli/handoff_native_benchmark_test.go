package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// BenchmarkNativeFirstPreview isolates enumeration, headers and 50 labels from
// terminal rendering and process launch. Files are synthetic; no app is started.
func BenchmarkNativeFirstPreview(b *testing.B) {
	b.Run("muse", benchmarkMuseNativeFirstPreview)
	for _, count := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			root := b.TempDir()
			cwd := b.TempDir()
			project := filepath.Join(root, "project")
			if err := os.Mkdir(project, 0o700); err != nil {
				b.Fatal(err)
			}
			for i := range count {
				id := fmt.Sprintf("native-%05d", i)
				record, err := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "Fix widget callback"}})
				if err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(project, id+".jsonl"), append(record, '\n'), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			roots := []nativesessions.StoreRoot{{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: root}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				files := &nativeReadMeter{}
				result, err := nativesessions.Discover(context.Background(), productionAgents, files, roots, nativesessions.Scope{Directories: []string{cwd}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
				if err != nil {
					b.Fatal(err)
				}
				catalog := &nativePreviewCatalog{sources: productionAgents, previews: productionAgents, ctx: context.Background(), files: files, candidates: result.Candidates, reserved: result.Coverage.ReservedBytes, stderr: io.Discard, now: time.Unix(0, 0)}
				if _, err := catalog.load(); err != nil {
					b.Fatal(err)
				}
				read, _, active, full := files.counts()
				if active != 0 || full != 0 || read > nativeReadBudget || catalog.next > 50 {
					b.Fatal("pipeline exceeded deterministic bounds")
				}
				b.ReportMetric(float64(read), "read-bytes/op")
				b.ReportMetric(float64(files.peak), "open-files")
				b.ReportMetric(float64(full), "full-filters/op")
			}
		})
	}
}

func benchmarkMuseNativeFirstPreview(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			data := b.TempDir()
			cwd := b.TempDir()
			for i := range count {
				id := fmt.Sprintf("00000000-0000-7000-8000-%012d", i+1)
				writeMuseNative(b, data, id, museNativeBytes(b, "exec", id, cwd))
			}
			registry, counter := museCountingRegistry(b)
			headers, _ := registry.LookupNativeHeaders("muse")
			roots := headers.Roots(agentapi.NativeLocations{DataHome: data}, agentapi.DiscoveryHandoff)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				files := &nativeReadMeter{}
				result, err := nativesessions.Discover(context.Background(), registry, files, roots, nativesessions.Scope{Directories: []string{cwd}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
				if err != nil {
					b.Fatal(err)
				}
				catalog := &nativePreviewCatalog{sources: registry, previews: registry, ctx: context.Background(), files: files, candidates: result.Candidates, reserved: result.Coverage.ReservedBytes, stderr: io.Discard, now: time.Unix(0, 0)}
				if _, err := catalog.load(); err != nil {
					b.Fatal(err)
				}
				read, opens, active, _ := files.counts()
				if active != 0 || counter.calls.Load() != 0 || read > nativeReadBudget || catalog.next > 50 {
					b.Fatal("pipeline exceeded deterministic bounds")
				}
				b.ReportMetric(float64(read), "read-bytes/op")
				b.ReportMetric(float64(opens), "opens/op")
				b.ReportMetric(float64(files.peak), "open-files")
				b.ReportMetric(float64(counter.calls.Load()), "full-filters/op")
				b.ReportMetric(float64(len(result.Candidates)), "verified-ids/op")
			}
		})
	}
}
