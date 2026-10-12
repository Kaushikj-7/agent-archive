package codex

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"io"
	"sync"
	"time"
)

// SourceProvider reads verified bounded transcript files.
type SourceProvider struct{}

// Filter delegates the current pure privacy codec behind the native input port.
type Filter struct{ nativecodec.CodexAdapter }

// Filter consumes verified native input under shared collection limits.
func (f Filter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	return f.filterEncoded(ctx, in, c, nil, nil)
}

func (f Filter) filterEncoded(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext, encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error)) (archive.FilteredTranscript, error) {
	if in.Records != nil {
		return filterHistoryEncoded(ctx, in.Records, encoder, beforeRecord, archive.CaptureBoundary{RetainedRecords: c.Limits.Records, RetainedBytes: int(c.Limits.FilteredBytes)})
	}
	return sourceio.FilterJSONL(ctx, in, c, func(r io.Reader) (archive.FilteredTranscript, error) {
		return nativecodec.FilterCodexCaptureEncodedJSONL(r, c.Filename, encoder, beforeRecord, archive.CaptureBoundary{RetainedRecords: c.Limits.Records, RetainedBytes: int(c.Limits.FilteredBytes)})
	})
}

// Refilter applies current privacy rules to retained native evidence.
func (f Filter) Refilter(ctx context.Context, b archive.SourceBundle, at time.Time) (archive.FilteredTranscript, error) {
	if b.History == nil && b.SchemaVersion != archive.HistorySourceSchemaVersion {
		return sourceio.RefilterJSONL(ctx, f, b)
	}
	return f.RefilterBounded(ctx, b, at, agentapi.ReadLimits{Records: archive.MaxHistoryRecords, FilteredBytes: 32 << 20})
}

// RefilterBounded refuses output beyond the caller's reserved ceiling before accumulation.
func (f Filter) RefilterBounded(ctx context.Context, b archive.SourceBundle, _ time.Time, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	boundary := archive.CaptureBoundary{RetainedRecords: limits.Records, RetainedBytes: int(limits.FilteredBytes)}
	if b.SchemaVersion == archive.HistorySourceSchemaVersion || b.History != nil {
		if err := b.ValidateHistory(); err != nil {
			return archive.FilteredTranscript{}, err
		}
		return filterHistoryEncoded(ctx, &retainedHistory{bundle: b}, retainedEncoder(ctx, limits), nil, boundary)
	}
	return sourceio.RefilterJSONL(ctx, boundedRetainedFilter{boundary: boundary, encoder: retainedEncoder(ctx, limits)}, b)
}

type boundedRetainedFilter struct {
	boundary     archive.CaptureBoundary
	encoder      func(map[string]any) ([]byte, error)
	beforeRecord func(int) (func(), error)
}

func (f boundedRetainedFilter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return nativecodec.FilterCodexRetainedEncodedJSONL(r, f.encoder, f.beforeRecord, f.boundary)
}

// EvidenceExtends compares retained native facts under unchanged codec versions.
func (Filter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	if !sameHistory(previous, candidate) {
		return false
	}
	return nativecodec.EvidenceExtends(previous, candidate)
}

// MeaningfulRevisionRecord excludes physical headers from revision activity.
func (Filter) MeaningfulRevisionRecord(b archive.SourceBundle, i int) bool {
	if i < 0 || i >= len(b.NativeRecords) {
		return false
	}
	kind, _ := b.NativeRecords[i]["type"].(string)
	return kind != "session_meta"
}

func retainedEncoder(ctx context.Context, limits agentapi.ReadLimits) func(map[string]any) ([]byte, error) {
	return func(value map[string]any) ([]byte, error) {
		n, err := jsonwire.Bound(ctx, value, limits.FilteredBytes)
		if err != nil {
			return nil, agentapi.ReadBudgetLimit(err)
		}
		if !limits.ReadBudget.Reserve(n) {
			return nil, agentapi.ReadBudgetLimit(errors.New("safe record encoder exceeds shared budget"))
		}
		defer limits.ReadBudget.Release(n)
		return json.Marshal(value)
	}
}

// FilterLeased charges decoded row scratch before decode and both JSON encoding
// owners before serialization. Returned immutable rows remain charged until the
// caller releases them. Native input continues to belong to its SourceSnapshot.
func (f Filter) FilterLeased(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	return f.filterLeased(ctx, in, c, budget, nil)
}

func (f Filter) filterLeased(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext, budget *agentapi.NativeReadBudget, retained *archive.SourceBundle) (archive.FilteredTranscript, func(), error) {
	var total int64
	if in.File != nil {
		scanner, err := nativeScannerCharge(ctx, in.File, c.Limits, budget)
		if err != nil {
			return archive.FilteredTranscript{}, func() {}, err
		}
		if !budget.Reserve(scanner) {
			return archive.FilteredTranscript{}, func() {}, errFilterBudget
		}
		defer budget.Release(scanner)
	}
	history := in.Records != nil
	if history {
		in.Records = &leasedHistoryInput{input: in.Records, budget: budget, owned: &total}
	}
	var once sync.Once
	release := func() { once.Do(func() { budget.Release(total) }) }
	before := func(n int) (func(), error) {
		if !budget.Reserve(int64(n)) {
			return nil, agentapi.ReadBudgetLimit(errors.New("native decoded row exceeds shared budget"))
		}
		return func() { budget.Release(int64(n)) }, nil
	}
	encoder := func(value map[string]any) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		const countScratch = 32 << 10
		if !budget.Reserve(countScratch) {
			return nil, agentapi.ReadBudgetLimit(errors.New("native encoding preflight exceeds shared budget"))
		}
		available := budget.Available()
		n, err := jsonwire.Bound(ctx, value, available/2)
		if errors.Is(err, jsonwire.ErrLimit) {
			// The encoding and its retained copy need more than all that
			// remains, beside the scratch still held.
			budget.Refuse(available + 2)
		}
		budget.Release(countScratch)
		if err != nil {
			return nil, agentapi.ReadBudgetLimit(err)
		}
		if !budget.Reserve(n) {
			return nil, agentapi.ReadBudgetLimit(errors.New("native encoder exceeds shared budget"))
		}
		defer budget.Release(n)
		if !budget.Reserve(n) {
			return nil, agentapi.ReadBudgetLimit(errors.New("native filtered output exceeds shared budget"))
		}
		ordinalBytes := int64(0)
		if history {
			ordinalBytes = 8
			if !budget.Reserve(ordinalBytes) {
				budget.Release(n)
				return nil, errFilterBudget
			}
		}
		data, err := json.Marshal(value)
		if err != nil {
			budget.Release(n + ordinalBytes)
			return nil, err
		}
		actual := int64(len(data))
		if actual > n {
			budget.Release(n + ordinalBytes)
			return nil, agentapi.ReadBudgetLimit(jsonwire.ErrLimit)
		}
		budget.Release(n - actual)
		total += actual + ordinalBytes
		return data, nil
	}
	var out archive.FilteredTranscript
	var err error
	if retained != nil && retained.History == nil {
		out, err = sourceio.RefilterJSONL(ctx, boundedRetainedFilter{boundary: archive.CaptureBoundary{RetainedRecords: c.Limits.Records, RetainedBytes: int(c.Limits.FilteredBytes)}, encoder: encoder, beforeRecord: before}, *retained)
	} else {
		out, err = f.filterEncoded(ctx, in, c, encoder, before)
	}
	if err != nil {
		release()
		return archive.FilteredTranscript{}, func() {}, err
	}
	return out, release, nil
}

// RefilterLeased grows the owned output charge per safe row rather than holding
// the maximum output ceiling for a small retained snapshot.
func (f Filter) RefilterLeased(ctx context.Context, b archive.SourceBundle, _ time.Time, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	if err := b.ValidateHistory(); err != nil {
		return archive.FilteredTranscript{}, func() {}, err
	}
	var input agentapi.RecordInput
	if b.History != nil {
		input = &retainedHistory{bundle: b}
	}
	in := agentapi.NativeInput{Records: input}
	c := agentapi.FilterContext{Limits: agentapi.ReadLimits{Records: archive.MaxHistoryRecords, FilteredBytes: 32 << 20}}
	return f.filterLeased(ctx, in, c, budget, &b)
}

// LeasedFilterFor admits this concrete codec instance. A decorator must opt in
// explicitly and preserve its own Filter behavior through FilterLeased.
func (Filter) LeasedFilterFor(adapter archive.Adapter) bool { _, ok := adapter.(Filter); return ok }
