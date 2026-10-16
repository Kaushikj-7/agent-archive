package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
)

// uninstall runs uninstall --yes with args and returns the exit code and
// what it printed to both streams.
func (l *linuxInstall) uninstall(args ...string) (int, string) {
	l.t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"uninstall", "--yes"}, args...), strings.NewReader(""), &out, &errOut, l.env)
	return code, out.String() + errOut.String()
}

// hooksInstalled reports whether Claude Code's settings hold this tool's hooks.
func (l *linuxInstall) hooksInstalled() bool {
	data, _ := os.ReadFile(l.userHome + "/.claude/settings.json")
	return bytes.Contains(data, []byte("agent-archive"))
}

// manualStop is the command that stops the job by hand.
func (l *linuxInstall) manualStop() string {
	return "systemctl --user stop " + l.ref() + ".timer " + l.ref() + ".service"
}

// Without --skip-scheduler, an unreachable scheduler stops uninstall before it
// changes anything: it says what is wrong, the adapter's next step, the
// command that stops the job by hand, and the flag that goes on anyway.
func TestLinuxUninstallRefusesWhenTheSchedulerCannotBeReached(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.noBus = true
	l.manager.calls = nil
	code, output := l.uninstall()
	if code != 1 {
		t.Fatalf("uninstall: exit %d\n%s", code, output)
	}
	for _, want := range []string{
		"Uninstall incomplete: cannot determine background job state: the systemd user manager cannot be reached (this session has no user bus).",
		"loginctl enable-linger",
		"To stop the job by hand, run this from a session that can reach systemd: " + l.manualStop() + ".",
		"agent-archive uninstall --skip-scheduler",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("uninstall removed %s: %v", path, err)
		}
	}
	if !l.hooksInstalled() || !mustLoadConfig(t, l.home).Archive.Enabled {
		t.Errorf("uninstall changed something before it refused: hooks %v, config %+v", l.hooksInstalled(), mustLoadConfig(t, l.home).Archive)
	}
	if changes := l.manager.changing(); len(changes) != 0 {
		t.Errorf("uninstall changed the manager by %q", changes)
	}
}

// With --skip-scheduler it goes on: it tries to stop the job, deletes the unit
// files, removes the hooks and disables capture as uninstall does, prints the
// manual command, and says, last, that the job was not verified stopped.
func TestLinuxUninstallSkippingTheSchedulerRemovesWhatItCanAndSaysSo(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.noBus = true
	l.manager.calls = nil
	code, output := l.uninstall("--skip-scheduler")
	if code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	searchRef := string(l.env.installation(l.home, l.userHome).searchRole().ref())
	for _, want := range []string{
		"Not verified stopped: systemd's " + l.ref() + " job may still be running, because the systemd user manager cannot be reached (this session has no user bus).",
		"To stop it, run this from a session that can reach systemd: " + l.manualStop(),
		"Not verified stopped: systemd's " + searchRef + " job may still be running, because the systemd user manager cannot be reached (this session has no user bus).",
		"To stop search indexing, run this from a session that can reach systemd: systemctl --user stop " + searchRef + ".timer " + searchRef + ".service",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the summary lacks %q:\n%s", want, output)
		}
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "Uninstall complete, except that one or more background jobs were not verified stopped") {
		t.Errorf("the last line is %q", last)
	}
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is left after uninstall --skip-scheduler (%v)", path, err)
		}
	}
	if l.hooksInstalled() {
		t.Error("the hooks are left after uninstall --skip-scheduler")
	}
	if cfg := mustLoadConfig(t, l.home); cfg.Archive.Enabled {
		t.Error("capture is still enabled after uninstall --skip-scheduler")
	}
	// The stop was tried: the adapter asked the manager again before it
	// refused to stop what it could not describe.
	asked := 0
	for _, call := range l.manager.all() {
		if call == "systemctl --version" {
			asked++
		}
	}
	if asked < 2 {
		t.Errorf("uninstall asked the manager %d times, want a question and the attempt: %q", asked, l.manager.all())
	}
}

// The summary names the scheduler the job was under even after
// --delete-local-data has deleted the configuration that recorded it.
func TestLinuxUninstallSkippingTheSchedulerAndDeletingLocalData(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.noBus = true
	code, output := l.uninstall("--skip-scheduler", "--delete-local-data")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, output)
	}
	if _, err := os.Stat(l.home + "/config.json"); !os.IsNotExist(err) {
		t.Errorf("the configuration is left after --delete-local-data (%v)", err)
	}
	for _, want := range []string{"Not verified stopped: systemd's " + l.ref() + " job", "To stop it, run this from a session that can reach systemd: " + l.manualStop()} {
		if !strings.Contains(output, want) {
			t.Errorf("the summary lacks %q:\n%s", want, output)
		}
	}
}

// With a scheduler that can be reached, the flag changes nothing: the job is
// stopped and verified, the files go, and the summary is the usual one.
func TestLinuxUninstallSkippingTheSchedulerStillStopsAJobItCanReach(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.calls = nil
	code, output := l.uninstall("--skip-scheduler")
	if code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	if strings.Contains(output, "verified") || !strings.HasSuffix(strings.TrimSpace(output), "Uninstall complete. Remote archives and the CLI executable were kept.") {
		t.Errorf("uninstall says:\n%s", output)
	}
	if got := l.manager.held(l.ref()); got != scheduler.Missing {
		t.Errorf("the job is %q after uninstall", got)
	}
	if changes := l.manager.changing(); len(changes) == 0 || changes[0] != "systemctl --user disable --now "+l.ref()+".timer" {
		t.Errorf("uninstall changed the manager by %q, want it to stop the job", changes)
	}
}

// A manager that described the job as loaded and could not be reached when the
// stop came (the bus went away in between) is reported as one that could not be
// reached from the start: what is wrong, and the command that stops the job by
// hand, not only the refusal's words and a pointer to systemctl, which would
// leave the person to guess a `disable` that fails once the unit files are gone.
func TestLinuxUninstallSkippingTheSchedulerWhenTheBusGoesAwayBeforeTheStop(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	shows := 0
	l.env.Scheduler = systemd.Scheduler{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && args[1] == "show" {
			shows++
			if shows > 1 {
				l.manager.noBus = true
			}
		}
		return l.manager.run(ctx, name, args...)
	}}
	code, output := l.uninstall("--skip-scheduler")
	if code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	for _, want := range []string{
		"Not verified stopped: systemd's " + l.ref() + " job may still be running, because the systemd user manager cannot be reached (this session has no user bus).",
		"To stop it, run this from a session that can reach systemd: " + l.manualStop(),
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the summary lacks %q:\n%s", want, output)
		}
	}
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is left after uninstall --skip-scheduler (%v)", path, err)
		}
	}
}

// A job whose manager says another installation owns it is left running with
// or without the flag, and its files stay: the flag is for a scheduler that
// cannot be reached, not for another installation's job.
func TestLinuxUninstallSkippingTheSchedulerLeavesAnotherInstallationsJob(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.manager.put(l.ref(), scheduler.AnotherInstallation)
	code, output := l.uninstall("--skip-scheduler")
	if code != 0 || !strings.Contains(output, "Left systemd's "+l.ref()+" job running: it was loaded from another unit file") {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("uninstall removed %s, which belongs to a job it left running: %v", path, err)
		}
	}
	if strings.Contains(output, "Not verified stopped") {
		t.Errorf("uninstall says it could not verify a job it left on purpose:\n%s", output)
	}
}

// A recorded backend this system cannot use (a configuration from another
// system, or a scheduler this build has never heard of) refuses uninstall in
// the same way, and the flag goes on.
func TestUninstallOfARecordedBackendThisSystemCannotUse(t *testing.T) {
	// Not parallel: asksFor replaces the scheduler lookup.
	l := newLinuxInstall(t)
	l.setup()
	cfg := mustLoadConfig(t, l.home)
	cfg.BackgroundBackend = "cron"
	must(t, config.Save(l.home, cfg))
	l.asksFor(t)
	l.manager.calls = nil

	code, output := l.uninstall()
	if code != 1 || !strings.Contains(output, "no cron scheduler on this system") || !strings.Contains(output, "--skip-scheduler") || !l.hooksInstalled() {
		t.Fatalf("uninstall: exit %d, hooks %v\n%s", code, l.hooksInstalled(), output)
	}
	code, output = l.uninstall("--skip-scheduler")
	if code != 0 || !strings.Contains(output, "Not verified stopped") || !strings.Contains(output, "no cron scheduler on this system") || l.hooksInstalled() {
		t.Fatalf("uninstall --skip-scheduler: exit %d, hooks %v\n%s", code, l.hooksInstalled(), output)
	}
	if changes := l.manager.changing(); len(changes) != 0 {
		t.Errorf("a scheduler that was not the job's was told to change it: %q", changes)
	}
}

// The same flag on macOS: launchd that cannot say refuses as it always did (the
// refusal's text is pinned in testdata/scheduler/refusals), and with the flag
// the stop is tried, the plist is removed, and the summary says the job was not
// verified stopped, naming launchctl.
func TestMacOSUninstallSkippingTheSchedulerSaysSo(t *testing.T) {
	// Not parallel: newSchedRun replaces launchctl.
	r := newSchedRun(t, true)
	r.install()
	r.answers[r.ownLabel()] = []launchdAnswer{answerUnknown}
	code, output := r.run("uninstall", "--yes", "--skip-scheduler")
	if code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	for _, want := range []string{
		"Not verified stopped: launchd's " + r.ownLabel() + " job may still be running, because launchctl did not say whether the job is loaded.",
		"To stop it, use launchctl from a session that can reach launchd.",
		"Uninstall complete, except that the background collector was not verified stopped (see above).",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the summary lacks %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(r.own()); !os.IsNotExist(err) {
		t.Errorf("the plist is left after uninstall --skip-scheduler: %v", err)
	}
	if r.hooksInstalled() {
		t.Error("the hooks are left after uninstall --skip-scheduler")
	}
	for _, call := range r.lines {
		if strings.HasPrefix(call, "bootout") || strings.HasPrefix(call, "bootstrap") {
			t.Errorf("launchctl changed something for a job it could not describe: %s", call)
		}
	}
}

// A scheduler that cannot say what state a job is in but then stops it when
// asked (it only failed to answer) has stopped it: uninstall says nothing was
// left unverified.
func TestUninstallSkippingTheSchedulerCountsAStopThatWorked(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	fake := fakeSched(env)
	ref := env.installation(home, userHome).ref()
	fake.set(ref, "unknown")
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes", "--skip-scheduler"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s%s", code, &out, &errOut)
	}
	if strings.Contains(out.String(), "verified") || !strings.Contains(out.String(), "Uninstall complete. Remote archives") {
		t.Errorf("uninstall says:\n%s", &out)
	}
	if got := fake.unloaded(); len(got) != 1 || got[0] != ref {
		t.Errorf("uninstall tried to stop %q, want %s", got, ref)
	}

	// And one that refuses the stop is reported with what it said.
	env = setupTestEnv(t, t.TempDir(), t.TempDir(), newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	fake = fakeSched(env)
	ref = env.installation(mustHome(t, env), mustUserHome(t, env)).ref()
	fake.set(ref, "loaded")
	fake.beforeUnload = func(scheduler.Ref) error { return errors.New("launchctl bootout: exit status 5") }
	out.Reset()
	if code := Run([]string{"uninstall", "--yes"}, strings.NewReader(""), &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "stop collector: launchctl bootout: exit status 5") {
		t.Fatalf("uninstall without the flag: exit %d\n%s%s", code, &out, &errOut)
	}
	out.Reset()
	if code := Run([]string{"uninstall", "--yes", "--skip-scheduler"}, strings.NewReader(""), &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "because launchctl bootout: exit status 5") {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, &out)
	}
}

// A job the scheduler could not describe, or stop, that the attempt to stop it
// finds is another installation's (the manager answered the second time) is
// left running with its definition, as without the flag: it is not reported
// unverified, and its files stay.
func TestUninstallSkippingTheSchedulerLeavesAJobThatTurnsOutToBeAnotherInstallations(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"unknown", "loaded"} {
		home, userHome := t.TempDir(), t.TempDir()
		env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
		fake := fakeSched(env)
		ref := env.installation(home, userHome).ref()
		plist := launchd.PlistPath(userSite(userHome), ref)
		fake.set(ref, state)
		fake.beforeUnload = func(scheduler.Ref) error {
			return &scheduler.NotOwnedError{Words: fake.Words(), Problem: scheduler.Problem{Kind: scheduler.ProblemNotOwned, Ref: ref, Expected: plist, LoadedFrom: "/elsewhere/" + string(ref) + ".plist"}}
		}
		var out, errOut bytes.Buffer
		if code := Run([]string{"uninstall", "--yes", "--skip-scheduler"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
			t.Fatalf("%s: uninstall --skip-scheduler: exit %d\n%s%s", state, code, &out, &errOut)
		}
		if !strings.Contains(out.String(), "Left launchd's "+string(ref)+" job running") || strings.Contains(out.String(), "Not verified stopped") || !strings.Contains(out.String(), "Uninstall complete. Remote archives") {
			t.Errorf("%s: uninstall says:\n%s", state, &out)
		}
		if _, err := os.Stat(plist); err != nil {
			t.Errorf("%s: uninstall removed the definition of a job it left running: %v", state, err)
		}
	}
}

func mustHome(t *testing.T, env Env) string {
	t.Helper()
	home, err := env.readHome()
	must(t, err)
	return home
}

func mustUserHome(t *testing.T, env Env) string {
	t.Helper()
	userHome, err := env.userHomeDir()
	must(t, err)
	return userHome
}
