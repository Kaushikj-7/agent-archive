package backfill

import (
	"fmt"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestPlanningProgressCountsSelectedTranscripts(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	for i := range 3 {
		id := fmt.Sprintf("session-%d", i)
		tr.write("home/"+claudeFile("repo", id), claudeTranscript(id, repo, fixedCreated))
	}
	var updates []PlanningProgress
	env := tr.env()
	env.Progress = func(p PlanningProgress) { updates = append(updates, p) }
	p := plan(t, env, states{"session-0": SkipAlreadyArchived}, config.Config{}, Filters{})
	if len(p.Candidates) != 3 {
		t.Fatalf("progress changed plan: %d candidates", len(p.Candidates))
	}
	var found, checked []PlanningProgress
	for _, u := range updates {
		if u.Stage == PlanningStageDiscoveringSessions {
			found = append(found, u)
		}
		if u.Stage == PlanningStageCheckingSessions {
			checked = append(checked, u)
		}
	}
	if len(found) != 4 || found[3].Done != 3 || found[3].Total != 0 {
		t.Fatalf("discovery counts: %+v", found)
	}
	if len(checked) != 3 {
		t.Fatalf("checking counts: %+v", checked)
	}
	for i, u := range checked {
		if u.Done != i || u.Total != 2 {
			t.Fatalf("checking counts must exclude archived transcript: %+v", checked)
		}
	}
}

func TestPlanningCounterSerializesWorkerUpdates(t *testing.T) {
	var updates []PlanningProgress
	env := Environment{Progress: func(p PlanningProgress) { updates = append(updates, p) }}
	const total = 100
	completed := env.planningCounter(PlanningStageCheckingSessions, total)
	var wg sync.WaitGroup
	for range total {
		wg.Go(completed)
	}
	wg.Wait()
	if len(updates) != total+1 {
		t.Fatalf("got %d updates", len(updates))
	}
	for i, u := range updates {
		if u.Done != i || u.Total != total {
			t.Fatalf("out of order update: %+v at %d", u, i)
		}
	}
}
