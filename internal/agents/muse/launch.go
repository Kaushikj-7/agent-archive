package muse

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Launcher builds a new conversation using the ordinary Muse CLI.
// It does not resume or import the source session, or change permission policy.
type Launcher struct{}

// Executables returns Muse's CLI name without probing the host.
func (Launcher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"muse"}, Install: "Muse Code"}
}

// Args supplies the workspace and a single positional handoff prompt.
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	if slices.Contains(r.ExtraArgs, "--") {
		return nil, fmt.Errorf("arguments for muse cannot include `--`: the handoff prompt must remain the final positional argument")
	}
	if strings.HasPrefix(r.Prompt, "-") {
		return nil, fmt.Errorf("muse handoff prompt cannot begin with `-`: positional option separation is not qualified")
	}
	var args []string
	if len(r.ExtraArgs) > 0 && r.ExtraArgs[0] == "exec" {
		// Muse selects exec before interpreting its workspace and tool options.
		args = slices.Concat([]string{"exec", "--workspace", r.ProjectDir}, r.ExtraArgs[1:])
	} else {
		args = slices.Concat([]string{"--workspace", r.ProjectDir}, r.ExtraArgs)
	}
	return append(args, r.Prompt), nil
}
