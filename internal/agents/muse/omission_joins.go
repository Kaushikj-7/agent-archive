package muse

import "reflect"

type pendingKey struct {
	run     string
	pending string
}

type reviewKey struct {
	pending pendingKey
	review  string
}

type effectKey struct {
	run    string
	effect string
}

type taskKey struct {
	run  string
	task string
}

type pendingIdentity struct {
	task string
	call string
}

type effectIdentity struct {
	task  string
	call  string
	index int64
}

type taskCallKey struct {
	run  string
	task string
	call string
}

type familyState struct {
	runs         map[string]bool
	pending      map[pendingKey]pendingIdentity
	taskPending  map[taskCallKey]pendingKey
	reviews      map[reviewKey]bool
	completed    map[reviewKey]bool
	reviewOwners map[runIdentity]pendingKey
	decisions    map[pendingKey]bool
	waits        map[pendingKey]bool
	terminals    map[pendingKey]bool
	effects      map[effectKey]effectIdentity
	ended        map[effectKey]bool
	tasks        map[taskKey]bool
}

func validateNewFamilies(rows []retained) error {
	s := familyState{runs: map[string]bool{}, pending: map[pendingKey]pendingIdentity{}, taskPending: map[taskCallKey]pendingKey{}, reviews: map[reviewKey]bool{}, completed: map[reviewKey]bool{}, reviewOwners: map[runIdentity]pendingKey{}, decisions: map[pendingKey]bool{}, waits: map[pendingKey]bool{}, terminals: map[pendingKey]bool{}, effects: map[effectKey]effectIdentity{}, ended: map[effectKey]bool{}, tasks: map[taskKey]bool{}}
	for _, r := range rows {
		run := str(r.Fields["run_id"])
		if r.Type == payloadRuntimeUserIntentMaterialized {
			s.runs[run] = true
		}
		if !newFamily(r) {
			continue
		}
		if !s.runs[run] || s.observe(r) != nil {
			return errFormat
		}
	}
	return nil
}

func (s *familyState) observe(r retained) error {
	f := r.Fields
	run := str(f["run_id"])
	ek := str(f["event_kind"])
	pk := pendingKey{run, str(f["pending_action_id"])}
	switch omittedKind(str(f["kind"])) {
	case omittedApproval:
		return s.approval(pk, ek, f)
	case omittedWait:
		return s.wait(pk, ek, f)
	case omittedTool:
		return s.effect(run, ek, f)
	case omittedRun:
		return nil
	case omittedTask:
		if !s.tasks[taskKey{run, str(f["task_id"])}] {
			return errFormat
		}
	}
	return nil
}

func (s *familyState) approval(pk pendingKey, ek string, f map[string]any) error {
	rk := reviewKey{pk, str(f["review_id"])}
	_, present := s.pending[pk]
	switch approvalEvent(ek) {
	case approvalRequested:
		if present {
			return errFormat
		}
		identity := pendingIdentity{str(f["task_id"]), str(f["tool_call_id"])}
		key := taskCallKey{pk.run, identity.task, identity.call}
		if _, duplicate := s.taskPending[key]; duplicate {
			return errFormat
		}
		s.pending[pk] = identity
		s.taskPending[key] = pk
	case approvalReviewStarted:
		ownerKey := runIdentity{pk.run, rk.review}
		if _, duplicate := s.reviewOwners[ownerKey]; duplicate {
			return errFormat
		}
		s.reviewOwners[ownerKey] = pk
		if !present || s.reviews[rk] {
			return errFormat
		}
		s.reviews[rk] = true
	case approvalReviewCompleted:
		if !s.reviews[rk] || s.completed[rk] {
			return errFormat
		}
		s.completed[rk] = true
	case approvalDecisionApplied:
		if !s.completed[rk] || s.decisions[pk] {
			return errFormat
		}
		s.decisions[pk] = true
	}
	return nil
}

func (s *familyState) wait(pk pendingKey, ek string, f map[string]any) error {
	identity, present := s.pending[pk]
	if !present {
		return errFormat
	}
	if ek == "started" {
		if s.waits[pk] || identity.task != str(f["task_id"]) || identity.call != str(f["tool_call_id"]) {
			return errFormat
		}
		s.waits[pk] = true
	} else {
		if !s.waits[pk] || !s.decisions[pk] || s.terminals[pk] {
			return errFormat
		}
		s.terminals[pk] = true
	}
	return nil
}

func (s *familyState) effect(run, ek string, f map[string]any) error {
	key := effectKey{run, str(f["effect_id"])}
	identity := effectIdentity{str(f["task_id"]), str(f["tool_call_id"]), num(f["model_call_index"])}
	if ek == "started" {
		if _, exists := s.effects[key]; exists {
			return errFormat
		}
		pending, matched := s.taskPending[taskCallKey{run, identity.task, identity.call}]
		if !matched || !s.terminals[pending] {
			return errFormat
		}
		s.effects[key] = identity
		s.tasks[taskKey{run, identity.task}] = true
	} else {
		if prior, ok := s.effects[key]; !ok || prior != identity || s.ended[key] {
			return errFormat
		}
		s.ended[key] = true
	}
	return nil
}

// nativeBodyJoins checks redundant private copies before dropping their bodies.
// Values live only in the charged decode reservation, never in retained output.
type nativeBodyJoins struct {
	requested     map[pendingKey]map[string]any
	reviewStarted map[reviewKey]int64
	taskIDs       map[taskKey]bool
	sources       map[runIdentity]bool
	lastSource    map[string]int64
	firstMetadata map[string]any
	sourceInvalid bool
	hasNew        bool
}

func (s *nativeBodyJoins) observe(r retained, p map[string]any) error {
	if s.requested == nil {
		s.requested = map[pendingKey]map[string]any{}
		s.reviewStarted = map[reviewKey]int64{}
		s.taskIDs = map[taskKey]bool{}
		s.sources = map[runIdentity]bool{}
		s.lastSource = map[string]int64{}
	}
	if r.Sequence == 3 && r.Type == payloadRuntimeSessionMetadata {
		s.firstMetadata = p
	}
	if r.Sequence == 4 && r.Type == payloadRuntimeSessionMetadata && s.firstMetadata != nil && !nativeMetadataPair(s.firstMetadata, p) {
		return errFormat
	}
	run := str(p["run_id"])
	e := object(p["event"])
	kind, ek := str(p["kind"]), str(e["kind"])
	s.observeSources(r, p, run, kind, ek)
	s.hasNew = s.hasNew || nativeNewFamily(r, p)
	if s.hasNew && s.sourceInvalid {
		return errFormat
	}
	if !nativeNewFamily(r, p) {
		return nil
	}
	if r.Type == payloadRuntimeSession && kind == "task" && !s.taskIDs[taskKey{run, str(p["task_id"])}] {
		return errFormat
	}
	if kind == "approval" {
		return s.approval(run, ek, e)
	}
	if r.Type == payloadApprovalWaitStarted || r.Type == payloadToolBatchStarted {
		rec := object(p["record"])
		call := str(rec["tool_call_id"])
		if r.Type == payloadToolBatchStarted {
			call = str(rec["call_id"])
		}
		match := false
		for key, request := range s.requested {
			if key.run == run && request["task_id"] == rec["task_id"] && request["tool_call_id"] == call && request["tool_name"] == rec["tool_name"] {
				if r.Type == payloadApprovalWaitStarted && key.pending != str(rec["pending_action_id"]) {
					continue
				}
				match = true
			}
		}
		if !match {
			return errFormat
		}
	}
	return nil
}

func nativeMetadataPair(first, second map[string]any) bool {
	a, b := object(first["record"]), object(second["record"])
	if !keys(first, "kind", "record") || !keys(second, "kind", "record") || str(first["kind"]) != "metadata" || str(second["kind"]) != "metadata" || !keys(a, "workspace_root", "provider_id", "web_search_mode", "build", "tool_surface_version") || !keys(b, "workspace_root", "provider_id", "model_id", "web_search_mode", "build", "tool_surface_version") || !stringFields(a, "workspace_root", "provider_id", "web_search_mode", "tool_surface_version") || !stringFields(b, "workspace_root", "provider_id", "model_id", "web_search_mode", "tool_surface_version") {
		return false
	}
	for _, m := range []map[string]any{a, b} {
		if !keys(object(m["build"]), "sha", "semver") || !stringFields(object(m["build"]), "sha", "semver") {
			return false
		}
	}
	for _, k := range []string{"workspace_root", "provider_id", "web_search_mode", "build", "tool_surface_version"} {
		if !reflect.DeepEqual(a[k], b[k]) {
			return false
		}
	}
	return true
}

func (s *nativeBodyJoins) approval(run, ek string, e map[string]any) error {
	pk := pendingKey{run, str(e["pending_action_id"])}
	requested := s.requested[pk]
	switch approvalEvent(ek) {
	case approvalRequested:
		if requested != nil {
			return errFormat
		}
		s.requested[pk] = e
	case approvalReviewStarted:
		if requested == nil {
			return errFormat
		}
		if !equalPendingCopy(requested, object(object(e["exact_action"])["pending"])) {
			return errFormat
		}
		s.reviewStarted[reviewKey{pk, str(e["review_id"])}] = num(e["started_at_ms"])
	case approvalReviewCompleted:
		start, ok := s.reviewStarted[reviewKey{pk, str(e["review_id"])}]
		if !ok || num(e["completed_at_ms"]) < start {
			return errFormat
		}
	case approvalDecisionApplied:
		if requested == nil {
			return errFormat
		}
		a := object(requested["approval_subject"])["stages"].([]any)
		b := e["stage_evidence"].([]any)
		for i := range a {
			for _, k := range []string{"argv", "requirement_id", "source_position", "total_stages"} {
				if !reflect.DeepEqual(object(a[i])[k], object(b[i])[k]) {
					return errFormat
				}
			}
		}
	}
	return nil
}

func (s *nativeBodyJoins) observeSources(r retained, p map[string]any, run, kind, ek string) {
	if r.Type == payloadRuntimeSession && kind == "run" {
		key := runIdentity{run, str(p["source_run_record_id"])}
		seq := num(p["source_run_record_sequence"])
		if !identifier(p["run_id"]) || !identifier(p["source_run_record_id"]) || !integer(p["source_run_record_sequence"], 1) {
			s.sourceInvalid = true
		} else {
			if s.sources[key] || seq <= s.lastSource[run] {
				s.sourceInvalid = true
			}
			s.sources[key] = true
			s.lastSource[run] = seq
		}
	}
	if r.Type == payloadRuntimeSession && kind == "task" && ek != "output" {
		if identifier(p["run_id"]) && identifier(p["task_id"]) {
			s.taskIDs[taskKey{run, str(p["task_id"])}] = true
		}
	}
}

func equalPendingCopy(requested, copyPending map[string]any) bool {
	for _, k := range []string{"pending_action_id", "task_id", "session_stream", "run_stream", "task_stream", "tool_call_id", "tool_name", "raw_args", "idempotency_key", "approval_subject", "local_policy_persistence", "current_requirement_id", "candidate_decisions"} {
		if !reflect.DeepEqual(requested[k], copyPending[k]) {
			return false
		}
	}
	if object(requested["execution_context"])["canonical_workspace_root"] != object(copyPending["execution_context"])["canonical_workspace_root"] {
		return false
	}
	a := requested["available_choices"].([]any)
	b := copyPending["available_choices"].([]any)
	for i := range a {
		for _, k := range []string{"choice_id", "label", "decision"} {
			if !reflect.DeepEqual(object(a[i])[k], object(b[i])[k]) {
				return false
			}
		}
	}
	return true
}
