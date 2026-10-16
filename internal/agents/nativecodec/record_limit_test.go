package nativecodec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// bigToolResultRecord is a Claude Code user record carrying a tool result,
// shaped like the ones observed in large local sessions: a small message
// content block, and the bulk (bulkBytes of it) in toolUseResult, a field the
// filter drops.
func bigToolResultRecord(bulkBytes int) string {
	bulk := strings.Repeat("x", bulkBytes)
	return fmt.Sprintf(`{"type":"user","uuid":"r1","sessionId":"native-claude","timestamp":"2026-09-22T12:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"short summary of the output"}]},"toolUseResult":{"stdout":%q,"stderr":""}}`, bulk)
}

const smallClaudePrompt = `{"type":"user","uuid":"p1","sessionId":"native-claude","timestamp":"2026-09-22T12:00:00Z","message":{"role":"user","content":"Run the tests."}}`

// Filter 4 stopped at 2 MB per record and refused the whole transcript. A 5 MB
// record whose bulk is a dropped field now filters, and only allowed fields
// are retained.
func TestFilterReadsARecordOverTheOldTwoMegabyteLimit(t *testing.T) {
	t.Parallel()
	jsonl := smallClaudePrompt + "\n" + bigToolResultRecord(5<<20) + "\n"
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("a 5 MB record was refused: %v", err)
	}
	if len(filtered.Records) != 2 {
		t.Fatalf("retained %d records, want 2", len(filtered.Records))
	}
	big := filtered.Records[1]
	if len(big) > 4096 || bytes.Contains(big, []byte("toolUseResult")) || bytes.Contains(big, []byte("xxxxxxxx")) {
		t.Fatalf("the dropped field survived (%d bytes retained)", len(big))
	}
	var record map[string]any
	if err := json.Unmarshal(big, &record); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(big), "toolu_1") || !strings.Contains(string(big), "short summary of the output") {
		t.Fatalf("allowed fields were not retained: %s", big)
	}
	if detail := gapDetail(filtered.Gaps, "unknown_field_omitted"); !strings.Contains(detail, "toolUseResult") {
		t.Fatalf("the dropped field is not reported: %q", detail)
	}
}

func withRecordLimit(t *testing.T, limit int) {
	t.Helper()
	saved := maxRecordBytes
	maxRecordBytes = limit
	t.Cleanup(func() { maxRecordBytes = saved })
}

// A record longer than the limit is refused with ErrRecordTooLarge, a distinct
// FilterError, and a record of exactly the limit is still read.
func TestFilterRefusesARecordOverTheLimit(t *testing.T) {
	withRecordLimit(t, 4096)
	exact := bigToolResultRecord(0)
	exact = exact[:len(exact)-2] + `,"pad":"` + strings.Repeat("y", 4096-len(exact)-9) + `"}}`
	if len(exact) != 4096 {
		t.Fatalf("test record is %d bytes, want 4096", len(exact))
	}
	for _, trailing := range []string{"\n", ""} {
		if _, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(smallClaudePrompt + "\n" + exact + trailing)); err != nil {
			t.Fatalf("a record of exactly the limit (trailing %q) was refused: %v", trailing, err)
		}
	}
	_, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(smallClaudePrompt + "\n" + bigToolResultRecord(8192) + "\n" + smallClaudePrompt + "\n"))
	if !errors.Is(err, ErrRecordTooLarge) || !IsFilterError(err) {
		t.Fatalf("err = %v, want ErrRecordTooLarge", err)
	}
}

// The memory ceiling for one very large record, recorded in the ledger. The
// record's bulk is in a field the filter drops, so the retained output is
// tiny; what costs memory is reading the line (the scanner's buffer grows to
// hold it) and decoding it into map[string]any (which copies the bulk into a
// Go string). The test measures both the peak heap in use, sampled while the
// filter runs, and the total allocated.
func TestLargeRecordMemoryCeiling(t *testing.T) {
	recordBytes := uint64(32 << 20)
	if testing.Short() {
		recordBytes = 2 << 20
	}
	jsonl := smallClaudePrompt + "\n" + bigToolResultRecord(int(recordBytes)) + "\n"

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var (
		peak uint64
		stop = make(chan struct{})
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var stats runtime.MemStats
		for {
			runtime.ReadMemStats(&stats)
			if stats.HeapInuse > peak {
				peak = stats.HeapInuse
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(jsonl))
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	peakAbove := int64(peak) - int64(before.HeapInuse)
	t.Logf("%d MiB record (bulk in a dropped field): peak heap in use %.1f MiB above baseline (sampled), %.1f MiB allocated in total, %d bytes retained",
		recordBytes>>20, float64(peakAbove)/(1<<20), float64(allocated)/(1<<20), filtered.Boundary.RetainedBytes)
	// A loose regression bound, not a target: the measured cost is a few
	// times the record's size.
	if allocated > 12*recordBytes {
		t.Fatalf("filtering one %d MiB record allocated %.1f MiB", recordBytes>>20, float64(allocated)/(1<<20))
	}
}
