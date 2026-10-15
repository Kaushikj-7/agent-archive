package launchd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

func TestSearchRoleHasIndependentDefinitionAndNoCollectorAliases(t *testing.T) {
	t.Parallel()
	s := Scheduler{Run: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("pure search planning reached manager")
		return nil, nil
	}}
	site := scheduler.Site{UserHome: t.TempDir()}
	inst := scheduler.Installation{DataHome: "/synthetic/data", Role: scheduler.Search}
	spec := scheduler.JobSpec{Executable: "/opt/agent-archive", DataHome: inst.DataHome, Args: []string{"_search-index"}, Interval: time.Minute, RunAtLoad: true}
	plan, err := s.Plan(site, inst, spec)
	if err != nil {
		t.Fatal(err)
	}
	data := string(plan.Artifacts[0].After)
	if !strings.HasPrefix(string(plan.Ref), "com.agent-archive.search.") || !strings.Contains(data, "<string>_search-index</string>") || !strings.Contains(data, "search-error.log") || strings.Contains(data, "collector") {
		t.Fatal(string(plan.Ref), data)
	}
	jobs, err := s.Installed(t.Context(), site, inst)
	if err != nil || len(jobs) != 1 || jobs[0].Ref != plan.Ref {
		t.Fatal(jobs, err)
	}
	spec.Args = []string{"_collect"}
	if _, err = s.Plan(site, inst, spec); err == nil {
		t.Fatal("search role accepted collector command")
	}
	inst.Role = "arbitrary"
	if _, err = s.Plan(site, inst, spec); err == nil {
		t.Fatal("unknown role accepted")
	}
}
