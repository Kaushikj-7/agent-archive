package cursor

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives common analysis from retained cursor source without host effects.
type Parser struct{}

// Version identifies the native derivation policy.
func (Parser) Version() string { return "0.21.0" }

// Parse derives facts once from retained safe evidence. A chat name retained
// as session label evidence names a session whose own source carries none
// (Cursor's agent transcripts); a name in a cursor-composer session record
// still wins.
func (Parser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	analysis, err := nativecodec.ParseCursor(ctx, bundle)
	if analysis.Facts.Name == "" {
		if label, _, ok := archive.CurrentSessionLabel(bundle); ok && label.State == archive.SessionLabelPresent && supportedSessionLabel(label) {
			analysis.Facts.Name = archive.CollapseSessionTitle(label.Name)
		}
	}
	return analysis, err
}
