package backfill

import "sync"

// PlanningStage identifies the current planning step and its display label.
type PlanningStage string

const (
	// PlanningStageDiscoveringSessions enumerates native session stores.
	PlanningStageDiscoveringSessions PlanningStage = "Discovering sessions"
	// PlanningStageResolvingProjects maps sessions to their projects.
	PlanningStageResolvingProjects PlanningStage = "Resolving projects"
	// PlanningStageCheckingProjectHistory checks project recovery evidence.
	PlanningStageCheckingProjectHistory PlanningStage = "Checking project history"
	// PlanningStageCheckingNativeHistory checks native session relationships.
	PlanningStageCheckingNativeHistory PlanningStage = "Checking native history"
	// PlanningStageCheckingArchive classifies sessions against archive state.
	PlanningStageCheckingArchive PlanningStage = "Checking archive"
	// PlanningStageCheckingSessions inspects selected transcripts.
	PlanningStageCheckingSessions PlanningStage = "Checking sessions"
	// PlanningStageFindingSubagentSessions enumerates imported parents' children.
	PlanningStageFindingSubagentSessions PlanningStage = "Finding subagent sessions"
	// PlanningStageCheckingSubagentSessions inspects child transcripts.
	PlanningStageCheckingSubagentSessions PlanningStage = "Checking subagent sessions"
	// PlanningStageCheckingDatabaseSessions checks database-only sessions.
	PlanningStageCheckingDatabaseSessions PlanningStage = "Checking database sessions"
	// PlanningStageFinishingPlan completes nested-session and recovery decisions.
	PlanningStageFinishingPlan PlanningStage = "Finishing plan"
)

// PlanningProgress describes one planning phase. Total is zero when discovery
// has not established a work count. Counts describe this phase, not the import.
type PlanningProgress struct {
	Stage PlanningStage
	Done  int
	Total int
}

func (e Environment) planningProgress(stage PlanningStage, done, total int) {
	if e.Progress != nil {
		e.Progress(PlanningProgress{Stage: stage, Done: done, Total: total})
	}
}

// planningCounter serializes worker notifications and counts completed work.
func (e Environment) planningCounter(stage PlanningStage, total int) func() {
	if e.Progress == nil {
		return func() {}
	}
	e.planningProgress(stage, 0, total)
	var mu sync.Mutex
	done := 0
	return func() {
		mu.Lock()
		defer mu.Unlock()
		done++
		e.planningProgress(stage, done, total)
	}
}
