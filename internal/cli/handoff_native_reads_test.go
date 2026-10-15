package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

type nativeReadMeter struct {
	nativesessions.OS
	mu         sync.Mutex
	bytes      int64
	opens      int
	active     int
	peak       int
	fullStarts int
	beforeFull func()
}

type nativeMeterFile struct {
	transcriptio.File
	meter *nativeReadMeter
}

func (m *nativeReadMeter) OpenRegular(path string) (transcriptio.File, error) {
	file, err := m.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.opens++
	m.active++
	m.peak = max(m.peak, m.active)
	m.mu.Unlock()
	return &nativeMeterFile{File: file, meter: m}, nil
}

func (f *nativeMeterFile) ReadAt(p []byte, offset int64) (int, error) {
	if offset == 0 && len(p) >= 64*1024 && f.meter.beforeFull != nil {
		f.meter.beforeFull()
	}
	n, err := f.File.ReadAt(p, offset)
	f.meter.mu.Lock()
	f.meter.bytes += int64(n)
	// Preview/header windows use 32 KiB buffers. The full filter begins with
	// its existing 64 KiB scanner buffer on this deliberately large fixture.
	if offset == 0 && len(p) >= 64*1024 {
		f.meter.fullStarts++
	}
	f.meter.mu.Unlock()
	return n, err
}

func TestNativeFullReadRejectsWriteDuringFiltering(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	path := f.add(t, "claude", "native-source", "Widget work", 0)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": strings.Repeat("visible work ", 10000)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(record, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	meter := &nativeReadMeter{beforeFull: func() {
		once.Do(func() {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := file.Write([]byte("{}\n")); err != nil {
				t.Error(err)
			}
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
	}}

	f.env.nativeFS = meter
	out, errOut, code := runHandoff(t, f.env, "native-source")
	_, _, active, full := meter.counts()
	if code == 0 || out != "" || !strings.Contains(errOut, "changed while reading") || active != 0 || full != 1 {
		t.Fatalf("write accepted code=%d active=%d full=%d out=%s stderr=%s", code, active, full, out, errOut)
	}
}

func (f *nativeMeterFile) Close() error {
	f.meter.mu.Lock()
	f.meter.active--
	f.meter.mu.Unlock()
	return f.File.Close()
}

func (m *nativeReadMeter) counts() (int64, int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes, m.opens, m.active, m.fullStarts
}

func TestNativeBrowserFilterScrollAndRedrawDoNotReadTranscripts(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	for _, id := range []string{"native-one", "native-two", "native-three"} {
		f.add(t, "claude", id, "Widget work", 0)
	}
	meter := &nativeReadMeter{}
	f.env.nativeFS = meter
	fake := newFakeKeys("/Widget", "\x1b[B", "\x1b[6~", string(fakeResize), "\x1b", "q")
	var beforeBytes int64
	var beforeOpens int
	f.env.openKeys = func(io.Reader) (keyTerminal, bool) {
		beforeBytes, beforeOpens, _, _ = meter.counts()
		return fake, true
	}
	var out strings.Builder
	var errOut strings.Builder
	in := strings.NewReader("")
	f.env.IsTerminal = func(any) bool { return true }
	f.env.TerminalSize = fixedTerminal{100, 12}.terminalSize
	code := Run([]string{"handoff", "--no-preamble"}, in, &out, &errOut, f.env)
	afterBytes, afterOpens, active, full := meter.counts()
	if code != 0 || afterBytes != beforeBytes || afterOpens != beforeOpens || active != 0 || full != 0 {
		t.Fatalf("redraw reads: code=%d before=%d/%d after=%d/%d active=%d full=%d stderr=%s", code, beforeBytes, beforeOpens, afterBytes, afterOpens, active, full, errOut.String())
	}
}

func TestNativeSelectionFiltersFullSourceExactlyOnce(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	path := f.add(t, "claude", "native-selected", "Widget work", 0)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		record, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": strings.Repeat("visible work ", 10000)}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(record, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	meter := &nativeReadMeter{}
	f.env.nativeFS = meter
	out, errOut, code := runHandoff(t, f.env, "native-selected", "--max-bytes", "0")
	_, _, active, full := meter.counts()
	if code != 0 || !strings.Contains(out, "Widget work") || active != 0 || full != 1 {
		t.Fatalf("full count code=%d active=%d full=%d stderr=%s", code, active, full, errOut)
	}
}

func TestNativePreviewBudgetIsCumulativeAcrossExplicitBatches(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	for i := range 53 {
		f.add(t, "claude", strings.Repeat("a", i+1), "Widget work", 0)
	}
	meter := &nativeReadMeter{}
	r, err := nativesessions.Discover(context.Background(), productionAgents, meter, f.env.nativeStoreRoots, nativesessions.Scope{Directories: []string{f.cwd}}, nativesessions.Limits{Files: 100, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	before, _, _, _ := meter.counts()
	n := &nativePreviewCatalog{sources: productionAgents, previews: productionAgents, ctx: context.Background(), files: meter, candidates: r.Candidates, reserved: nativeReadBudget - 1, stderr: io.Discard, now: f.at}
	more, err := n.load()
	after, _, active, full := meter.counts()
	if err != nil || more || !n.exhausted || n.next != 0 || len(n.rows) != 53 || before != after || active != 0 || full != 0 {
		t.Fatalf("exhausted batch reads: %v %v %d %d %+v", more, err, before, after, n)
	}
	for i, row := range n.rows {
		if nativeRowIndex(row, r.Candidates) != i || row.Index != i+1 || row.Title != r.Candidates[i].NativeID || row.Hint != "label not inspected" || row.fields.Title != "" || row.fields.Name != "" {
			t.Fatalf("untruthful fallback row %d: %+v", i, row)
		}
	}
	more, err = n.load()
	after, _, _, _ = meter.counts()
	if err != nil || more || len(n.rows) != 53 || before != after {
		t.Fatal("exhaustion allowed further reads or duplicated fallback rows")
	}
	// Allow exactly the first batch's verified sizes, then attempt another.
	allowance := int64(0)
	for _, c := range r.Candidates[:50] {
		allowance += c.Stamp.Size
	}
	n = &nativePreviewCatalog{sources: productionAgents, previews: productionAgents, ctx: context.Background(), files: meter, candidates: r.Candidates, reserved: nativeReadBudget - allowance, stderr: io.Discard, now: f.at}
	more, err = n.load()
	if err != nil || !more || n.next != 50 {
		t.Fatalf("initial batch %v %v next=%d", more, err, n.next)
	}
	before, _, _, _ = meter.counts()
	more, err = n.load()
	after, _, active, full = meter.counts()
	if err != nil || more || !n.exhausted || n.next != 50 || len(n.rows) != 53 || before != after || n.reserved > nativeReadBudget || active != 0 || full != 0 {
		t.Fatalf("raised cumulative budget %+v %v before=%d after=%d", n, err, before, after)
	}
}

func TestNativeCanceledPreviewQueueClosesAllHandles(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.add(t, "claude", "native-source", "Widget work", 0)
	meter := &nativeReadMeter{}
	r, err := nativesessions.Discover(context.Background(), productionAgents, meter, f.env.nativeStoreRoots, nativesessions.Scope{Directories: []string{f.cwd}}, nativesessions.Limits{Files: 100, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := &nativePreviewCatalog{sources: productionAgents, previews: productionAgents, ctx: ctx, files: meter, candidates: r.Candidates, stderr: io.Discard, now: f.at}
	_, err = n.load()
	_, _, active, _ := meter.counts()
	if !errors.Is(err, context.Canceled) || active != 0 {
		t.Fatalf("cancel err=%v active=%d", err, active)
	}
}
