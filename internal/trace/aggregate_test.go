package trace

import (
	"io"
	"strings"
	"sync"
	"testing"
)

func TestAggregateReadCountsAndReset(t *testing.T) {
	input := strings.NewReader("disabled")
	reader, finish := CountReads(input, HeaderBytes)
	if reader != input {
		t.Fatal("disabled tracing allocated wrapper")
	}
	finish()
	disable := Enable()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			r, done := CountReads(strings.NewReader("synthetic"), HeaderBytes)
			_, err := io.Copy(io.Discard, r)
			if err != nil {
				t.Error(err)
			}
			done()
			Add(AdapterInvocations, 1)
			Maximum(PeakWorkers, 2)
		})
	}
	wg.Wait()
	var out strings.Builder
	Write(&out)
	if !strings.Contains(out.String(), "header_bytes 900") || !strings.Contains(out.String(), "adapter_invocations 100") || !strings.Contains(out.String(), "peak_workers 2") {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), "synthetic") {
		t.Fatal("trace leaked input")
	}
	disable()
	stop := Enable()
	defer stop()
	out.Reset()
	Write(&out)
	if strings.Contains(out.String(), "native totals") {
		t.Fatal("new command reused aggregates")
	}
}
