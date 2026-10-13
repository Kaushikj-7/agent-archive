package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
)

// Imports declares this integration's historical native observation rules.
func Imports() agentapi.ImportInspector {
	return importPolicy()
}

func importPolicy() importconfig.Provider {
	return importconfig.Provider{PreferHeaderStart: true, NativeStart: true, ConversationTypes: []string{"user", "assistant", "message", "response_item", "tool_use", "tool_result", "tool_call"}}
}
