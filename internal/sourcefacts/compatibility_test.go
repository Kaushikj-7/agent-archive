package sourcefacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexProducerFormatFixtures(t *testing.T) {
	t.Parallel()
	for name, profile := range map[string]CodexProfile{
		"codex-150-absent-history.jsonl":  CodexLegacyJSONL,
		"codex-155-legacy.jsonl":          CodexLegacyJSONL,
		"codex-155-alpha-paginated.jsonl": CodexPaginatedJSONL,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			h := ReadCodexHeader(strings.NewReader(string(data)), "rollout-"+testID+".jsonl")
			if h.Outcome != "native_format" || h.Profile != profile || !SupportedCodexProducer(h.Meta) || h.FirstTaskAt.Unix() != 1790856060 || h.Started.Nanosecond() != 750000000 {
				t.Fatalf("fixture rejected or timestamp precision lost: %+v", h)
			}
		})
	}
}

func TestCompatibleUnknownProducerStillNeedsOriginalAdmissionEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		alter func(map[string]any, map[string]any)
		want  string
	}{
		{"additive-fields", func(m, task map[string]any) {
			m["new_setting"] = map[string]any{"ignored": true}
			task["new_field"] = true
		}, "native_format"},
		{"older-native-no-root", func(_, task map[string]any) { delete(task, "root_turn_id") }, "native_format"},
		{"no-version", func(m, _ map[string]any) { delete(m, "cli_version") }, "unsupported_producer"},
		{"wrong-version-type", func(m, _ map[string]any) { m["cli_version"] = 155 }, "invalid_metadata"},
		{"no-originator", func(m, _ map[string]any) { delete(m, "originator") }, "unsupported_producer"},
		{"no-source", func(m, _ map[string]any) { delete(m, "source") }, "unsupported_execution"},
		{"wrong-id", func(m, _ map[string]any) { m["id"] = "bad" }, "invalid_identity"},
		{"root-session-id", func(m, _ map[string]any) { m["session_id"] = "00000000-0000-0000-0000-000000000002" }, "native_format"},
		{"no-created-at", func(m, _ map[string]any) { delete(m, "timestamp") }, "invalid_metadata"},
		{"compressed", func(m, _ map[string]any) { m["history_mode"] = "compressed" }, "unsupported_history"},
		{"null-history-mode", func(m, _ map[string]any) { m["history_mode"] = nil }, "invalid_metadata"},
		{"empty-history-mode", func(m, _ map[string]any) { m["history_mode"] = "" }, "invalid_metadata"},
		{"malformed-history-mode", func(m, _ map[string]any) { m["history_mode"] = map[string]any{"referenced": "other"} }, "invalid_metadata"},
		{"referenced", func(m, _ map[string]any) { m["history_mode"] = "referenced" }, "unsupported_history"},
		{"fork", func(m, _ map[string]any) { m["forked_from_id"] = testID }, "invalid_relationship"},
		{"parent", func(m, _ map[string]any) { m["parent_thread_id"] = testID }, "invalid_relationship"},
		{"history-base", func(m, _ map[string]any) { m["history_base"] = map[string]any{"thread_id": testID, "ordinal": 1} }, "invalid_relationship"},
		{"agent-type", func(m, _ map[string]any) { m["agent_type"] = "worker" }, "native_format"},
		{"subagent-source", func(m, _ map[string]any) { m["source"] = map[string]any{"subagent": "review"} }, "native_format"},
		{"import-first-turn", func(_, task map[string]any) { task["turn_id"] = "external-import-turn-1"; task["root_turn_id"] = nil }, "inherited_history"},
		{"no-turn-id", func(_, task map[string]any) { delete(task, "turn_id") }, "inherited_history"},
		{"no-started-at", func(_, task map[string]any) { delete(task, "started_at") }, "inherited_history"},
		{"bad-started-at", func(_, task map[string]any) { task["started_at"] = "bad" }, "inherited_history"},
		{"attributed-root", func(_, task map[string]any) { task["root_turn_id"] = "00000000-0000-0000-0000-000000000002" }, "native_format"},
		{"invalid-root", func(_, task map[string]any) { task["root_turn_id"] = "invalid" }, "inherited_history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": "vscode", "originator": "new-client", "cli_version": "0.999.0-alpha.1", "history_mode": "paginated"}
			task := map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-01T12:00:00Z"}
			tc.alter(m, task)
			// A later native start cannot repair an invalid/imported first task.
			data := testRecords(t, m, task) + strings.SplitN(testRecords(t, nil, nil), "\n", 2)[1]
			h := ReadCodexHeader(strings.NewReader(data), "rollout-"+testID+".jsonl")
			if h.Outcome != tc.want {
				t.Fatalf("got %s want %s", h.Outcome, tc.want)
			}
			if h.Outcome != "native_format" && (h.Profile != "" || h.Meta.ID != "" || !h.Started.Equal(time.Time{})) {
				t.Fatal("rejected evidence retained")
			}
			encoded, err := json.Marshal(h)
			if err != nil || strings.Contains(string(encoded), "new_setting") {
				t.Fatal("retained unrecognized content")
			}
		})
	}
}

func TestOlderProducerWithoutTaskTimeCannotUseEnvelopeOrLaterResume(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "codex-100-missing-task-time.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	input := string(data) + strings.SplitN(testRecords(t, nil, nil), "\n", 2)[1]
	h := ReadCodexHeader(strings.NewReader(input), "rollout-"+testID+".jsonl")
	if h.Outcome != "inherited_history" || h.Profile != "" {
		t.Fatalf("missing task time was reconstructed: %+v", h)
	}
}
