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

// authorityShape refuses new authority-bearing members even when the known
// fields would otherwise project to a plausible root. Explicitly named private
// context/diagnostic fields are bounded by the JSON decoder and never retained.
func authorityShape(r retained, p map[string]any) bool {
	switch r.Type {
	case "runtime.session.metadata":
		return shapeRuntimeSessionMetadata(r, p)
	case "session.opened.observed":
		return shapeSessionOpenedObserved(r, p)
	case "runtime.session.capture_selection":
		return shapeRuntimeSessionCaptureSelection(r, p)
	case "runtime.command_intake.received":
		return shapeRuntimeCommandIntakeReceived(r, p)
	case "runtime.session_start.intake_v3":
		return shapeRuntimeSessionStartIntakeV3(r, p)
	case "session.started":
		return shapeSessionStarted(r, p)
	case "session.resumed":
		return shapeSessionResumed(r, p)
	case "runtime.session.route_facts":
		return shapeRuntimeSessionRouteFacts(r, p)
	case "runtime.user_intent.accepted":
		return shapeRuntimeUserIntentAccepted(r, p)
	case "runtime.user_intent.materialized":
		return shapeRuntimeUserIntentMaterialized(r, p)
	case "session.name.changed":
		return shapeSessionNameChanged(r, p)
	case "run.model.configured":
		return shapeRunModelConfigured(r, p)
	case "session.end":
		return shapeSessionEnd(r, p)
	case "runtime.session":
		return shapeRuntimeSession(r, p)
	case "runtime.retained_fact":
		return keys(p, "kind", "run_id", "record") && keys(recObject(p), "payload_type", "payload_schema_version", "payload")
	case "runtime.session.task":
		return keys(p, "task_id", "event")
	case "runtime.command_intake.session_name.received":
		return keys(p, "kind", "record") && keys(recObject(p), "kind", "command_id", "session_stream", "command")
	case "runtime.command_intake.settled":
		return keys(p, "kind", "record") && keys(recObject(p), "kind", "command_id", "outcome")
	}
	return true
}
func shapeRuntimeSessionMetadata(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "workspace_root", "provider_id", "model_id", "tool_surface_version", "web_search_mode", "build") && (rec["build"] == nil || keys(object(rec["build"]), "sha", "semver") && stringFields(object(rec["build"]), "sha", "semver"))

}

func shapeSessionOpenedObserved(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "schema_version", "session_id", "resume", "security_mode", "workspace_free_bytes", "data_dir_free_bytes", "credential_backend", "keychain_fallback_reason", "client_info_name", "client_info_version")

}

func shapeRuntimeSessionCaptureSelection(r retained, p map[string]any) bool {
	return keys(p, "schema_version", "session_stream", "publication_id", "selection")

}

func shapeRuntimeCommandIntakeReceived(r retained, p map[string]any) bool {
	rec := object(p["record"])
	if !keys(p, "kind", "record") {
		return false
	}
	if r.PayloadSchema == 2 {
		return keys(rec, "kind", "command_id", "session_stream", "command") && keys(object(rec["session_stream"]), "kind", "id") && keys(object(rec["command"]), "kind", "background_tasks", "session_metadata") && only(object(object(rec["command"])["session_metadata"]), "workspace_root", "provider_id", "model_id", "tool_surface_version")
	}
	return only(rec, "kind", "command_id", "session_stream", "command", "acceptance_identity")

}

func shapeRuntimeSessionStartIntakeV3(r retained, p map[string]any) bool {
	b := object(p["payload"])
	return keys(p, "kind", "command_id", "payload", "command_payload_fingerprint") && keys(b, "schema_version", "session_stream", "background_tasks", "session_metadata", "activation_source") && keys(object(b["session_stream"]), "kind", "id") && only(object(b["session_metadata"]), "workspace_root", "provider_id", "model_id", "tool_surface_version")

}

func shapeSessionStarted(r retained, p map[string]any) bool {
	return keys(p, "kind", "command_id", "previous_session_stream", "background_tasks")

}

func shapeSessionResumed(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "schema_version", "session_id", "prior_turn_count", "resumed_from_sequence", "resumed_from_record_id")

}

func shapeRuntimeSessionRouteFacts(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "cwd", "pid", "terminal_title_identity")

}

func shapeRuntimeUserIntentAccepted(r retained, p map[string]any) bool {
	return only(p, "intent_id", "source_session_id", "surface", "semantic_kind", "refill_blocks", "model_messages", "bindings", "delivery_policy", "wake_policy") && keys(object(p["semantic_kind"]), "kind")

}

func shapeRuntimeUserIntentMaterialized(r retained, p map[string]any) bool {
	return keys(p, "intent_id", "envelope_record_id", "envelope_sequence", "source_session_id", "outcome") && keys(object(p["outcome"]), "kind", "run_id", "run_started_session_record_id", "run_started_source_record_id", "cut_before")

}

func shapeSessionNameChanged(r retained, p map[string]any) bool {
	return only(p, "session_id", "new_name", "previous_name", "source", "operation_id", "authority_id")

}

func shapeRunModelConfigured(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "command_id", "run_stream", "provider_id", "model_id", "source", "display_label", "profile_id") && keys(object(rec["run_stream"]), "kind", "id")

}

func shapeSessionEnd(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return keys(p, "kind", "record") && only(rec, "schema_version", "session_id", "exit_reason", "resource_usage", "shutdown_steps", "uptime_ms")

}

func shapeRuntimeSession(r retained, p map[string]any) bool {
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
		switch str(event["kind"]) {
		case "started":
			return keys(event, "kind", "prompt")
		case "assistant_message_committed":
			return keys(event, "kind", "message_id", "response_id", "text")
		}
		return true
	}
	if kind == "task" {
		return keys(p, "kind", "run_id", "task_id", "event")
	}
	return false
}

func recObject(p map[string]any) map[string]any { return object(p["record"]) }
