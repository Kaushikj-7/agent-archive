package agy

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
)

// Imports declares Antigravity's historical native observation rules.
func Imports() agentapi.ImportInspector {
	return importconfig.Provider{
		RequireSessionMembership: true,
		NativeStart:              true,
		ConversationTypes:        []string{"USER_INPUT", "PLANNER_RESPONSE", "user", "assistant", "message", "response_item", "tool_use", "tool_result", "tool_call"},
	}
}
