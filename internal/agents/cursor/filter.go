package cursor

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"io"
	"time"
)

// Filter selects only the previously accepted Cursor file and composer formats.
type Filter struct{ nativecodec.CursorAdapter }

// Filter consumes verified native input under shared collection limits.
func (f Filter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	if in.File != nil {
		out, err := sourceio.FilterJSONL(ctx, in, c, f.FilterJSONL)
		if err == nil || !agentapi.Deterministic(err) || agentapi.HasFailure(err, agentapi.Unsafe) || agentapi.HasFailure(err, agentapi.Limit) || agentapi.Failure(err) != agentapi.FormatMismatch {
			return out, err
		}
		limit := c.Limits.RecordBytes
		if limit <= 0 {
			limit = archive.MaxRecordBytes
		}
		if in.File.Length() > limit {
			return archive.FilteredTranscript{}, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		input := sourceio.Reader(ctx, in.File, in.File.Length())
		out, err = f.FilterText(input, c.StartedAt)
		if err != nil {
			return out, errors.Join(sourceio.Classify(err), input.ReadError(), sourceio.Classify(in.File.Check()), ctx.Err())
		}
		return out, errors.Join(input.ReadError(), sourceio.Classify(in.File.Check()), ctx.Err())
	}
	if in.Records == nil {
		return archive.FilteredTranscript{}, errors.New("cursor native input required")
	}
	r, ok, err := in.Records.Next(ctx)
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	if !ok || r.Kind != agentapi.ComposerRecord || r.Missing {
		return archive.FilteredTranscript{}, agentapi.Wrap(agentapi.Unsafe, errors.New("cursor composer framing required"))
	}
	out, err := nativecodec.FilterComposerRecords(ctx, r.Raw, func(ctx context.Context) (nativecodec.CursorBubble, bool, error) {
		row, ok, err := in.Records.Next(ctx)
		if err != nil || !ok {
			return nativecodec.CursorBubble{}, ok, err
		}
		if row.Kind != agentapi.BubbleRecord {
			return nativecodec.CursorBubble{}, false, agentapi.Wrap(agentapi.Unsafe, errors.New("unexpected cursor record framing"))
		}
		var value []byte
		if !row.Missing {
			value = row.Raw
		}
		return nativecodec.CursorBubble{ID: row.Key, Value: value}, true, nil
	})
	return out, errors.Join(sourceio.Classify(err), ctx.Err())
}

// Refilter applies current privacy rules to retained native evidence.
func (f Filter) Refilter(ctx context.Context, b archive.SourceBundle, at time.Time) (archive.FilteredTranscript, error) {
	if len(b.NativeText) > 0 {
		return sourceio.RefilterText(ctx, b, at, f.FilterText)
	}
	return sourceio.RefilterJSONL(ctx, f, b)
}

// RefilterBounded applies current privacy rules within the retained output allowance.
func (f Filter) RefilterBounded(ctx context.Context, b archive.SourceBundle, at time.Time, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	if len(b.NativeText) > 0 {
		if limits.FilteredBytes > 0 && len(b.NativeText[0].Content) > int(limits.FilteredBytes) {
			return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
		}
		out, err := sourceio.RefilterText(ctx, b, at, f.FilterText)
		if limits.FilteredBytes > 0 && out.Boundary.RetainedBytes > int(limits.FilteredBytes) {
			return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
		}
		return out, err
	}
	return sourceio.RefilterJSONL(ctx, boundedRetainedFilter{archive.CaptureBoundary{RetainedRecords: limits.Records, RetainedBytes: int(limits.FilteredBytes)}}, b)
}

type boundedRetainedFilter struct{ boundary archive.CaptureBoundary }

func (f boundedRetainedFilter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return nativecodec.FilterCursorRetainedBounded(r, f.boundary)
}

// EvidenceExtends compares retained native facts under unchanged codec versions.
func (Filter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	return nativecodec.EvidenceExtends(previous, candidate)
}
