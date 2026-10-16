package collector

import (
	"strconv"
	"strings"
	"testing"
)

func TestKeptTranscriptPreservesWholeRecords(t *testing.T) {
	header := `{"type":"turn_context","model":"gpt-test"}` + "\n"
	record := `{"type":"response_item","timestamp":"2026-01-01T00:01:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("kept words ", 6000) + `"}]}}` + "\n"
	for _, size := range []int{-1, 0, 1, len(header), len(header) + 1, len(header) + len(record), len(header) + len(record) + 1, 1 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			rows := 0
			if size > len(header) {
				rows = (size - len(header) + len(record) - 1) / len(record)
			}
			want := header + strings.Repeat(record, rows)
			if got := keptTranscript(size); got != want {
				t.Fatalf("fixture bytes changed for requested size %d: got %d bytes, want %d", size, len(got), len(want))
			}
		})
	}
}

func BenchmarkKeptTranscript(b *testing.B) {
	for _, size := range []int{(40 << 20) / 16, 40 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if got := keptTranscript(size); len(got) < size {
					b.Fatal("fixture did not reach requested size")
				}
			}
		})
	}
}
