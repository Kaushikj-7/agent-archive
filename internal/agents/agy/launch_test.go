package agy

import (
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
)

func TestLaunchConformance(t *testing.T) {
	t.Parallel()
	agenttest.LaunchConformance(t, Launcher{}, []string{"agy"}, []string{"--add-dir", "/project", "--model", "x", "-i", "PROMPT"})
}

func TestLauncherArgs(t *testing.T) {
	got, err := (Launcher{}).Args(agentapi.LaunchRequest{ProjectDir: `C:\work`, Prompt: "continue", ExtraArgs: []string{"--model", "gemini"}})
	if err != nil {
		t.Fatalf("Args() error = %v", err)
	}
	want := []string{"--add-dir", `C:\work`, "--model", "gemini", "-i", "continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

func TestLauncherArgsUsesInteractivePrompt(t *testing.T) {
	got, err := (Launcher{}).Args(agentapi.LaunchRequest{ProjectDir: `C:\work`, Prompt: "continue", ExtraArgs: []string{"--model", "gemini"}})
	if err != nil {
		t.Fatalf("Args() error = %v", err)
	}
	want := []string{"--add-dir", `C:\work`, "--model", "gemini", "-i", "continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

func TestLauncherRejectsPromptFlagConflict(t *testing.T) {
	if _, err := (Launcher{}).Args(agentapi.LaunchRequest{ExtraArgs: []string{"-p"}, Prompt: "handoff"}); err == nil {
		t.Fatal("Args() accepted conflicting -p")
	}
}

func TestLauncherRejectsAllConflictingPromptFlags(t *testing.T) {
	for _, flag := range []string{"--", "-p", "--prompt", "--print", "-i", "--prompt-interactive"} {
		if _, err := (Launcher{}).Args(agentapi.LaunchRequest{ExtraArgs: []string{flag}, Prompt: "handoff"}); err == nil {
			t.Fatalf("Args() accepted conflicting flag %q", flag)
		}
	}
}

func TestLauncherRejectsPromptFlagPrefixSyntax(t *testing.T) {
	for _, flag := range []string{"--prompt=foo", "--prompt-interactive=bar", "-p=test", "-i=val", "--print=msg"} {
		if _, err := (Launcher{}).Args(agentapi.LaunchRequest{ExtraArgs: []string{flag}, Prompt: "handoff"}); err == nil {
			t.Fatalf("Args() accepted conflicting prefix flag %q", flag)
		}
	}
}

func TestLauncherExecutables(t *testing.T) {
	names := (Launcher{}).Executables().Names
	if len(names) != 1 || names[0] != "agy" {
		t.Fatalf("executables = %#v", names)
	}
}
