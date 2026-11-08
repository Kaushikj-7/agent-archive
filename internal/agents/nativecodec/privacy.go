package nativecodec

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"sort"
	"strings"
)

var allowedKeys = map[string]bool{
	"type": true, "id": true, "uuid": true, "session_id": true, "parent_id": true,
	"parent_uuid": true, "parentuuid": true, "timestamp": true, "created_at": true, "updated_at": true,
	"cwd": true, "model": true, "model_provider": true, "role": true, "content": true,
	"channel": true,
	"text":    true, "message": true, "item": true, "event": true, "payload": true,
	"tool_name": true, "tool_input": true, "tool_output": true, "tool_use": true,
	"tool_result": true, "call_id": true, "input": true, "output": true, "result": true, "arguments": true,
	"command": true, "path": true, "query": true, "url": true, "description": true,
	"status": true, "event_name": true, "turn_id": true, "reasoning_effort": true, "name": true, "items": true,
	"sha256": true, "message_id": true, "settings": true, "model_id": true, "discovered": true, "installed": true, "snapshot": true, "source": true,
	"coverage": true, "skills": true, "redacted": true, "observed_at": true,
	"model_params": true, "value": true, "cli_version": true, "agent_id": true,
	// "version" is the per-record Claude Code build stamp; it attributes a
	// published capture to an installed version. Values still pass sanitizeValue.
	"version":     true,
	"uncertainty": true, "scope": true, "original_bytes": true, "event_id": true,
	"truncated": true, "omitted_count": true, "snapshot_omitted_count": true, "inventory_complete": true, "root_status": true,
	"gaps":  true,
	"skill": true,
	// Claude Code marks a subagent's records with is_sidechain. Retaining the
	// flag lets a parent's normalized view exclude any inlined child records
	// from its own counts; the child is archived as its own session.
	"is_sidechain": true, "issidechain": true,
	"file_path":          true,
	"archive_session_id": true, "relationship": true,
	// Filter 3 additions. A tool result can only be joined back to the call it
	// answers through tool_use_id, and a turn's own identity (sessionId,
	// requestId, gitBranch) is what lets a reader place a record in its
	// session and working branch. usage and the Codex *_token_usage subtrees
	// are retained as numbers only (see numericSubtreeKeys).
	"tool_use_id": true, "tooluseid": true, "is_error": true, "stop_reason": true,
	"usage": true, "sessionid": true, "requestid": true, "gitbranch": true,
	// Codex token accounting records.
	"info": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true, "last_agent_message": true,
	"thread_id": true, "root_turn_id": true, "completed_at_ms": true, "started_at_ms": true,
	// Filter 4: Claude Code marks harness-written user records (an expanded
	// skill or slash command, a local-command caveat) with isMeta. The flag is
	// what tells a parser such a record is not a human prompt; the record's
	// text itself is stripped (see stripMetaRecordText).
	"ismeta": true,
	// Filter 5: after /compact or auto-compaction Claude Code writes a user
	// record carrying a model-written summary of the earlier conversation,
	// marked isCompactSummary (and usually isVisibleInTranscriptOnly). The
	// flags tell a parser it is not a prompt. Unlike an isMeta record, the
	// summary's text is kept: it is model output, useful for a handoff. Both
	// are admitted as booleans only (see booleanFlagKeys).
	"iscompactsummary": true, "isvisibleintranscriptonly": true,
	// Filter 6: Claude Code says who produced a user record. A person's prompt
	// carries origin.kind "human"; a background-task completion carries
	// "task-notification" and promptSource "system". origin is retained only
	// as its kind string (see originKindOnly) and promptSource only as a
	// string, so a parser can tell a notification from a prompt.
	"origin": true, "promptsource": true,
	// Filter 33: Antigravity trajectory keys.
	"modelname": true, "model_name": true, "step_id": true, "conversationid": true,
	"conversation_id": true, "prompt": true, "toolcall": true, "args": true,
}

// booleanFlagKeys are allowed only as the boolean flag the harness writes. Any
// other value under one of these names is prose the allowlist never retained.
var booleanFlagKeys = map[string]bool{"ismeta": true, "iscompactsummary": true, "isvisibleintranscriptonly": true}

// Retaining tool arguments wholesale has two exceptions, applied at every
// depth of a tool-argument subtree. The argument's key name is recorded in a
// gap; its value is never retained.
var numericSubtreeKeys = map[string]bool{
	"usage": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true,
}

func sanitizeObject(in map[string]any, state *archive.PrivacyState) (map[string]any, bool) {
	if archive.PrivacyHiddenObject(in) {
		state.AddGap("hidden_instruction_omitted", state.Record, "record omitted")
		return nil, false
	}
	// Filter 9: a pasted screenshot, a PDF, or an image a tool read arrives
	// as a content block whose data is base64. Its key names pass the
	// allowlist (type, source), so the block is recognized by shape and
	// dropped whole, wherever it sits, rather than kept key by key.
	if detail, binary := archive.PrivacyBinaryObject(in); binary && !state.NumericOnly {
		state.AddGap("binary_content_omitted", state.Record, detail)
		return nil, false
	}
	out := make(map[string]any)
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Filter 11: a value whose sibling label says it is a password, PIN,
	// one-time code, or card number (`{"name": "Password", "value": …}`,
	// `{"name": "DB_PASSWORD", "value": …}`) is typed input, whatever the
	// tool.
	sensitiveLabelled := state.RetainAllKeys && archive.PrivacySensitiveLabel(in)
	for _, key := range keys {
		value := in[key]
		lower := strings.ToLower(key)
		if archive.PrivacyBlockedKey(lower) {
			state.AddGap("sensitive_or_hidden_field_omitted", state.Record, "field omitted")
			continue
		}
		switch {
		case state.NumericOnly:
			if !isNumericSubtreeValue(value) {
				state.PrivacyOmit(key)
				continue
			}
		case state.RetainAllKeys:
			// A tool argument's own name is retained; its value is not trusted,
			// and a typed-input or credential-named argument is dropped whole.
			if archive.PrivacyDeniedArgument(key, state.ToolName, sensitiveLabelled) {
				state.PrivacyDeny(key)
				continue
			}
		case !allowedKeys[lower] && !state.ExtraAllowed[lower]:
			state.PrivacyOmit(key)
			continue
		case lower == "origin":
			// Filter 6 admits origin only as {kind: <string>}. Every other
			// member is omitted by name without being inspected further.
			kind, ok := originKindOnly(value, state)
			if !ok {
				state.PrivacyOmit(key)
				continue
			}
			out[key] = kind
			continue
		case lower == "promptsource":
			if _, isString := value.(string); !isString {
				state.PrivacyOmit(key)
				continue
			}
		case typedRecordFlagKey(lower):
			// Filter 4 admits isMeta, and filter 5 isCompactSummary and
			// isVisibleInTranscriptOnly, only as the boolean flag Claude Code
			// writes. Any other value under those names is prose the allowlist
			// never retained, and stays omitted.
			if !typedRecordFlagValue(lower, value) {
				state.PrivacyOmit(key)
				continue
			}
		}
		retainAll, numericOnly, toolName := state.RetainAllKeys, state.NumericOnly, state.ToolName
		switch {
		case numericSubtreeKeys[lower]:
			state.NumericOnly, state.RetainAllKeys = true, false
		case toolArgumentKeys[lower] && !state.NumericOnly:
			if !state.RetainAllKeys {
				state.ToolName = firstString(in, "name", "tool_name")
			}
			state.RetainAllKeys = true
		}
		safe, keep := sanitizeValue(value, state)
		state.RetainAllKeys, state.NumericOnly, state.ToolName = retainAll, numericOnly, toolName
		if keep {
			out[key] = safe
		}
	}
	if len(out) == 0 {
		state.AddGap("record_without_allowed_fields_omitted", state.Record, "record omitted")
		return nil, false
	}
	for _, key := range []string{"payload", "message", "item", "event"} {
		if _, had := in[key]; had {
			if _, kept := out[key]; !kept {
				state.AddGap("hidden_or_unknown_nested_content_omitted", state.Record, "record omitted")
				return nil, false
			}
		}
	}
	return out, true
}

// originKindOnly reduces a Claude Code origin object to {kind: <string>},
// reporting every other member as omitted. It keeps nothing when origin is not
// an object or has no string kind.
func originKindOnly(value any, state *archive.PrivacyState) (map[string]any, bool) {
	origin, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range origin {
		if key != "kind" {
			state.PrivacyOmit(key)
		}
	}
	kind, ok := origin["kind"].(string)
	if !ok || strings.TrimSpace(kind) == "" {
		return nil, false
	}
	safe, keep := sanitizeValue(kind, state)
	if !keep {
		return nil, false
	}
	return map[string]any{"kind": safe}, true
}

// isNumericSubtreeValue reports whether a value may appear in a numbers-only
// subtree. Objects and arrays are admitted so the recursion can prune them;
// everything else, including strings and booleans, is omitted there.
func isNumericSubtreeValue(value any) bool {
	switch value.(type) {
	case float64, json.Number, map[string]any, []any:
		return true
	default:
		return false
	}
}

// Native envelope shaping stays in the codec. Tool-content and scalar
// sanitization always use the mandatory shared privacy policy.
func sanitizeValue(value any, state *archive.PrivacyState) (any, bool) {
	if state.RetainAllKeys || state.NumericOnly {
		return archive.SanitizeValue(value, state)
	}
	switch v := value.(type) {
	case map[string]any:
		return sanitizeObject(v, state)
	case []any:
		if redacted, hit := archive.PrivacyRedactArgv(v); hit {
			state.AddGap("sensitive_content_redacted", state.Record, "content redacted")
			v = redacted
		}
		out := make([]any, 0, len(v))
		for _, item := range v {
			safe, keep := sanitizeValue(item, state)
			if keep {
				out = append(out, safe)
			}
		}
		if len(v) > 0 && len(out) == 0 {
			state.AddGap("hidden_or_unknown_nested_content_omitted", state.Record, "field omitted")
			return nil, false
		}
		return out, true
	default:
		return archive.SanitizeValue(value, state)
	}
}

func typedRecordFlagKey(key string) bool {
	return booleanFlagKeys[key] || key == "history_mode"
}

func typedRecordFlagValue(key string, value any) bool {
	if booleanFlagKeys[key] {
		_, ok := value.(bool)
		return ok
	}
	mode, ok := value.(string)
	return ok && (codexmeta.HistoryMode(mode) == codexmeta.CodexHistoryLegacy || codexmeta.HistoryMode(mode) == codexmeta.CodexHistoryPaginated)
}
