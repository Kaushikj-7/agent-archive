package cli

import (
	"fmt"
	"sync"

	"github.com/wangjohn/agent-archive/internal/backfill"
)

// planningDisplay keeps worker updates separate from the spinner's writer.
type planningDisplay struct {
	mu       sync.Mutex
	initial  string
	progress backfill.PlanningProgress
}

func newPlanningDisplay(initial string) *planningDisplay {
	return &planningDisplay{initial: initial}
}

func (d *planningDisplay) observe(p backfill.PlanningProgress) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.progress = p
}

func (d *planningDisplay) label() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.progress
	label := string(p.Stage)
	if label == "" {
		label = d.initial
	}
	if p.Total > 0 {
		label += fmt.Sprintf(": %d/%d (%d%%)", p.Done, p.Total, 100*p.Done/p.Total)
	} else if p.Stage == backfill.PlanningStageDiscoveringSessions {
		label += fmt.Sprintf(": %d found", p.Done)
	}
	return label
}
