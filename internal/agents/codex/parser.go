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

// sessionLabelName is the name a session whose records name it name has
// under label: the label's, none when it confirms there is none, when Codex
// can interpret it, and name otherwise.
func sessionLabelName(label archive.SessionLabel, name string) string {
	if supportedSessionLabel(label) {
		return label.Name
	}
	return name
}
