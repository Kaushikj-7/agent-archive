package nativecodec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// codexSpawnedChildMeta is a synthetic Codex multi-agent child's session_meta.
const codexSpawnedChildMeta = `{"type":"session_meta","timestamp":"2026-10-15T16:20:06.354Z","payload":{"id":"01a1405d-1c3f-79f2-942c-72d471a5fe01","session_id":"01a10e11-4bd0-7041-b71c-58103545883a","parent_thread_id":"01a10e11-4bd0-7041-b71c-58103545883a","timestamp":"2026-10-15T16:20:06.354Z","cwd":"/tmp/synthetic","cli_version":"0.162.0-alpha.2","source":{"subagent":{"thread_spawn":{"parent_thread_id":"01a10e11-4bd0-7041-b71c-58103545883a","depth":1,"agent_path":"/root/fix_upload_retry","agent_nickname":"Ada","agent_role":null}}},"thread_source":"subagent","agent_nickname":"Ada","agent_path":"/root/fix_upload_retry","multi_agent_version":"v2"}}`

// codexTaskMessage is the agent_message a parent sends: the header (a JSON
// string) as plain text, and the task as encrypted content.
func codexTaskMessage(header string) string {
	return `{"type":"response_item","timestamp":"2026-10-15T16:20:07Z","payload":{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/fix_upload_retry","content":[{"type":"input_text","text":` + header + `},{"type":"encrypted_content","encrypted_content":"gAAAAABsynthetic"}]}}`
}

// codexChildReply is the thread's first visible record: the child's reply.
const codexChildReply = `{"type":"response_item","timestamp":"2026-10-15T16:20:08Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will read the upload code first."}]}}`

// codexSpawnedBundle filters lines and registers them as a native child, as
// discovery registers a spawned thread, or as a top-level session.
func codexSpawnedBundle(t *testing.T, child bool, lines ...string) SourceBundle {
	t.Helper()
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.NativeChild, reg.ParentSessionID = child, ""
	if child {
		reg.ParentSessionID = "archive-parent"
	}
	bundle, err := NewSourceBundle(reg, CodexAdapter{}, filtered, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func codexSpawnedChildMetadata(t *testing.T, header string, child bool) Metadata {
	t.Helper()
	return parserTestMetadata(t, codexSpawnedBundle(t, child, codexSpawnedChildMeta, codexTaskMessage(header), codexChildReply))
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
		{`"Message Type: NEW_TASK\nSender: /root\nPayload:\nTask name: /root/from_the_task_text\n"`, true},
	} {
		if m := codexSpawnedChildMetadata(t, tc.header, tc.child); m.Name != "" {
			t.Errorf("header %s child=%t named the session %q", tc.header, tc.child, m.Name)
		}
	}
}

// A forked child can carry its parent's own NEW_TASK in an inherited prefix;
// the thread is named from the last one, the task it received itself.
func TestCodexSpawnedChildIsNamedFromItsOwnLastTask(t *testing.T) {
	t.Parallel()
	inherited := codexTaskMessage(`"Message Type: NEW_TASK\nTask name: /root/parent_task\nSender: /root\nPayload:\n"`)
	own := codexTaskMessage(`"Message Type: NEW_TASK\nTask name: /root/parent_task/child_task\nSender: /root/parent_task\nPayload:\n"`)
	m := parserTestMetadata(t, codexSpawnedBundle(t, true, codexSpawnedChildMeta, inherited, codexChildReply, own, codexChildReply))
	if m.Name != "child task" {
		t.Fatalf("name = %q, want the child's own task", m.Name)
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

// Ordinary follow-ups and body text cannot replace the latest assignment.
func TestCodexSpawnedNameIgnoresFollowupAndPayloadTaskNames(t *testing.T) {
	t.Parallel()
	inherited := codexTaskMessage(`"Message Type: NEW_TASK\nTask name: /root/parent_task\nSender: /root\nPayload:\n"`)
	own := codexTaskMessage(`"Message Type: NEW_TASK\nTask name: /root/parent_task/child_task\nSender: /root/parent_task\nPayload:\nTask name: /root/body_name\n"`)
	followup := codexTaskMessage(`"Message Type: MESSAGE\nTask name: /root/incorrect_followup\nSender: /root\nPayload:\n"`)
	m := parserTestMetadata(t, codexSpawnedBundle(t, true, codexSpawnedChildMeta, inherited, own, followup, codexChildReply))
	if m.Name != "child task" {
		t.Fatalf("name = %q, want child task", m.Name)
	}
}

// Names retain Unicode, collapse whitespace, and use the common display bound.
func TestCodexSpawnedNameHasCommonUnicodeDisplayBound(t *testing.T) {
	t.Parallel()
	task := strings.Repeat("界", 140) + "_ignored-tail"
	header, err := json.Marshal("Message Type: NEW_TASK\nTask name: /root/" + task + "\nSender: /root\nPayload:\n")
	if err != nil {
		t.Fatal(err)
	}
	m := codexSpawnedChildMetadata(t, string(header), true)
	want := strings.Repeat("界", 128) + "…"
	if m.Name != want || !utf8.ValidString(m.Name) {
		t.Fatalf("name = %q, want bounded Unicode name", m.Name)
	}
	spaced, err := json.Marshal("Message Type: NEW_TASK\nTask name: /root/修复_上传--重试\t计划\nSender: /root\nPayload:\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := codexSpawnedChildMetadata(t, string(spaced), true).Name; got != "修复 上传 重试 计划" {
		t.Fatalf("normalized name = %q", got)
	}
}
