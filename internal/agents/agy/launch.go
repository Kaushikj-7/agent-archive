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

var conflictingPromptFlags = []string{"-p", "--prompt", "--print", "-i", "--prompt-interactive"}

// Args starts a new AGY conversation with the handoff prompt and project root.
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	for _, flag := range conflictingPromptFlags {
		if slices.Contains(r.ExtraArgs, flag) {
			return nil, fmt.Errorf("arguments for agy cannot include %s: the handoff prompt is managed by agent-archive", flag)
		}
	}
	args := slices.Clone(r.ExtraArgs)
	if r.ProjectDir != "" {
		args = append([]string{"--add-dir", r.ProjectDir}, args...)
	}
	args = append(args, "-i", r.Prompt)
	return args, nil
}
