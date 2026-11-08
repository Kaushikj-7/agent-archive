package muse

import "strings"

// originKind names who started a run, when a retained runtime fact says.
type originKind string

// originInboxDelivery is a run an inbox delivery started, not a user turn.
const originInboxDelivery originKind = "inbox_delivery"

// inboxOrigin validates a retained inbox-delivery run origin.
func inboxOrigin(r retained) bool {
	f := r.Fields
	return r.PayloadSchema == 1 && keys(f, "kind", "origin_kind", "run_id") && str(f["kind"]) == "retained_runtime_fact" && str(f["origin_kind"]) == string(originInboxDelivery) && identifier(f["run_id"])
}

// workflowFactRecord classifies an omitted workflow-engine retained fact. Like
// every omission row it carries nothing of the record it replaces.
var workflowFactRecord = omittedRecord{"event", "durable", "", 0, "", "workflow_fact_v1"}

// workflowFact reports a retained runtime fact of the workflow engine. Its
// workflow owner IDs, binding, script and argument hashes and prompts describe
// the workflow engine's own state, not this session's identity, permissions
// or proofs, so it is left out instead of refusing the session.
func workflowFact(r retained, p map[string]any) bool {
	return r.Type == payloadRuntimeRetainedFact && r.Position.Line >= 2 && str(p["kind"]) == "retained_runtime_fact" && strings.HasPrefix(str(object(p["record"])["payload_type"]), "workflow.")
}

// workflowRow replaces a workflow fact with a content-free omission row.
func workflowRow(r retained) logicalRow {
	row := unsupportedRow(r)
	row.omission.Omitted = workflowFactRecord
	return row
}

// inboxStart records an inbox-delivery origin, and drops the prompt of the
// run it started: a delivery is not a user turn, and its text is not kept as
// one. The parser requires the origin before such a start.
func (s *nativeBodyJoins) inboxStart(r *retained) {
	f := r.Fields
	if r.Type == payloadRuntimeRetainedFact && str(f["origin_kind"]) == string(originInboxDelivery) {
		if s.inboxRuns == nil {
			s.inboxRuns = map[string]bool{}
		}
		s.inboxRuns[str(f["run_id"])] = true
		return
	}
	if r.Type == payloadRuntimeSession && str(f["kind"]) == string(roleRun) && str(f["event_kind"]) == string(eventStarted) && s.inboxRuns[str(f["run_id"])] {
		delete(f, "prompt")
	}
}
