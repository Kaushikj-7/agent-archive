package setupjournal

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
)

type managedBackendModel struct {
	scheduler.Scheduler
	loaded map[scheduler.Ref]bool
	fail   scheduler.Ref
}

type managedScenario string

const (
	managedScenarioRollback     managedScenario = "rollback"
	managedScenarioMixedRefresh managedScenario = "mixed_refresh"
	managedScenarioPartialFiles managedScenario = "partial_files"
	managedScenarioBothLoaded   managedScenario = "both_loaded"
)

func (m *managedBackendModel) Inspect(_ context.Context, site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	status := m.Definition(site, ref)
	status.State = scheduler.Missing
	if m.loaded[ref] {
		status.State = scheduler.Loaded
	}
	return status
}

func (m *managedBackendModel) Load(_ context.Context, _ scheduler.Site, ref scheduler.Ref) error {
	if m.fail == ref {
		m.fail = ""
		return errors.New("synthetic second-role load failure")
	}
	m.loaded[ref] = true
	return nil
}

func (m *managedBackendModel) Unload(_ context.Context, _ scheduler.Site, ref scheduler.Ref) error {
	delete(m.loaded, ref)
	return nil
}

func TestManagedLifecycleRollbackAndMixedRefreshAcrossBackends(t *testing.T) {
	t.Parallel()
	for _, backend := range []scheduler.Scheduler{launchd.Scheduler{}, systemd.Scheduler{}} {
		for _, scenario := range []managedScenario{managedScenarioRollback, managedScenarioMixedRefresh, managedScenarioPartialFiles, managedScenarioBothLoaded} {
			t.Run(backend.Name()+"/"+string(scenario), func(t *testing.T) {
				t.Parallel()
				home := local.CanonicalPath(t.TempDir())
				site := scheduler.Site{UserHome: t.TempDir()}
				model := &managedBackendModel{Scheduler: backend, loaded: map[scheduler.Ref]bool{}}
				var j Journal
				for _, role := range []scheduler.Role{scheduler.Collector, scheduler.Search} {
					inst := scheduler.Installation{DataHome: home, Role: role}
					command, err := role.Command()
					must(t, err)
					spec := scheduler.JobSpec{Executable: "/opt/old-agent-archive", DataHome: home, Args: []string{command}, Interval: time.Minute, RunAtLoad: true}
					oldPlan, err := backend.Plan(site, inst, spec)
					must(t, err)
					spec.Executable = "/opt/new-agent-archive"
					newPlan, err := backend.Plan(site, inst, spec)
					must(t, err)
					for i, artifact := range oldPlan.Artifacts {
						path, _ := artifact.Path()
						must(t, local.WriteBytes(path, artifact.After))
						j.Changes = append(j.Changes, fileapply.Change{Path: path, Before: artifact.After, After: newPlan.Artifacts[i].After, Existed: true, Mode: 0o600})
					}
					name := CollectorRole
					if role == scheduler.Search {
						name = SearchRole
					}
					definition, _ := newPlan.Artifacts[0].Path()
					j.Jobs = append(j.Jobs, ManagedJob{Role: name, Backend: backend.Name(), Ref: string(newPlan.Ref), Definition: definition, WasLoaded: true, Intent: EnsureRunning})
					model.loaded[newPlan.Ref] = true
				}
				collector, search := scheduler.Ref(j.Jobs[0].Ref), scheduler.Ref(j.Jobs[1].Ref)
				lookup := func(string) (scheduler.Scheduler, error) { return model, nil }
				switch scenario {
				case managedScenarioMixedRefresh:
					j.Jobs[0].Intent = Keep
					j.Jobs[1].Intent = RestartIfLoaded
					j.Jobs[1].WasLoaded = false
					delete(model.loaded, search)
					j.Changes = j.Changes[len(j.Changes)/2:]
					must(t, Commit(home, j, lookup))
					if !model.loaded[collector] || model.loaded[search] {
						t.Fatal("mixed refresh changed manager activation")
					}
					return
				case managedScenarioRollback:
					model.fail = search
					if err := Commit(home, j, lookup); err == nil {
						t.Fatal("second-role failure did not roll back")
					}
				case managedScenarioPartialFiles, managedScenarioBothLoaded:
					must(t, local.Write(JournalPath(home), j))
					delete(model.loaded, collector)
					delete(model.loaded, search)
					if scenario == managedScenarioPartialFiles {
						must(t, fileapply.Apply(j.Changes[:1]))
					} else {
						must(t, fileapply.Apply(j.Changes))
						model.loaded[collector] = true
						model.loaded[search] = true
					}
					must(t, Recover(home, lookup, noLock))
				}
				if !model.loaded[collector] || !model.loaded[search] || TransactionPending(home) {
					t.Fatal("multi-role restore incomplete", model.loaded)
				}
				for _, change := range j.Changes {
					data, err := os.ReadFile(change.Path)
					must(t, err)
					if string(data) != string(change.Before) {
						t.Fatal("role definition not restored", change.Path)
					}
				}
			})
		}
	}
}
