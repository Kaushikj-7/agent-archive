package muse

import "strings"

// project is an exact type/schema/kind dispatch. Known optional content has an
// explicit empty projection; unknown discriminators never become optional.
func project(r retained, p map[string]any) (map[string]any, error) {
	if p == nil || !authorityShape(r, p) {
		return nil, errFormat
	}
	switch r.Type {
	case payloadRuntimeSessionPermissionFormatDeclared:
		return projectRuntimeSessionPermissionFormatDeclared(r, p)
	case payloadRuntimeSessionPermissionProfileCommitted:
		return projectRuntimeSessionPermissionProfileCommitted(r, p)
	case payloadRuntimeSessionMetadata:
		return projectRuntimeSessionMetadata(r, p)
	case payloadSessionOpenedObserved:
		return projectSessionOpenedObserved(r, p)
	case payloadRuntimeSessionCaptureSelection:
		return projectRuntimeSessionCaptureSelection(r, p)
	case payloadRuntimeCommandIntakeReceived:
		return projectRuntimeCommandIntakeReceived(r, p)
	case payloadRuntimeSessionStartIntakeV3:
		return projectRuntimeSessionStartIntakeV3(r, p)
	case payloadSessionStarted:
		return projectSessionStarted(r, p)
	case payloadSessionResumed:
		return projectSessionResumed(r, p)
	case payloadRuntimeSessionRouteFacts:
		return projectRuntimeSessionRouteFacts(r, p)
	case payloadRuntimeUserIntentAccepted:
		return projectRuntimeUserIntentAccepted(r, p)
	case payloadRuntimeUserIntentMaterialized:
		return projectRuntimeUserIntentMaterialized(r, p)
	case payloadSessionNameChanged:
		return projectSessionNameChanged(r, p)
	case payloadRunModelConfigured:
		return projectRunModelConfigured(r, p)
	case payloadSessionEnd:
		return projectSessionEnd(r, p)
	case payloadRuntimeSession:
		return projectRuntimeSession(r, p)
	case payloadRuntimeSessionTask:
		return projectRuntimeSessionTask(r, p)
	case payloadRuntimeRetainedFact:
		return projectRuntimeRetainedFact(r, p)
	case payloadRuntimeCommandIntakeSessionNameReceived:
		return projectRuntimeCommandIntakeSessionNameReceived(r, p)
	case payloadRuntimeCommandIntakeSettled:
		return projectRuntimeCommandIntakeSettled(r, p)
	default:
		return nil, errFormat
	}
}

func projectRuntimeSessionPermissionFormatDeclared(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	if r.PayloadSchema != 1 || !keys(p, "schema_version", "format") || num(p["schema_version"]) != 1 || str(p["format"]) != "profile_v1" {
		return nil, errFormat
	}
	copyFields(f, p, "schema_version", "format")

	return f, nil
}

func projectRuntimeSessionPermissionProfileCommitted(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	if r.PayloadSchema != 1 || !validProfile(p) {
		return nil, errFormat
	}
	copyFields(f, p, "schema_version", "permission_epoch", "cause")

	return f, nil
}

func projectRuntimeSessionMetadata(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "metadata" || !stringFields(rec, "workspace_root", "provider_id", "tool_surface_version") {
		return nil, errFormat
	}
	copyFields(f, rec, "workspace_root", "provider_id", "tool_surface_version")
	if model, present := rec["model_id"]; present {
		if _, ok := model.(string); !ok {
			return nil, errFormat
		}
		f["model_id"] = model
	}

	return f, nil
}

func projectSessionOpenedObserved(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "session_opened" || num(rec["schema_version"]) != 1 || !copyFields(f, rec, "session_id", "resume") {
		return nil, errFormat
	}
	if _, ok := rec["resume"].(bool); !ok {
		return nil, errFormat
	}

	return f, nil
}

func projectRuntimeSessionCaptureSelection(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	st := object(p["session_stream"])
	if r.PayloadSchema != 1 || num(p["schema_version"]) != 1 || !keys(st, "kind", "id") || str(st["kind"]) != "session" || str(st["id"]) != r.Stream.ID || (str(p["selection"]) != "armed" && str(p["selection"]) != "unarmed") || str(p["publication_id"]) == "" {
		return nil, errFormat
	}
	copyFields(f, p, "schema_version", "selection", "publication_id")

	return f, nil
}

func projectRuntimeCommandIntakeReceived(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if kind != "command_intake" || str(rec["kind"]) != "received" {
		return nil, errFormat
	}
	cmd := object(rec["command"])
	if r.PayloadSchema == 2 && str(cmd["kind"]) == "session_start" {
		st := object(rec["session_stream"])
		if str(st["kind"]) != "session" || str(st["id"]) != r.Stream.ID || str(cmd["background_tasks"]) != "keep" {
			return nil, errFormat
		}
		md := object(cmd["session_metadata"])
		if !stringFields(md, "workspace_root", "provider_id", "model_id", "tool_surface_version") {
			return nil, errFormat
		}
		copyFields(f, rec, "command_id")
		copyFields(f, md, "workspace_root", "provider_id", "model_id", "tool_surface_version")
		f["command_kind"] = "session_start"
	} else if r.PayloadSchema == 1 && str(cmd["kind"]) == "turn_queue_submit" {
		// Intake prompt/model context is deliberately not conversation authority.
		copyFields(f, rec, "command_id")
		f["command_kind"] = "turn_queue_submit"
	} else {
		return nil, errFormat
	}

	return f, nil
}

func projectRuntimeSessionStartIntakeV3(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	body := object(p["payload"])
	st := object(body["session_stream"])
	md := object(body["session_metadata"])
	if r.PayloadSchema != 3 || kind != "session_start_intake_v3" || num(body["schema_version"]) != 3 || str(st["kind"]) != "session" || str(st["id"]) != r.Stream.ID || str(body["background_tasks"]) != "keep" || !stringFields(md, "workspace_root", "provider_id", "model_id", "tool_surface_version") || (str(body["activation_source"]) != "startup" && str(body["activation_source"]) != "resume") || str(p["command_payload_fingerprint"]) == "" {
		return nil, errFormat
	}
	copyFields(f, p, "command_id")
	copyFields(f, body, "activation_source")
	copyFields(f, md, "workspace_root", "provider_id", "model_id", "tool_surface_version")

	return f, nil
}

func projectSessionStarted(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	if r.PayloadSchema != 1 || kind != "session_started" || str(p["background_tasks"]) != "keep" || p["previous_session_stream"] != nil {
		return nil, errFormat
	}
	if !copyFields(f, p, "command_id") {
		return nil, errFormat
	}

	return f, nil
}

func projectSessionResumed(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "session_resumed" || num(rec["schema_version"]) != 1 || !copyFields(f, rec, "session_id") {
		return nil, errFormat
	}

	return f, nil
}

func projectRuntimeSessionRouteFacts(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 2 || kind != "route_facts" || !stringFields(rec, "cwd") {
		return nil, errFormat
	}
	copyFields(f, rec, "cwd")

	return f, nil
}

func projectRuntimeUserIntentAccepted(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	if r.PayloadSchema != 1 || !stringFields(p, "intent_id", "source_session_id", "surface") || str(p["surface"]) != "main" || str(object(p["semantic_kind"])["kind"]) != "chat" {
		return nil, errFormat
	}
	copyFields(f, p, "intent_id", "source_session_id", "surface")
	f["semantic_kind"] = object(p["semantic_kind"])["kind"]

	return f, nil
}

func projectRuntimeUserIntentMaterialized(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	outcome := object(p["outcome"])
	if r.PayloadSchema != 1 || !stringFields(p, "intent_id", "envelope_record_id", "source_session_id") || num(p["envelope_sequence"]) < 1 || str(outcome["kind"]) != "top_level_turn_started" || !stringFields(outcome, "run_id", "run_started_session_record_id", "run_started_source_record_id", "cut_before") {
		return nil, errFormat
	}
	copyFields(f, p, "intent_id", "envelope_record_id", "envelope_sequence", "source_session_id")
	copyFields(f, outcome, "run_id", "run_started_session_record_id", "run_started_source_record_id")
	f["outcome_kind"] = "top_level_turn_started"

	return f, nil
}

func projectSessionNameChanged(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	if r.PayloadSchema != 1 || !stringFields(p, "session_id", "new_name", "source", "operation_id") {
		return nil, errFormat
	}
	if p["previous_name"] != nil {
		if _, ok := p["previous_name"].(string); !ok {
			return nil, errFormat
		}
	}
	copyFields(f, p, "session_id", "new_name", "previous_name", "source", "operation_id")

	return f, nil
}

func projectRunModelConfigured(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	st := object(rec["run_stream"])
	if r.PayloadSchema != 1 || kind != "run_model" || str(st["kind"]) != "run" || str(st["id"]) == "" || !stringFields(rec, "command_id", "provider_id", "model_id", "source") {
		return nil, errFormat
	}
	copyFields(f, rec, "command_id")
	f["run_id"] = st["id"]
	// Model switches need a separately qualified causal policy. Do not attribute
	// these labels to a visible turn or retain provider profile/display content.

	return f, nil
}

func projectSessionEnd(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "session_end" || num(rec["schema_version"]) != 1 || !stringFields(rec, "session_id") {
		return nil, errFormat
	}
	copyFields(f, rec, "session_id")

	return f, nil
}

func projectRuntimeSession(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := runtimeRole(str(p["kind"]))
	if kind != "" {
		f["kind"] = string(kind)
	}
	rec := object(p["record"])
	if r.PayloadSchema == 2 && kind == roleAgentTreeInitialized {
		if num(rec["schema_version"]) != 1 || num(rec["writer_protocol"]) != 1 || num(rec["execution_capacity"]) < 1 || !stringFields(rec, "root_agent_id", "root_session_id") {
			return nil, errFormat
		}
		copyFields(f, rec, "root_agent_id", "root_session_id")
		return f, nil
	}
	if r.PayloadSchema != 1 || (kind != roleRun && kind != roleTask) {
		return nil, errFormat
	}
	event := object(p["event"])
	ek := runtimeEvent(str(event["kind"]))
	f["event_kind"] = string(ek)
	if kind == roleRun && (ek == eventStarted || ek == eventAssistantMessageCommitted) {
		if !stringFields(p, "run_id", "source_run_record_id") || num(p["source_run_record_sequence"]) < 1 {
			return nil, errFormat
		}
		copyFields(f, p, "run_id", "source_run_record_id", "source_run_record_sequence")
		if ek == eventStarted {
			if !stringFields(event, "prompt") {
				return nil, errFormat
			}
			copyFields(f, event, "prompt")
		} else {
			if !stringFields(event, "message_id", "response_id", "text") {
				return nil, errFormat
			}
			copyFields(f, event, "message_id", "response_id", "text")
		}
	} else if !optionalRuntime(kind, ek) {
		return nil, errFormat
	}

	return f, nil
}

func projectRuntimeSessionTask(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	if r.PayloadSchema != 1 || kind != "" || !optionalRuntime(roleTask, runtimeEvent(str(object(p["event"])["kind"]))) {
		return nil, errFormat
	}
	f["event_kind"] = object(p["event"])["kind"]

	return f, nil
}

func projectRuntimeRetainedFact(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "retained_runtime_fact" || str(rec["payload_type"]) != "top_level_turn.run_origin" || num(rec["payload_schema_version"]) != 1 || !keys(object(rec["payload"])) {
		return nil, errFormat
	}

	return f, nil
}

func projectRuntimeCommandIntakeSessionNameReceived(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "command_intake" || str(rec["kind"]) != "received" {
		return nil, errFormat
	}

	return f, nil
}

func projectRuntimeCommandIntakeSettled(r retained, p map[string]any) (map[string]any, error) {
	f := map[string]any{}
	kind := str(p["kind"])
	if kind != "" {
		f["kind"] = kind
	}
	rec := object(p["record"])
	if r.PayloadSchema != 1 || kind != "command_intake" || str(rec["kind"]) != "settled" || str(object(rec["outcome"])["kind"]) != "accepted" || !stringFields(rec, "command_id") {
		return nil, errFormat
	}
	copyFields(f, rec, "command_id")
	return f, nil
}

func optionalRuntime(kind runtimeRole, event runtimeEvent) bool {
	if kind == roleTask {
		switch event {
		case eventProposed, eventAccepted, eventScheduled, eventSideEffectIntent, eventStarted, eventStatus, eventCompleted, eventFailed, eventCancelled:
			return true
		case eventAssistantMessageCommitted, eventAssistantToolCallsCommitted, eventContextBlockDiagnostic, eventContextBlockUpdated, eventGoalUsageAttribution, eventHookRunStarted, eventHookRunTerminal, eventModelCompleted, eventModelInputTraceRecorded, eventModelRequestConfigured, eventModelResponseCreated, eventProviderRequestOptionsConfigured, eventResourceUsageSampled, eventTaskStreamLinked, eventTerminal, eventToolResultBatchCommitted:
			return false
		}
		return false
	}
	if kind != roleRun {
		return false
	}
	switch event {
	case eventHookRunStarted, eventHookRunTerminal, eventContextBlockDiagnostic, eventModelRequestConfigured, eventContextBlockUpdated, eventProviderRequestOptionsConfigured, eventModelInputTraceRecorded, eventTaskStreamLinked, eventModelResponseCreated, eventGoalUsageAttribution, eventModelCompleted, eventResourceUsageSampled, eventTerminal, eventAssistantToolCallsCommitted, eventToolResultBatchCommitted, eventSideEffectIntent, eventCompleted, eventFailed:
		return true
	case eventAccepted, eventAssistantMessageCommitted, eventCancelled, eventProposed, eventScheduled, eventStarted, eventStatus:
		return false
	}
	return false
}

func digest(v any) bool {
	s := str(v)
	if !strings.HasPrefix(s, "sha256:") || len(s) != 71 {
		return false
	}
	for _, c := range s[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func stringArray(v any) bool {
	a, ok := v.([]any)
	if !ok {
		return false
	}
	for _, x := range a {
		if _, ok := x.(string); !ok {
			return false
		}
	}
	return true
}

func validProfile(p map[string]any) bool {
	if !keys(p, "schema_version", "permission_epoch", "source", "resolved_snapshot", "definition_sha256", "managed_ancestor_sha256", "managed_enforcement", "resulting_snapshot_sha256", "cause", "actor", "command", "pending_action_cancellations") || num(p["schema_version"]) != 1 || num(p["permission_epoch"]) != 1 || str(p["cause"]) != "new_session_default" || p["command"] != nil || !emptyArray(p["pending_action_cancellations"]) || !digest(p["definition_sha256"]) || !digest(p["resulting_snapshot_sha256"]) || !emptyArray(p["managed_ancestor_sha256"]) {
		return false
	}
	s := object(p["source"])
	a := object(p["actor"])
	if !keys(s, "kind", "id", "display_name") || str(s["kind"]) != "built_in" || !stringFields(s, "id", "display_name") || !keys(a, "kind", "id") || str(a["kind"]) != "runtime" || a["id"] != nil {
		return false
	}
	return validSnapshot(p["resolved_snapshot"]) && validManaged(p["managed_enforcement"])
}

func validSnapshot(value any) bool {
	snapshot := object(value)
	fs := object(snapshot["filesystem"])
	network := object(snapshot["local_command_network"])
	if !keys(snapshot, "schema_version", "filesystem", "local_command_network", "approval", "reviewer") || num(snapshot["schema_version"]) != 1 || !stringFields(snapshot, "approval", "reviewer") || !keys(fs, "mode", "minimal_runtime_set_version", "workspace_roots", "rules", "protected_metadata") || !stringFields(fs, "mode") || num(fs["minimal_runtime_set_version"]) != 1 || !emptyArray(fs["workspace_roots"]) {
		return false
	}
	if _, ok := fs["protected_metadata"].(bool); !ok {
		return false
	}
	rules, ok := fs["rules"].([]any)
	if !ok {
		return false
	}
	for _, v := range rules {
		rule := object(v)
		subject := object(rule["subject"])
		if !keys(rule, "subject", "access") || !stringFields(rule, "access") || !keys(subject, "kind", "value") || !stringFields(subject, "kind", "value") {
			return false
		}
	}
	if !keys(network, "mode", "targets", "unix_sockets") || !stringFields(network, "mode") || !emptyArray(network["targets"]) || !emptyArray(network["unix_sockets"]) {
		return false
	}
	return true
}

func validManaged(value any) bool {
	managed := object(value)
	ceil := object(managed["component_ceilings"])
	if !keys(managed, "schema_version", "constraint_epoch", "constraint_sha256", "component_ceilings", "filesystem_rules", "network_targets", "unix_sockets", "managed_targets_only", "platform", "backend_id") || num(managed["schema_version"]) != 1 || num(managed["constraint_epoch"]) < 0 || !digest(managed["constraint_sha256"]) || !stringFields(managed, "platform", "backend_id") || !emptyArray(managed["filesystem_rules"]) || !emptyArray(managed["network_targets"]) || !emptyArray(managed["unix_sockets"]) || !keys(ceil, "filesystem_modes", "network_modes", "approval", "reviewer") {
		return false
	}
	if _, ok := managed["managed_targets_only"].(bool); !ok {
		return false
	}
	for _, k := range []string{"filesystem_modes", "network_modes", "approval", "reviewer"} {
		if !stringArray(ceil[k]) {
			return false
		}
	}
	return true
}

func copyFields(dst, src map[string]any, names ...string) bool {
	for _, key := range names {
		value, ok := src[key]
		if !ok {
			return false
		}
		dst[key] = value
	}
	return true
}
