package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// retireeFixture is a default installation with the prototype's upload job and
// two collectors under earlier labels on disk, on a fake scheduler.
type retireeFixture struct {
	env       Env
	in        installation
	userHome  string
	sched     *fakeScheduler
	prototype string
	earlier   [2]string
}

func newRetireeFixture(t *testing.T) *retireeFixture {
	t.Helper()
	account, userHome := t.TempDir(), t.TempDir()
	home := filepath.Join(account, ".local", "share", "agent-archive")
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.AccountHome = func() (string, error) { return account, nil }
	agents := filepath.Join(userHome, "Library", "LaunchAgents")
	f := &retireeFixture{
		env: env, userHome: userHome, in: env.installation(home, userHome), sched: fakeSched(env),
		prototype: filepath.Join(agents, launchd.LegacyLaunchLabel+".plist"),
	}
	must(t, os.MkdirAll(agents, 0o700))
	must(t, os.WriteFile(f.prototype, []byte(legacyPlist), 0o640))
	must(t, os.Chmod(f.prototype, 0o640))
	for i, spelling := range []string{"/old/spelling/a", "/old/spelling/b"} {
		label := launchd.CollectorLabel(spelling, "")
		f.earlier[i] = filepath.Join(agents, label+".plist")
		plist, err := launchd.LaunchAgent("/opt/old/agent-archive", home, label, nil)
		must(t, err)
		must(t, os.WriteFile(f.earlier[i], plist, 0o600))
	}
	return f
}

// The jobs setup retires are the aliases the scheduler lists, as found: their
// definition's bytes and mode, the scheduler that runs them, and whether they
// are loaded. The prototype comes first, then earlier labels in the scheduler's
// order, and each is asked about once, after its file is read.
func TestPlanRetireesReadsEachAliasAsFound(t *testing.T) {
	t.Parallel()
	f := newRetireeFixture(t)
	f.sched.set(jobRef(f.prototype), "running").set(jobRef(f.earlier[0]), "loaded").set(jobRef(f.earlier[1]), "missing")
	retirees, err := planRetirees(f.env, f.in, f.userHome)
	must(t, err)
	if len(retirees) != 3 {
		t.Fatalf("retirees %+v", retirees)
	}
	first := retirees[0]
	if path, _ := first.Artifacts[0].Path(); first.Alias != scheduler.Prototype || first.Backend != "launchd" || first.Ref != launchd.LegacyLaunchLabel || path != f.prototype ||
		string(first.Artifacts[0].After) != legacyPlist || first.Artifacts[0].Mode != 0o640 || !first.WasLoaded {
		t.Errorf("the prototype's retiree: %+v", first)
	}
	for _, r := range retirees[1:] {
		if r.Alias != scheduler.EarlierLabel || r.Backend != "launchd" || len(r.Artifacts) != 1 || r.Artifacts[0].Mode != 0o600 {
			t.Errorf("an earlier label's retiree: %+v", r)
		}
	}
	if !retirees[1].WasLoaded && !retirees[2].WasLoaded || retirees[1].WasLoaded && retirees[2].WasLoaded {
		t.Errorf("exactly one earlier collector was loaded: %+v", retirees[1:])
	}
	legacy, relabeled, err := setupjournal.RetireeJobs(retirees)
	if err != nil || legacy == nil || legacy.Change.Path != f.prototype || len(relabeled) != 2 {
		t.Errorf("journal jobs: %+v %+v %v", legacy, relabeled, err)
	}
	// Only the default installation retires the prototype.
	other := f.env.installation(t.TempDir(), f.userHome)
	if retirees, err = planRetirees(f.env, other, f.userHome); err != nil || len(retirees) != 0 {
		t.Errorf("another data directory's setup retires %+v (%v)", retirees, err)
	}
}

// What blocks setup says the words it always has, for the prototype and for an
// earlier label, and a collector an earlier label runs from another
// installation's plist is left alone instead.
func TestPlanRetireesRefusalsAndSkips(t *testing.T) {
	t.Parallel()
	f := newRetireeFixture(t)
	proto, first, second := jobRef(f.prototype), jobRef(f.earlier[0]), jobRef(f.earlier[1])

	f.sched.set(proto, "unknown")
	if _, err := planRetirees(f.env, f.in, f.userHome); err == nil || err.Error() != "cannot determine legacy upload job state; restore launchctl access and retry" {
		t.Errorf("an unknown prototype: %v", err)
	}
	f.sched.set(proto, "another_installation")
	want := "launchd's legacy upload job was loaded from a plist other than " + f.prototype + "; preserve it and resolve it before setup"
	if _, err := planRetirees(f.env, f.in, f.userHome); err == nil || err.Error() != want {
		t.Errorf("a prototype another installation runs: %v, want %q", err, want)
	}
	f.sched.set(proto, "missing").set(first, "unknown")
	want = "cannot determine the state of " + f.earlier[0] + "; restore access to launchctl and retry"
	if _, err := planRetirees(f.env, f.in, f.userHome); err == nil || err.Error() != want {
		t.Errorf("an unknown earlier collector: %v, want %q", err, want)
	}
	f.sched.set(first, "another_installation").set(second, "loaded")
	retirees, err := planRetirees(f.env, f.in, f.userHome)
	if err != nil || len(retirees) != 2 || retirees[1].Ref != second {
		t.Errorf("an earlier collector another installation runs: %+v, %v; want it left out", retirees, err)
	}
	// A prototype plist that is not the prototype's blocks setup before anything is asked.
	must(t, os.WriteFile(f.prototype, []byte(strings.Replace(legacyPlist, "skill_runs.py", "other.py", 1)), 0o600))
	f.sched.forget()
	if _, err := planRetirees(f.env, f.in, f.userHome); err == nil || !strings.Contains(err.Error(), "preserve "+f.prototype+" and resolve it before setup") {
		t.Errorf("an unrecognized prototype: %v", err)
	}
	if calls := f.sched.all(); len(calls) != 0 {
		t.Errorf("the scheduler was asked %q about a prototype that is not one", calls)
	}
}
