package agy

import (
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

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
	for _, flag := range []string{"-p", "--prompt", "--print", "-i", "--prompt-interactive"} {
		if _, err := (Launcher{}).Args(agentapi.LaunchRequest{ExtraArgs: []string{flag}, Prompt: "handoff"}); err == nil {
			t.Fatalf("Args() accepted conflicting flag %q", flag)
		}
	}
}

func TestLauncherAdvertisesWindowsAndUnixNames(t *testing.T) {
	names := (Launcher{}).Executables().Names
	if len(names) != 2 || names[0] != "agy" || names[1] != "agy.exe" {
		t.Fatalf("executables = %#v", names)
	}
}
