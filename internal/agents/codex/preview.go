package codex

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Previewer filters bounded complete records without full-session analysis.
type Previewer struct{}

// PreviewRecord derives safe Codex facts from one bounded complete record.
func (Previewer) PreviewRecord(ctx context.Context, record []byte) (archive.RecordPreview, error) {
	return nativecodec.Preview(ctx, nativecodec.CodexAdapter{}, record)
}

// SessionLabelName names a previewed session from its thread name as Parser
// names it.
func (Previewer) SessionLabelName(label archive.SessionLabel, name string) string {
	return sessionLabelName(label, name)
}
