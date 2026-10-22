package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// The scenarios of a Linux box for the setup, status and refresh commands over
// the real systemd adapter and a fake user manager (scheduler_systemd_fake_test.go).

// setupInput is the answers of an interactive S3 setup of one project.
func (l *linuxInstall) setupInput() string {
	return s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, l.t.TempDir())
}

// nothingChanged fails unless setup left the box as it found it: no unit
// file, no configuration, no journal, no hook file, and nothing changed at
// the manager.
func (l *linuxInstall) nothingChanged() {
	l.t.Helper()
	timer, service := l.units()
	for _, path := range []string{timer, service, filepath.Join(l.home, "config.json"), setupjournal.JournalPath(l.home), filepath.Join(l.userHome, ".claude", "settings.json"), filepath.Join(l.userHome, ".codex", "hooks.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			l.t.Errorf("%s exists after a setup that changed nothing (%v)", path, err)
		}
	}
	if changes := l.manager.changing(); len(changes) != 0 {
		l.t.Errorf("setup changed the manager by %q", changes)
	}
}

// Setup needs a systemd user manager it can ask: with no user bus (an SSH
// session, a container) it stops before its first question, says so and how to
// get one, and changes nothing.
func TestLinuxSetupRefusesWithoutAUserBus(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.manager.noBus = true
	output := setupRun(t, l.env, l.setupInput(), 1)
	for _, want := range []string{"Background job", "the systemd user manager cannot be reached", "no user bus", "loginctl enable-linger", "then run agent-archive setup again", "Nothing was changed"} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{"launchctl", "launchd", "plist", "LaunchAgent"} {
		if strings.Contains(output, unwanted) {
			t.Errorf("a Linux refusal says %q:\n%s", unwanted, output)
		}
	}
	l.nothingChanged()

	// The same from a script: the error names the check, for a log nobody sees
	// the checklist of.
	out := setupYes(t, l.env, "", 1, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--region", "us-east-1", "--project", t.TempDir(), "--apps", "claude")
	if !strings.Contains(out, "Background job") || !strings.Contains(out, "no user bus") {
		t.Errorf("setup --yes says:\n%s", out)
	}
	l.nothingChanged()
}

// A systemd too old for the collector's logs is refused the same way, with
// what to upgrade to.
func TestLinuxSetupRefusesAnOldSystemd(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.manager.version = "systemd 237 (237-3ubuntu10)\n"
	output := setupRun(t, l.env, l.setupInput(), 1)
	for _, want := range []string{"this is systemd 237, older than 240", "Upgrade systemd to version 240 or newer"} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	l.nothingChanged()
}

// A system with no scheduler at all (neither launchd nor systemd) is refused
// cleanly by the same check: the scheduler says it is called none, and setup
// writes nothing, loads nothing and records nothing.
func TestSetupOnASystemWithoutASchedulerChangesNothing(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.Scheduler = host.New(platform.Unknown, func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Errorf("a system with no scheduler ran %s %q", name, args)
		return nil, nil
	})
	output := setupRun(t, l.env, l.setupInput(), 1)
	for _, want := range []string{"Background job", "agent-archive has no background scheduler for this system", "Run agent-archive on macOS, or on Linux with a systemd user manager", "Nothing was changed"} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	l.nothingChanged()
	if l.env.Scheduler.Name() != "none" {
		t.Errorf("the scheduler is called %q", l.env.Scheduler.Name())
	}
}

// A manager whose own environment sets XDG_CONFIG_HOME does not look in
// ~/.config/systemd/user: loading fails, and what reaches the person says where
// the units were written and why the manager did not find them. Setup rolls
// back to nothing.
func TestLinuxSetupThatTheManagerCannotLoadNamesTheUnitDirectoryAndRollsBack(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.manager.searchHome = t.TempDir()
	input := l.setupInput()
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(input), &out, &errOut, l.env); code != 1 {
		t.Fatalf("setup: exit %d\n%s%s", code, &out, &errOut)
	}
	text := out.String() + errOut.String()
	unitDir := filepath.Join(l.userHome, ".config", "systemd", "user")
	// systemctl's own line ends with a newline, which the error leaves out,
	// so the directory is said on the same line.
	for _, want := range []string{"does not exist. (the units are in " + unitDir, "XDG_CONFIG_HOME", "UnitPath"} {
		if !strings.Contains(text, want) {
			t.Errorf("the failure lacks %q:\n%s", want, text)
		}
	}
	for _, path := range []string{filepath.Join(unitDir, l.ref()+".timer"), filepath.Join(unitDir, l.ref()+".service"), filepath.Join(l.home, "config.json"), setupjournal.JournalPath(l.home), filepath.Join(l.userHome, ".claude", "settings.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is left after the rollback (%v)", path, err)
		}
	}
	if got := l.manager.held(l.ref()); got != scheduler.Missing {
		t.Errorf("the job is %q after the rollback", got)
	}
	if changes := l.manager.changing(); !slices.Equal(changes, []string{"systemctl --user daemon-reload", "systemctl --user enable --now " + l.ref() + ".timer"}) {
		t.Errorf("the manager was changed by %q, want the reload and the load that failed", changes)
	}
}

// Status shows what works, but not robustly: lingering off, and a drop-in
// that overrides the unit, each as a note row under the ordinary rows and as an
// additive background_warnings array in the JSON, and the state is still
// Ready.
func TestStatusShowsWhatTheBackgroundJobDoesNotDoRobustly(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.linger = "no"
	l.manager.dropIn = true

	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status --json: exit %d\n%s%s", code, &out, &errOut)
	}
	var status struct {
		Background string   `json:"background"`
		Warnings   []string `json:"background_warnings"`
	}
	must(t, json.Unmarshal(out.Bytes(), &status))
	if status.Background != "loaded" || len(status.Warnings) != 2 {
		t.Fatalf("status --json: background %q, background_warnings %q", status.Background, status.Warnings)
	}
	sorted := slices.Clone(status.Warnings)
	slices.Sort(sorted)
	if !strings.HasPrefix(sorted[0], "A drop-in overrides "+l.ref()+".timer") || !strings.HasPrefix(sorted[1], "Lingering is off, so the collector runs while you are logged in") || !strings.Contains(sorted[1], "`loginctl enable-linger`") || !strings.HasSuffix(sorted[1], ".") {
		t.Errorf("background_warnings = %q", status.Warnings)
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "--verbose", "--no-pager"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
	}
	text := out.String()
	for _, warning := range status.Warnings {
		if warning = strings.ReplaceAll(warning, l.userHome, "~"); !strings.Contains(text, warning) {
			t.Errorf("the status screen lacks the note %q:\n%s", warning, text)
		}
	}
	if !strings.Contains(text, "Background collector on") {
		t.Errorf("the status screen does not say the collector is on:\n%s", text)
	}

	// Nothing to note: the field is absent, not empty.
	l.manager.linger, l.manager.dropIn = "yes", false
	out.Reset()
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status --json: exit %d", code)
	}
	if strings.Contains(out.String(), "background_warnings") {
		t.Errorf("a job with nothing to note has background_warnings:\n%s", out.String())
	}
}

// A job of another installation under this one's name is described in the
// scheduler's own nouns: a systemd unit name, not a launchd label.
func TestLinuxStatusNamesAnotherInstallationsJobInSystemdWords(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.put(l.ref(), scheduler.AnotherInstallation)
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--verbose", "--no-pager"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
	}
	text := out.String()
	for _, want := range []string{"Another installation's collector has this installation's unit name", "systemd unit name (" + l.ref() + ")"} {
		if !strings.Contains(text, want) {
			t.Errorf("the status lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"label", "launchd", "launchctl", "plist"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("a Linux status says %q:\n%s", unwanted, text)
		}
	}
}

// Status with a manager it cannot reach names the tool that could not say.
func TestLinuxStatusWithNoUserBusNamesSystemctl(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.noBus = true
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--verbose", "--no-pager"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
	}
	if text := out.String(); !strings.Contains(text, "Background collector state unknown") || !strings.Contains(text, "systemctl couldn't say") || strings.Contains(text, "launchctl") {
		t.Errorf("the status says:\n%s", text)
	}
}

// setup --refresh for a loaded job rewrites the unit files, and the manager
// is told to read them again before the job starts again: the job is stopped
// (which reloads), the files change, and Load reloads and enables, so a running
// job runs the new definition (the manager would otherwise keep the old one, and
// NeedDaemonReload).
func TestLinuxRefreshReloadsTheManagerForALoadedJob(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	_, service := l.units()
	old, err := os.ReadFile(service)
	must(t, err)
	next := upgradedTo(t, &l.env)
	l.manager.calls = nil
	unitChangedAt := -1
	watch := l.manager.scheduler()
	run := watch.Run
	watch.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if data, _ := os.ReadFile(service); !bytes.Equal(data, old) && unitChangedAt < 0 {
			unitChangedAt = len(l.manager.all())
		}
		return run(ctx, name, args...)
	}
	l.env.Scheduler = watch

	code, stdout, stderr := refreshRun(t, l.env)
	if code != 0 {
		t.Fatalf("refresh: exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "the background collector (restarted)") {
		t.Errorf("refresh says %q", stdout)
	}
	got := l.manager.changing()
	want := []string{
		"systemctl --user disable --now " + l.ref() + ".timer",
		"systemctl --user stop " + l.ref() + ".service",
		"systemctl --user reset-failed " + l.ref() + ".service",
		"systemctl --user daemon-reload", // the stop, before the files change
		"systemctl --user daemon-reload", // the load, after
		"systemctl --user enable --now " + l.ref() + ".timer",
	}
	if !slices.Equal(got, want) {
		t.Errorf("refresh changed the manager by\n%q\nwant\n%q", got, want)
	}
	if data, err := os.ReadFile(service); err != nil || !bytes.Contains(data, []byte(next)) {
		t.Errorf("the unit file runs %q (%v), want it to run %s", data, err, next)
	}
	if held := l.manager.held(l.ref()); held != scheduler.Loaded {
		t.Errorf("the job is %q after the refresh", held)
	}
	// The second reload is after the unit files changed: the manager reads
	// the new ones. The probe of the service file at each call sees the change
	// first at a call after the stop's own reload.
	calls := l.manager.all()
	if unitChangedAt < 0 || !slices.Contains(calls[unitChangedAt-1:], "systemctl --user daemon-reload") {
		t.Errorf("the unit files changed at call %d of %q with no reload after", unitChangedAt, calls)
	}
}

// A job that is not loaded is left as it is by a refresh: the unit files are
// rewritten and the manager is not asked to load them (loading would start a job
// the person had stopped).
func TestLinuxRefreshOfAJobThatIsNotLoadedLeavesTheManagerAlone(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.put(l.ref(), scheduler.Missing)
	_, service := l.units()
	next := upgradedTo(t, &l.env)
	l.manager.calls = nil
	code, stdout, stderr := refreshRun(t, l.env)
	if code != 0 {
		t.Fatalf("refresh: exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "the background collector's unit file (its job is not loaded, and was left so)") || strings.Contains(stdout, "plist") {
		t.Errorf("refresh says %q", stdout)
	}
	if changes := l.manager.changing(); len(changes) != 0 {
		t.Errorf("refresh changed the manager by %q", changes)
	}
	if data, err := os.ReadFile(service); err != nil || !bytes.Contains(data, []byte(next)) {
		t.Errorf("the unit file runs %q (%v), want it to run %s", data, err, next)
	}
}

// A masked unit is refused before the first question too, with the unmask
// that lets setup load it, and nothing changes.
func TestLinuxSetupRefusesAMaskedUnit(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.manager.masked = true
	output := setupRun(t, l.env, l.setupInput(), 1)
	for _, want := range []string{"Background job", l.ref() + ".timer is masked", "systemctl --user unmask " + l.ref() + ".timer " + l.ref() + ".service", "then run agent-archive setup again", "Nothing was changed"} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	l.nothingChanged()
}

// A Load that fails for a reason of the manager's own (not a unit file it
// cannot find) reaches the person as systemctl said it, and setup rolls back
// to nothing: no unit file, configuration, journal or hook is left.
func TestLinuxSetupWhoseLoadFailsRollsBackToNothing(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.manager.failLoad = "Failed to start the timer: Transaction is destructive.\n"
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(l.setupInput()), &out, &errOut, l.env); code != 1 {
		t.Fatalf("setup: exit %d\n%s%s", code, &out, &errOut)
	}
	if text := out.String() + errOut.String(); !strings.Contains(text, "Transaction is destructive") || strings.Contains(text, "launchctl") || strings.Contains(text, "plist") {
		t.Errorf("the failure says:\n%s", text)
	}
	timer, service := l.units()
	// The failed start left the link enabling made, which dangles once the unit
	// files are gone: the rollback removes it too.
	for _, path := range []string{timer, service, l.manager.enableLink(l.ref()), filepath.Join(l.home, "config.json"), setupjournal.JournalPath(l.home), filepath.Join(l.userHome, ".claude", "settings.json"), filepath.Join(l.userHome, ".codex", "hooks.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s is left after the rollback (%v)", path, err)
		}
	}
	if got := l.manager.held(l.ref()); got != scheduler.Missing {
		t.Errorf("the job is %q after the rollback", got)
	}
}

// A refresh that cannot ask the manager about the job's state refuses in the
// adapter's words, with its fix, and changes nothing: the unit files still run
// the old executable and the manager was not touched.
func TestLinuxRefreshWithNoUserBusRefusesAndChangesNothing(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	timer, _ := l.units()
	before := tree(t, filepath.Dir(timer), l.home)
	upgradedTo(t, &l.env)
	l.manager.noBus = true
	l.manager.calls = nil
	code, stdout, stderr := refreshRun(t, l.env)
	if code == 0 {
		t.Fatalf("refresh with no user bus: exit 0\n%s%s", stdout, stderr)
	}
	text := stdout + stderr
	for _, want := range []string{"cannot determine the background job's state: the systemd user manager cannot be reached (this session has no user bus)", "loginctl enable-linger", "then retry"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"launchctl", "plist", "LaunchAgent"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("a Linux refusal says %q:\n%s", unwanted, text)
		}
	}
	if changes := l.manager.changing(); len(changes) != 0 {
		t.Errorf("refresh changed the manager by %q", changes)
	}
	if after := tree(t, filepath.Dir(timer), l.home); !maps.Equal(before, after) {
		t.Errorf("refresh changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// The status rows of a Linux job in each state it can be in, in systemd's
// words: running and loaded are on, missing is not running, and a unit file
// whose executable is gone is broken, said of the user timer.
func TestLinuxStatusRowsForEachState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state scheduler.JobState
		row   string
	}{
		{scheduler.Loaded, "Background collector on"},
		{scheduler.Running, "Background collector on"},
		{scheduler.Missing, "Background collector isn't running"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			l := newLinuxInstall(t)
			l.setup()
			l.manager.put(l.ref(), tc.state)
			var out, errOut bytes.Buffer
			if code := Run([]string{"status", "--verbose", "--no-pager"}, nil, &out, &errOut, l.env); code != 0 {
				t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
			}
			if text := out.String(); !strings.Contains(text, tc.row) || strings.Contains(text, "launchctl") || strings.Contains(text, "LaunchAgent") {
				t.Errorf("status for a %s job:\n%s", tc.state, text)
			}
			out.Reset()
			if code := Run([]string{"status", "--json"}, nil, &out, &errOut, l.env); code != 0 {
				t.Fatalf("status --json: exit %d", code)
			}
			var status struct {
				Background string `json:"background"`
			}
			must(t, json.Unmarshal(out.Bytes(), &status))
			if status.Background != string(tc.state) {
				t.Errorf("status --json: background %q, want %q", status.Background, tc.state)
			}
		})
	}
	t.Run("broken", func(t *testing.T) {
		t.Parallel()
		l := newLinuxInstall(t)
		l.setup()
		must(t, os.Remove(mustLoadConfig(t, l.home).InstalledExecutable))
		var out, errOut bytes.Buffer
		if code := Run([]string{"status", "--verbose", "--no-pager"}, nil, &out, &errOut, l.env); code != 0 {
			t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
		}
		text := out.String()
		if !strings.Contains(text, "Background collector is broken") || !strings.Contains(text, "background collector's user timer runs") || strings.Contains(text, "LaunchAgent") {
			t.Errorf("status for a broken job:\n%s", text)
		}
	})
}
