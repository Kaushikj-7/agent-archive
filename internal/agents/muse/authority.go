package muse

import "slices"

func only(m map[string]any, names ...string) bool {
	if m == nil {
		return false
	}
	for k := range m {
		if !slices.Contains(names, k) {
			return false
		}
	}
	return true
}

// known accepts a record whose listed members are optional and whose other
// members the projection never reads: it reports them as dropped instead of
// refusing the record.
func known(m map[string]any, dropped *bool, names ...string) bool {
	if m == nil {
		return false
	}
	if !only(m, names...) {
		*dropped = true
	}
	return true
}

// authorityShape refuses new authority-bearing members even when the known
// fields would otherwise project to a plausible root. Members outside a list of
// optional descriptive members are dropped and reported through dropped.
// Explicitly named private context/diagnostic fields are bounded by the JSON
// decoder and never retained.
func authorityShape(r retained, p map[string]any, dropped *bool) bool {
	switch r.Type {
	case payloadApprovalWaitStarted, payloadApprovalWaitTerminal, payloadToolBatchStarted, payloadToolBatchTerminal:
		return false
	case payloadRuntimeSessionMetadata:
		return shapeRuntimeSessionMetadata(p, dropped)
	case payloadSessionOpenedObserved:
		return shapeSessionOpenedObserved(p, dropped)
	case payloadRuntimeSessionCaptureSelection:
		return shapeRuntimeSessionCaptureSelection(p)
	case payloadRuntimeCommandIntakeReceived:
		return shapeRuntimeCommandIntakeReceived(r, p, dropped)
	case payloadRuntimeSessionStartIntakeV3:
		return shapeRuntimeSessionStartIntakeV3(p, dropped)
	case payloadSessionStarted:
		return shapeSessionStarted(p)
	case payloadSessionResumed:
		return shapeSessionResumed(p, dropped)
	case payloadSessionLoginCompleted:
		return shapeSessionLoginCompleted(p)
	case payloadRuntimeSessionRouteFacts:
		return shapeRuntimeSessionRouteFacts(p)
	case payloadRuntimeUserIntentAccepted:
		return shapeRuntimeUserIntentAccepted(p, dropped)
	case payloadRuntimeUserIntentMaterialized:
		return shapeRuntimeUserIntentMaterialized(p)
	case payloadSessionNameChanged:
		return shapeSessionNameChanged(p)
	case payloadRunModelConfigured:
		return shapeRunModelConfigured(p)
	case payloadSessionEnd:
		return shapeSessionEnd(p)
	case payloadRuntimeSession:
		return shapeRuntimeSession(p)
	case payloadRuntimeRetainedFact:
		return keys(p, "kind", "run_id", "record") && keys(recObject(p), "payload_type", "payload_schema_version", "payload")
	case payloadRuntimeSessionTask:
		return keys(p, "task_id", "event")
	case payloadRuntimeCommandIntakeSessionNameReceived:
		return keys(p, "kind", "record") && keys(recObject(p), "kind", "command_id", "session_stream", "command")
	case payloadRuntimeCommandIntakeSettled:
		return keys(p, "kind", "record") && keys(recObject(p), "kind", "command_id", "outcome")

	case payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted:
		// These known variants keep the existing non-special-case behavior.
	}
	return true
}

func shapeRuntimeSessionMetadata(p map[string]any, dropped *bool) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && known(rec, dropped, "workspace_root", "provider_id", "model_id", "tool_surface_version", "web_search_mode", "build") && (rec["build"] == nil || keys(object(rec["build"]), "sha", "semver") && stringFields(object(rec["build"]), "sha", "semver"))

}

func shapeSessionOpenedObserved(p map[string]any, dropped *bool) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && known(rec, dropped, "schema_version", "session_id", "resume", "security_mode", "workspace_free_bytes", "data_dir_free_bytes", "credential_backend", "keychain_fallback_reason", "client_info_name", "client_info_version")

}

func shapeRuntimeSessionCaptureSelection(p map[string]any) bool {
	return keys(p, "schema_version", "session_stream", "publication_id", "selection")

}

func shapeRuntimeCommandIntakeReceived(r retained, p map[string]any, dropped *bool) bool {
	rec := object(p["record"])
	if !keys(p, "kind", "record") {
		return false
	}
	if r.PayloadSchema == 2 {
		return keys(rec, "kind", "command_id", "session_stream", "command") && keys(object(rec["session_stream"]), "kind", "id") && keys(object(rec["command"]), "kind", "background_tasks", "session_metadata") && known(object(object(rec["command"])["session_metadata"]), dropped, "workspace_root", "provider_id", "model_id", "tool_surface_version")
	}
	return only(rec, "kind", "command_id", "session_stream", "command", "acceptance_identity")

}

func shapeRuntimeSessionStartIntakeV3(p map[string]any, dropped *bool) bool {
	b := object(p["payload"])
	return keys(p, "kind", "command_id", "payload", "command_payload_fingerprint") && keys(b, "schema_version", "session_stream", "background_tasks", "session_metadata", "activation_source") && keys(object(b["session_stream"]), "kind", "id") && known(object(b["session_metadata"]), dropped, "workspace_root", "provider_id", "model_id", "tool_surface_version")

}

func shapeSessionStarted(p map[string]any) bool {
	return keys(p, "kind", "command_id", "previous_session_stream", "background_tasks")

}

func shapeSessionResumed(p map[string]any, dropped *bool) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && known(rec, dropped, "schema_version", "session_id", "prior_turn_count", "resumed_from_sequence", "resumed_from_record_id")

}

// A login completion names this session; its account facts (method,
// subscription, trigger, duration) are validated and dropped. Its shape is
// closed, like every record that bears identity.
func shapeSessionLoginCompleted(p map[string]any) bool {
	return keys(p, "kind", "record") && keys(object(p["record"]), "duration_ms", "method", "schema_version", "session_id", "subscribed", "trigger")
}

// Route facts carry no authority beyond cwd, the only retained member. Other
// local facts (pid, terminal identity, runtime socket paths) are dropped,
// including members newer Muse builds add.
func shapeRuntimeSessionRouteFacts(p map[string]any) bool {
	return keys(p, "kind", "record") && object(p["record"]) != nil

}

func shapeRuntimeUserIntentAccepted(p map[string]any, dropped *bool) bool {
	return known(p, dropped, "intent_id", "source_session_id", "surface", "semantic_kind", "refill_blocks", "model_messages", "bindings", "delivery_policy", "wake_policy") && keys(object(p["semantic_kind"]), "kind")

}

func shapeRuntimeUserIntentMaterialized(p map[string]any) bool {
	return keys(p, "intent_id", "envelope_record_id", "envelope_sequence", "source_session_id", "outcome") && keys(object(p["outcome"]), "kind", "run_id", "run_started_session_record_id", "run_started_source_record_id", "cut_before")

}

func shapeSessionNameChanged(p map[string]any) bool {
	return only(p, "session_id", "new_name", "previous_name", "source", "operation_id", "authority_id")

}

func shapeRunModelConfigured(p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "command_id", "run_stream", "provider_id", "model_id", "source", "display_label", "profile_id") && keys(object(rec["run_stream"]), "kind", "id")

}

func shapeSessionEnd(p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "schema_version", "session_id", "exit_reason", "resource_usage", "shutdown_steps", "uptime_ms")

}

func shapeRuntimeSession(p map[string]any) bool {
	rec := object(p["record"])
	kind := str(p["kind"])
	if kind == "agent_tree_initialized" {
		return keys(p, "kind", "record") && keys(rec, "schema_version", "writer_protocol", "root_agent_id", "root_session_id", "execution_capacity")
	}
	if kind == "run" {
		if !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") {
			return false
		}
		event := object(p["event"])
		switch runtimeEvent(str(event["kind"])) {
		case eventReasoningCommitted:
			return false
		case eventStarted:
			return keys(event, "kind", "prompt")
		case eventAssistantMessageCommitted:
			return keys(event, "kind", "message_id", "response_id", "text") || keys(event, "kind", "message_id", "response_id", "text", "provider_item_id") && stringFields(event, "provider_item_id")

		case eventSkillReadObserved, eventChildSessionLinked, eventWorkflowChildLifecycle, eventAccepted, eventAssistantToolCallsCommitted, eventCancelled, eventCompleted, eventContextBlockDiagnostic, eventContextBlockUpdated, eventFailed, eventGoalUsageAttribution, eventHookRunStarted, eventHookRunTerminal, eventModelCompleted, eventModelInputTraceRecorded, eventModelRequestConfigured, eventModelResponseCreated, eventProposed, eventProviderRequestOptionsConfigured, eventResourceUsageSampled, eventScheduled, eventSideEffectIntent, eventStatus, eventTaskStreamLinked, eventTerminal, eventToolResultBatchCommitted:
			// These known variants keep the existing non-special-case behavior.
		}
		return true
	}
	if kind == "task" {
		return keys(p, "kind", "run_id", "task_id", "event")
	}
	return false
}

func recObject(p map[string]any) map[string]any { return object(p["record"]) }
