package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// passLedger is the shared ledger a collector pass gives its sources, and
// sessionCeiling the share one session's scan may hold of it
// (collector.SessionReadCeiling).
const (
	passLedger           = agentapi.PassReadBudget
	sessionCeiling int64 = 160 << 20
)

// largeRollout writes an ordinary Codex rollout of at least size bytes: a
// short user message every turn, and a command whose output, the bulk of a
// real rollout, is outputBytes long.
func largeRollout(tb testing.TB, dir string, size int64, outputBytes int) (agentapi.SourceRef, int) {
	tb.Helper()
	ref := agentapi.SourceRef{Path: filepath.Join(dir, "rollout-2026-10-01T12-00-00-"+threadA+".jsonl")}
	file, err := os.Create(ref.Path)
	if err != nil {
		tb.Fatal(err)
	}
	w := bufio.NewWriterSize(file, 1<<20)
	enc := json.NewEncoder(w)
	meta := map[string]any{"id": threadA, "cwd": dir, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli"}
	if err := enc.Encode(map[string]any{"type": "session_meta", "timestamp": "2026-10-01T12:00:00Z", "payload": meta}); err != nil {
		tb.Fatal(err)
	}
	output := strings.Repeat("tool output line\n", outputBytes/17+1)[:outputBytes]
	turns := 0
	var written int64
	for written < size {
		user := map[string]any{"type": "response_item", "timestamp": "2026-10-01T12:01:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "please run the check"}}}}
		call := map[string]any{"type": "event_msg", "timestamp": "2026-10-01T12:01:01Z", "payload": map[string]any{"type": "exec_command_end", "call_id": "c", "aggregated_output": output}}
		for _, record := range []map[string]any{user, call} {
			if err := enc.Encode(record); err != nil {
				tb.Fatal(err)
			}
		}
		turns++
		if err := w.Flush(); err != nil {
			tb.Fatal(err)
		}
		info, err := file.Stat()
		if err != nil {
			tb.Fatal(err)
		}
		written = info.Size()
	}
	if err := file.Close(); err != nil {
		tb.Fatal(err)
	}
	return ref, turns
}

// filterRollout reads and filters ref as a collector pass does: one shared
// ledger, the session's scan under its own ceiling.
func filterRollout(tb testing.TB, ref agentapi.SourceRef, ledger *agentapi.NativeReadBudget, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	tb.Helper()
	defer ledger.BeginScope(sessionCeiling)()
	pass, err := (SourceProvider{}).OpenPass(tb.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
	if err != nil {
		tb.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			tb.Error(err)
		}
	}()
	snap, err := pass.Read(tb.Context(), ref, limits)
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	defer func() {
		if err := snap.Close(); err != nil {
			tb.Error(err)
		}
	}()
	c := agentapi.FilterContext{Filename: filepath.Base(ref.Path), Limits: limits}
	out, release, err := (Filter{}).FilterLeased(tb.Context(), snap.Input(), c, ledger)
	defer release()
	return out, err
}

// TestLargeRolloutCapturesWithinSessionBudget: a 40 MB rollout, mostly tool
// output, is read within one session's share of a pass. The ledger's peak
// follows the longest record and what the filter keeps, not the file: the
// whole file was charged before, beside a scanner buffer as long as the file,
// so the same rollout exhausted the shared budget on every pass.
func TestLargeRolloutCapturesWithinSessionBudget(t *testing.T) {
	t.Parallel()
	ref, turns := largeRollout(t, t.TempDir(), 40<<20, 64<<10)
	ledger := agentapi.NewNativeReadBudget(passLedger)
	limits := agentapi.ReadLimits{RawBytes: archive.MaxCodexRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes}
	out, err := filterRollout(t, ref, ledger, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 2*turns+1 {
		t.Fatalf("records = %d, want %d", len(out.Records), 2*turns+1)
	}
	used, peak := ledger.Charged()
	if used != 0 {
		t.Fatalf("charge leaked: %d", used)
	}
	// One 64 KiB record buffered and decoded at a time, plus what is kept.
	bound := int64(out.Boundary.RetainedBytes) + 8<<20
	if peak > bound || peak > 40<<20/2 {
		t.Fatalf("peak charge %d MiB for a 40 MB rollout keeping %d MiB; bound %d MiB", peak>>20, out.Boundary.RetainedBytes>>20, bound>>20)
	}
}

// TestRolloutOverRawLimitNamesItsSizes: a rollout over the raw limit is
// refused before it is read, with the sizes and an observation a gap can
// be recorded at.
func TestRolloutOverRawLimitNamesItsSizes(t *testing.T) {
	t.Parallel()
	ref, _ := largeRollout(t, t.TempDir(), 2<<20, 64<<10)
	info, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	ledger := agentapi.NewNativeReadBudget(passLedger)
	_, err = filterRollout(t, ref, ledger, agentapi.ReadLimits{RawBytes: 1 << 20, RecordBytes: archive.MaxRecordBytes})
	var raw *agentapi.RawLimitError
	if !errors.As(err, &raw) || !errors.Is(err, agentapi.ErrRawLimit) || raw.Size != info.Size() || raw.Limit != 1<<20 {
		t.Fatalf("raw limit refusal: %v", err)
	}
	if o, ok := agentapi.ErrorObservation(err); !ok || o.Size != info.Size() {
		t.Fatalf("no observation to record the gap at: %+v", o)
	}
	if errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("a size limit was reported as memory pressure")
	}
}

// TestRolloutOverSessionCeilingIsTheSessionsOwn: keeping more than one
// session's share is refused at the session's ceiling (ScopeExceeded),
// which the collector records as a gap, while the same read under shared
// pressure from others is not.
func TestRolloutOverSessionCeilingIsTheSessionsOwn(t *testing.T) {
	t.Parallel()
	// Every user message is kept: 4 MiB of them against a 1 MiB ceiling.
	dir := t.TempDir()
	text := strings.Repeat("kept words ", 6000)
	texts := make([]string, 64)
	for i := range texts {
		texts[i] = text
	}
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, texts...)
	limits := agentapi.ReadLimits{RecordBytes: archive.MaxRecordBytes}
	read := func(ledger *agentapi.NativeReadBudget, ceiling int64) error {
		defer ledger.BeginScope(ceiling)()
		pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pass.Close() }()
		snap, err := pass.Read(t.Context(), ref, limits)
		if err != nil {
			return err
		}
		defer func() { _ = snap.Close() }()
		_, release, err := (Filter{}).FilterLeased(t.Context(), snap.Input(), agentapi.FilterContext{Filename: filepath.Base(ref.Path), Limits: limits}, ledger)
		release()
		if ledger.ScopeExceeded() {
			return errors.Join(err, errScopeExceeded)
		}
		return err
	}
	own := agentapi.NewNativeReadBudget(passLedger)
	if err := read(own, 1<<20); !errors.Is(err, agentapi.ErrReadBudget) || !errors.Is(err, errScopeExceeded) {
		t.Fatalf("over the session ceiling: %v", err)
	}
	if strings.Contains(errorText(read(own, 1<<20)), "unsafe source format") {
		t.Fatal("memory refusal reported as an unsafe source format")
	}
	shared := agentapi.NewNativeReadBudget(passLedger)
	held := shared.Available() - (1 << 20)
	if !shared.Reserve(held) {
		t.Fatal("reserve")
	}
	if err := read(shared, sessionCeiling); !errors.Is(err, agentapi.ErrReadBudget) || errors.Is(err, errScopeExceeded) {
		t.Fatalf("under others' pressure: %v", err)
	}
	shared.Release(held)
	if err := read(shared, sessionCeiling); err != nil {
		t.Fatal("retry with the ledger free", err)
	}
}

var errScopeExceeded = errors.New("scope exceeded")

// maxRSSBytes is ru_maxrss in bytes: macOS reports bytes, Linux KiB.
func maxRSSBytes(usage syscall.Rusage) int64 {
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	return usage.Maxrss << 10
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// BenchmarkFilterRollout256MB documents peak memory for a rollout at the
// general raw limit.
// It reports the ledger's peak charge and the process's peak RSS.
func BenchmarkFilterRollout256MB(b *testing.B) {
	size := int64(256 << 20)
	ref, _ := largeRollout(b, b.TempDir(), size, 64<<10)
	limits := agentapi.ReadLimits{RawBytes: archive.MaxCodexRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes}
	runtime.GC()
	b.ResetTimer()
	var peak int64
	for b.Loop() {
		ledger := agentapi.NewNativeReadBudget(passLedger)
		if _, err := filterRollout(b, ref, ledger, limits); err != nil {
			b.Fatal(err)
		}
		_, p := ledger.Charged()
		peak = max(peak, p)
	}
	b.StopTimer()
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	b.ReportMetric(float64(peak)/(1<<20), "ledger-peak-MiB")
	b.ReportMetric(float64(maxRSSBytes(usage))/(1<<20), "maxrss-MiB")
	b.SetBytes(size)
}
