package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const (
	syntheticWorkflowChild   = "00000000-0000-7000-8000-0000000000c3"
	syntheticWorkflowControl = "5c5c5c5c-5c5c-4c5c-8c5c-5c5c5c5c5c5c"
	syntheticWorkflowRun     = "6d6d6d6d-6d6d-4d6d-8d6d-6d6d6d6d6d6d"
	syntheticWorkflowTask    = "7e7e7e7e-7e7e-4e7e-8e7e-7e7e7e7e7e7e"
	// taskProposedMarker stands for a task record withWorkflowSteps writes in
	// place of a lifecycle record.
	taskProposedMarker = "synthetic_task_proposed"
)

// workflowSentinels are dropped native members no retained output may contain.
var workflowSentinels = []string{"PRIVATE_WORKFLOW_LABEL_SENTINEL", "PRIVATE_WORKFLOW_MODEL_SENTINEL", "PRIVATE_ROUTING_SENTINEL", "PRIVATE_OUTPUT_REF_SENTINEL", "PRIVATE_RESULT_REF_SENTINEL", "PRIVATE_WORKFLOW_RUN_SENTINEL", "PRIVATE_TOOL_CALL_SENTINEL", syntheticWorkflowControl, syntheticWorkflowRun}

// workflowStep is a synthetic lifecycle step of one workflow child of the
// synthetic omissions session's run, for a task it starts after scheduling it.
func workflowStep(status workflowChildStatus, change ...func(e map[string]any)) func(owner string) map[string]any {
	return func(string) map[string]any {
		e := map[string]any{
			"kind": string(eventWorkflowChildLifecycle), "attempt": json.Number("1"), "status": string(status),
			"child_id": syntheticWorkflowControl, "run_stream": map[string]any{"kind": "run", "id": syntheticWorkflowRun},
			"session_stream": map[string]any{"kind": "session", "id": syntheticWorkflowChild},
			"task_id":        syntheticWorkflowTask, "task_stream": "task/" + syntheticWorkflowTask,
			"workflow_run_id": "PRIVATE_WORKFLOW_RUN_SENTINEL", "runtime_tool_call_id": "PRIVATE_TOOL_CALL_SENTINEL",
		}
		switch status {
		case workflowChildScheduled:
			e["label"] = "PRIVATE_WORKFLOW_LABEL_SENTINEL"
		case workflowChildStarted:
			e["isolated_worktree"], e["model_id"] = false, "PRIVATE_WORKFLOW_MODEL_SENTINEL"
			e["routing_receipt"] = map[string]any{"profile_id": "PRIVATE_ROUTING_SENTINEL"}
		case workflowChildUsage:
			e["usage"] = map[string]any{"input_tokens": json.Number("7"), "output_tokens": json.Number("3")}
		case workflowChildCompleted, workflowChildTerminal:
			e["duration_ms"], e["output_ref"], e["result_ref"] = json.Number("1200"), "PRIVATE_OUTPUT_REF_SENTINEL", "PRIVATE_RESULT_REF_SENTINEL"
			if status == workflowChildTerminal {
				e["terminal"] = "completed"
			}
		}
		for _, c := range change {
			c(e)
		}
		return e
	}
}

// taskProposed stands for the record that starts the workflow child's task.
func taskProposed(string) map[string]any { return map[string]any{"kind": taskProposedMarker} }

// withWorkflowSteps inserts the given steps, as withChildLinks does, writing
// each taskProposed step as the run's proposal of the workflow task.
func withWorkflowSteps(t *testing.T, change func(m map[string]any), events ...func(owner string) map[string]any) []byte {
	t.Helper()
	return withChildLinks(t, func(m map[string]any) {
		p := object(m["payload"])
		if str(object(p["event"])["kind"]) == taskProposedMarker {
			m["payload"] = map[string]any{"kind": "task", "run_id": syntheticRun, "task_id": syntheticWorkflowTask, "event": map[string]any{"kind": "proposed"}}
		}
		if change != nil {
			change(m)
		}
	}, events...)
}

// fullLifecycle is a workflow child's observed order: Muse schedules the child
// before it records the task.
var fullLifecycle = []func(string) map[string]any{workflowStep(workflowChildScheduled), taskProposed, workflowStep(workflowChildStarted), workflowStep(workflowChildUsage), workflowStep(workflowChildCompleted), workflowStep(workflowChildTerminal)}

func TestVerifiedWorkflowChildRetainsOnlyTheChildAndSteps(t *testing.T) {
	t.Parallel()
	raw := withWorkflowSteps(t, nil, fullLifecycle...)
	// The native input is also a fuzz seed.
	golden.Check(t, filepath.Join("testdata", "synthetic-workflow-child.jsonl"), raw)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.NativeChildIDs, []string{syntheticWorkflowChild}) {
		t.Fatalf("verified child IDs = %v", out.NativeChildIDs)
	}
	retained := append(bytes.Join(out.Records, []byte("\n")), '\n')
	for _, private := range append(slices.Clone(workflowSentinels), "task_stream", "run_stream", "session_stream", "label", "routing_receipt", "duration_ms", "isolated_worktree") {
		if bytes.Contains(retained, []byte(private)) {
			t.Fatalf("workflow child retained %q", private)
		}
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-workflow-child.golden.jsonl"), retained)
	b := bundleFor(t, out)
	if _, err := (Parser{}).Parse(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !slices.EqualFunc(again.Records, out.Records, bytes.Equal) || !slices.Equal(again.NativeChildIDs, out.NativeChildIDs) {
		t.Fatalf("refilter changed the workflow child: %v", err)
	}
}

func TestWorkflowChildBesideAMemoryReminderChildLinksBoth(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, withWorkflowSteps(t, nil, append([]func(string) map[string]any{childLinkEvent}, fullLifecycle...)...), agentapi.ReadLimits{})
	if err != nil || !slices.Equal(out.NativeChildIDs, []string{syntheticChild, syntheticWorkflowChild}) {
		t.Fatalf("children = %v: %v", out.NativeChildIDs, err)
	}
}

func TestUnverifiedWorkflowChildrenRefuseTheSession(t *testing.T) {
	t.Parallel()
	other := "00000000-0000-7000-8000-0000000000c9"
	steps := func(first func(e map[string]any)) []func(string) map[string]any {
		return []func(string) map[string]any{workflowStep(workflowChildScheduled, first), taskProposed, workflowStep(workflowChildStarted)}
	}
	later := func(change func(e map[string]any)) []func(string) map[string]any {
		return []func(string) map[string]any{workflowStep(workflowChildScheduled), taskProposed, workflowStep(workflowChildStarted, change)}
	}
	otherChild := func(e map[string]any) {
		e["session_stream"] = map[string]any{"kind": "session", "id": other}
	}
	cases := map[string][]func(string) map[string]any{
		"task never started":         {workflowStep(workflowChildScheduled)},
		"later step before its task": {workflowStep(workflowChildScheduled), workflowStep(workflowChildStarted), taskProposed},
		"unknown task":               {workflowStep(workflowChildScheduled, func(e map[string]any) { e["task_id"], e["task_stream"] = other, "task/"+other }), taskProposed},
		"task stream mismatch":       steps(func(e map[string]any) { e["task_stream"] = "task/" + other }),
		"task stream object":         steps(func(e map[string]any) { e["task_stream"] = map[string]any{"kind": "task", "id": syntheticWorkflowTask} }),
		"self as child": steps(func(e map[string]any) {
			e["session_stream"] = map[string]any{"kind": "session", "id": "00000000-0000-7000-8000-000000000001"}
		}),
		"child not an ID": steps(func(e map[string]any) { e["session_stream"] = map[string]any{"kind": "session", "id": "child"} }),
		"child stream not a session": steps(func(e map[string]any) {
			e["session_stream"] = map[string]any{"kind": "run", "id": syntheticWorkflowChild}
		}),
		"child run is this run": steps(func(e map[string]any) { e["run_stream"] = map[string]any{"kind": "run", "id": syntheticRun} }),
		"control ID not an ID":  steps(func(e map[string]any) { e["child_id"] = "agent-1" }),
		"unknown status":        steps(func(e map[string]any) { e["status"] = "failed" }),
		"attempt zero":          steps(func(e map[string]any) { e["attempt"] = json.Number("0") }),
		"empty workflow run":    steps(func(e map[string]any) { e["workflow_run_id"] = "" }),
		"unknown member":        steps(func(e map[string]any) { e["future"] = true }),
		"missing member":        steps(func(e map[string]any) { delete(e, "label") }),
		"another step's member": steps(func(e map[string]any) { e["usage"] = map[string]any{} }),
		"label not text":        steps(func(e map[string]any) { e["label"] = json.Number("1") }),
		"steps out of order":    {workflowStep(workflowChildStarted), taskProposed, workflowStep(workflowChildScheduled)},
		"step repeated":         {workflowStep(workflowChildScheduled), taskProposed, workflowStep(workflowChildStarted), workflowStep(workflowChildStarted)},
		"control ID changed":    later(func(e map[string]any) { e["child_id"] = "8f8f8f8f-8f8f-4f8f-8f8f-8f8f8f8f8f8f" }),
		"child run changed":     later(func(e map[string]any) { e["run_stream"] = map[string]any{"kind": "run", "id": other} }),
		"workflow run changed":  later(func(e map[string]any) { e["workflow_run_id"] = "another" }),
		"tool call changed":     later(func(e map[string]any) { e["runtime_tool_call_id"] = "another" }),
		"task changed":          later(func(e map[string]any) { e["task_id"], e["task_stream"] = syntheticTask, "task/"+syntheticTask }),
		"attempt changed":       later(func(e map[string]any) { e["attempt"] = json.Number("2") }),
		"two children, one control ID": {workflowStep(workflowChildScheduled), taskProposed, workflowStep(workflowChildScheduled, otherChild, func(e map[string]any) {
			e["run_stream"] = map[string]any{"kind": "run", "id": other}
		})},
		"two children, one child run": {workflowStep(workflowChildScheduled), taskProposed, workflowStep(workflowChildScheduled, otherChild, func(e map[string]any) {
			e["child_id"] = "8f8f8f8f-8f8f-4f8f-8f8f-8f8f8f8f8f8f"
		})},
		"also a memory-reminder child": {func(owner string) map[string]any {
			e := childLinkEvent(owner)
			e["child_session_id"] = syntheticWorkflowChild
			return e
		}, workflowStep(workflowChildScheduled), taskProposed},
		"memory-reminder link after the steps": {workflowStep(workflowChildScheduled), taskProposed, func(owner string) map[string]any {
			e := childLinkEvent(owner)
			e["child_session_id"] = syntheticWorkflowChild
			return e
		}},
	}
	// Each case changes one thing in one of these accepted bases.
	unchanged := func(map[string]any) {}
	bases := map[string][]func(string) map[string]any{
		"steps": steps(unchanged), "later": later(unchanged),
		"two children": {workflowStep(workflowChildScheduled), taskProposed, workflowStep(workflowChildScheduled, otherChild, func(e map[string]any) {
			e["run_stream"], e["child_id"] = map[string]any{"kind": "run", "id": other}, "8f8f8f8f-8f8f-4f8f-8f8f-8f8f8f8f8f8f"
		})},
	}
	for name, events := range bases {
		if out, err := filterBytes(t, withWorkflowSteps(t, nil, events...), agentapi.ReadLimits{}); err != nil || len(out.NativeChildIDs) == 0 {
			t.Fatalf("base %s refused: %v", name, err)
		}
	}
	for name, events := range cases {
		out, err := filterBytes(t, withWorkflowSteps(t, nil, events...), agentapi.ReadLimits{})
		if err == nil || len(out.Records) != 0 {
			t.Errorf("%s: unverified workflow child was not refused", name)
		}
	}
	for name, change := range map[string]func(m map[string]any){
		"caused": func(m map[string]any) {
			if str(object(object(m["payload"])["event"])["kind"]) == string(eventWorkflowChildLifecycle) {
				m["causation_id"] = "00000000-0000-4000-8000-000000000005"
			}
		},
		"run of no turn": func(m map[string]any) { object(m["payload"])["run_id"] = other },
	} {
		out, err := filterBytes(t, withWorkflowSteps(t, change, workflowStep(workflowChildScheduled), taskProposed), agentapi.ReadLimits{})
		if err == nil || len(out.Records) != 0 {
			t.Errorf("%s: unverified workflow child was not refused", name)
		}
	}
}

func TestWorkflowChildWithoutItsTaskNamesTheChildRecord(t *testing.T) {
	t.Parallel()
	_, err := filterBytes(t, withWorkflowSteps(t, nil, workflowStep(workflowChildScheduled)), agentapi.ReadLimits{})
	var refused *agentapi.NativeRefusalError
	if !errors.As(err, &refused) || refused.Family != "runtime.session/workflow_child_lifecycle" || refused.Line != 20 {
		t.Fatalf("refusal = %v", err)
	}
}

func TestRetainedWorkflowChildrenAreClosed(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, withWorkflowSteps(t, nil, append([]func(string) map[string]any{childLinkEvent}, fullLifecycle...)...), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// Each change edits the retained fields of the child's steps, in order.
	for name, change := range map[string]func(steps []map[string]any){
		"self as child":      func(steps []map[string]any) { steps[0]["child_session_id"] = "00000000-0000-7000-8000-000000000001" },
		"extra member":       func(steps []map[string]any) { steps[0]["task_id"] = syntheticWorkflowTask },
		"unknown status":     func(steps []map[string]any) { steps[0]["status"] = "failed" },
		"attempt text":       func(steps []map[string]any) { steps[0]["attempt"] = "1" },
		"attempt zero":       func(steps []map[string]any) { steps[0]["attempt"] = json.Number("0") },
		"steps out of order": func(steps []map[string]any) { steps[0]["status"] = string(workflowChildCompleted) },
		"step repeated":      func(steps []map[string]any) { steps[1]["status"] = string(workflowChildScheduled) },
		"attempt changed":    func(steps []map[string]any) { steps[1]["attempt"] = json.Number("2") },
		"also a memory-reminder child": func(steps []map[string]any) {
			for _, step := range steps {
				step["child_session_id"] = syntheticChild
			}
		},
	} {
		b := bundleFor(t, out)
		var steps []map[string]any
		for _, m := range b.NativeRecords {
			if f := object(m["fields"]); str(f["event_kind"]) == string(eventWorkflowChildLifecycle) {
				steps = append(steps, f)
			}
		}
		if len(steps) != len(workflowChildStatuses) {
			t.Fatalf("retained %d workflow steps", len(steps))
		}
		change(steps)
		if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
			t.Errorf("%s: altered retained workflow child parsed", name)
		}
	}
}
