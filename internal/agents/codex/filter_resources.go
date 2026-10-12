package codex

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// nativeScannerCharge counts bounded framing in the already admitted snapshot.
// Check verifies the snapshot contract before the codec's second read. This
// additional I/O is measured; no native content is memoized or persisted here.
func nativeScannerCharge(ctx context.Context, file agentapi.FileInput, limits agentapi.ReadLimits, budget *agentapi.NativeReadBudget) (int64, error) {
	const countScratch = 32 << 10
	if !budget.Reserve(countScratch) {
		return 0, errFilterBudget
	}
	defer budget.Release(countScratch)
	length := file.Length()
	if length < 0 {
		return 0, errFilterBudget
	}
	if limits.RawBytes > 0 && length > limits.RawBytes {
		return 0, agentapi.RawLimit(length, limits.RawBytes, nil)
	}
	maxRecord := limits.RecordBytes
	if maxRecord <= 0 {
		maxRecord = archive.MaxRecordBytes
	}
	var buffer [countScratch]byte
	var line, longest int64
	for offset := int64(0); offset < length; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		want := min(int64(len(buffer)), length-offset)
		n, err := file.ReadAt(buffer[:want], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if n == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		for _, b := range buffer[:n] {
			if b == '\n' {
				longest = max(longest, line)
				line = 0
			} else {
				line++
			}
			if line > maxRecord {
				return 0, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
			}
		}
		offset += int64(n)
	}
	if err := file.Check(); err != nil {
		return 0, agentapi.Wrap(agentapi.Changed, err)
	}
	// The codec ignores an incomplete final record but its scanner can buffer it.
	longest = max(longest, line)
	capacity := int64(min(64<<10, maxRecord+1))
	for capacity < longest+1 && capacity < maxRecord+1 {
		capacity = min(capacity*2, maxRecord+1)
	}
	return capacity, nil
}

var errFilterBudget = agentapi.ReadBudgetLimit(errors.New("native filtered work exceeds shared data budget"))

type leasedHistoryInput struct {
	input  agentapi.RecordInput
	budget *agentapi.NativeReadBudget
	owned  *int64
}

func (r *leasedHistoryInput) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	row, more, err := r.input.Next(ctx)
	if err != nil || !more || row.Kind != agentapi.CodexHistoryHeader || row.History == nil {
		return row, more, err
	}
	available := r.budget.Available()
	n, err := jsonwire.Bound(ctx, row.History, available)
	if errors.Is(err, jsonwire.ErrLimit) {
		r.budget.Refuse(available + 1)
	}
	if err != nil {
		return agentapi.NativeRecord{}, false, errors.Join(errFilterBudget, err)
	}
	if !r.budget.Reserve(n) {
		return agentapi.NativeRecord{}, false, errFilterBudget
	}
	*r.owned += n
	// These identifiers may otherwise borrow a provider/cache whose snapshot is
	// closed before the filtered records' consumers. Reserve before cloning them.
	h := *row.History
	h.ThreadID = strings.Clone(h.ThreadID)
	h.ActiveRolloutID = strings.Clone(h.ActiveRolloutID)
	h.Spans = append([]archive.HistorySpan(nil), h.Spans...)
	for i := range h.Spans {
		h.Spans[i].ThreadID = strings.Clone(h.Spans[i].ThreadID)
		h.Spans[i].RolloutID = strings.Clone(h.Spans[i].RolloutID)
	}
	if h.OwnStart != nil {
		value := *h.OwnStart
		h.OwnStart = &value
	}
	row.History = &h
	return row, true, nil
}
