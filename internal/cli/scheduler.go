package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// newScheduler makes the scheduler a nil Env.Scheduler means, by the name of a
// backend (config.Config.BackgroundBackend; "" is this system's own, which is
// what a configuration that records none means): host.Lookup, made when a
// command needs it, and making it runs nothing. It is a variable so that tests
// fail closed: isolateProcessForTesting replaces it with one whose launchctl
// stops the test, so a test that builds a bare Env{} can never reach the
// developer's real launchd (or, on Linux, its systemd). A test that means to
// drive launchd's own code stubs launchctl with stubLaunchctl.
var newScheduler = host.Lookup

// collectorJob is the background collector as a scheduler defines it: the
// executable running `_collect` every minute, and once at load, for the data
// directory home, with the environment its storage needs (which never holds
// a credential: see collectorEnvironment).
func collectorJob(executable, home string, environment map[string]string) scheduler.JobSpec {
	return scheduler.JobSpec{Executable: executable, Args: []string{"_collect"}, DataHome: home, Env: environment, Interval: time.Minute, RunAtLoad: true}
}

// planJob is the plan that defines spec as this installation's job at the
// site of userHome. It reads nothing.
func (in installation) planJob(userHome string, spec scheduler.JobSpec) (scheduler.Plan, error) {
	if in.role == scheduler.Search {
		spec.Args = []string{"_search-index"}
	}
	return in.sched().Plan(userSite(userHome), in.schedulerInstallation(), spec)
}

// artifactChanges are the changes a plan makes to disk, as the setup journal
// records them: each artifact's file as it is now, and what it becomes. The
// port's artifacts are files alone.
func artifactChanges(artifacts []scheduler.Artifact) ([]hooks.Change, error) {
	if len(artifacts) == 0 {
		return nil, errors.New("the scheduler defined the job with nothing to write")
	}
	changes := make([]hooks.Change, 0, len(artifacts))
	for _, artifact := range artifacts {
		path, ok := artifact.Path()
		if !ok {
			return nil, fmt.Errorf("cannot write %s: only files are defined", artifact.ID)
		}
		before, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		changes = append(changes, hooks.Change{Path: path, Before: before, After: artifact.After, Existed: err == nil, Mode: artifact.Mode})
	}
	return changes, nil
}

// problemOf is the facts a status gives for a state that blocks a command,
// empty when the scheduler gave none.
func problemOf(status scheduler.Status) scheduler.Problem {
	if status.Problem == nil {
		return scheduler.Problem{}
	}
	return *status.Problem
}

// unknownJobMessage says that a command cannot go on because the scheduler
// cannot say what state the job is in. An adapter that says what is wrong (its
// Problem's Reason) has it said, with its next step; otherwise it is the
// words it always was, which name the tool to restore access to.
func unknownJobMessage(words scheduler.Words, problem scheduler.Problem) string {
	if problem.Reason == "" {
		return fmt.Sprintf("cannot determine the background job's state; restore access to %s and retry", words.Tool)
	}
	return fmt.Sprintf("cannot determine the background job's state: %s. %s, then retry", problem.Reason, problem.Fix)
}

// jobActive is whether a job is loaded, whether or not it is running now.
func jobActive(state scheduler.JobState) bool { return state.Active() }

// scheduler is e's job scheduler: Env.Scheduler, or the backend the
// installation's own configuration records, or, for a command that is
// choosing one (setup), this system's own.
func (e Env) scheduler() scheduler.Scheduler {
	if e.Scheduler != nil {
		return e.Scheduler
	}
	if e.choosesBackend {
		return e.namedScheduler("")
	}
	return e.namedScheduler(e.recordedBackend())
}

// choosingBackend is e for a command that picks the scheduler an installation
// runs under, rather than addressing the one it already has: setup, which uses
// this system's own and records it (see prepareSetupConfig), whatever an
// earlier configuration recorded. A recorded backend this system cannot use
// can have no job here to retire: its manager is another system's.
func (e Env) choosingBackend() Env {
	e.choosesBackend = true
	return e
}

// recordedBackend is the backend name config.json records, "" when it records
// none or cannot be read (every command that needs the configuration reports
// that itself). It is read when asked, since setup and uninstall keep the field
// as they rewrite the file.
func (e Env) recordedBackend() string {
	home, err := e.readHome()
	if err != nil {
		return ""
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		return ""
	}
	return cfg.BackgroundBackend
}

// namedScheduler is the scheduler called name (see newScheduler). A backend
// this system cannot use is not replaced by another: it is a scheduler that
// says nothing and changes nothing, and its problem says why, so status reports
// the job unknown and uninstall refuses (or, with --skip-scheduler, goes on
// without verifying the job stopped).
func (e Env) namedScheduler(name string) scheduler.Scheduler {
	s, err := newScheduler(name)
	if err != nil {
		return host.Unavailable(name, err.Error(), "Run agent-archive setup on this system to schedule the collector with its own scheduler")
	}
	return s
}

// userSite is the site of the user home userHome, spelled as the scheduler's
// Locate spells the site of a definition in it (cleaned: $HOME may end in a
// separator), so a setup that plans with one and commits with the other names
// its job at one site.
func userSite(userHome string) scheduler.Site {
	return scheduler.Site{UserHome: filepath.Clean(userHome)}
}

// defaultPATH is the PATH the scheduler gives a job whose definition sets
// none.
func (e Env) defaultPATH() string { return e.scheduler().DefaultPATH() }

// jobStatus is what the scheduler and the disk say about the job ref names
// (see scheduler.Inspector.Inspect).
func (e Env) jobStatus(userHome string, ref scheduler.Ref) scheduler.Status {
	return e.scheduler().Inspect(context.Background(), userSite(userHome), ref)
}

// unloadJob stops the job ref names (see scheduler.Controller.Unload).
func (e Env) unloadJob(userHome string, ref scheduler.Ref) error {
	return e.scheduler().Unload(context.Background(), userSite(userHome), ref)
}

// jobDefinition is what the definition of the job ref names says, without
// asking the scheduler about the job (see scheduler.Inspector.Definition).
func (e Env) jobDefinition(userHome string, ref scheduler.Ref) scheduler.Status {
	return e.scheduler().Definition(userSite(userHome), ref)
}

// backends is the schedulers as internal/setupjournal drives them, by the name
// a journal records: the adapter that name means (newScheduler), or, when a
// test stands in with Env.Scheduler, that one, so its stand-in (and TestMain's
// failing launchctl) applies there too. A journal that names a backend this
// system cannot use is refused rather than driven through another: its jobs
// are not that scheduler's to stop or start.
//
// setupjournal names each job by the definition it recorded, and a journal is
// recovered by whichever setup runs next for its data directory, perhaps with
// another $HOME (a sandbox overrides $HOME, and so where definitions are, but
// not the manager). So a job is addressed at the site its own definition is in
// (scheduler.Definer.Locate), never at the current user home: recovery stops
// and starts exactly the definition it restores, as it always has, and never a
// job of the same name elsewhere.
func (e Env) backends() setupjournal.Backends {
	return func(name string) (scheduler.Scheduler, error) {
		if e.Scheduler != nil {
			if e.Scheduler.Name() != name {
				return nil, fmt.Errorf("the interrupted setup used the %s scheduler, and this system's is %s; its jobs were left as they are", name, e.Scheduler.Name())
			}
			return e.Scheduler, nil
		}
		s, err := newScheduler(name)
		if err != nil {
			return nil, fmt.Errorf("the interrupted setup used the %s scheduler, and this system's is %s; its jobs were left as they are", name, e.namedScheduler("").Name())
		}
		return s, nil
	}
}
