package codex

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives common analysis from retained codex source without host effects.
type Parser struct{}

// Version identifies the native derivation policy.
func (Parser) Version() string { return "0.28.0" }

// Parse derives facts once from retained safe evidence.
func (Parser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	analysis, err := nativecodec.ParseCodex(ctx, bundle)
	if label, _, ok := archive.CurrentSessionLabel(bundle); ok {
		analysis.Facts.Name = sessionLabelName(label, analysis.Facts.Name)
	}
	return analysis, err
}

// sessionLabelName returns the session name to record given label and name,
// the name from the session's own records. A label Codex can interpret wins,
// including one that confirms the session has no name; otherwise name stands.
func sessionLabelName(label archive.SessionLabel, name string) string {
	if supportedSessionLabel(label) {
		return label.Name
	}
	return name
}
