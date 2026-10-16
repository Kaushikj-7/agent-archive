package muse

import (
	"context"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Previewer derives safe name and activity facts without conversation graphs.
type Previewer struct{}

// PreviewRecord interprets one bounded record. A lone run prompt cannot establish
// human origin, so prompt titles require the complete retained conversation.
func (Previewer) PreviewRecord(ctx context.Context, raw []byte) (archive.RecordPreview, error) {
	if err := ctx.Err(); err != nil {
		return archive.RecordPreview{}, err
	}
	r, err := envelope(raw, position{Line: 1})
	if err != nil {
		rows, frameErr := frame(raw, 1)
		if frameErr == nil && initialAuthority(rows) && validFields(rows[0]) && validFields(rows[1]) && rows[0].Timestamp <= exactMax {
			return archive.RecordPreview{Activity: time.UnixMicro(rows[0].Timestamp).UTC()}, nil
		}
		return partialPreview(), nil
	}
	if !validFields(r) || r.Timestamp > exactMax {
		return partialPreview(), nil
	}
	safe, gaps, ok := previewPrivacy(r)
	if !ok {
		return partialPreview(), nil
	}
	var name, nativeID string
	var source archive.SessionNameSource
	if safe.Type == payloadSessionNameChanged {
		if str(safe.Fields["session_id"]) != safe.Stream.ID || str(safe.Fields["source"]) != "automatic" {
			return partialPreview(), nil
		}
		name, nativeID, source = str(safe.Fields["new_name"]), safe.Stream.ID, archive.SessionNameGenerated
	}
	return archive.RecordPreview{Activity: time.UnixMicro(safe.Timestamp).UTC(), Gaps: gaps, Name: name, NameNativeID: nativeID, NameSource: source}, nil
}

func previewPrivacy(r retained) (retained, []archive.CaptureGap, bool) {
	gaps := privacyGaps{}
	safe, err := privacyWithGaps(r, 1, gaps.add)
	return safe, gaps.list(), err == nil
}

func partialPreview() archive.RecordPreview {
	return archive.RecordPreview{Gaps: []archive.CaptureGap{{Code: "preview_partial"}}}
}
