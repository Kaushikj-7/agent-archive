package nativecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
)

// Preview filters a single complete native record and retains only bounded display facts.
func Preview(ctx context.Context, adapter interface {
	FilterJSONL(io.Reader) (archive.FilteredTranscript, error)
}, record []byte) (archive.RecordPreview, error) {
	if err := ctx.Err(); err != nil {
		return archive.RecordPreview{}, err
	}
	filtered, err := adapter.FilterJSONL(bytes.NewReader(record))
	if err != nil && !errors.Is(err, archive.ErrUnsafeSourceFormat) {
		return archive.RecordPreview{}, err
	}
	out := archive.RecordPreview{Gaps: filtered.Gaps}
	for _, encoded := range filtered.Records {
		var safe map[string]any
		if err := json.Unmarshal(encoded, &safe); err != nil {
			return archive.RecordPreview{}, err
		}
		if isSidechainRecord(safe) {
			continue
		}
		if isClaudeTitleRecord(safe) {
			facts := claudeTitlePreview(safe)
			out.Name, out.NameNativeID, out.NameSource = facts.Name, facts.NameNativeID, facts.NameSource
			continue
		}
		if link, ok := pullRequestLink(safe); ok {
			out.PullRequest = link
		}
		if branch := archive.ValidBranch(firstStringDeep(safe, "gitBranch")); branch != "HEAD" {
			out.Branch = branch
		}
		out.Activity = parseNativeTimestamp(safe)
		_, text, kind, ok := visibleMessage(safe)
		if ok {
			out.Kind = refineUserKind(safe, kind, text)
			if out.Kind == archive.TurnKindHumanPrompt {
				out.Title = archive.CollapseSessionTitle(text)
			}
			if out.Kind == archive.TurnKindLocalCommand {
				out.Command = archive.CollapseSessionTitle(text)
			}
		}
	}
	return out, nil
}

// Missing IDs on legacy title records are accepted only from the same source
// bundle or verified preview handle. A present ID must match its owner.
func claudeTitlePreview(record map[string]any) archive.RecordPreview {
	key, source := "customTitle", archive.SessionNameCustom
	if firstString(record, "type") == "ai-title" {
		key, source = "aiTitle", archive.SessionNameGenerated
	}
	return archive.RecordPreview{Name: archive.CollapseSessionTitle(firstString(record, key)), NameNativeID: firstString(record, "sessionId"), NameSource: source}
}
