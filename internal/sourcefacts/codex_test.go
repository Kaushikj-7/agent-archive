package sourcefacts

import (
	"context"
	"encoding/json"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testID = "00000000-0000-0000-0000-000000000001"

func testRecords(t *testing.T, m map[string]any, task map[string]any) string {
	t.Helper()
	if m == nil {
		m = map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": "cli", "originator": "synthetic", "cli_version": "test"}
	}
	if task == nil {
		task = map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-01T12:00:00Z"}
	}
	first, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": m})
	second, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": task})
	return string(first) + "\n" + string(second) + "\n"
}

func TestCodexNativeShapeAdmitsUntestedCompatibleProducer(t *testing.T) {
	t.Parallel()
	h := ReadCodexHeader(strings.NewReader(testRecords(t, nil, nil)), "rollout-2026-10-01T12-00-00-"+testID+".jsonl")
	if h.Outcome != "native_format" {
		t.Fatal(h.Outcome)
	}
	if !SupportedCodexProducer(h.Meta) || h.Profile != CodexLegacyJSONL || CodexProducerEvidence(h.Meta) != CodexCompatibleUntested {
		t.Fatal("compatible untested format rejected or mislabeled")
	}
}

func TestCodexFirstImportedTurnNeverBecomesNativeOnResume(t *testing.T) {
	t.Parallel()
	data := testRecords(t, nil, map[string]any{"type": "task_started", "turn_id": "external-import-turn-1", "root_turn_id": nil}) + testRecords(t, nil, nil)
	h := ReadCodexHeader(strings.NewReader(data), "rollout-"+testID+".jsonl")
	if h.Outcome != "inherited_history" {
		t.Fatal(h.Outcome)
	}
}

func TestCodexInheritedAndUnknownExecutionFailClosed(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"forked_from_id", "forked_from_ordinal_exclusive", "parent_thread_id", "history_base", "subagent_history_start_ordinal", "source", "thread_source", "agent_path", "agent_role", "agent_type", "agent_nickname"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			m := map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": "cli", "originator": "synthetic", "cli_version": "test"}
			m[field] = map[string]any{"content": "never copied to diagnostics"}
			h := ReadCodexHeader(strings.NewReader(testRecords(t, m, nil)), "rollout-"+testID+".jsonl")
			if h.Outcome == "native_format" {
				t.Fatal("unsupported shape admitted")
			}
		})
	}
}

func TestCodexNativeUserExecutionTagsRemainUsable(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"cli", "exec", "vscode"} {
		for _, thread := range []any{nil, "user"} {
			m := map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": source, "originator": "synthetic", "cli_version": "test", "thread_source": thread}
			h := ReadCodexHeader(strings.NewReader(testRecords(t, m, nil)), "rollout-"+testID+".jsonl")
			if h.Outcome != "native_format" {
				t.Fatalf("source=%s thread=%v: %s", source, thread, h.Outcome)
			}
		}
	}
}

func TestCodexHeaderLimitsAndIncompleteRecords(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"{", strings.Repeat("x", HeaderBytes), testRecords(t, nil, nil)[:20], strings.Repeat("{}\n", HeaderRecords+1)} {
		h := ReadCodexHeader(strings.NewReader(input), "rollout-"+testID+".jsonl")
		if h.Outcome == "native_format" || h.Bytes > HeaderBytes {
			t.Fatalf("unbounded or accepted: %#v", h)
		}
	}
}

func TestRegularSourceRejectsEscapesAndFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "rollout-"+testID+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegular(root, link); err == nil {
		_ = f.Close()
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { _, err := OpenRegular(root, fifo); done <- err != nil }()
	select {
	case rejected := <-done:
		if !rejected {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked")
	}
	if _, err := OpenRegular(root, target); err == nil {
		t.Fatal("escape accepted")
	}
}

func TestMalformedFirstTaskCannotBeSkippedForANativeResume(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"root_turn_id", "turn_id"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			task := map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-01T12:00:00Z"}
			task[field] = map[string]any{"wrong": "JSON type"}
			data := testRecords(t, nil, task) + testRecords(t, nil, nil)
			h := ReadCodexHeader(strings.NewReader(data), "rollout-"+testID+".jsonl")
			if h.Outcome != "inherited_history" {
				t.Fatalf("malformed first task skipped: %s", h.Outcome)
			}
		})
	}
}

func TestCodexMalformedPreStartAndInconsistentTaskTimesFailClosed(t *testing.T) {
	t.Parallel()
	valid := testRecords(t, nil, nil)
	meta := strings.SplitN(valid, "\n", 2)[0] + "\n"
	for _, event := range []string{
		`{"type":"event_msg","payload":{"type":[],"turn_id":"ignored"}}`,
		`{"type":"event_msg","payload":null}`,
		`{"type":{},"payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + testID + `","root_turn_id":"` + testID + `","started_at":"2026-10-01T11:59:00Z"}}`,
	} {
		h := ReadCodexHeader(strings.NewReader(meta+event+"\n"+strings.SplitN(valid, "\n", 2)[1]), "rollout-"+testID+".jsonl")
		if h.Outcome == "native_format" {
			t.Fatal("rejected first event repaired by later native start")
		}
	}
}

func TestFirstTaskAfterLongIdleRetainsSeparateNativeTimestamps(t *testing.T) {
	t.Parallel()
	task := map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-02T12:00:00Z"}
	h := ReadCodexHeader(strings.NewReader(testRecords(t, nil, task)), "rollout-"+testID+".jsonl")
	if h.Outcome != "native_format" || h.FirstTaskAt.Sub(h.Started) != 24*time.Hour {
		t.Fatalf("ordinary idle gap rejected: %#v", h)
	}
}

func TestApprovedAncestorAliasOpensCanonicalSnapshotWithoutEscapes(t *testing.T) {
	t.Parallel()
	physical, aliasParent, outside := t.TempDir(), t.TempDir(), t.TempDir()
	alias := filepath.Join(aliasParent, "approved")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	name := "rollout-" + testID + ".jsonl"
	if err := os.WriteFile(filepath.Join(physical, name), []byte(testRecords(t, nil, nil)), 0600); err != nil {
		t.Fatal(err)
	}
	h := ReadHeader(context.Background(), alias, filepath.Join(alias, name))
	if h.Outcome != "native_format" {
		t.Fatalf("approved ancestor alias rejected: %#v", h)
	}
	secret := filepath.Join(outside, "outside.jsonl")
	if err := os.WriteFile(secret, []byte(testRecords(t, nil, nil)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(physical, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(physical, name)); err != nil {
		t.Fatal(err)
	}
	if h := ReadHeader(context.Background(), alias, filepath.Join(alias, name)); h.Outcome != "source_unavailable" {
		t.Fatal("alias weakened source confinement", h.Outcome)
	}
}

func TestNativeStartOptionalRootTurnID(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"task_started", "turn_started"} {
		for _, root := range []any{nil, testID, "00000000-0000-0000-0000-000000000002", "invalid", 42} {
			task := map[string]any{"type": kind, "turn_id": testID, "started_at": "2026-10-01T12:00:00Z"}
			if root != nil {
				task["root_turn_id"] = root
			}
			h := ReadCodexHeader(strings.NewReader(testRecords(t, nil, task)), "rollout-"+testID+".jsonl")
			want := "inherited_history"
			if root == nil || root == testID || root == "00000000-0000-0000-0000-000000000002" {
				want = "native_format"
			}
			if h.Outcome != want {
				t.Errorf("kind=%s root=%v: got %s want %s", kind, root, h.Outcome, want)
			}
		}
	}
}

func TestCodexHeaderUsesOuterMetadataTimestamp(t *testing.T) {
	t.Parallel()
	data := testRecords(t, nil, nil)
	var meta map[string]any
	lines := strings.Split(data, "\n")
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatal(err)
	}
	payload := meta["payload"].(map[string]any)
	meta["timestamp"] = payload["timestamp"]
	delete(payload, "timestamp")
	first, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	lines[0] = string(first)
	h := ReadCodexHeader(strings.NewReader(strings.Join(lines, "\n")), "rollout-"+testID+".jsonl")
	if h.Outcome != "native_format" || h.Started.Format(time.RFC3339Nano) != "2026-10-01T12:00:00Z" {
		t.Fatalf("outer timestamp lost: %#v", h)
	}
}
