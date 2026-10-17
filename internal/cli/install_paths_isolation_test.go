package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// twoInstallations is the account's default installation and a test one
// (AGENT_ARCHIVE_HOME elsewhere) sharing one HOME, so one set of app hook
// files, as `AGENT_ARCHIVE_HOME=/tmp/x agent-archive setup` without a
// sandboxed HOME does.
func twoInstallations(t *testing.T) (primary, secondary Env, userHome string) {
	t.Helper()
	account, userHome := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	primary = setupTestEnv(t, filepath.Join(account, ".local", "share", "agent-archive"), userHome, newFakeKeychain(), now)
	primary.AccountHome = func() (string, error) { return account, nil }
	secondary = setupTestEnv(t, t.TempDir(), userHome, newFakeKeychain(), now)
	secondary.AccountHome = func() (string, error) { return account, nil }
	// One executable for both, so each one's status finds its commands.
	secondary.Executable = primary.Executable
	return primary, secondary, userHome
}

func hookFileSnapshot(t *testing.T, userHome string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, app := range allHarnesses {
		path := legacyHookFiles(userHome)[app]
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[app] = string(data)
	}
	return out
}

func statusViewOf(t *testing.T, env Env) statusView {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("status exit %d: %s", code, &errOut)
	}
	var view statusView
	must(t, json.Unmarshal(out.Bytes(), &view))
	return view
}

// H-20: a second installation in the same HOME never takes over, changes, or
// removes the first one's hooks, in setup, status, or uninstall, whichever
// was installed first.
func TestSecondInstallationNeverTouchesTheFirstsHooks(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"default first", "test first"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			primary, secondary, userHome := twoInstallations(t)
			first, second := primary, secondary
			if order == "test first" {
				first, second = secondary, primary
			}
			input := s3SetupInput("b", "us-east-1", "p", true, true, true, t.TempDir())
			setupRun(t, first, input, 0)
			before := hookFileSnapshot(t, userHome)
			firstHome, _ := first.home()
			secondHome, _ := second.home()
			executable, _ := first.executable()

			// The second setup refuses, names the first installation and how
			// to resolve it, and changes no hook file.
			output := setupRun(t, second, s3SetupInput("b", "us-east-1", "p", true, true, true, t.TempDir()), 1)
			wantOwner := "the installation in " + firstHome
			if order == "default first" {
				wantOwner = "the default installation in " + local.CanonicalPath(firstHome)
			}
			if !strings.Contains(output, wantOwner) || !strings.Contains(output, "agent-archive uninstall") || !strings.Contains(output, "own HOME") || strings.Count(output, wantOwner) != 6 {
				t.Fatalf("setup output does not name the other installation:\n%s", output)
			}
			if after := hookFileSnapshot(t, userHome); !equalMaps(before, after) {
				t.Fatalf("the refused setup changed hook files")
			}
			view := statusViewOf(t, first)
			for _, app := range view.Apps {
				if app.Hooks != "installed" || len(app.OtherInstallations) != 0 {
					t.Fatalf("first installation's %s: hooks=%s others=%v", app.Name, app.Hooks, app.OtherInstallations)
				}
			}

			// The old bug left both installations' handlers in the files.
			// Each status then names the other, and each uninstall removes only
			// its own.
			for _, app := range allHarnesses {
				changes, err := hooks.Plan(legacyHookFiles(userHome), second.installation(secondHome, userHome).hook(executable), []string{app})
				must(t, err)
				must(t, hooks.Apply(changes))
			}
			view = statusViewOf(t, first)
			for _, app := range view.Apps {
				if app.Hooks != "installed" || len(app.OtherInstallations) != 1 {
					t.Fatalf("first installation's %s beside another: hooks=%s others=%v", app.Name, app.Hooks, app.OtherInstallations)
				}
			}
			if len(view.Warnings) == 0 || !strings.Contains(strings.Join(view.Warnings, "\n"), "another agent-archive installation") {
				t.Fatalf("status warnings: %v", view.Warnings)
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, first); code != 0 {
				t.Fatalf("uninstall exit %d: %s%s", code, &out, &errOut)
			}
			if !strings.Contains(out.String(), "Kept: ") {
				t.Fatalf("uninstall does not say what it kept:\n%s", &out)
			}
			for _, app := range allHarnesses {
				path := legacyHookFiles(userHome)[app]
				owner := second.installation(secondHome, userHome).hook(executable)
				if ok, err := hooks.Installed(legacyHookFiles(userHome), owner, app); !ok || err != nil {
					data, _ := os.ReadFile(path)
					t.Fatalf("uninstalling the first installation removed the second's %s hooks (%v):\n%s", app, err, data)
				}
				others, err := hooks.OtherInstallations(legacyHookFiles(userHome), owner, app)
				must(t, err)
				if len(others) != 0 {
					t.Fatalf("the first installation's %s hooks were left: %+v", app, others)
				}
			}
		})
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// With AGENT_ARCHIVE_HOME set elsewhere, setup installs hooks that carry it
// and a collector under a label of its own, into the files the apps read
// under CLAUDE_CONFIG_DIR and CODEX_HOME; status and uninstall find them
// from a shell without those variables.
func TestRelocatedInstallationStaysSelfContained(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	claudeDir, codexDir := filepath.Join(userHome, "cfg", "claude"), filepath.Join(userHome, "cfg", "codex")
	must(t, os.MkdirAll(claudeDir, 0700))
	must(t, os.MkdirAll(codexDir, 0700))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	vars := map[string]string{"CLAUDE_CONFIG_DIR": claudeDir, "CODEX_HOME": codexDir}
	env.LookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", true, true, false, t.TempDir()), 0)

	if loaded := fakeSched(env).loaded(); len(loaded) != 1 || loaded[0] == launchd.LaunchLabel {
		t.Fatalf("a relocated data directory loaded %v", loaded)
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(codexDir, "hooks.json")} {
		b, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(b), "AGENT_ARCHIVE_HOME='"+home+"'") {
			t.Fatalf("%s: %v\n%s", path, err, b)
		}
	}
	for _, path := range []string{filepath.Join(userHome, ".claude"), filepath.Join(userHome, ".codex")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("hooks were written under %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "handoff", "SKILL.md")); err != nil {
		t.Fatalf("Claude Code's /handoff skill is not in CLAUDE_CONFIG_DIR: %v", err)
	}
	vars = nil
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range view.Apps {
		if app.Hooks != "installed" {
			t.Fatalf("%s hooks: %s", app.Name, app.Hooks)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall: %s", &errOut)
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(codexDir, "hooks.json")} {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), hooks.Owner) {
			t.Fatalf("uninstall left hooks in %s", path)
		}
	}
	// Claude Code's /handoff skill went beside its settings, and is gone.
	if _, err := os.Stat(filepath.Join(claudeDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left Claude Code's skill: %v", err)
	}
}

// An installation from before labels were derived from the data directory
// runs its collector under the default label. Setup moves it to the
// directory's own label, and never touches a default-label job that belongs
// to another data directory.
func TestSetupMovesARelocatedCollectorOffTheDefaultLabel(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"this", "another"} {
		t.Run(owner, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			dataHome := home
			if owner == "another" {
				dataHome = env.installation(home, userHome).defaultDataHome()
			}
			old := filepath.Join(userHome, "Library", "LaunchAgents", launchd.LaunchLabel+".plist")
			plist, _ := launchd.LaunchAgent("/opt/old/agent-archive", dataHome, launchd.LaunchLabel, nil)
			if err := local.WriteBytes(old, plist); err != nil {
				t.Fatal(err)
			}
			sched := fakeSched(env).set(jobRef(old), "loaded")
			setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
			_, err := os.Stat(old)
			unloaded := sched.unloaded()
			if owner == "this" && (!os.IsNotExist(err) || len(unloaded) != 1 || unloaded[0] != jobRef(old)) {
				t.Fatalf("the old default-label job was not retired: unloaded %v, stat %v", unloaded, err)
			}
			if owner == "another" && (err != nil || len(unloaded) != 0) {
				t.Fatalf("another directory's job was touched: unloaded %v, stat %v", unloaded, err)
			}
			if sched.state(jobRef(env.installation(home, userHome).collectorPlist())) != "loaded" {
				t.Fatal("the collector was not loaded under its own label")
			}
		})
	}
}

// Uninstall stops and removes the collector of its own data directory,
// including one an earlier release left under the default label, and never
// the default-label job of another directory: a sandboxed test install must
// not reach the user's real collector.
func TestUninstallTouchesOnlyThisDirectorysCollector(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"this", "another"} {
		t.Run(owner, func(t *testing.T) {
			t.Parallel()
			home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
			dataHome := home
			if owner == "another" {
				dataHome = env.installation(home, userHome).defaultDataHome()
			}
			old := filepath.Join(userHome, "Library", "LaunchAgents", launchd.LaunchLabel+".plist")
			plist, _ := launchd.LaunchAgent("/opt/old/agent-archive", dataHome, launchd.LaunchLabel, nil)
			if err := local.WriteBytes(old, plist); err != nil {
				t.Fatal(err)
			}
			current := env.installation(home, userHome).collectorPlist()
			sched := fakeSched(env).set(jobRef(old), "running").set(jobRef(current), "loaded")
			var out, errOut bytes.Buffer
			if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
				t.Fatalf("exit %d\n%s", code, &errOut)
			}
			_, err := os.Stat(old)
			want := []scheduler.Ref{jobRef(current)}
			if owner == "this" {
				want = append(want, jobRef(old))
				if !os.IsNotExist(err) {
					t.Fatal("the old default-label plist for this directory remains")
				}
			} else if err != nil {
				t.Fatal("another directory's plist was removed")
			}
			if unloaded := sched.unloaded(); !slices.Equal(unloaded, want) {
				t.Fatalf("unloaded %v, want %v", unloaded, want)
			}
		})
	}
}

// The default label belongs to the account's own default installation
// only. A sandbox that overrides $HOME (and so moves the data directory with
// it) gets a label of its own, as does AGENT_ARCHIVE_HOME set elsewhere.
func TestOnlyTheAccountsDefaultInstallationGetsTheDefaultLabel(t *testing.T) {
	t.Parallel()
	account, sandbox := t.TempDir(), t.TempDir()
	env := Env{AccountHome: func() (string, error) { return account, nil }}
	accountDefault := filepath.Join(account, ".local", "share", "agent-archive")
	sandboxDefault := filepath.Join(sandbox, ".local", "share", "agent-archive")
	for _, tc := range []struct {
		name      string
		home      string
		userHome  string
		isDefault bool
	}{
		{"the account's own install", accountDefault, account, true},
		{"HOME-only sandbox", sandboxDefault, sandbox, false},
		{"AGENT_ARCHIVE_HOME elsewhere", filepath.Join(account, "other"), account, false},
		{"sandbox HOME pointed at the real data directory", accountDefault, sandbox, true},
	} {
		in := env.installation(tc.home, tc.userHome)
		label := launchd.Label(in.collectorPlist())
		if in.isDefault() != tc.isDefault || (label == launchd.LaunchLabel) != tc.isDefault {
			t.Errorf("%s: default=%v label=%s", tc.name, in.isDefault(), label)
		}
		if got := launchd.ServiceTarget(in.collectorPlist()); got != fmt.Sprintf("gui/%d/%s", os.Getuid(), label) {
			t.Errorf("%s: service target %s", tc.name, got)
		}
	}
	unknown := Env{AccountHome: func() (string, error) { return "", fmt.Errorf("no user database") }}
	if unknown.installation(accountDefault, account).isDefault() {
		t.Error("an unreadable account home made an installation the default one")
	}
}

// With the real launchctl path (nothing injected but launchctl itself), a
// sandboxed install whose label launchd has loaded from another plist, the
// user's real collector, neither stops nor replaces it: uninstall leaves it
// running and setup refuses before touching anything.
func TestAnotherInstallationsJobIsNeverStopped(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.Scheduler = nil
	var calls []string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "print" {
			return []byte("\tpath = /Users/real/Library/LaunchAgents/x.plist\n\tstate = running\n"), nil
		}
		return nil, nil
	})
	var out, errOut bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir())), &out, &errOut, env)
	// The check that blocks is marked ✗ on standard output; the exit
	// line on standard error points at it.
	if code != 1 || !strings.Contains(out.String(), "another installation") || !strings.Contains(errOut.String(), "Fix what is marked ✗ above") {
		t.Fatalf("setup: exit %d\n%s%s", code, &out, &errOut)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "print ") {
			t.Fatalf("setup ran launchctl %s", call)
		}
	}
	launchd, err := newScheduler("")
	if err != nil {
		t.Fatal(err)
	}
	if err := launchd.Unload(context.Background(), scheduler.Site{UserHome: userHome}, jobRef(env.installation(home, userHome).collectorPlist())); err == nil || !strings.Contains(err.Error(), "another installation") {
		t.Fatalf("unload: %v", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "bootout") || strings.HasPrefix(call, "bootstrap") {
			t.Fatalf("launchctl %s", call)
		}
	}
}

// Uninstall leaves another installation's job running and says so.
func TestUninstallLeavesAnotherInstallationsJob(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	sched := newFakeScheduler(t, string(scheduler.AnotherInstallation))
	env.Scheduler = sched
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
	if unloaded := sched.unloaded(); len(unloaded) != 0 {
		t.Fatalf("unloaded %v", unloaded)
	}
	if !strings.Contains(out.String(), "another installation") {
		t.Fatalf("not reported:\n%s", &out)
	}
	// This installation's plist stays, since its job was not stopped.
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); err != nil {
		t.Fatalf("plist removed although its bootout was skipped: %v", err)
	}
}

// When another installation runs this installation's label, status points
// at that, not at setup, which refuses to replace the job.
func TestStatusNamesAnotherInstallationsJob(t *testing.T) {
	t.Parallel()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	env.Scheduler = newFakeScheduler(t, string(scheduler.AnotherInstallation))
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Background != string(scheduler.AnotherInstallation) || !strings.Contains(view.Next, "Another agent-archive installation") || strings.Contains(view.Next, "restore the background collector") {
		t.Fatalf("background %q next %q", view.Background, view.Next)
	}
}

// Even a first setup refuses when launchd cannot say whether a job already
// runs under the label.
func TestFirstSetupRefusesAnUnknownJobState(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sched := newFakeScheduler(t, "unknown")
	env.Scheduler = sched
	out := setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 1)
	if loaded := sched.loaded(); len(loaded) != 0 {
		t.Fatalf("loaded %v", loaded)
	}
	if !strings.Contains(out, "launchctl") {
		t.Fatalf("output:\n%s", out)
	}
}
