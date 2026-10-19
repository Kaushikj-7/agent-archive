package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
)

func TestClipboardSelectsAProviderForTheConnectedDesktop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		wayland   string
		display   string
		system    platform.OS
		installed []string
		want      string
		args      []string
	}{
		{name: "macOS", system: platform.Darwin, installed: []string{"pbcopy", "wl-copy"}, want: "pbcopy"},
		{name: "Wayland preferred", system: platform.Linux, wayland: "wayland-0", display: ":0", installed: []string{"wl-copy", "xclip"}, want: "wl-copy", args: []string{"--type", "text/plain;charset=utf-8"}},
		{name: "X11", system: platform.Linux, display: ":0", installed: []string{"xclip", "xsel"}, want: "xclip", args: []string{"-selection", "clipboard"}},
		{name: "X11 fallback", system: platform.Linux, display: ":0", installed: []string{"xsel"}, want: "xsel", args: []string{"--clipboard", "--input"}},
		{name: "XWayland fallback", system: platform.Linux, wayland: "wayland-0", display: ":0", installed: []string{"xclip"}, want: "xclip", args: []string{"-selection", "clipboard"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			program := filepath.Join(dir, "clipboard-helper")
			argsPath, dataPath := filepath.Join(dir, "args"), filepath.Join(dir, "data")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CLIPBOARD_ARGS\"\n/bin/cat > \"$CLIPBOARD_DATA\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			env := testEnv(t, dir, time.Now())
			env.OS, env.Clipboard = tc.system, nil
			env.LookupEnv = func(key string) (string, bool) {
				values := map[string]string{"WAYLAND_DISPLAY": tc.wayland, "DISPLAY": tc.display}
				value := values[key]
				return value, value != ""
			}
			env.Environ = func() []string { return []string{"CLIPBOARD_ARGS=" + argsPath, "CLIPBOARD_DATA=" + dataPath} }
			var selected string
			env.LookPath = func(name string) (string, error) {
				if slices.Contains(tc.installed, name) {
					selected = name
					return program, nil
				}
				return "", exec.ErrNotFound
			}
			if !env.clipboardAvailable() {
				t.Fatal("connected provider was not offered")
			}
			data := []byte("# Synthetic handoff\nUnicode: café\n")
			if err := env.clipboard(data); err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(dataPath)
			if err != nil || string(copied) != string(data) {
				t.Fatalf("copied=%q err=%v", copied, err)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil || string(args) != strings.Join(tc.args, "\n")+"\n" || selected != tc.want {
				t.Fatalf("provider=%q args=%q err=%v", selected, args, err)
			}
		})
	}
}

func TestClipboardUnavailableDoesNotOfferCopy(t *testing.T) {
	t.Parallel()
	for _, system := range []platform.OS{platform.Linux, platform.Unknown} {
		env := testEnv(t, t.TempDir(), time.Now())
		env.OS, env.Clipboard = system, nil
		env.LookPath = func(string) (string, error) {
			t.Error("headless or unknown platforms must not search for a clipboard executable")
			return "", exec.ErrNotFound
		}
		if env.clipboardAvailable() || !errors.Is(env.clipboard([]byte("synthetic")), errClipboardUnavailable) {
			t.Fatal("clipboard should be unavailable")
		}
		var out strings.Builder
		choice, err := chooseDestination(newPrompter(strings.NewReader("c\ncopy\nw\n"), &out), nil, "", env.clipboardAvailable(), productionAgents.Catalog(), nil)
		if err != nil || choice.action != handoffWrite || strings.Contains(out.String(), "c) copy") || strings.Contains(out.String(), "Enter p, c") {
			t.Fatalf("choice=%+v err=%v menu=%q", choice, err, out.String())
		}
	}
}

func TestClipboardMissingProviderAndFailedProvider(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	env.OS, env.Clipboard = platform.Linux, nil
	env.LookupEnv = func(key string) (string, bool) { return ":0", key == "DISPLAY" }
	env.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if env.clipboardAvailable() || !errors.Is(env.clipboard(nil), errClipboardUnavailable) {
		t.Fatal("missing providers must not offer copying")
	}
	program := filepath.Join(t.TempDir(), "failing-provider")
	if err := os.WriteFile(program, []byte("#!/bin/sh\necho 'display disconnected' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env.LookPath = func(string) (string, error) { return program, nil }
	if err := env.clipboard(nil); err == nil || !strings.Contains(err.Error(), "display disconnected") {
		t.Fatalf("provider failure=%v", err)
	}
}

func TestClipboardProviderCanKeepOwningTheSelectionAfterExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	program := filepath.Join(dir, "daemonizing-provider")
	dataPath := filepath.Join(dir, "copied")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n/bin/cat > \"$CLIPBOARD_DATA\"\n/bin/sleep 2 &\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, dir, time.Now())
	env.OS, env.Clipboard = platform.Linux, nil
	env.LookupEnv = func(key string) (string, bool) { return ":0", key == "DISPLAY" }
	env.LookPath = func(string) (string, error) { return program, nil }
	env.Environ = func() []string { return []string{"CLIPBOARD_DATA=" + dataPath} }
	if err := env.clipboard([]byte("synthetic handoff")); err != nil {
		t.Fatalf("successful daemonizing provider: %v", err)
	}
	if data, err := os.ReadFile(dataPath); err != nil || string(data) != "synthetic handoff" {
		t.Fatalf("copied=%q err=%v", data, err)
	}
}
