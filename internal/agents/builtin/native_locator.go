package builtin

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
)

// ValidateNativeLocator composes the supported provisional startup validator.
// Calling it grants no capture, binding, consent or namespace authority; builtin
// construction remains pure, and the host supplies a counted confined environment.
func ValidateNativeLocator(ctx context.Context, env agentapi.SourceEnvironment, ref agentapi.SourceRef, nativeID string) error {
	if env.NativeRoot == nil || env.NativeRoot.Agent != "muse" {
		return agentapi.ErrInvalidSourceBinding
	}
	return muse.ValidateNativeLocator(ctx, env, ref, nativeID)
}
