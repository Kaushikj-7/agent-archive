package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// installedWithBinary runs setup for Codex and Claude and returns the data
// home, the user home, the environment, and the executable setup installed.
func installedWithBinary(t *testing.T) (string, string, Env, string) {
	t.Helper()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstalledExecutable == "" {
		t.Fatal("setup recorded no installed executable")
	}
	if view, err := readStatus(env); err != nil || view.Background == backgroundBroken {
		t.Fatalf("a fresh install already reads as broken: %+v %v", view, err)
	}
	return home, userHome, env, cfg.InstalledExecutable
}

func statusJSON(t *testing.T, env Env) map[string]any {
	t.Helper()
	var out strings.Builder
	if code := runStatusCommand([]string{"--json"}, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status --json exit=%d", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// Every installed hook and the LaunchAgent run the executable setup recorded.
// Once it is moved or deleted, the hook configuration still matches exactly,
// but every hook and every scheduled collection fails.
func TestStatusReportsAMovedOrDeletedBinaryAsBroken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		break_  func(t *testing.T, path string)
		problem string
	}{
		{"deleted", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "missing"},
		{"moved", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Rename(path, path+"-moved"); err != nil {
				t.Fatal(err)
			}
		}, "missing"},
		{"not executable", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, env, installed := installedWithBinary(t)
			tc.break_(t, installed)

			view, err := readStatus(env)
			if err != nil {
				t.Fatal(err)
			}
			for _, app := range view.Apps {
				if app.Hooks != hooksBroken {
					t.Errorf("%s hooks = %q, want %q", app.Name, app.Hooks, hooksBroken)
				}
			}
			if view.Background != backgroundBroken {
				t.Errorf("background = %q, want %q (launchd still calls the job loaded)", view.Background, backgroundBroken)
			}
			if view.State != "Needs attention" || !strings.Contains(view.Next, "agent-archive setup --refresh from the binary's new location") || !strings.Contains(view.Next, installed) {
				t.Errorf("state=%q next=%q", view.State, view.Next)
			}
			warnings := strings.Join(view.Warnings, "\n")
			if !strings.Contains(warnings, installed+" is "+tc.problem) || !strings.Contains(warnings, "LaunchAgent runs "+installed+", which is "+tc.problem) {
				t.Errorf("warnings=%q", view.Warnings)
			}

			decoded := statusJSON(t, env)
			if decoded["background"] != "broken" {
				t.Errorf("JSON background = %v", decoded["background"])
			}
			for _, raw := range decoded["applications"].([]any) {
				if app := raw.(map[string]any); app["hooks"] != "broken" {
					t.Errorf("JSON hooks for %v = %v", app["name"], app["hooks"])
				}
			}
			var out strings.Builder
			if code := runStatusCommand([]string{"--verbose"}, &out, os.Stderr, env); code != 0 || !strings.Contains(out.String(), "hooks broken") || !strings.Contains(out.String(), "✗ Background collector is broken") {
				t.Errorf("status exit=%d output=%s", code, out.String())
			}
		})
	}
}

// Pausing collection does not stop the apps from running hooks that now fail,
// so the moved binary still decides the next action.
func TestMovedBinaryOutranksPause(t *testing.T) {
	t.Parallel()
	home, _, env, installed := installedWithBinary(t)
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Paused || !strings.Contains(view.Next, "binary's new location") {
		t.Fatalf("paused=%t next=%q", view.Paused, view.Next)
	}
}

// After uninstall there are no hooks left to break: deleting the binary then
// is the expected last step, not a fault.
func TestUninstalledArchiveDoesNotReportTheDeletedBinary(t *testing.T) {
	t.Parallel()
	home, _, env, installed := installedWithBinary(t)
	cfg, _, _ := config.Load(home)
	cfg.Archive.Enabled = false
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Not installed" || view.Background == backgroundBroken || len(view.Warnings) != 0 {
		t.Fatalf("state=%q background=%q warnings=%q", view.State, view.Background, view.Warnings)
	}
	for _, app := range view.Apps {
		if app.Hooks == hooksBroken {
			t.Fatalf("%s hooks reported broken after uninstall", app.Name)
		}
	}
}

// The LaunchAgent can point somewhere the hooks do not (an older install left
// in place). Only the background is broken then, and the next action names
// the path the LaunchAgent actually runs.
func TestBackgroundAloneBrokenWhenTheLaunchAgentRunsAMissingFile(t *testing.T) {
	t.Parallel()
	home, userHome, env, _ := installedWithBinary(t)
	stale := filepath.Join(t.TempDir(), "old", "agent-archive")
	plist, err := launchd.LaunchAgent(stale, home, launchd.Label(env.installation(home, userHome).collectorPlist()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.installation(home, userHome).collectorPlist(), plist, 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Background != backgroundBroken || !strings.Contains(view.Next, stale) || !strings.Contains(view.Next, "binary's new location") {
		t.Fatalf("background=%q next=%q", view.Background, view.Next)
	}
	for _, app := range view.Apps {
		if app.Hooks != "installed" {
			t.Fatalf("%s hooks = %q; only the background is broken", app.Name, app.Hooks)
		}
	}
}
