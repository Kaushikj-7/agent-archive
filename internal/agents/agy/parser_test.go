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
