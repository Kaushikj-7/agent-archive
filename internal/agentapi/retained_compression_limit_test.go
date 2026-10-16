package agentapi

import (
	"errors"
	"testing"
)

func TestRetainedCompressedOutputPreservesReaderCeiling(t *testing.T) {
	t.Parallel()
	const ceiling = 32 << 20
	budget := NewNativeReadBudget(3 * ceiling)
	input := budget.Reservation()
	if !input.Reserve(ceiling) {
		t.Fatal("reserve caller-owned compressed input")
	}
	raw := make([]byte, ceiling)
	output := budget.Reservation()
	writer := &retainedCompressedWriter{ctx: t.Context(), reservation: output}
	if n, err := writer.Write(raw); err != nil || n != ceiling {
		t.Fatal("exact reader ceiling refused", n, err)
	}
	before, _ := budget.Charged()
	if budget.Available() < ceiling {
		t.Fatal("fixture does not distinguish capacity from the source ceiling")
	}
	if n, err := writer.Write([]byte{1}); n != 0 || !errors.Is(err, ErrReadBudget) {
		t.Fatal("source larger than the retained reader ceiling was emitted", n, err)
	}
	if after, _ := budget.Charged(); after != before || writer.Len() != ceiling {
		t.Fatal("refused output changed bytes or reservation", after, before, writer.Len())
	}
	output.Release(writer.charged)
	input.Release(ceiling)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("bounded output leaked ownership", used)
	}
}
