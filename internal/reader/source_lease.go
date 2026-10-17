package reader

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// DecodeReferencedSourceLeased retains the shared decoder's wire charge until release.
func DecodeReferencedSourceLeased(ctx context.Context, metadata archive.Metadata, data []byte, limits Limits, budget *agentapi.NativeReadBudget) (archive.SourceBundle, func(), error) {
	return agentapi.DecodeReferencedSourceLeased(ctx, metadata, data, limits, budget)
}

// DecodeRevisionSourceLeased retains one revision's shared wire charge until release.
func DecodeRevisionSourceLeased(ctx context.Context, metadata archive.Metadata, revision string, data []byte, limits Limits, budget *agentapi.NativeReadBudget) (archive.SourceBundle, func(), error) {
	return agentapi.DecodeRevisionSourceLeased(ctx, metadata, revision, data, limits, budget)
}
