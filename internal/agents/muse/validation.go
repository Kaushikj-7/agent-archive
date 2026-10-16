package muse

import (
	"encoding/json"
	"path/filepath"
	"slices"
)

// validateRows also runs after native loss. Only the minimal safe authority
// projection is needed to revalidate owner, start, workspace and causal joins.
func validateRootRows(rows []retained) error {
	if len(rows) < 4 || !initialAuthority(rows) {
		return errFormat
	}
	state := rootState{owner: rows[0].Stream.ID, start: rows[0].Timestamp, ids: map[string]bool{}}
	for i, r := range rows {
		if !state.envelope(r, i) || !validFields(r) {
			return errFormat
		}
		if err := state.authority(rows, i); err != nil {
			return err
		}
	}
	if state.original != 1 || state.workspace == "" || !startupPrefix(rows) {
		return errFormat
	}
	return nil
}

type rootState struct {
	owner     string
	start     int64
	ids       map[string]bool
	workspace string
	original  int
}

func initialAuthority(rows []retained) bool {
	a, b := rows[0], rows[1]
	return a.Type == payloadRuntimeSessionPermissionFormatDeclared && b.Type == payloadRuntimeSessionPermissionProfileCommitted && a.Position.Line == 1 && b.Position.Line == 1 && a.Position.Child != nil && *a.Position.Child == 0 && b.Position.Child != nil && *b.Position.Child == 1 && b.Timestamp == a.Timestamp && a.Causation == nil && b.Causation == nil
}

func (s *rootState) envelope(r retained, i int) bool {
	if r.Version != 1 || r.Schema != 1 || r.RecordType != "event" || r.Durability != "durable" || r.PayloadSchema < 1 || r.Stream.Kind != streamSession || r.Stream.ID != s.owner || r.Sequence != int64(i+1) || r.Timestamp < 1 || r.Timestamp < s.start || r.Timestamp > exactMax || s.ids[r.ID] {
		return false
	}
	s.ids[r.ID] = true
	if _, ok := uuid(r.ID); !ok {
		return false
	}
	if _, ok := uuid(r.Stream.ID); !ok {
		return false
	}
	if r.Causation != nil {
		if _, ok := uuid(*r.Causation); !ok {
			return false
		}
	}
	return i < 2 || r.Position.Child == nil && r.Position.Line == int64(i)
}

func (s *rootState) authority(rows []retained, i int) error {
	r := rows[i]
	f := r.Fields
	switch r.Type {
	case payloadApprovalWaitStarted, payloadApprovalWaitTerminal, payloadToolBatchStarted, payloadToolBatchTerminal:
		return nil
	case payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted:
		if i > 1 {
			return errFormat
		}
	case payloadRuntimeSessionMetadata:
		w := str(f["workspace_root"])
		if w == "" || !filepath.IsAbs(w) || s.workspace != "" && s.workspace != w {
			return errFormat
		}
		s.workspace = w
	case payloadSessionOpenedObserved:
		if str(f["session_id"]) != s.owner {
			return errFormat
		}
		if !f["resume"].(bool) {
			s.original++
			if s.original > 1 {
				return errFormat
			}
		}
	case payloadRuntimeSession:
		if str(f["kind"]) == "agent_tree_initialized" && (str(f["root_agent_id"]) != s.owner || str(f["root_session_id"]) != s.owner) {
			return errFormat
		}
	case payloadSessionResumed, payloadSessionEnd, payloadSessionNameChanged:
		if str(f["session_id"]) != s.owner {
			return errFormat
		}
	case payloadRunModelConfigured:
		if r.Causation == nil || *r.Causation != str(f["command_id"]) {
			return errFormat
		}
	case payloadRuntimeSessionRouteFacts:
		if str(f["cwd"]) != s.workspace {
			return errFormat
		}
	case payloadRuntimeUserIntentAccepted, payloadRuntimeUserIntentMaterialized:
		if str(f["source_session_id"]) != s.owner {
			return errFormat
		}
	case payloadRuntimeCommandIntakeReceived, payloadRuntimeCommandIntakeSessionNameReceived, payloadRuntimeCommandIntakeSettled, payloadRuntimeRetainedFact, payloadRuntimeSessionCaptureSelection, payloadRuntimeSessionTask, payloadRuntimeSessionStartIntakeV3, payloadSessionStarted:
		return startupRecord(rows, i)
	default:
		return startupRecord(rows, i)
	}
	return nil
}

func startupRecord(rows []retained, i int) error {
	r := rows[i]
	switch r.Type {
	case payloadApprovalWaitStarted, payloadApprovalWaitTerminal, payloadToolBatchStarted, payloadToolBatchTerminal:
		return nil
	case payloadRuntimeCommandIntakeReceived:
		if str(r.Fields["command_kind"]) == "session_start" {
			return startupCommand(rows, i)
		}
	case payloadRuntimeSessionStartIntakeV3:
		if i < 1 || rows[i-1].Type != payloadRuntimeCommandIntakeReceived || str(rows[i-1].Fields["command_kind"]) != "session_start" {
			return errFormat
		}
	case payloadSessionStarted:
		if i < 2 || rows[i-1].Type != payloadRuntimeSessionStartIntakeV3 {
			return errFormat
		}
	case payloadRunModelConfigured, payloadRuntimeCommandIntakeSessionNameReceived, payloadRuntimeCommandIntakeSettled, payloadRuntimeRetainedFact, payloadRuntimeSession, payloadRuntimeSessionCaptureSelection, payloadRuntimeSessionMetadata, payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted, payloadRuntimeSessionRouteFacts, payloadRuntimeSessionTask, payloadRuntimeUserIntentAccepted, payloadRuntimeUserIntentMaterialized, payloadSessionEnd, payloadSessionNameChanged, payloadSessionOpenedObserved, payloadSessionResumed:
		// These records do not open an MSP startup block.

	}
	return nil
}

func startupCommand(rows []retained, i int) error {
	if i+3 >= len(rows) {
		return errFormat
	}
	r := rows[i]
	f := r.Fields
	a, b, c := rows[i+1], rows[i+2], rows[i+3]
	command := str(f["command_id"])
	if command == "" || r.Causation == nil || *r.Causation != command || a.Type != payloadRuntimeSessionStartIntakeV3 || b.Type != payloadSessionStarted || c.Type != payloadRuntimeSessionMetadata || str(a.Fields["command_id"]) != command || str(b.Fields["command_id"]) != command || a.Causation == nil || *a.Causation != command || b.Causation == nil || *b.Causation != command {
		return errFormat
	}
	for _, k := range []string{"workspace_root", "provider_id", "model_id", "tool_surface_version"} {
		if f[k] != a.Fields[k] || f[k] != c.Fields[k] {
			return errFormat
		}
	}
	if i == 3 {
		if str(a.Fields["activation_source"]) != "startup" {
			return errFormat
		}
	} else {
		if str(a.Fields["activation_source"]) != "resume" || i+4 >= len(rows) || rows[i+4].Type != payloadSessionResumed {
			return errFormat
		}
	}
	return nil
}

func startupPrefix(rows []retained) bool {
	if rows[2].Type == payloadRuntimeSessionMetadata {
		if rows[3].Type == payloadRuntimeSessionMetadata {
			return twoMetadataPrefix(rows)
		}
		return rows[3].Type == payloadSessionOpenedObserved && rows[3].Fields["resume"] == false && rows[2].Causation == nil && rows[3].Causation == nil
	}
	return len(rows) >= 8 && rows[2].Type == payloadRuntimeSessionCaptureSelection && rows[3].Type == payloadRuntimeCommandIntakeReceived && str(rows[3].Fields["command_kind"]) == "session_start" && rows[6].Type == payloadRuntimeSessionMetadata && rows[7].Type == payloadSessionOpenedObserved && rows[7].Fields["resume"] == false && rows[6].Causation == nil && rows[7].Causation == nil
}

func fieldShape(r retained) (int64, []string, bool) {
	if newFamily(r) {
		return newFieldShape(r)
	}
	switch r.Type {
	case payloadApprovalWaitStarted, payloadApprovalWaitTerminal, payloadToolBatchStarted, payloadToolBatchTerminal:
		return 0, nil, false
	case payloadRuntimeSessionPermissionFormatDeclared:
		return fieldsRuntimeSessionPermissionFormatDeclared(r)
	case payloadRuntimeSessionPermissionProfileCommitted:
		return fieldsRuntimeSessionPermissionProfileCommitted(r)
	case payloadRuntimeSessionMetadata:
		return fieldsRuntimeSessionMetadata(r)
	case payloadSessionOpenedObserved:
		return fieldsSessionOpenedObserved(r)
	case payloadRuntimeSessionCaptureSelection:
		return fieldsRuntimeSessionCaptureSelection(r)
	case payloadRuntimeCommandIntakeReceived:
		return fieldsRuntimeCommandIntakeReceived(r)
	case payloadRuntimeSessionStartIntakeV3:
		return fieldsRuntimeSessionStartIntakeV3(r)
	case payloadSessionStarted:
		return fieldsSessionStarted(r)
	case payloadSessionResumed:
		return fieldsSessionResumed(r)
	case payloadRuntimeSessionRouteFacts:
		return fieldsRuntimeSessionRouteFacts(r)
	case payloadRuntimeUserIntentAccepted:
		return fieldsRuntimeUserIntentAccepted(r)
	case payloadRuntimeUserIntentMaterialized:
		return fieldsRuntimeUserIntentMaterialized(r)
	case payloadSessionNameChanged:
		return fieldsSessionNameChanged(r)
	case payloadRunModelConfigured:
		return fieldsRunModelConfigured(r)
	case payloadSessionEnd:
		return fieldsSessionEnd(r)
	case payloadRuntimeSession:
		return fieldsRuntimeSession(r)
	case payloadRuntimeSessionTask:
		return fieldsRuntimeSessionTask(r)
	case payloadRuntimeRetainedFact:
		return fieldsRuntimeRetainedFact(r)
	case payloadRuntimeCommandIntakeSessionNameReceived:
		return fieldsRuntimeCommandIntakeSessionNameReceived(r)
	case payloadRuntimeCommandIntakeSettled:
		return fieldsRuntimeCommandIntakeSettled(r)
	}
	return 0, nil, false
}

func validFields(r retained) bool {
	f := r.Fields
	schema, names, valid := fieldShape(r)
	if !valid {
		return false
	}

	if r.PayloadSchema != schema || !keys(f, names...) {
		return false
	}
	for k, v := range f {
		switch fieldName(k) {
		case fieldSchemaVersion, fieldPermissionEpoch, fieldEnvelopeSequence, fieldSourceRunRecordSequence, fieldModelCallIndex:
			if _, ok := v.(json.Number); !ok {
				return false
			}
		case fieldObservedInput, fieldObservedOutput:
			if !integer(v, 0) {
				return false
			}
		case fieldResume:
		case fieldPreviousName:
		case fieldPrompt, fieldText, fieldNewName, fieldKind, fieldEventKind, fieldSourceRunRecordID, fieldObservedModel:
			fallthrough
		default:
			if _, ok := v.(string); !ok {
				return false
			}
		}
	}
	return true
}

func fieldsRuntimeSessionPermissionFormatDeclared(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"schema_version", "format"}
	if num(f["schema_version"]) != 1 || str(f["format"]) != "profile_v1" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSessionPermissionProfileCommitted(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"schema_version", string(fieldPermissionEpoch), "cause"}
	if num(f["schema_version"]) != 1 || num(f["permission_epoch"]) != 1 || str(f["cause"]) != "new_session_default" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSessionMetadata(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "workspace_root", "provider_id", "tool_surface_version"}
	if _, present := f["model_id"]; present {
		names = append(names, "model_id")
	}
	if str(f["kind"]) != "metadata" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsSessionOpenedObserved(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "session_id", "resume"}
	if str(f["kind"]) != "session_opened" {
		return 0, nil, false
	}
	if _, ok := f["resume"].(bool); !ok {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSessionCaptureSelection(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"schema_version", "selection", "publication_id"}
	if num(f["schema_version"]) != 1 || !slices.Contains([]string{"armed", "unarmed"}, str(f["selection"])) {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeCommandIntakeReceived(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "command_id", "command_kind"}
	if str(f["kind"]) != "command_intake" {
		return 0, nil, false
	}
	if str(f["command_kind"]) == "session_start" {
		schema = 2
		names = append(names, "workspace_root", "provider_id", "model_id", "tool_surface_version")
	} else if str(f["command_kind"]) != "turn_queue_submit" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSessionStartIntakeV3(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(3)
	names = []string{"kind", "command_id", "activation_source", "workspace_root", "provider_id", "model_id", "tool_surface_version"}
	if str(f["kind"]) != "session_start_intake_v3" || !slices.Contains([]string{"startup", "resume"}, str(f["activation_source"])) {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsSessionStarted(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "command_id"}
	if str(f["kind"]) != "session_started" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsSessionResumed(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "session_id"}
	if str(f["kind"]) != "session_resumed" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSessionRouteFacts(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(2)
	names = []string{"kind", "cwd"}
	if str(f["kind"]) != "route_facts" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeUserIntentAccepted(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"intent_id", "source_session_id", "surface", "semantic_kind"}
	if str(f["surface"]) != "main" || str(f["semantic_kind"]) != "chat" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeUserIntentMaterialized(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"intent_id", "envelope_record_id", string(fieldEnvelopeSequence), "source_session_id", "run_id", "run_started_session_record_id", "run_started_source_record_id", "outcome_kind"}
	if str(f["outcome_kind"]) != "top_level_turn_started" || num(f["envelope_sequence"]) < 1 {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsSessionNameChanged(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"session_id", string(fieldNewName), string(fieldPreviousName), "source", "operation_id"}
	if f["previous_name"] != nil {
		if _, ok := f["previous_name"].(string); !ok {
			return 0, nil, false
		}
	}

	return schema, names, true
}

func fieldsRunModelConfigured(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "command_id", "run_id"}
	if str(f["kind"]) != "run_model" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsSessionEnd(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "session_id"}
	if str(f["kind"]) != "session_end" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSession(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	kind := runtimeRole(str(f["kind"]))
	if kind == roleAgentTreeInitialized {
		schema = 2
		names = []string{"kind", "root_agent_id", "root_session_id"}
		return schema, names, true
	}
	ek := runtimeEvent(str(f["event_kind"]))
	names = []string{"kind", "event_kind"}
	if kind == roleRun && ek == eventModelCompleted && completionEnriched(f) {
		return schema, []string{"kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "model", "input_tokens", "output_tokens"}, completionFields(r)
	}
	if kind == roleRun && (ek == eventStarted || ek == eventAssistantMessageCommitted) {
		names = append(names, "run_id", "source_run_record_id", "source_run_record_sequence")
		if num(f["source_run_record_sequence"]) < 1 {
			return 0, nil, false
		}
		if ek == eventStarted {
			names = append(names, "prompt")
		} else {
			names = append(names, "message_id", "response_id", "text")
		}
	} else if !optionalRuntime(kind, ek) {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeSessionTask(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"event_kind"}
	if !optionalRuntime(roleTask, runtimeEvent(str(f["event_kind"]))) {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeRetainedFact(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind"}
	if str(f["kind"]) != "retained_runtime_fact" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeCommandIntakeSessionNameReceived(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind"}
	if str(f["kind"]) != "command_intake" {
		return 0, nil, false
	}

	return schema, names, true
}

func fieldsRuntimeCommandIntakeSettled(r retained) (int64, []string, bool) {
	f := r.Fields
	var names []string
	schema := int64(1)
	names = []string{"kind", "command_id"}
	if str(f["kind"]) != "command_intake" {
		return 0, nil, false
	}
	return schema, names, true
}

// twoMetadataPrefix is the one observed model-resolution startup lane. The
// permission frame still supplies birth; neither metadata row invents a time.
func twoMetadataPrefix(rows []retained) bool {
	if len(rows) < 5 {
		return false
	}
	a, b, o := rows[2], rows[3], rows[4]
	if a.Type != payloadRuntimeSessionMetadata || b.Type != payloadRuntimeSessionMetadata || o.Type != payloadSessionOpenedObserved || a.Causation != nil || b.Causation != nil || o.Causation != nil || o.Fields["resume"] != false || a.Timestamp <= rows[0].Timestamp || b.Timestamp < a.Timestamp || o.Timestamp < b.Timestamp {
		return false
	}
	if !keys(a.Fields, "kind", "workspace_root", "provider_id", "tool_surface_version") || !keys(b.Fields, "kind", "workspace_root", "provider_id", "model_id", "tool_surface_version") {
		return false
	}
	for _, k := range []string{"kind", "workspace_root", "provider_id", "tool_surface_version"} {
		if a.Fields[k] != b.Fields[k] {
			return false
		}
	}
	_, ok := b.Fields["model_id"].(string)
	return ok
}
