package agy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives bounded conversation facts from retained AGY records only.
type Parser struct{}

// Version identifies the AGY derivation policy.
func (Parser) Version() string { return "1.0.0" }

// Parse converts documented AGY trajectory records into the shared view.
func (Parser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	if err := ctx.Err(); err != nil {
		return archive.Analysis{}, err
	}
	if len(bundle.NativeRecords) == 0 {
		return archive.Analysis{}, &archive.ParseError{Reason: "AGY transcript has no retained records"}
	}
	view := archive.NormalizedView{}
	var firstPrompt string
	var firstAt time.Time
	var toolCalls int
	for index, record := range bundle.NativeRecords {
		if err := ctx.Err(); err != nil {
			return archive.Analysis{}, err
		}
		kind, ok := record["type"].(string)
		if !ok || strings.TrimSpace(kind) == "" {
			return archive.Analysis{}, fmt.Errorf("AGY record %d has no type", index)
		}
		at, err := timestamp(record)
		if err != nil {
			return archive.Analysis{}, fmt.Errorf("AGY record %d: %w", index, err)
		}
		if !at.IsZero() && (view.LatestRecordAt.IsZero() || at.After(view.LatestRecordAt)) {
			view.LatestRecordAt = at
		}
		text, hasText := recordText(record)
		id := firstString(record, "id", "step_id", "conversationId")
		model := firstString(record, "modelName", "model_name", "model")
		switch kind {
		case "USER_INPUT":
			if !hasText {
				return archive.Analysis{}, fmt.Errorf("AGY record %d USER_INPUT has no text", index)
			}
			text = cleanUserInput(text)
			if firstPrompt == "" {
				firstPrompt = text
				firstAt = at
			}
			view.Turns = append(view.Turns, archive.NormalizedTurn{RecordIndex: index, Role: "user", Kind: archive.TurnKindHumanPrompt, Text: text, ID: id, Timestamp: at.Format(time.RFC3339Nano)})
		case "PLANNER_RESPONSE":
			if hasText {
				view.Turns = append(view.Turns, archive.NormalizedTurn{RecordIndex: index, Role: "assistant", Kind: archive.TurnKindAssistant, Text: text, Model: model, ModelSource: archive.TurnModelSourceNativeTranscript, ID: id, Timestamp: at.Format(time.RFC3339Nano)})
			}
		case "RUN_COMMAND", "TOOL_CALL":
			toolCalls++
			view.ToolCalls = append(view.ToolCalls, archive.NormalizedToolCall{RecordIndex: index, CallID: id, Name: toolName(record), Model: model})
		case "TOOL_RESULT":
			view.ToolResults = append(view.ToolResults, archive.NormalizedToolResult{RecordIndex: index, CallID: id})
		}
	}
	if len(view.Turns) == 0 {
		return archive.Analysis{}, &archive.ParseError{Reason: "AGY transcript contains no visible conversation turns"}
	}
	structured := archive.Availability{State: archive.AvailabilityAvailable}
	tools := archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	if toolCalls > 0 {
		tools = structured
	}
	return archive.Analysis{
		View: view,
		Facts: archive.NativeFacts{
			Name: firstPrompt, TextTitle: firstPrompt, NativeStartedAt: firstAt,
			EarliestRecordAt: firstAt, Text: true, TokenScopeUnknown: true,
		},
		Observability: archive.Observability{
			StructuredCounts: structured, ToolCalls: tools,
			FilesTouched: archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			GitActivity:  archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			Compactions:  archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			ToolErrors:   archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			ToolResults:  tools,
		},
	}, nil
}

func timestamp(record map[string]any) (time.Time, error) {
	value := firstString(record, "created_at", "timestamp")
	if value == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
	}
	return at.UTC(), nil
}

func recordText(record map[string]any) (string, bool) {
	for _, key := range []string{"content", "text", "prompt", "message"} {
		if value, ok := record[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func cleanUserInput(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<USER_REQUEST>"); start >= 0 {
		value = value[start+len("<USER_REQUEST>"):]
		if end := strings.Index(value, "</USER_REQUEST>"); end >= 0 {
			value = value[:end]
		}
	}
	return strings.TrimSpace(value)
}

func toolName(record map[string]any) string {
	if nested, ok := record["toolCall"].(map[string]any); ok {
		return firstString(nested, "name", "tool_name")
	}
	return firstString(record, "tool_name", "name", "command")
}

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := record[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
