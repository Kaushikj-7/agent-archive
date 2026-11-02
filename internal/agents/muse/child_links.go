package muse

import "github.com/wangjohn/agent-archive/internal/agentapi"

// childLinkFieldNames are the retained members of a verified link from this
// session to a child session: the run that made the link and the child's ID.
var childLinkFieldNames = []string{"kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "child_session_id"}

// childLogChecker reports whether a child session's own log exists in the
// native store, beside the parent session that names it. A source snapshot
// supplies it; input without one cannot verify a named child log.
type childLogChecker interface {
	childLogPresent(path string) (bool, error)
}

// childLogPath is the only child log location a link may name: the child's
// own session file under the parent session's subagent directory.
func childLogPath(child string) string { return "subagent/" + child + "/session.jsonl" }

// projectChildLink qualifies a native link to a child session from within this
// session alone. The link must name this session as its parent and the run
// that carries it, a task stream of that task, and a child other than this
// session; a named child log must be the child's own path. Only the child ID
// is retained. Joins to the task and the child log are checked by
// nativeBodyJoins.childLink, and the run's place in the conversation by the
// parser.
func projectChildLink(r retained, p, event map[string]any) (map[string]any, error) {
	names := []string{"kind", "child_session_id", "generation_id", "parent_run_id", "parent_session_id", "reminder_agent_id", "task_id", "task_stream"}
	logPath, named := event["child_session_log_path"]
	if named {
		names = append(names, "child_session_log_path")
	}
	child := str(event["child_session_id"])
	if r.Causation != nil || !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") || !keys(event, names...) || !stringFields(event, "parent_run_id", "parent_session_id", "reminder_agent_id") || str(event["parent_session_id"]) != r.Stream.ID || str(event["parent_run_id"]) != str(p["run_id"]) || !identifier(event["task_id"]) || !streamEquals(event["task_stream"], "task", str(event["task_id"])) || !integer(event["generation_id"], 0) || str(event["reminder_agent_id"]) == "" || named && str(logPath) != childLogPath(child) {
		return nil, errFormat
	}
	f := map[string]any{"kind": p["kind"], "event_kind": event["kind"], "child_session_id": child}
	copyFields(f, p, "run_id", "source_run_record_id", "source_run_record_sequence")
	r.Fields = f
	if !childLinkFields(r) {
		return nil, errFormat
	}
	return f, nil
}

// projectLinkedChild qualifies a run's report of a child session: a
// memory-reminder link or a workflow child's lifecycle step.
func projectLinkedChild(r retained, p, event map[string]any) (map[string]any, error) {
	if runtimeEvent(str(event["kind"])) == eventWorkflowChildLifecycle {
		return projectWorkflowChild(r, p, event)
	}
	return projectChildLink(r, p, event)
}

// childLinkFields validates a retained child link.
func childLinkFields(r retained) bool {
	f := r.Fields
	child := str(f["child_session_id"])
	return r.PayloadSchema == 1 && r.Causation == nil && keys(f, childLinkFieldNames...) && str(f["kind"]) == "run" && str(f["event_kind"]) == string(eventChildSessionLinked) && conversationSourceShape(f) && identifier(child) && child != r.Stream.ID
}

// filterJoins starts a filter pass's private joins. A filter pass checks each
// named child log, through its input when the input can.
func filterJoins(content *skillContentJoin, in agentapi.NativeInput) nativeBodyJoins {
	childLogs, _ := in.File.(childLogChecker)
	return nativeBodyJoins{skillContent: content, childLogs: childLogs, checkChildLogs: true}
}

// observeChildLink accepts a retained child link: each child is linked once,
// from a run of this session's own conversation.
func observeChildLink(r retained, children, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool) error {
	child := str(r.Fields["child_session_id"])
	if !childLinkFields(r) || children[child] || observeConversationSource(r.Fields, str(r.Fields["run_id"]), joined, last, seen) != nil {
		return errFormat
	}
	children[child] = true
	return nil
}

// childLink joins a native child link to this session's own records: its task
// must be a task the session started in that run, each child is linked once,
// and a named child log must exist beside this session. When the input cannot
// check child logs, a link naming one is refused.
func (s *nativeBodyJoins) childLink(p map[string]any) error {
	event := object(p["event"])
	child := str(event["child_session_id"])
	if !s.taskIDs[taskKey{str(p["run_id"]), str(event["task_id"])}] || s.children[child] {
		return errFormat
	}
	s.children[child] = true
	if _, named := event["child_session_log_path"]; !named || !s.checkChildLogs {
		return nil
	}
	if s.childLogs == nil {
		return errFormat
	}
	present, err := s.childLogs.childLogPresent(childLogPath(child))
	if err != nil {
		return err
	}
	if !present {
		return errFormat
	}
	return nil
}
