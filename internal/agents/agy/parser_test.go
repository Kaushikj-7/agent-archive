package agy

import (
	"context"
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
