package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
)

// Imports declares this integration's historical native observation rules.
func Imports() agentapi.ImportInspector {
	return importInspector{Provider: importPolicy()}
}

func importPolicy() importconfig.Provider {
	return importconfig.Provider{PreferHeaderStart: true, NativeStart: true, ConversationTypes: []string{"user", "assistant", "message", "response_item", "tool_use", "tool_result", "tool_call"}}
}

// importInspector attests the conversation policy used by the Codex summary sink.
// Generic configured providers and embedded overrides retain their full inspection.
type importInspector struct{ importconfig.Provider }

func (importInspector) SummaryFor(inspector agentapi.ImportInspector) bool {
	_, ok := inspector.(importInspector)
	return ok
}
