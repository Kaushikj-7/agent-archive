package muse

import (
	"encoding/json"
	"reflect"
	"slices"
)

const (
	payloadApprovalWaitStarted  payloadType  = "approval_wait.effect.started"
	payloadApprovalWaitTerminal payloadType  = "approval_wait.effect.terminal"
	payloadToolBatchStarted     payloadType  = "tool_batch.effect.started"
	payloadToolBatchTerminal    payloadType  = "tool_batch.effect.terminal"
	eventReasoningCommitted     runtimeEvent = "reasoning_committed"
)

func newFamily(r retained) bool {
	if r.Type == payloadRuntimeSession {
		return r.PayloadSchema == 3 || str(r.Fields["event_kind"]) == "reasoning_committed" || str(r.Fields["event_kind"]) == "output"
	}
	return slices.Contains([]payloadType{payloadApprovalWaitStarted, payloadApprovalWaitTerminal, payloadToolBatchStarted, payloadToolBatchTerminal}, r.Type)
}

func nativeNewFamily(r retained, p map[string]any) bool {
	r.Fields = map[string]any{"event_kind": object(p["event"])["kind"]}
	return newFamily(r)
}

func integer(v any, minimum int64) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	x, e := n.Int64()
	return e == nil && x >= minimum && x <= exactMax
}

func boolean(v any) bool { _, ok := v.(bool); return ok }

func identifier(v any) bool { _, ok := uuid(str(v)); return ok }

func streamEquals(v any, kind, id string) bool {
	m := object(v)
	return keys(m, "kind", "id") && str(m["kind"]) == kind && identifier(m["id"]) && str(m["id"]) == id
}

func tagged(v any, kind string) bool {
	m := object(v)
	return keys(m, "kind") && str(m["kind"]) == kind
}

func modelShape(v any) bool {
	m := object(v)
	return keys(m, "provider_id", "model_id", "reasoning_effort") && stringFields(m, "provider_id", "model_id", "reasoning_effort")
}

func requirement(v any, pending string) bool {
	m := object(v)
	return keys(m, "pending_action_id", "source_index") && str(m["pending_action_id"]) == pending && integer(m["source_index"], 0)
}

func stage(v any, pending string, evidence bool) bool {
	m := object(v)
	names := []string{"argv", "requirement_id", "resolution", "source_position", "total_stages"}
	kind := "allow_once"
	if !evidence {
		names = append(names, "argv_complete", "prefix_candidates", "suggested_prefix")
		kind = "unresolved"
		if !boolean(m["argv_complete"]) || m["suggested_prefix"] != nil {
			return false
		}
		prefixes, ok := m["prefix_candidates"].([]any)
		if !ok {
			return false
		}
		for _, p := range prefixes {
			if !stringArray(p) {
				return false
			}
		}
	}
	return keys(m, names...) && stringArray(m["argv"]) && requirement(m["requirement_id"], pending) && num(object(m["requirement_id"])["source_index"]) == 0 && integer(m["source_position"], 0) && num(m["source_position"]) == 1 && integer(m["total_stages"], 1) && num(m["total_stages"]) == 1 && tagged(m["resolution"], kind)
}

func oneStage(v any, pending string, evidence bool) bool {
	a, ok := v.([]any)
	return ok && len(a) == 1 && stage(a[0], pending, evidence)
}

func choices(v any, copyChoice bool) bool {
	a, ok := v.([]any)
	if !ok || len(a) != 2 {
		return false
	}
	kinds := map[string]bool{}
	for _, v := range a {
		m := object(v)
		names := []string{"choice_id", "decision", "label"}
		if copyChoice {
			names = append(names, "matcher", "rule_preview")
			if m["matcher"] != nil || m["rule_preview"] != nil {
				return false
			}
		}
		k := str(object(m["decision"])["kind"])
		if !keys(m, names...) || !stringFields(m, "choice_id", "label") || !tagged(m["decision"], k) || !slices.Contains([]string{"approved", "abort"}, k) || kinds[k] {
			return false
		}
		kinds[k] = true
	}
	return true
}

func candidates(v any) bool {
	a, ok := v.([]any)
	if !ok || len(a) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range a {
		m := object(v)
		k := str(m["decision"])
		if !keys(m, "decision", "matcher") || m["matcher"] != nil || !slices.Contains([]string{"approved", "abort"}, k) || seen[k] {
			return false
		}
		seen[k] = true
	}
	return true
}

func pendingShape(m map[string]any, owner, run string, copyPending bool) bool {
	names := []string{"pending_action_id", "session_stream", "run_stream", "task_id", "task_stream", "tool_call_id", "tool_name", "idempotency_key", "execution_context", "approval_subject", "raw_args", "available_choices", "candidate_decisions", "current_requirement_id", "local_policy_persistence"}
	if copyPending {
		names = append(names, "created_at", "policy_diagnostics", "shell_stage_plan")
		if !integer(m["created_at"], 1) || !emptyArray(m["policy_diagnostics"]) {
			return false
		}
	} else {
		names = append(names, "kind", "presentation_phase")
		if str(m["kind"]) != "requested" || str(m["presentation_phase"]) != "automated_reviewing" {
			return false
		}
	}
	pending := str(m["pending_action_id"])
	task := str(m["task_id"])
	subject := object(m["approval_subject"])
	context := object(m["execution_context"])
	contextNames := []string{"canonical_workspace_root"}
	if copyPending {
		contextNames = append(contextNames, "side_chat_read_only_policy", "tool_workspace_root_override")
		if !boolean(context["side_chat_read_only_policy"]) || context["tool_workspace_root_override"] != nil || !reflect.DeepEqual(m["shell_stage_plan"], subject["stages"]) {
			return false
		}
	}
	return keys(m, names...) && identifier(pending) && identifier(task) && streamEquals(m["session_stream"], "session", owner) && streamEquals(m["run_stream"], "run", run) && streamEquals(m["task_stream"], "task", task) && stringFields(m, "tool_call_id", "tool_name", "idempotency_key", "raw_args") && keys(context, contextNames...) && stringFields(context, "canonical_workspace_root") && keys(subject, "kind", "canonical_workspace_root", "raw_command", "stages") && str(subject["kind"]) == "shell_command" && stringFields(subject, "canonical_workspace_root", "raw_command") && subject["canonical_workspace_root"] == context["canonical_workspace_root"] && oneStage(subject["stages"], pending, false) && requirement(m["current_requirement_id"], pending) && num(object(m["current_requirement_id"])["source_index"]) == 0 && boolean(m["local_policy_persistence"]) && choices(m["available_choices"], copyPending) && candidates(m["candidate_decisions"])
}

func approvalShape(r retained, p map[string]any) bool {
	if r.PayloadSchema != 3 || !keys(p, "kind", "run_id", "event") || str(p["kind"]) != "approval" || !identifier(p["run_id"]) {
		return false
	}
	e := object(p["event"])
	pending := str(e["pending_action_id"])
	if !identifier(pending) {
		return false
	}
	switch approvalEvent(str(e["kind"])) {
	case approvalRequested:
		return pendingShape(e, r.Stream.ID, str(p["run_id"]), false)
	case approvalReviewStarted:
		return reviewStartedShape(r, p, e, pending)
	case approvalReviewCompleted:
		return reviewCompletedShape(e)
	case approvalDecisionApplied:
		return decisionAppliedShape(r, e, pending)
	}
	return false
}

func projectNewFamily(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	run := str(p["run_id"])
	if !identifier(run) {
		return nil, errFormat
	}
	f["run_id"] = run
	if r.Type == payloadRuntimeSession {
		return projectNewRuntime(r, p, f)
	}
	return projectNewEffect(r, p, f)
}

func projectNewRuntime(r retained, p, f map[string]any) (map[string]any, error) {
	kind := str(p["kind"])
	e := object(p["event"])
	ek := str(e["kind"])
	f["kind"] = kind
	f["event_kind"] = ek
	switch {
	case kind == "approval":
		if !approvalShape(r, p) {
			return nil, errFormat
		}
		copyFields(f, e, "pending_action_id")
		switch approvalEvent(ek) {
		case approvalRequested:
			copyFields(f, e, "task_id", "tool_call_id")
		case approvalDecisionApplied:
			copyFields(f, object(e["decision_source"]), "review_id")
		case approvalReviewStarted, approvalReviewCompleted:
			copyFields(f, e, "review_id")
		}
	case kind == "run" && ek == "reasoning_committed":
		if r.PayloadSchema != 1 || !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") || !identifier(p["source_run_record_id"]) || !integer(p["source_run_record_sequence"], 1) || !keys(e, "kind", "message_id", "response_id", "text", "provider_item_id", "encrypted_content") || !identifier(e["message_id"]) || !stringFields(e, "response_id", "text", "provider_item_id", "encrypted_content") || e["text"] != "" || str(e["encrypted_content"]) == "" {
			return nil, errFormat
		}
		copyFields(f, p, "source_run_record_id", "source_run_record_sequence")
	case kind == "task" && ek == "output":
		if r.PayloadSchema != 1 || !keys(p, "kind", "run_id", "task_id", "event") || !identifier(p["task_id"]) || !keys(e, "kind", "task_id", "chunk", "final_result") || e["task_id"] != p["task_id"] || !stringFields(e, "chunk") || !boolean(e["final_result"]) {
			return nil, errFormat
		}
		copyFields(f, p, "task_id")
	default:
		return nil, errFormat
	}
	return f, nil
}

func projectNewEffect(r retained, p, f map[string]any) (map[string]any, error) {
	run := str(p["run_id"])
	if r.PayloadSchema != 1 || !keys(p, "kind", "run_id", "record") {
		return nil, errFormat
	}
	e := object(p["record"])
	k := str(e["kind"])
	f["kind"] = p["kind"]
	f["event_kind"] = k
	switch r.Type {
	case payloadRunModelConfigured, payloadRuntimeCommandIntakeReceived, payloadRuntimeCommandIntakeSessionNameReceived, payloadRuntimeCommandIntakeSettled, payloadRuntimeRetainedFact, payloadRuntimeSession, payloadRuntimeSessionCaptureSelection, payloadRuntimeSessionMetadata, payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted, payloadRuntimeSessionRouteFacts, payloadRuntimeSessionTask, payloadRuntimeSessionStartIntakeV3, payloadRuntimeUserIntentAccepted, payloadRuntimeUserIntentMaterialized, payloadSessionEnd, payloadSessionNameChanged, payloadSessionOpenedObserved, payloadSessionResumed, payloadSessionStarted:
		return nil, errFormat
	case payloadApprovalWaitStarted:
		if str(p["kind"]) != "approval_wait_effect" || k != "started" || !keys(e, "kind", "pending_action_id", "run_stream", "task_id", "task_stream", "tool_call_id", "tool_name") || !identifier(e["pending_action_id"]) || !identifier(e["task_id"]) || !streamEquals(e["run_stream"], "run", run) || !streamEquals(e["task_stream"], "task", str(e["task_id"])) || !stringFields(e, "tool_call_id", "tool_name") {
			return nil, errFormat
		}
		copyFields(f, e, "pending_action_id", "task_id", "tool_call_id")
	case payloadApprovalWaitTerminal:
		if str(p["kind"]) != "approval_wait_effect" || k != "terminal" || !keys(e, "kind", "pending_action_id", "outcome") || !identifier(e["pending_action_id"]) || !tagged(e["outcome"], "approved") {
			return nil, errFormat
		}
		copyFields(f, e, "pending_action_id")
	case payloadToolBatchStarted, payloadToolBatchTerminal:
		return projectToolEffect(r, p, e, f)

	default:
		return nil, errFormat
	}
	return f, nil
}

func newFieldShape(r retained) (int64, []string, bool) {
	f := r.Fields
	names := []string{"kind", "event_kind", "run_id"}
	schema := int64(1)
	kind, ek := str(f["kind"]), str(f["event_kind"])
	switch r.Type {
	case payloadRunModelConfigured, payloadRuntimeCommandIntakeReceived, payloadRuntimeCommandIntakeSessionNameReceived, payloadRuntimeCommandIntakeSettled, payloadRuntimeRetainedFact, payloadRuntimeSessionCaptureSelection, payloadRuntimeSessionMetadata, payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted, payloadRuntimeSessionRouteFacts, payloadRuntimeSessionTask, payloadRuntimeSessionStartIntakeV3, payloadRuntimeUserIntentAccepted, payloadRuntimeUserIntentMaterialized, payloadSessionEnd, payloadSessionNameChanged, payloadSessionOpenedObserved, payloadSessionResumed, payloadSessionStarted:
		return 0, nil, false
	case payloadRuntimeSession:
		switch {
		case kind == "run" && ek == "reasoning_committed":
			names = append(names, "source_run_record_id", "source_run_record_sequence")
			if !identifier(f["source_run_record_id"]) || !integer(f["source_run_record_sequence"], 1) {
				return 0, nil, false
			}
		case kind == "task" && ek == "output":
			names = append(names, "task_id")
		case kind == "approval":
			schema = 3
			names = append(names, "pending_action_id")
			if ek == "requested" {
				names = append(names, "task_id", "tool_call_id")
			} else if slices.Contains([]string{"automated_review_started", "automated_review_completed", "decision_applied"}, ek) {
				names = append(names, "review_id")
			} else {
				return 0, nil, false
			}
		default:
			return 0, nil, false
		}
	case payloadApprovalWaitStarted, payloadApprovalWaitTerminal:
		if kind != "approval_wait_effect" {
			return 0, nil, false
		}
		names = append(names, "pending_action_id")
		if r.Type == payloadApprovalWaitStarted && ek == "started" {
			names = append(names, "task_id", "tool_call_id")
		} else if r.Type != payloadApprovalWaitTerminal || ek != "terminal" {
			return 0, nil, false
		}
	case payloadToolBatchStarted, payloadToolBatchTerminal:
		if kind != "tool_batch_effect" || r.Type == payloadToolBatchStarted && ek != "started" || r.Type == payloadToolBatchTerminal && ek != "terminal" || !integer(f["model_call_index"], 0) {
			return 0, nil, false
		}
		names = append(names, "effect_id", "task_id", "tool_call_id", "model_call_index")
		if value, present := f["effect_join"]; present {
			if r.Type != payloadToolBatchStarted || value != committedFileReadJoin {
				return 0, nil, false
			}
			names = append(names, "effect_join")
		}
	default:
		return 0, nil, false
	}
	for _, k := range []string{"run_id", "task_id", "pending_action_id", "review_id", "effect_id"} {
		if v, present := f[k]; present && !identifier(v) {
			return 0, nil, false
		}
	}
	if v, present := f["tool_call_id"]; present && str(v) == "" {
		return 0, nil, false
	}
	return schema, names, true
}

func reviewStartedShape(r retained, p, e map[string]any, pending string) bool {
	action := object(e["exact_action"])
	sec := object(action["declared_security"])
	return keys(e, "kind", "pending_action_id", "review_id", "started_at_ms", "session_kind", "prior_context", "reviewer_rollout_id", "context_mode", "model", "action_digest", "exact_action", "action_projection_truncated") && identifier(e["review_id"]) && identifier(e["reviewer_rollout_id"]) && integer(e["started_at_ms"], 1) && str(e["session_kind"]) == "trunk" && boolean(e["prior_context"]) && str(e["context_mode"]) == "full" && modelShape(e["model"]) && digest(e["action_digest"]) && boolean(e["action_projection_truncated"]) && keys(action, "pending", "request_source", "declared_security", "trigger") && str(action["request_source"]) == "main_turn" && tagged(action["trigger"], "none") && keys(sec, "effects", "isolation") && stringArray(sec["effects"]) && stringFields(sec, "isolation") && pendingShape(object(action["pending"]), r.Stream.ID, str(p["run_id"]), true) && object(action["pending"])["pending_action_id"] == pending
}

func reviewCompletedShape(e map[string]any) bool {
	a := object(e["assessment"])
	u := object(e["usage"])
	if !keys(u, "input_tokens", "cached_input_tokens", "non_cached_input_tokens", "output_tokens", "reasoning_tokens", "total_tokens") {
		return false
	}
	for _, v := range u {
		if !integer(v, 0) {
			return false
		}
	}
	return keys(e, "kind", "pending_action_id", "review_id", "completed_at_ms", "session_kind", "prior_context", "model", "status", "assessment", "failure", "timed_out", "attempts", "duration_ms", "ttft_ms", "usage") && identifier(e["review_id"]) && integer(e["completed_at_ms"], 1) && str(e["session_kind"]) == "trunk" && boolean(e["prior_context"]) && modelShape(e["model"]) && str(e["status"]) == "approved" && keys(a, "outcome", "risk_level", "user_authorization", "rationale") && stringFields(a, "outcome", "risk_level", "user_authorization", "rationale") && e["failure"] == nil && e["ttft_ms"] == nil && e["timed_out"] == false && integer(e["attempts"], 1) && integer(e["duration_ms"], 0)
}

func decisionAppliedShape(r retained, e map[string]any, pending string) bool {
	d := object(e["decision_source"])
	return keys(e, "kind", "pending_action_id", "session_stream", "decision", "amendment", "policy_result", "decision_source", "stage_evidence") && streamEquals(e["session_stream"], "session", r.Stream.ID) && str(e["decision"]) == "approved" && e["amendment"] == nil && str(e["policy_result"]) == "allow" && keys(d, "kind", "review_id", "context_digest", "prompt_version", "params_version") && str(d["kind"]) == "llm_judge" && identifier(d["review_id"]) && digest(d["context_digest"]) && integer(d["prompt_version"], 1) && integer(d["params_version"], 1) && oneStage(e["stage_evidence"], pending, true)
}

func projectToolEffect(r retained, p, e, f map[string]any) (map[string]any, error) {
	k := str(e["kind"])
	names := []string{"kind", "effect_id", "task_stream", "task_id", "call_id", "model_call_index"}
	if r.Type == payloadToolBatchStarted {
		names = append(names, "tool_name", "parallel_profile")
		if k != "started" || !stringFields(e, "tool_name") || !toolParallelProfile(e["parallel_profile"]) {
			return nil, errFormat
		}
	} else {
		names = append(names, "outcome")
		if k != "terminal" || !toolEffectOutcome(e["outcome"]) {
			return nil, errFormat
		}
	}
	if str(p["kind"]) != "tool_batch_effect" || !keys(e, names...) || !identifier(e["effect_id"]) || !identifier(e["task_id"]) || !streamEquals(e["task_stream"], "task", str(e["task_id"])) || !stringFields(e, "call_id") || !integer(e["model_call_index"], 0) {
		return nil, errFormat
	}
	copyFields(f, e, "effect_id", "task_id", "model_call_index")
	f["tool_call_id"] = e["call_id"]
	return f, nil
}

// These private bodies are validated only; neither their tags nor leaf strings
// contribute retained tool results, error attribution or parallelism facts.
func toolParallelProfile(v any) bool {
	m := object(v)
	return tagged(v, "ineligible") || keys(m, "kind", "subject") && str(m["kind"]) == "file_read" && stringFields(m, "subject")
}

func toolEffectOutcome(v any) bool {
	m := object(v)
	switch str(m["kind"]) {
	case "failed":
		return keys(m, "kind", "reason") && stringFields(m, "reason")
	case "completed":
		t := object(m["task_completion"])
		return keys(m, "kind", "task_completion", "output_ref_count") && integer(m["output_ref_count"], 0) && (tagged(t, "complete") || keys(t, "kind", "terminal") && str(t["kind"]) == "terminal" && tagged(t["terminal"], "completed"))
	}
	return false
}
