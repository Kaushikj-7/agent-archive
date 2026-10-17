package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

func TestMuseHandoffUsesPrivateFilteredContext(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/synthetic/agent-archive", nil }
	f.env.LookPath = func(name string) (string, error) { return "/synthetic/bin/" + name, nil }
	var launched launchSpec
	f.env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		launched = spec
		return nil
	}
	out, stderr, code := runPicker(t, f.env, "", f.id, "--to", "muse")
	if code != 0 || out != "" || !strings.Contains(stderr, "launching local muse") {
		t.Fatalf("handoff exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	path := handoffPathFromPrompt(t, launched)
	want := []string{"--workspace", f.project, launched.Args[len(launched.Args)-1]}
	if launched.Binary != "/synthetic/bin/muse" || !slices.Equal(launched.Args, want) || launched.HandoffFile != path {
		t.Fatalf("unexpected launch %+v", launched)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "Fix the flaky widget test.") || strings.Contains(string(content), "hunter2secret") {
		t.Fatalf("unsafe or missing filtered context: %v", err)
	}
	for location, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(location)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private context permissions: %v %v", info, err)
		}
	}
}

func TestEachExistingAgentHandsOffToMuse(t *testing.T) {
	t.Parallel()
	for source, prompt := range map[string]string{
		"claude": "Why does the widget test fail?",
		"codex":  "Rename Widget.parts to Widget.pieces everywhere.",
		"cursor": "Tighten the intro paragraph of the megafauna post.",
	} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join("..", "archive", "testdata", "handoff", source+".jsonl")
			env := testEnv(t, filepath.Join(t.TempDir(), "not-configured"), time.Now())
			env.Environ = func() []string { return nil }
			env.LookPath = func(name string) (string, error) { return "/synthetic/bin/" + name, nil }
			env.Executable = func() (string, error) { return "/synthetic/agent-archive", nil }
			project := t.TempDir()
			env.WorkingDir = func() (string, error) { return project, nil }
			var opened termlaunch.Spec
			env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
				opened = spec
				return "a synthetic terminal", nil
			}
			_, stderr, code := runHandoff(t, env, "--file", path, "--harness", source, "--to", "muse")
			if code != 0 || len(opened.Argv) < 2 || opened.Argv[0] != "/synthetic/bin/muse" {
				t.Fatalf("%s to Muse exit %d: %s", source, code, stderr)
			}
			artifact := handoffPathFromPrompt(t, launchSpec{Args: opened.Argv[1:]})
			content, err := os.ReadFile(artifact)
			if err != nil || !strings.Contains(string(content), prompt) || !strings.Contains(string(content), "historical context") || strings.Contains(string(content), "hunter2secret") {
				t.Fatalf("%s to Muse has unsafe or missing context: %v", source, err)
			}
		})
	}
}

func TestMuseHandoffUsesSharedTerminalLaunch(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/synthetic/agent-archive", nil }
	f.env.LookPath = func(name string) (string, error) { return "/synthetic/bin/" + name, nil }
	f.env.Environ = func() []string { return []string{archive.ReplayEnv + "=caller-replay", "MUSE_THREAD_ID=caller"} }
	var opened termlaunch.Spec
	f.env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
		opened = spec
		return "a synthetic terminal", nil
	}
	_, stderr, code := runPicker(t, f.env, "", f.id, "--to", "muse", "--new-window")
	if code != 0 || !strings.Contains(stderr, "opened muse in a synthetic terminal") {
		t.Fatalf("terminal handoff exit %d: %s", code, stderr)
	}
	if opened.Dir != f.project || len(opened.Argv) != 4 || opened.Argv[0] != "/synthetic/bin/muse" || !slices.Contains(opened.Unset, "MUSE_SESSION_ID") || !slices.Contains(opened.Unset, "MUSE_THREAD_ID") || !slices.Contains(opened.Unset, archive.ReplayEnv) {
		t.Fatalf("terminal specification %+v", opened)
	}
}

func TestOrdinaryHandoffDoesNotInheritReplayMarker(t *testing.T) {
	t.Parallel()
	for _, destination := range []handoffDestination{"claude", "codex", "cursor", "muse"} {
		t.Run(string(destination), func(t *testing.T) {
			t.Parallel()
			env := testEnv(t, t.TempDir(), time.Now())
			env.LookPath = func(name string) (string, error) { return "/synthetic/bin/" + name, nil }
			kept := []string{"PATH=/bin", "UNRELATED=value", "AGENT_ARCHIVE_REPLAY_EXTRA=preserved"}
			env.Environ = func() []string {
				return append([]string{archive.ReplayEnv + "=caller-run", archive.ReplayEnv + "=duplicate", "MUSE_THREAD_ID=caller", "MUSE_SESSION_ID=root"}, kept...)
			}
			spec, err := buildLaunchSpec(destination, "Read context", "/private/handoff.md", "/project", nil, env)
			if err != nil || !slices.Equal(spec.Env, kept) {
				t.Fatalf("ordinary launch environment %q: %v", spec.Env, err)
			}
		})
	}
}

func TestMuseSourceHandoffToEachAgentUsesFilteredArtifact(t *testing.T) {
	t.Parallel()
	for _, destination := range []string{"claude", "codex", "cursor", "muse"} {
		t.Run(destination, func(t *testing.T) {
			t.Parallel()
			path := museLocalFixture(t, "synthetic-exec-conversation")
			home := filepath.Join(t.TempDir(), "not-configured")
			env := testEnv(t, home, time.Now())
			env.LookPath = func(name string) (string, error) { return "/synthetic/bin/" + name, nil }
			env.Environ = func() []string { return nil }
			env.Executable = func() (string, error) { return "/synthetic/agent-archive", nil }
			env.WorkingDir = func() (string, error) { return filepath.Dir(path), nil }
			var opened termlaunch.Spec
			env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
				opened = spec
				return "a synthetic terminal", nil
			}
			_, stderr, code := runHandoff(t, env, "--file", path, "--harness", "muse", "--to", destination)
			if code != 0 || len(opened.Argv) < 2 {
				t.Fatalf("destination %s exit %d: %s", destination, code, stderr)
			}
			artifact := handoffPathFromPrompt(t, launchSpec{Args: opened.Argv[1:]})
			content, err := os.ReadFile(artifact)
			if err != nil || !strings.Contains(string(content), "Explain the synthetic parser.") || !strings.Contains(string(content), "It validates bounded native records") || strings.Contains(string(content), "PRIVATE CONTEXT MUST NOT SURVIVE") {
				t.Fatalf("destination %s lost safe normalized context: %v", destination, err)
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("handoff created setup or capture state: %v", err)
			}
		})
	}
}

func TestMuseTerminalCommandPreservesArgumentsAndReadsArtifact(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/synthetic/agent-archive", nil }
	dir := filepath.Join(t.TempDir(), "λ 'quoted' $(touch unexpected)")
	must(t, os.Mkdir(dir, 0o700))
	binary := filepath.Join(dir, "synthetic muse")
	argumentsPath := filepath.Join(dir, "arguments")
	contextPath := filepath.Join(dir, "context")
	f.env.LookPath = func(string) (string, error) { return binary, nil }
	var launched []string
	f.env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
		launched = slices.Clone(spec.Argv)
		artifact := handoffPathFromPrompt(t, launchSpec{Args: spec.Argv[1:]})
		// This ordinary synthetic process reads the actual shared artifact.
		// It proves shell/terminal transport, not native Muse permission policy.
		stub := "#!/bin/sh\n" +
			"test -z \"${AGENT_ARCHIVE_REPLAY+x}\" || exit 21\n" +
			"test -z \"${MUSE_THREAD_ID+x}\" || exit 22\n" +
			"printf '%s\\0' \"$@\" > " + shellQuote(argumentsPath) + "\n" +
			"/bin/cat " + shellQuote(artifact) + " > " + shellQuote(contextPath) + "\n"
		must(t, os.WriteFile(binary, []byte(stub), 0o700))
		return termlaunch.Open(t.Context(), spec, termlaunch.Environment{
			OS:        platform.Linux,
			LookupEnv: agentEnv(map[string]string{"TMUX": "synthetic"}),
			Run: func(ctx context.Context, name string, args ...string) error {
				if name != "tmux" || len(args) != 4 || args[0] != "new-window" {
					t.Fatalf("unexpected terminal command %s %q", name, args)
				}
				command := exec.CommandContext(ctx, "/bin/sh", "-c", args[3])
				command.Env = []string{"PATH=/usr/bin:/bin", archive.ReplayEnv + "=calling-run", "MUSE_THREAD_ID=calling-session"}
				return command.Run()
			},
		})
	}
	_, stderr, code := runPicker(t, f.env, "", f.id, "--to", "muse", "--new-window")
	if code != 0 {
		t.Fatalf("synthetic terminal handoff exit %d: %s", code, stderr)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil || string(arguments) != strings.Join(launched[1:], "\x00")+"\x00" {
		t.Fatalf("terminal changed argv: %v", err)
	}
	content, err := os.ReadFile(contextPath)
	if err != nil || !strings.Contains(string(content), "Fix the flaky widget test.") || strings.Contains(string(content), "hunter2secret") {
		t.Fatalf("synthetic process could not read filtered context: %v", err)
	}
}
