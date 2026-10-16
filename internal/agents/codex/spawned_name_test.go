package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// The Codex parser keeps the name a spawned child takes from its NEW_TASK
// header: a child has no session index label to replace it.
func TestCodexParserNamesSpawnedChildFromItsTask(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"session_meta","timestamp":"2026-10-15T16:20:06Z","payload":{"id":"01a1405d-1c3f-79f2-942c-72d471a5fe01","session_id":"01a10e11-4bd0-7041-b71c-58103545883a","timestamp":"2026-10-15T16:20:06Z","cwd":"/tmp/synthetic","cli_version":"0.162.0-alpha.2","source":{"subagent":{"thread_spawn":{"parent_thread_id":"01a10e11-4bd0-7041-b71c-58103545883a","depth":1}}}}}`,
		`{"type":"response_item","timestamp":"2026-10-15T16:20:07Z","payload":{"type":"agent_message","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/fix_upload_retry\nSender: /root\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"gAAAAABsynthetic"}]}}`,
		`{"type":"response_item","timestamp":"2026-10-15T16:20:08Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will read the upload code first."}]}}`,
	}
	filtered, err := nativecodec.CodexAdapter{}.FilterJSONL(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 15, 16, 30, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic-child", NativeSessionID: "01a1405d-1c3f-79f2-942c-72d471a5fe01", ProjectID: "project", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/rollout.jsonl", SessionStartedAt: at, RegisteredAt: at, NativeChild: true, ParentSessionID: "synthetic-parent"}
	bundle, err := archive.NewSourceBundle(reg, nativecodec.CodexAdapter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := Parser{}.Parse(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Facts.Name != "fix upload retry" {
		t.Fatalf("name = %q, want the spawned task's name", analysis.Facts.Name)
	}
}
