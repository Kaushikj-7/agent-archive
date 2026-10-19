package nativesessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type countedFiles struct {
	OS
	mu        sync.Mutex
	reads     int64
	active    int
	peak      int
	canonical int
}

type countedFile struct {
	transcriptio.File
	owner *countedFiles
}

func (f *countedFile) ReadAt(p []byte, off int64) (int, error) {
	n, e := f.File.ReadAt(p, off)
	f.owner.mu.Lock()
	f.owner.reads += int64(n)
	f.owner.mu.Unlock()
	return n, e
}

func (f *countedFile) Close() error {
	f.owner.mu.Lock()
	f.owner.active--
	f.owner.mu.Unlock()
	return f.File.Close()
}

func (f *countedFiles) OpenRegular(p string) (transcriptio.File, error) {
	file, e := f.OS.OpenRegular(p)
	if e != nil {
		return nil, e
	}
	f.mu.Lock()
	f.active++
	f.peak = max(f.peak, f.active)
	f.mu.Unlock()
	return &countedFile{file, f}, nil
}

func (f *countedFiles) EvalSymlinks(p string) (string, error) {
	f.mu.Lock()
	f.canonical++
	f.mu.Unlock()
	return f.OS.EvalSymlinks(p)
}

func nativeFixture(tb testing.TB, count int) (StoreRoot, string) {
	tb.Helper()
	root := tb.TempDir()
	cwd := tb.TempDir()
	dir := filepath.Join(root, "project")
	if e := os.Mkdir(dir, 0o700); e != nil {
		tb.Fatal(e)
	}
	for i := range count {
		id := fmt.Sprintf("session-%05d", i)
		raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "Fix widget"}})
		path := filepath.Join(dir, id+".jsonl")
		if e := os.WriteFile(path, append(raw, '\n'), 0o600); e != nil {
			tb.Fatal(e)
		}
		at := time.Unix(int64(i), 0)
		if e := os.Chtimes(path, at, at); e != nil {
			tb.Fatal(e)
		}
	}
	return StoreRoot{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: root}, cwd
}

func discoveryLimits() Limits {
	return Limits{Files: 10000, HeaderBytes: 256 * 1024, RecordBytes: 256 * 1024, TotalBytes: 64 * 1024 * 1024, Workers: 2}
}

func TestDiscoverOrdersScopesAndBoundsReads(t *testing.T) {
	t.Parallel()
	root, cwd := nativeFixture(t, 103)
	files := &countedFiles{}
	result, e := Discover(context.Background(), builtin.NewBuiltins(), files, []StoreRoot{root, root}, Scope{Directories: []string{cwd}}, discoveryLimits())
	if e != nil {
		t.Fatal(e)
	}
	if len(result.Candidates) != 103 || !result.Coverage.IdentityComplete || result.Candidates[0].NativeID != "session-00102" || files.peak > 2 || files.active != 0 || files.reads > result.Coverage.ReservedBytes {
		t.Fatalf("result=%+v peak=%d active=%d reads=%d", result.Coverage, files.peak, files.active, files.reads)
	}
	// Canonical cwd resolution occurs once, besides two containment checks per file.
	if files.canonical > 2*103+3 {
		t.Fatalf("cwd not cached: %d", files.canonical)
	}
	other := t.TempDir()
	r, e := Discover(context.Background(), builtin.NewBuiltins(), files, []StoreRoot{root}, Scope{Directories: []string{other}}, discoveryLimits())
	if e != nil || len(r.Candidates) != 0 {
		t.Fatalf("broadened scope: %v %v", r.Candidates, e)
	}
}

func TestDiscoveryBudgetCapAndUnknownIdentityRefuseCompleteness(t *testing.T) {
	t.Parallel()
	root, cwd := nativeFixture(t, 6)
	limits := discoveryLimits()
	limits.Files = 3
	r, e := Discover(context.Background(), builtin.NewBuiltins(), &countedFiles{}, []StoreRoot{root}, Scope{Directories: []string{cwd}}, limits)
	if e != nil || r.Coverage.IdentityComplete || r.Coverage.LatestComplete(time.Unix(100000, 0)) || r.Coverage.Enumerated > 3 {
		t.Fatalf("cap %+v %v", r.Coverage, e)
	}
	limits = discoveryLimits()
	limits.TotalBytes = 1
	r, e = Discover(context.Background(), builtin.NewBuiltins(), &countedFiles{}, []StoreRoot{root}, Scope{Directories: []string{cwd}}, limits)
	if e != nil || r.Coverage.IdentityComplete || r.Coverage.LatestComplete(time.Unix(100000, 0)) || r.Coverage.ReservedBytes > 1 {
		t.Fatalf("budget %+v %v", r.Coverage, e)
	}
	path := filepath.Join(root.Path, "project", "session-00000.jsonl")
	if e := os.WriteFile(path, []byte(`{"type":"user","cwd":"`+cwd+`"}`+"\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	r, e = Discover(context.Background(), builtin.NewBuiltins(), &countedFiles{}, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
	if e != nil || r.Coverage.IdentityComplete || len(r.Candidates) != 5 {
		t.Fatalf("unknown %+v %v", r.Coverage, e)
	}
}

func BenchmarkDiscoverNativeCatalog(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			root, cwd := nativeFixture(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				files := &countedFiles{}
				r, e := Discover(context.Background(), builtin.NewBuiltins(), files, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
				if e != nil {
					b.Fatal(e)
				}
				b.ReportMetric(float64(files.reads), "read-bytes/op")
				b.ReportMetric(float64(files.peak), "open-files")
				if r.Coverage.Enumerated > 10000 || files.active != 0 {
					b.Fatal("unbounded catalog")
				}
			}
		})
	}
}

// Keep fake ports' signatures checked without adding mutable production hooks.
var _ FileSystem = (*countedFiles)(nil)

func TestDiscoveryCapsTenThousandAndBoundsNoisyOutOfScopeStores(t *testing.T) {
	t.Parallel()
	root, cwd := nativeFixture(t, 10001)
	files := &countedFiles{}
	r, err := Discover(context.Background(), builtin.NewBuiltins(), files, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
	if err != nil || r.Coverage.Enumerated != 10000 || r.Coverage.IdentityComplete || len(r.Candidates) != 10000 || files.active != 0 || files.peak > 2 {
		t.Fatalf("ten-thousand cap %+v %v files=%d", r.Coverage, err, len(r.Candidates))
	}
	other := t.TempDir()
	outside := t.TempDir()
	id := "12345678-1234-1234-1234-123456789099"
	header, e := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": outside}})
	if e != nil {
		t.Fatal(e)
	}
	raw := append(append(header, '\n'), bytes.Repeat([]byte("noise without complete record "), 400000)...)
	if e := os.WriteFile(filepath.Join(other, "rollout-2026-10-01-"+id+".jsonl"), raw, 0o600); e != nil {
		t.Fatal(e)
	}
	small, source := nativeFixture(t, 1)
	files = &countedFiles{}
	r, err = Discover(context.Background(), builtin.NewBuiltins(), files, []StoreRoot{small, {Harness: "codex", Prefix: "rollout-", Suffix: ".jsonl", Path: other, Recursive: true}, small}, Scope{Directories: []string{source}}, discoveryLimits())
	if err != nil || len(r.Candidates) != 1 || r.Coverage.Enumerated != 2 || !r.Coverage.IdentityComplete || files.reads > 2*discoveryLimits().HeaderBytes || files.active != 0 || files.peak > 2 {
		t.Fatalf("noisy multi-store %+v %v reads=%d", r.Coverage, err, files.reads)
	}
}
