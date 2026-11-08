package agy

import (
	"context"
	"encoding/json"
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
	var candidates []archive.ToolCallCandidate
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
			callID := ""
			if nested, ok := record["toolCall"].(map[string]any); ok {
				callID = firstString(nested, "id", "call_id")
			}
			if callID == "" {
				if nested, ok := record["tool_call"].(map[string]any); ok {
					callID = firstString(nested, "id", "call_id")
				}
			}
			if callID == "" {
				callID = firstString(record, "call_id", "tool_call_id", "id")
			}
			args := toolArguments(record)
			name := toolName(record)
			var fbs []map[string]any
			if nested, ok := record["toolCall"].(map[string]any); ok {
				fbs = append(fbs, nested)
			}
			if nested, ok := record["tool_call"].(map[string]any); ok {
				fbs = append(fbs, nested)
			}
			fbs = append(fbs, record)
			action, shellCmd := buildToolAction(name, args, fbs...)
			candidates = append(candidates, archive.ToolCallCandidate{
				Call: archive.NormalizedToolCall{
					RecordIndex:  index,
					CallID:       callID,
					Name:         name,
					Model:        model,
					Input:        args,
					Action:       action,
					ShellCommand: shellCmd,
					ObservedName: name,
					RecordedAt:   at,
				},
			})
		case "TOOL_RESULT":
			resultID := firstString(record, "call_id", "tool_call_id", "tool_use_id", "id")
			if resultID == "" {
				if nested, ok := record["toolCall"].(map[string]any); ok {
					resultID = firstString(nested, "id", "call_id")
				}
				if resultID == "" {
					if nested, ok := record["tool_call"].(map[string]any); ok {
						resultID = firstString(nested, "id", "call_id")
					}
				}
			}
			var isError bool
			if v, ok := record["is_error"].(bool); ok {
				isError = v
			} else if v, ok := record["isError"].(bool); ok {
				isError = v
			} else if status := firstString(record, "status"); strings.EqualFold(status, "ERROR") {
				isError = true
			}
			view.ToolResults = append(view.ToolResults, archive.NormalizedToolResult{
				RecordIndex: index,
				CallID:      resultID,
				IsError:     isError,
				OutputBytes: len(text),
				Text:        text,
				RecordedAt:  at,
			})
		}
	}
	if len(view.Turns) == 0 {
		return archive.Analysis{}, &archive.ParseError{Reason: "AGY transcript contains no visible conversation turns"}
	}
	view.ToolCalls = archive.FinalizeToolCalls(candidates, view.ToolResults)
	structured := archive.Availability{State: archive.AvailabilityAvailable}
	tools := archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	if len(view.ToolCalls) > 0 {
		tools = structured
	}
	toolResultsAvail := archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	if len(view.ToolResults) > 0 {
		toolResultsAvail = structured
	}
	return archive.Analysis{
		View: view,
		Facts: archive.NativeFacts{
			Name: firstPrompt, TextTitle: firstPrompt, NativeStartedAt: firstAt,
			EarliestRecordAt: firstAt, Text: true, TokenScopeUnknown: true,
		},
		Observability: archive.Observability{
			StructuredCounts: structured,
			ToolCalls:        tools,
			FilesTouched:     archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			GitActivity:      archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			Compactions:      archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			ToolErrors:       archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded},
			ToolResults:      toolResultsAvail,
		},
	}, nil
}

func buildToolAction(name string, args map[string]any, fallbacks ...map[string]any) (archive.ToolAction, string) {
	var action archive.ToolAction
	var shellCmd string
	find := func(keys ...string) string {
		if val := firstString(args, keys...); val != "" {
			return val
		}
		for _, fb := range fallbacks {
			if val := firstString(fb, keys...); val != "" {
				return val
			}
		}
		return ""
	}
	switch strings.ToLower(name) {
	case "run_command", "bash", "exec_command":
		action.Kind = archive.ToolActionShell
		action.Text = find("CommandLine", "command", "cmd")
		shellCmd = action.Text
	case "view_file", "read_file":
		action.Kind = archive.ToolActionRead
		action.Path = find("Path", "AbsolutePath", "path", "file_path")
	case "write_to_file", "replace_file_content", "edit_file", "write_file":
		action.Kind = archive.ToolActionEdit
		if target := find("Path", "TargetFile", "target_file", "path"); target != "" {
			action.Files = []string{target}
		}
	case "grep_search":
		action.Kind = archive.ToolActionSearch
		action.Text = find("Query", "query")
	}
	return action, shellCmd
}

func toolArguments(record map[string]any) map[string]any {
	var targets []map[string]any
	if nested, ok := record["toolCall"].(map[string]any); ok {
		targets = append(targets, nested)
	}
	if nested, ok := record["tool_call"].(map[string]any); ok {
		targets = append(targets, nested)
	}
	targets = append(targets, record)
	for _, target := range targets {
		for _, key := range []string{"args", "arguments", "input"} {
			switch value := target[key].(type) {
			case map[string]any:
				return value
			case string:
				var decoded map[string]any
				if strings.TrimSpace(value) != "" && json.Unmarshal([]byte(value), &decoded) == nil && len(decoded) > 0 {
					return decoded
				}
			}
		}
	}
	return nil
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
	for _, key := range []string{"content", "text", "prompt", "message", "output"} {
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
		if name := firstString(nested, "name", "tool_name"); name != "" {
			return name
		}
	}
	if nested, ok := record["tool_call"].(map[string]any); ok {
		if name := firstString(nested, "name", "tool_name"); name != "" {
			return name
		}
	}
	if name := firstString(record, "tool_name", "name", "command"); name != "" {
		return name
	}
	if kind, _ := record["type"].(string); kind == "RUN_COMMAND" {
		return "run_command"
	}
	return ""
}
