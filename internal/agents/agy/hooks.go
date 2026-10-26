package agy

import (
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
)

// Hooks configures the documented Antigravity global hooks file. Lifecycle
// events use direct handler arrays rather than matcher groups.
func Hooks() hookconfig.Configurator {
	return hookconfig.Configurator{Spec: hookconfig.Spec{
		Name: "agy", DefaultDirectory: ".gemini/config", Filename: "hooks.json",
		Flat: true, Direct: true, Rootless: true, Definition: "agent-archive",
		Owner:          "agent-archive lifecycle capture",
		PrototypeOwner: "Recording private skill-run evidence",
		Events:         []string{"PreInvocation", "PostInvocation", "Stop"},
	}}
}
