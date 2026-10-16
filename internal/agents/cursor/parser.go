package cursor

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives common analysis from retained cursor source without host effects.
type Parser struct{}

// Version identifies the native derivation policy.
func (Parser) Version() string { return "0.22.0" }

// Parse derives facts once from retained safe evidence.
func (Parser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	return nativecodec.ParseCursor(ctx, bundle)
}
