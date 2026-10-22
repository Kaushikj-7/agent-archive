package muse

import (
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
)

func TestLaunchConformance(t *testing.T) {
	t.Parallel()
	agenttest.LaunchConformance(t, Launcher{}, []string{"muse"}, []string{"--workspace", "/project", "--model", "x", "PROMPT"})
}

func TestLaunchPreservesPromptAndArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"interactive", []string{"--model", "model with spaces"}},
		{"exec", []string{"exec", "--provider", "meta", "--model", "model with spaces", "--disable-write", "--disable-shell"}},
		{"exec-model-name", []string{"--model", "exec"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			request := agentapi.LaunchRequest{
				ProjectDir:  "/project/λ 'quoted'; $(touch unwanted)",
				HandoffPath: "/private/context/λ 'quoted'.md",
				Prompt:      "Read λ 'quoted' \"double\"; $(touch unwanted)\nsecond line",
				ExtraArgs:   slices.Clone(tc.extra),
			}
			before := slices.Clone(request.ExtraArgs)
			args, err := (Launcher{}).Args(request)
			want := slices.Concat([]string{"--workspace", request.ProjectDir}, tc.extra, []string{request.Prompt})
			if tc.name == "exec" {
				want = []string{"exec", "--workspace", request.ProjectDir, "--provider", "meta", "--model", "model with spaces", "--disable-write", "--disable-shell", request.Prompt}
			}
			if err != nil || !slices.Equal(args, want) {
				t.Fatalf("argv = %q, error %v; want %q", args, err, want)
			}
			args[len(args)-2] = "changed"
			if !slices.Equal(request.ExtraArgs, before) {
				t.Fatal("launcher changed caller arguments")
			}
		})
	}
}

func TestLaunchRefusesUnqualifiedLeadingDashPrompt(t *testing.T) {
	t.Parallel()
	if _, err := (Launcher{}).Args(agentapi.LaunchRequest{Prompt: "--resume"}); err == nil {
		t.Fatal("unqualified positional option separator accepted")
	}
}
