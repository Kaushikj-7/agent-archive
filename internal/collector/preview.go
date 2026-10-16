package collector

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// PreviewLimits bounds both windows and each record independently.
type PreviewLimits struct {
	HeadBytes   int64
	TailBytes   int64
	RecordBytes int64
}

// TranscriptPreview contains filtered labels, never raw excerpts.
type TranscriptPreview struct {
	NativeID         string
	Name             string
	NameSource       archive.SessionNameSource
	Title            string
	Branch           string
	RecordedActivity time.Time
	NameComplete     bool
	Bytes            int64
	Gaps             []archive.CaptureGap
}

// PreviewTranscript reads bounded complete head/tail records on the verified handle.
func PreviewTranscript(ctx context.Context, snapshot agentapi.FileInput, preview agentapi.RecordPreviewer, limits PreviewLimits, nativeID string) (TranscriptPreview, error) {
	var recordErr error
	accumulator := archive.PreviewAccumulator{NativeID: nativeID}
	visit := func(first bool) func([]byte) bool {
		return func(record []byte) bool {
			var facts archive.RecordPreview
			facts, recordErr = preview.PreviewRecord(ctx, record)
			if recordErr == nil {
				accumulator.AddFacts(facts, first)
			}
			return recordErr == nil
		}
	}
	head, err := snapshot.Records(ctx, false, limits.HeadBytes, limits.RecordBytes, visit(true))
	out := TranscriptPreview{Bytes: head.Bytes, NativeID: accumulator.NativeID}
	if err != nil {
		return out, err
	}
	if recordErr != nil {
		return out, recordErr
	}
	out.NameComplete = head.Complete
	if !head.Complete && snapshot.Length() > limits.HeadBytes {
		tail, e := snapshot.Records(ctx, true, limits.TailBytes, limits.RecordBytes, visit(false))
		out.Bytes += tail.Bytes
		if e != nil {
			return out, e
		}
		if recordErr != nil {
			return out, recordErr
		}
	}
	out.Name, out.Title, out.Branch = accumulator.Labels.Name, accumulator.Labels.Title, accumulator.Labels.Branch
	out.NameSource = accumulator.NameSource
	out.RecordedActivity, out.Gaps = accumulator.Activity, accumulator.Gaps
	partialGap := false
	for _, gap := range out.Gaps {
		if gap.Code == "preview_partial" {
			out.NameComplete = false
			partialGap = true
		}
	}
	if !out.NameComplete && !partialGap {
		out.Gaps = append(out.Gaps, archive.CaptureGap{Code: "preview_partial"})
	}
	return out, nil
}
