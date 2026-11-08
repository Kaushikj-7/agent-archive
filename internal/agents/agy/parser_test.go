package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestFilterDropsCredentialFields(t *testing.T) {
	filtered, err := (Filter{}).FilterJSONL(strings.NewReader(`{"type":"USER_INPUT","content":"hello","api_key":"secret"}
{"type":"PLANNER_RESPONSE","content":"done"}
`))
	if err != nil {
		t.Fatalf("FilterJSONL() error = %v", err)
	}
	if len(filtered.Records) != 2 {
		t.Fatalf("retained records = %d, want 2", len(filtered.Records))
	}
	if strings.Contains(string(filtered.Records[0]), "secret") {
		t.Fatalf("credential leaked into filtered record: %s", filtered.Records[0])
	}
}

func TestParserBuildsVisibleTurnsAndFacts(t *testing.T) {
	bundle := archive.SourceBundle{
		NativeSessionID: "conv-1",
		NativeRecords: []map[string]any{
			{"type": "USER_INPUT", "content": "<USER_REQUEST>Fix the build</USER_REQUEST>", "created_at": "2026-10-26T10:00:00Z"},
			{"type": "PLANNER_RESPONSE", "content": "I will fix it.", "created_at": "2026-10-26T10:00:01Z", "modelName": "gemini"},
			{"type": "RUN_COMMAND", "toolCall": map[string]any{"name": "run_command"}, "created_at": "2026-10-26T10:00:02Z"},
		},
	}

	got, err := (Parser{}).Parse(context.Background(), bundle)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got.View.Turns) != 2 || got.View.Turns[0].Text != "Fix the build" || got.View.Turns[1].Role != "assistant" {
		t.Fatalf("unexpected turns: %#v", got.View.Turns)
	}
	if got.Facts.Name != "Fix the build" || !got.Facts.Text || got.Facts.NativeStartedAt.IsZero() {
		t.Fatalf("unexpected facts: %#v", got.Facts)
	}
}

func TestParserRejectsMalformedNativeRecord(t *testing.T) {
	_, err := (Parser{}).Parse(context.Background(), archive.SourceBundle{
		NativeRecords: []map[string]any{{"type": "USER_INPUT", "content": map[string]any{"bad": true}}},
	})
	if err == nil {
		t.Fatal("Parse() accepted non-string AGY content")
	}
}

func TestParserRejectsInvalidTimestampAndEmptyTranscript(t *testing.T) {
	_, err := (Parser{}).Parse(context.Background(), archive.SourceBundle{NativeRecords: []map[string]any{
		{"type": "USER_INPUT", "content": "hello", "timestamp": "not-a-time"},
	}})
	if err == nil {
		t.Fatal("Parse() accepted invalid timestamp")
	}
	_, err = (Parser{}).Parse(context.Background(), archive.SourceBundle{NativeRecords: []map[string]any{
		{"type": "TASK_COMPLETED", "created_at": "2026-10-26T10:00:00Z"},
	}})
	if err == nil {
		t.Fatal("Parse() accepted transcript without visible turns")
	}
}

func TestEvidenceExtendsRequiresPrefix(t *testing.T) {
	filter := Filter{}
	previous := archive.SourceBundle{NativeRecords: []map[string]any{{"type": "USER_INPUT"}}}
	if !filter.EvidenceExtends(previous, archive.SourceBundle{NativeRecords: []map[string]any{{"type": "USER_INPUT"}, {"type": "STOP"}}}) {
		t.Fatal("matching prefix was rejected")
	}
	if filter.EvidenceExtends(previous, archive.SourceBundle{NativeRecords: []map[string]any{{"type": "STOP"}}}) {
		t.Fatal("different prefix was accepted")
	}
}

func TestFilterPreservesAntigravityParserFieldsAndRedactsSecrets(t *testing.T) {
	raw := `{"type":"USER_INPUT","prompt":"<USER_REQUEST>run tests</USER_REQUEST>","conversationId":"conv-123","step_id":"step-1","created_at":"2026-10-26T10:00:00Z"}
{"type":"PLANNER_RESPONSE","content":"Running","modelName":"gemini-2.5-pro","created_at":"2026-10-26T10:00:01Z"}
{"type":"RUN_COMMAND","toolCall":{"name":"run_command","args":{"CommandLine":"go test ./...","api_key":"supersecret"}},"step_id":"step-2","created_at":"2026-10-26T10:00:02Z"}
`
	filtered, err := (Filter{}).FilterJSONL(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("FilterJSONL() error = %v", err)
	}
	filteredStr := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(filteredStr, "supersecret") {
		t.Fatal("api_key secret was not redacted")
	}
	if !strings.Contains(filteredStr, "go test ./...") {
		t.Fatal("CommandLine was dropped by privacy filter")
	}
	if !strings.Contains(filteredStr, "gemini-2.5-pro") {
		t.Fatal("modelName was dropped by privacy filter")
	}
	if !strings.Contains(filteredStr, "conv-123") {
		t.Fatal("conversationId was dropped by privacy filter")
	}

	var parsedRecords []map[string]any
	for _, rec := range filtered.Records {
		var item map[string]any
		if err := json.Unmarshal(rec, &item); err != nil {
			t.Fatalf("unmarshal filtered record: %v", err)
		}
		parsedRecords = append(parsedRecords, item)
	}
	analysis, err := (Parser{}).Parse(context.Background(), archive.SourceBundle{
		NativeSessionID: "conv-123",
		NativeRecords:   parsedRecords,
	})
	if err != nil {
		t.Fatalf("Parse() on filtered records failed: %v", err)
	}
	if len(analysis.View.Turns) != 2 || analysis.View.Turns[0].Text != "run tests" {
		t.Fatalf("unexpected turns from filtered records: %#v", analysis.View.Turns)
	}
	if analysis.View.Turns[1].Model != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %q", analysis.View.Turns[1].Model)
	}
	if len(analysis.View.ToolCalls) != 1 || analysis.View.ToolCalls[0].Name != "run_command" {
		t.Fatalf("unexpected tool calls from filtered records: %#v", analysis.View.ToolCalls)
	}
}

func TestParserPopulatesToolDetailsAndLinksResults(t *testing.T) {
	bundle := archive.SourceBundle{
		NativeSessionID: "conv-1",
		NativeRecords: []map[string]any{
			{"type": "USER_INPUT", "content": "run tests", "created_at": "2026-10-26T10:00:00Z"},
			{
				"type": "RUN_COMMAND",
				"id":   "call-1",
				"toolCall": map[string]any{
					"name": "run_command",
					"args": map[string]any{"CommandLine": "go test ./..."},
				},
				"created_at": "2026-10-26T10:00:01Z",
			},
			{
				"type":       "TOOL_RESULT",
				"id":         "call-1",
				"content":    "FAIL: tests failed",
				"is_error":   true,
				"created_at": "2026-10-26T10:00:02Z",
			},
		},
	}
	analysis, err := (Parser{}).Parse(context.Background(), bundle)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(analysis.View.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(analysis.View.ToolCalls))
	}
	call := analysis.View.ToolCalls[0]
	if call.ShellCommand != "go test ./..." {
		t.Fatalf("expected ShellCommand 'go test ./...', got %q", call.ShellCommand)
	}
	if call.Action.Kind != archive.ToolActionShell {
		t.Fatalf("expected ToolActionShell, got %v", call.Action.Kind)
	}
	if call.IsError == nil || !*call.IsError {
		t.Fatalf("expected IsError=true linked to call, got %v", call.IsError)
	}
	if call.ResultText != "FAIL: tests failed" {
		t.Fatalf("expected ResultText 'FAIL: tests failed', got %q", call.ResultText)
	}
	if analysis.Observability.ToolResults.State != archive.AvailabilityAvailable {
		t.Fatalf("expected ToolResults Available when results exist, got %v", analysis.Observability.ToolResults.State)
	}

	h, err := archive.BuildHandoffWithAnalysis(bundle, analysis, nil, archive.HandoffOptions{Source: "local"})
	if err != nil {
		t.Fatalf("BuildHandoffWithAnalysis() error = %v", err)
	}
	rendered := string(archive.RenderHandoffMarkdown(h, archive.HandoffRenderOptions{Preamble: true}))
	if !strings.Contains(rendered, "go test ./...") {
		t.Fatalf("expected rendered handoff to contain command 'go test ./...', got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "FAIL: tests failed") {
		t.Fatalf("expected rendered handoff to contain result 'FAIL: tests failed', got:\n%s", rendered)
	}
}

func TestParserToolResultAvailabilityUnavailableWhenNoResultsRecorded(t *testing.T) {
	bundle := archive.SourceBundle{
		NativeSessionID: "conv-2",
		NativeRecords: []map[string]any{
			{"type": "USER_INPUT", "content": "run tests", "created_at": "2026-10-26T10:00:00Z"},
			{
				"type": "RUN_COMMAND",
				"id":   "call-1",
				"toolCall": map[string]any{
					"name": "run_command",
					"args": map[string]any{"CommandLine": "go test ./..."},
				},
				"created_at": "2026-10-26T10:00:01Z",
			},
		},
	}
	analysis, err := (Parser{}).Parse(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Observability.ToolResults.State != archive.AvailabilityUnavailable {
		t.Fatalf("expected ToolResults Unavailable when no results recorded, got %v", analysis.Observability.ToolResults.State)
	}
}

func TestParserToolActionsForFileAndSearchOperations(t *testing.T) {
	bundle := archive.SourceBundle{
		NativeSessionID: "conv-3",
		NativeRecords: []map[string]any{
			{"type": "USER_INPUT", "content": "check code", "created_at": "2026-10-26T10:00:00Z"},
			{
				"type": "TOOL_CALL",
				"id":   "call-read",
				"toolCall": map[string]any{
					"name": "read_file",
					"args": map[string]any{"file_path": "internal/parser.go"},
				},
				"created_at": "2026-10-26T10:00:01Z",
			},
			{
				"type": "TOOL_CALL",
				"id":   "call-edit",
				"toolCall": map[string]any{
					"name": "write_to_file",
					"args": map[string]any{"TargetFile": "output.txt"},
				},
				"created_at": "2026-10-26T10:00:02Z",
			},
			{
				"type": "TOOL_CALL",
				"id":   "call-replace",
				"toolCall": map[string]any{
					"name": "replace_file_content",
					"args": map[string]any{"target_file": "config.json"},
				},
				"created_at": "2026-10-26T10:00:03Z",
			},
			{
				"type": "TOOL_CALL",
				"id":   "call-search",
				"toolCall": map[string]any{
					"name": "grep_search",
					"args": map[string]any{"Query": "func Parse"},
				},
				"created_at": "2026-10-26T10:00:04Z",
			},
		},
	}
	analysis, err := (Parser{}).Parse(context.Background(), bundle)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(analysis.View.ToolCalls) != 4 {
		t.Fatalf("expected 4 tool calls, got %d", len(analysis.View.ToolCalls))
	}
	readCall := analysis.View.ToolCalls[0]
	if readCall.Action.Kind != archive.ToolActionRead || readCall.Action.Path != "internal/parser.go" {
		t.Fatalf("unexpected read action: %#v", readCall.Action)
	}
	editCall := analysis.View.ToolCalls[1]
	if editCall.Action.Kind != archive.ToolActionEdit || len(editCall.Action.Files) != 1 || editCall.Action.Files[0] != "output.txt" {
		t.Fatalf("unexpected edit action: %#v", editCall.Action)
	}
	replaceCall := analysis.View.ToolCalls[2]
	if replaceCall.Action.Kind != archive.ToolActionEdit || len(replaceCall.Action.Files) != 1 || replaceCall.Action.Files[0] != "config.json" {
		t.Fatalf("unexpected replace action: %#v", replaceCall.Action)
	}
	searchCall := analysis.View.ToolCalls[3]
	if searchCall.Action.Kind != archive.ToolActionSearch || searchCall.Action.Text != "func Parse" {
		t.Fatalf("unexpected search action: %#v", searchCall.Action)
	}
}

