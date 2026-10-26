package agy

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
)

// Decoder maps Antigravity lifecycle payloads into archive lifecycle events.
func Decoder() hookconfig.Decoder {
	return hookconfig.Decoder{Spec: hookconfig.DecoderSpec{
		Agent: agentmeta.Agy,
		Events: map[string]agentapi.EventKind{
			"PreInvocation":  agentapi.EventTurnStart,
			"PostInvocation": agentapi.EventResponse,
			"Stop":           agentapi.EventStop,
		},
		PromptStarts: true,
	}}
}
