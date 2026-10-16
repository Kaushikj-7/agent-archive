package systemd

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

func TestSearchRoleOwnsDistinctUnitsAndEnableLink(t *testing.T) {
	t.Parallel()
	s := Scheduler{}
	site := scheduler.Site{UserHome: t.TempDir()}
	inst := scheduler.Installation{DataHome: "/synthetic/data", Role: scheduler.Search}
	spec := scheduler.JobSpec{Executable: "/opt/agent-archive", DataHome: inst.DataHome, Args: []string{"_search-index"}, Interval: time.Minute, RunAtLoad: true}
	plan, err := s.Plan(site, inst, spec)
	if err != nil {
		t.Fatal(err)
	}
	data := string(plan.Artifacts[0].After)
	if !strings.HasPrefix(string(plan.Ref), "agent-archive-search-") || !strings.Contains(data, " _search-index\n") || !strings.Contains(data, "search-error.log") || strings.Contains(data, "collector") {
		t.Fatal(plan.Ref, data)
	}
	path, _ := plan.Artifacts[1].Path()
	gotSite, ref, err := s.Locate(path)
	if err != nil || ref != plan.Ref || gotSite != site {
		t.Fatal(gotSite, ref, err)
	}
	for _, bad := range []scheduler.Ref{"agent-archive-search-other", "agent-archive-search-123", "agent-archive-search-../../bad"} {
		if validRef(bad) {
			t.Fatal("accepted invalid ref", bad)
		}
	}
	spec.Args = []string{"_collect"}
	if _, err = s.Plan(site, inst, spec); err == nil {
		t.Fatal("search accepted collector command")
	}
}
