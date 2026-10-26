package agy

import (
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Launcher builds a bounded Antigravity CLI handoff command.
type Launcher struct{}

// Executables returns native CLI candidates in preference order.
func (Launcher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"agy", "agy.exe"}, Install: "Antigravity CLI"}
}

// Args starts a new AGY conversation with the handoff prompt and project root.
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	if slices.Contains(r.ExtraArgs, "-p") || slices.Contains(r.ExtraArgs, "--prompt") {
		return nil, fmt.Errorf("arguments for agy cannot include -p or --prompt: the handoff prompt is appended")
	}
	args := slices.Clone(r.ExtraArgs)
	if r.ProjectDir != "" {
		args = append([]string{"--add-dir", r.ProjectDir}, args...)
	}
	args = append(args, "-p", r.Prompt)
	return args, nil
}
