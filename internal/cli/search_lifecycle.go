package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

func managedRole(role setupjournal.ManagedRole, in installation, definition string, loaded bool, intent setupjournal.Intent) setupjournal.ManagedJob {
	return setupjournal.ManagedJob{Role: role, Backend: in.sched().Name(), Ref: string(in.ref()), Definition: definition, WasLoaded: loaded, Intent: intent}
}

// planSearchSetup joins the optional role to the existing file transaction.
func planSearchSetup(home, userHome, executable string, old, next config.Config, env Env, j *setupjournal.Journal) error {
	if !old.SearchLifecycleProtection && !next.SearchLifecycleProtection {
		return nil
	}
	in := env.installation(home, userHome).searchRole()
	plan, err := in.planJob(userHome, collectorJob(executable, home, env.collectorEnvironment(next.Storage)))
	if err != nil {
		return err
	}
	status := env.jobStatus(userHome, plan.Ref)
	if err := searchLifecycleState(in, status); err != nil {
		return err
	}
	changes, err := artifactChanges(plan.Artifacts)
	if err != nil {
		return err
	}
	intent := setupjournal.EnsureRunning
	if !next.SearchIndexing {
		intent = setupjournal.EnsureStopped
		var deletes []hooks.Change
		for _, change := range changes {
			if change.Existed {
				change.After = nil
				change.Delete = true
				deletes = append(deletes, change)
			}
		}
		changes = deletes
	}
	definition, _ := plan.Artifacts[0].Path()
	j.Changes = append(j.Changes, changes...)
	j.Jobs = []setupjournal.ManagedJob{
		managedRole(setupjournal.CollectorRole, env.installation(home, userHome), j.Plist, j.WasLoaded, setupjournal.EnsureRunning),
		managedRole(setupjournal.SearchRole, in, definition, status.State.Active(), intent),
	}
	return nil
}

func searchLifecycleState(in installation, status scheduler.Status) error {
	switch status.State {
	case scheduler.Unknown:
		return errors.New(unknownJobMessage(in.sched().Words(), problemOf(status)))
	case scheduler.AnotherInstallation:
		return fmt.Errorf("background search job %s belongs to another installation; nothing changed", in.ref())
	case scheduler.Loaded, scheduler.Running, scheduler.Missing:
		return nil
	default:
		return errors.New("background search ownership could not be determined")
	}
}

// planSearchRefresh preserves each role's loaded state independently.
func planSearchRefresh(home, userHome, exe string, cfg config.Config, env Env, plan *refreshPlan) error {
	if !cfg.SearchLifecycleProtection {
		return nil
	}
	collectorIntent := setupjournal.Keep
	if !plan.journal.FilesOnly {
		collectorIntent = setupjournal.RestartIfLoaded
	}
	collector := managedRole(setupjournal.CollectorRole, env.installation(home, userHome), plan.journal.Plist, plan.journal.WasLoaded, collectorIntent)
	in := env.installation(home, userHome).searchRole()
	definition := env.jobDefinition(userHome, in.ref())
	changes, err := refreshJob(in, userHome, home, exe, definition)
	if err != nil {
		return err
	}
	intent := setupjournal.Keep
	loaded := false
	if len(changes) > 0 {
		status := env.jobStatus(userHome, in.ref())
		if err := searchLifecycleState(in, status); err != nil {
			return err
		}
		loaded = status.State.Active()
		intent = setupjournal.RestartIfLoaded
		plan.restarted = plan.restarted || loaded
		plan.plist = true
		plan.definition = in.sched().Words().Definition
	}
	// Even an absent search definition has a stable owned path for recovery.
	path := definitionPath(definition)
	if path == "" {
		desired, err := in.planJob(userHome, collectorJob(exe, home, env.collectorEnvironment(cfg.Storage)))
		if err != nil {
			return err
		}
		path, _ = desired.Artifacts[0].Path()
	}
	plan.journal.Changes = append(plan.journal.Changes, changes...)
	plan.journal.Jobs = []setupjournal.ManagedJob{collector, managedRole(setupjournal.SearchRole, in, path, loaded, intent)}
	return nil
}

// lockSearchDestination fences destination/credential authority through cutover.
func lockSearchDestination(home string, expected config.Config) (func(), error) {
	release, err := local.NamedLockWait(home, "search-worker.lock", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("search indexing is finishing; retry migration: %w", err)
	}
	current, found, err := config.Load(home)
	currentNamespace, _ := contentSearchLocation(home, current)
	expectedNamespace, _ := contentSearchLocation(home, expected)
	if err != nil || !found || currentNamespace != expectedNamespace || current.Storage != expected.Storage || current.SearchIndexing != expected.SearchIndexing || current.Paused != expected.Paused || current.Archive.Enabled != expected.Archive.Enabled || setupjournal.TransactionPending(home) {
		release()
		return nil, errors.New("destination or search lifecycle changed, or setup pending; retry migration")
	}
	return release, nil
}
