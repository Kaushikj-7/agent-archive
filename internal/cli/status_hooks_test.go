package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestStatusChecksHooksAgainstTheInstalledExecutable(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstalledExecutable != installed {
		t.Fatalf("setup recorded executable %q, want %q", cfg.InstalledExecutable, installed)
	}

	// status run through a different path (a shim, a symlink, a copy).
	env.Executable = func() (string, error) { return "/usr/local/bin/agent-archive", nil }
	hooksByApp := func() map[string]string {
		t.Helper()
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, app := range view.Apps {
			out[app.Name] = app.Hooks
		}
		return out
	}
	for app, state := range hooksByApp() {
		if state != "installed" {
			t.Fatalf("%s hooks = %q from a different executable path", app, state)
		}
	}

	// A configuration written before the field existed falls back to the
	// running executable, which here is not the installed one.
	cfg.InstalledExecutable = ""
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	for app, state := range hooksByApp() {
		if state != "missing or incomplete" {
			t.Fatalf("fallback: %s hooks = %q", app, state)
		}
	}
	env.Executable = func() (string, error) { return installed, nil }
	for app, state := range hooksByApp() {
		if state != "installed" {
			t.Fatalf("fallback with the installed path: %s hooks = %q", app, state)
		}
	}
}

// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestStatusExplainsWhyHookTrustIsUnknown(t *testing.T) {
	t.Parallel()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	setupYes(t, env, "", 0, "--yes", "--codex-discovery", "off")
	var stdout, stderr bytes.Buffer
	if code := runStatusCommand([]string{"codex"}, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "  ! Codex   discovery off; included projects   no sessions yet\n    Approve the archive hooks with /hooks in Codex.\n") {
		t.Fatalf("status does not say how to approve the hooks:\n%s", stdout.String())
	}
	stdout.Reset()
	if code := runStatusCommand([]string{"--verbose"}, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), "Run /hooks in Codex and approve the archive hooks; agent-archive can't see whether you have.") {
		t.Fatalf("status --verbose does not explain unknown trust:\n%s", stdout.String())
	}
	stdout.Reset()
	if code := runStatusCommand([]string{"--json"}, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), `"trust": "unknown"`) {
		t.Fatalf("the JSON value must stay unknown: code=%d\n%s", code, stdout.String())
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestStatusDetectsPartialHooks(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), 0)
	path := filepath.Join(userHome, ".codex", "hooks.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	delete(root["hooks"].(map[string]any), "SessionStart")
	if err := local.Write(path, root); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil || view.State != "Needs attention" || view.Apps[0].Hooks == "installed" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestDiscoveryStatusRepairsPartialOwnHooksAlongsideForeignOwner(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	cfg := mustLoadConfig(t, home)
	files := env.installedHookFiles(userHome, cfg)
	foreignHome := t.TempDir()
	foreign := env.installation(foreignHome, userHome).hook(cfg.InstalledExecutable)
	changes, err := hooks.Plan(files, foreign, []string{"codex"})
	must(t, err)
	must(t, hooks.Apply(changes))
	check := func(needsRepair bool) {
		t.Helper()
		view, err := readStatus(env)
		must(t, err)
		if (view.State == "Needs attention") != needsRepair {
			t.Fatalf("repair=%v state=%q: %+v", needsRepair, view.State, view.Apps)
		}
		if len(view.Apps) != 1 || len(view.Apps[0].OtherInstallations) != 1 || len(view.Warnings) == 0 {
			t.Fatalf("foreign owner warning lost: %+v", view)
		}
	}
	check(false) // Healthy own hooks plus foreign ownership remains usable.
	var settings map[string]any
	must(t, local.Read(files["codex"], &settings))
	own := env.installation(home, userHome).hook(cfg.InstalledExecutable)
	command, err := own.Command("codex")
	must(t, err)
	removed := 0
	for _, value := range settings["hooks"].(map[string]any)["SessionStart"].([]any) {
		group := value.(map[string]any)
		kept := []any{}
		for _, handler := range group["hooks"].([]any) {
			if handler.(map[string]any)["command"] == command {
				removed++
				continue
			}
			kept = append(kept, handler)
		}
		group["hooks"] = kept
	}
	if removed != 1 {
		t.Fatalf("removed %d own start handlers", removed)
	}
	must(t, local.Write(files["codex"], settings))
	check(true) // Only our first-start handler is missing; the foreign one remains.
	removal, found, err := hooks.PlanRemovalOf(files, env.installation(home, userHome).owner(), "codex")
	must(t, err)
	if !found {
		t.Fatal("partial own hooks not found")
	}
	must(t, hooks.Apply([]hooks.Change{removal}))
	check(false) // A genuinely absent own hook remains optional under discovery.
}
