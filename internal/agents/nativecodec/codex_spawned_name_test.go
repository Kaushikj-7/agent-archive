package nativecodec

import (
	"strings"
	"testing"
)

// codexSpawnedChildLines is a synthetic Codex multi-agent child rollout: the
// parent's task arrives as an agent_message whose only plain text is the
// NEW_TASK header, and the thread's first visible record is the child's reply.
func codexSpawnedChildLines(header string) []string {
	return []string{
		`{"type":"session_meta","timestamp":"2026-10-15T16:20:06.354Z","payload":{"id":"01a1405d-1c3f-79f2-942c-72d471a5fe01","session_id":"01a10e11-4bd0-7041-b71c-58103545883a","parent_thread_id":"01a10e11-4bd0-7041-b71c-58103545883a","timestamp":"2026-10-15T16:20:06.354Z","cwd":"/tmp/synthetic","cli_version":"0.162.0-alpha.2","source":{"subagent":{"thread_spawn":{"parent_thread_id":"01a10e11-4bd0-7041-b71c-58103545883a","depth":1,"agent_path":"/root/fix_upload_retry","agent_nickname":"Ada","agent_role":null}}},"thread_source":"subagent","agent_nickname":"Ada","agent_path":"/root/fix_upload_retry","multi_agent_version":"v2"}}`,
		`{"type":"response_item","timestamp":"2026-10-15T16:20:07Z","payload":{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/fix_upload_retry","content":[{"type":"input_text","text":` + header + `},{"type":"encrypted_content","encrypted_content":"gAAAAABsynthetic"}]}}`,
		`{"type":"response_item","timestamp":"2026-10-15T16:20:08Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will read the upload code first."}]}}`,
	}
}

func codexSpawnedChildMetadata(t *testing.T, header string, child bool) Metadata {
	t.Helper()
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(strings.Join(codexSpawnedChildLines(header), "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "codex", CodexAdapter{}, filtered)
	bundle.NativeChild = child
	return parserTestMetadata(t, bundle)
}

// A spawned Codex thread has no prompt of its own, so it is named from the
// task name in the NEW_TASK header its parent sent it.
func TestCodexSpawnedChildIsNamedFromItsTask(t *testing.T) {
	t.Parallel()
	header := `"Message Type: NEW_TASK\nTask name: /root/review/fix_upload-retry\nSender: /root\nPayload:\n"`
	m := codexSpawnedChildMetadata(t, header, true)
	if m.Name != "fix upload retry" || m.Title != "" {
		t.Fatalf("name = %q, title = %q, want the task name and no prompt title", m.Name, m.Title)
	}
}

// Only a child is named this way, and only from a NEW_TASK header: a later
// message to the thread, or one a top-level thread holds, names nothing.
func TestCodexTaskNameNeedsChildAndNewTaskHeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		header string
		child  bool
	}{
		{`"Message Type: NEW_TASK\nTask name: /root/fix_upload_retry\nSender: /root\nPayload:\n"`, false},
		{`"Message Type: MESSAGE\nTask name: /root/fix_upload_retry\nSender: /root\nPayload:\n"`, true},
		{`"Task name: /root/fix_upload_retry"`, true},
		{`"Message Type: NEW_TASK\nTask name: /root/\nSender: /root\n"`, true},
	} {
		if m := codexSpawnedChildMetadata(t, tc.header, tc.child); m.Name != "" {
			t.Errorf("header %s child=%t named the session %q", tc.header, tc.child, m.Name)
		}
	}
}

// FuzzCodexSpawnedTaskName reads arbitrary header text without panicking and
// names a thread only with one line of the task name's last path segment.
func FuzzCodexSpawnedTaskName(f *testing.F) {
	for _, seed := range []string{
		"Message Type: NEW_TASK\nTask name: /root/fix_upload_retry\nSender: /root\nPayload:\n",
		"Message Type: NEW_TASK\nTask name: \nSender: /root\n",
		"Message Type: NEW_TASK\nTask name: /\n",
		"Message Type: NEW_TASK\nTask name: plain-name\r\n",
		"Message Type: MESSAGE\nTask name: /root/x\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		record := map[string]any{"type": "response_item", "payload": map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
		name := codexSpawnedTaskName(record)
		if name == "" {
			return
		}
		if !strings.HasPrefix(text, codexNewTaskHeader+"\n") || strings.ContainsAny(name, "/_-") || name != strings.Join(strings.Fields(name), " ") {
			t.Fatalf("header %q named %q", text, name)
		}
	})
}
