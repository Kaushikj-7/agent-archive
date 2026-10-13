package cursor

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
)

// Imports declares this integration's historical native observation rules.
func Imports() agentapi.ImportInspector {
	return importconfig.Cursor()
}
