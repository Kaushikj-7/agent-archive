package muse

import (
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// workflowChildStatus is a workflow child's lifecycle step, in the only order
// a child may report them.
type workflowChildStatus string

const (
	workflowChildScheduled workflowChildStatus = "scheduled"
	workflowChildStarted   workflowChildStatus = "started"
	workflowChildUsage     workflowChildStatus = "usage"
	workflowChildCompleted workflowChildStatus = "completed"
	workflowChildTerminal  workflowChildStatus = "terminal"
)

var workflowChildStatuses = []workflowChildStatus{workflowChildScheduled, workflowChildStarted, workflowChildUsage, workflowChildCompleted, workflowChildTerminal}

// workflowChildFieldNames are the retained members of a verified workflow
// child lifecycle record: the run that reported it, the child session, and
// the step and attempt.
var workflowChildFieldNames = []string{"kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "child_session_id", "status", "attempt"}

// workflowChildCommon are the native members every lifecycle record carries.
var workflowChildCommon = []string{"kind", "attempt", "child_id", "run_stream", "runtime_tool_call_id", "session_stream", "status", "task_id", "task_stream", "workflow_run_id"}

// workflowChildExtra are the dropped native members each step adds, with
// their types; a record with any other member is refused.
func workflowChildExtra(status workflowChildStatus, event map[string]any) ([]string, bool) {
	switch status {
	case workflowChildScheduled:
		return []string{"label"}, stringFields(event, "label")
	case workflowChildStarted:
		return []string{"isolated_worktree", "model_id", "routing_receipt"}, boolean(event["isolated_worktree"]) && stringFields(event, "model_id") && object(event["routing_receipt"]) != nil
	case workflowChildUsage:
		return []string{"usage"}, object(event["usage"]) != nil
	case workflowChildCompleted:
		return []string{"duration_ms", "output_ref", "result_ref"}, integer(event["duration_ms"], 0) && stringFields(event, "output_ref", "result_ref")
	case workflowChildTerminal:
		return []string{"duration_ms", "output_ref", "result_ref", "terminal"}, integer(event["duration_ms"], 0) && stringFields(event, "output_ref", "result_ref", "terminal")
	}
	return nil, false
}

// projectWorkflowChild qualifies a native workflow child lifecycle record
// from within the record alone. It must name a child session other than this
// session, the child's own run, and a task stream of its task. Only the child
// session ID, status and attempt are retained; the label, child and workflow
// identifiers, routing, usage and result references are dropped. Joins to the
// task and to the child's earlier records are checked by
// nativeBodyJoins.workflowChild, and the run's place in the conversation by
// the parser.
func projectWorkflowChild(r retained, p, event map[string]any) (map[string]any, error) {
	status := workflowChildStatus(str(event["status"]))
	extra, typed := workflowChildExtra(status, event)
	session, run := object(event["session_stream"]), object(event["run_stream"])
	child := str(session["id"])
	if r.Causation != nil || !typed || !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") || !keys(event, append(slices.Clone(workflowChildCommon), extra...)...) || !streamEquals(session, "session", child) || !streamEquals(run, "run", str(run["id"])) || str(run["id"]) == str(p["run_id"]) || !identifier(event["child_id"]) || !identifier(event["task_id"]) || str(event["task_stream"]) != "task/"+str(event["task_id"]) || !integer(event["attempt"], 1) || str(event["workflow_run_id"]) == "" || str(event["runtime_tool_call_id"]) == "" {
		return nil, errFormat
	}
	f := map[string]any{"kind": p["kind"], "event_kind": event["kind"], "child_session_id": child, "status": string(status), "attempt": event["attempt"]}
	copyFields(f, p, "run_id", "source_run_record_id", "source_run_record_sequence")
	r.Fields = f
	if !workflowChildFields(r) {
		return nil, errFormat
	}
	return f, nil
}

// workflowChildFields validates a retained workflow child lifecycle record.
func workflowChildFields(r retained) bool {
	f := r.Fields
	child := str(f["child_session_id"])
	return r.PayloadSchema == 1 && r.Causation == nil && keys(f, workflowChildFieldNames...) && str(f["kind"]) == "run" && str(f["event_kind"]) == string(eventWorkflowChildLifecycle) && conversationSourceShape(f) && identifier(child) && child != r.Stream.ID && slices.Contains(workflowChildStatuses, workflowChildStatus(str(f["status"]))) && integer(f["attempt"], 1)
}

// workflowChildStep is a child's last lifecycle step and what each later step
// must repeat. The retained proof knows only the run, attempt and step.
type workflowChildStep struct {
	// line is the native line of the child's first step.
	line int64
	// run is the parent run reporting the child.
	run string
	// task is the parent task that runs the child.
	task string
	// childID is the child's workflow control ID.
	childID string
	// childRun is the child's own run.
	childRun string
	// workflowRun is the workflow run the child belongs to.
	workflowRun string
	// toolCall is the workflow tool call that launched the child.
	toolCall string
	// attempt is the child's attempt number.
	attempt int64
	// step is the index of the step's status in workflowChildStatuses.
	step int
}

// nextWorkflowChildStep accepts a child's next lifecycle step: its first step
// links a child no other record has linked, and each later step repeats the
// first's identities and moves strictly forward through the statuses.
func nextWorkflowChildStep(steps map[string]workflowChildStep, children map[string]bool, child string, next workflowChildStep) error {
	prior, linked := steps[child]
	if linked {
		next.line = prior.line
	}
	if !linked && children[child] || linked && (prior.run != next.run || prior.task != next.task || prior.childID != next.childID || prior.childRun != next.childRun || prior.workflowRun != next.workflowRun || prior.toolCall != next.toolCall || prior.attempt != next.attempt || prior.step >= next.step) {
		return errFormat
	}
	children[child] = true
	steps[child] = next
	return nil
}

// workflowChild joins a native lifecycle record to this session's own
// records: its task must be a task the session starts in that run, and each
// child keeps one child ID, child run, workflow run and tool call. No two
// children share a child ID or child run. Muse schedules a child before it
// records the task, so only a child's later steps require the task already;
// finish checks the rest once the whole session is read.
func (s *nativeBodyJoins) workflowChild(r retained, p map[string]any) error {
	event := object(p["event"])
	child := str(object(event["session_stream"])["id"])
	next := workflowChildStep{line: r.Position.Line, run: str(p["run_id"]), task: str(event["task_id"]), childID: str(event["child_id"]), childRun: str(object(event["run_stream"])["id"]), workflowRun: str(event["workflow_run_id"]), toolCall: str(event["runtime_tool_call_id"]), attempt: num(event["attempt"]), step: slices.Index(workflowChildStatuses, workflowChildStatus(str(event["status"])))}
	_, linked := s.workflowSteps[child]
	if linked && !s.taskIDs[taskKey{next.run, next.task}] || next.step < 0 {
		return errFormat
	}
	if !linked && (s.workflowControls[next.childID] || s.workflowRuns[next.childRun]) {
		return errFormat
	}
	if err := nextWorkflowChildStep(s.workflowSteps, s.children, child, next); err != nil {
		return err
	}
	s.workflowControls[next.childID], s.workflowRuns[next.childRun] = true, true
	return nil
}

// finish ends a filter pass that read all of in: the input must be unchanged,
// and every workflow child's task must be one this session started in the
// child's run. A child without one refuses at its first step.
func (s *nativeBodyJoins) finish(in agentapi.NativeInput) error {
	if err := in.File.Check(); err != nil {
		return err
	}
	var first int64
	for _, step := range s.workflowSteps {
		if !s.taskIDs[taskKey{step.run, step.task}] && (first == 0 || step.line < first) {
			first = step.line
		}
	}
	if first != 0 {
		return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, string(payloadRuntimeSession)+"/"+string(eventWorkflowChildLifecycle), first, errFormat)
	}
	return nil
}

// observeWorkflowChild accepts a retained lifecycle record from a run of this
// session's own conversation, each child's steps in order from one run.
func observeWorkflowChild(r retained, children map[string]bool, steps map[string]workflowChildStep, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool) error {
	f := r.Fields
	if !workflowChildFields(r) || observeConversationSource(f, str(f["run_id"]), joined, last, seen) != nil {
		return errFormat
	}
	next := workflowChildStep{run: str(f["run_id"]), attempt: num(f["attempt"]), step: slices.Index(workflowChildStatuses, workflowChildStatus(str(f["status"])))}
	return nextWorkflowChildStep(steps, children, str(f["child_session_id"]), next)
}
