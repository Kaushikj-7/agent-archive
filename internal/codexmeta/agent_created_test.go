package codexmeta

import "testing"

// The Codex app opens an agent_created_thread for a task one thread's agent
// hands off: a top-level thread with its own identity, captured as a user
// thread is. The same source on anything carrying child markers is not.
func TestAgentCreatedThreadIsTopLevel(t *testing.T) {
	t.Parallel()
	path := "rollout-" + thread + ".jsonl"
	m := metadata(t, `,"session_id":"`+thread+`","thread_source":"agent_created_thread","history_mode":"paginated"`)
	if got := m.CaptureOutcome(path); got != NativeFormat {
		t.Fatalf("agent-created thread: %s", got)
	}
	if facts, outcome := m.Identity(path); outcome != "" || facts.Child || facts.ThreadID != thread {
		t.Fatalf("identity %+v %s", facts, outcome)
	}
	for _, fields := range []string{
		`,"thread_source":"agent_created_thread","parent_thread_id":"` + parent + `"`,
		`,"thread_source":"agent_created_thread","agent_role":"worker"`,
		`,"thread_source":"agent_created_thread","session_id":"` + root + `"`,
		`,"thread_source":"agent_created_thread","source":{"subagent":"review"}`,
	} {
		if got := metadata(t, fields).FormatOutcome(); got != UnsupportedExecution {
			t.Fatalf("child-shaped agent-created thread %s: %s", fields, got)
		}
	}
	for _, source := range []string{"guardian_review", "memory_consolidation", "future_task"} {
		if got := metadata(t, `,"thread_source":"`+source+`"`).FormatOutcome(); got != UnsupportedExecution {
			t.Fatalf("%s: %s", source, got)
		}
	}
}
