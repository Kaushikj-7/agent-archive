package nativecodec

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// harnessTextKinds classify a user record by the tag its text starts with.
// These records are written by the harness around something the person did,
// not typed as a prompt: Claude Code wraps a `!` shell command in
// <bash-input>, its output in <bash-stdout>/<bash-stderr>, a local command's
// output in <local-command-stdout>/<local-command-stderr>, the note it adds
// before local-command output in <local-command-caveat>, and a typed slash
// command in <command-name>/<command-message>/<command-args>.
var harnessTextKinds = []struct {
	tag  string
	kind archive.TurnKind
}{
	{"bash-input", archive.TurnKindShellCommand},
	{"bash-stdout", archive.TurnKindCommandOutput},
	{"bash-stderr", archive.TurnKindCommandOutput},
	{"local-command-stdout", archive.TurnKindCommandOutput},
	{"local-command-stderr", archive.TurnKindCommandOutput},
	{"local-command-caveat", archive.TurnKindCommandOutput},
	{"command-name", archive.TurnKindLocalCommand},
	{"command-message", archive.TurnKindLocalCommand},
	{"command-args", archive.TurnKindLocalCommand},
}

// isPlaceholderModel reports a model name a harness writes on a message no
// model produced: Claude Code labels the messages it synthesizes itself
// (an interruption notice, "No response requested.") "<synthetic>". Such a
// name is not a model, and its usage is no model's usage.
func isPlaceholderModel(name string) bool {
	return strings.HasPrefix(name, "<") && strings.HasSuffix(name, ">")
}

// recordModel is the model a native record names, or "" for none or a
// placeholder.
func recordModel(record map[string]any) string {
	if model := firstStringDeep(record, "model", "model_id"); !isPlaceholderModel(model) {
		return model
	}
	return ""
}

// interruptionMarker is the user record Claude Code writes when the person
// stops a turn ("[Request interrupted by user]", "[Request interrupted by
// user for tool use]"). The harness writes it, not the person, so it is not
// a prompt and does not start an exchange.
var interruptionMarker = regexp.MustCompile(`^\[Request interrupted by user[^\]\n]*\]$`)

// refineUserKind reclassifies a record that looked like a human prompt but was
// written by the harness. A slash command is provisionally a local command;
// resolveSlashCommands promotes it to a prompt if the assistant answered it.
func refineUserKind(record map[string]any, kind archive.TurnKind, text string) archive.TurnKind {
	if kind != archive.TurnKindHumanPrompt {
		return kind
	}
	if isCompactSummaryRecord(record) {
		return archive.TurnKindCompactSummary
	}
	if isMetaRecord(record) {
		return archive.TurnKindHarnessMeta
	}
	if isHarnessOrigin(record) {
		return archive.TurnKindHarnessNotification
	}
	trimmed := strings.TrimSpace(text)
	if interruptionMarker.MatchString(trimmed) {
		return archive.TurnKindHarnessNotification
	}
	for _, candidate := range harnessTextKinds {
		if strings.HasPrefix(trimmed, "<"+candidate.tag+">") {
			return candidate.kind
		}
	}
	return kind
}

// harnessOriginKinds are the origin.kind values Claude Code is known to write
// on user records it produced itself. It is an allowlist on purpose: an
// origin kind not listed here, including "human" and any kind a later version
// adds, leaves the record a prompt, so an unfamiliar kind can never silently
// drop something the person sent.
var harnessOriginKinds = map[string]bool{"task-notification": true}

// isHarnessOrigin reports whether a record's origin.kind names a known
// harness-produced record (a background-task notification). A record with no
// origin, which every pre-filter-6 bundle is, is not reclassified.
func isHarnessOrigin(record map[string]any) bool {
	origin, ok := record["origin"].(map[string]any)
	if !ok {
		return false
	}
	return harnessOriginKinds[strings.ToLower(strings.TrimSpace(firstString(origin, "kind")))]
}

// resolveSlashCommands decides which typed slash commands were prompts. A
// slash command that expands into a skill or custom command is answered by the
// assistant; a local one such as /model or /clear is answered only by
// local-command output. So a slash command counts as a prompt only if an
// assistant record follows before the next thing the person did, skipping
// harness-written records (isMeta expansions, command output, tool results).
// A compaction summary ends the scan without promoting: the conversation
// before it is summarized away, so an assistant record after it cannot be an
// answer to a command before it, and /compact itself is never a prompt.

// isCompactSummaryRecord reports whether a Claude Code user record is the
// summary it writes after compaction. Only the boolean true that filter 5
// retains counts.
func isCompactSummaryRecord(record map[string]any) bool {
	flag, ok := record["isCompactSummary"].(bool)
	return ok && flag
}

// isCodexStartupShell reports whether a completed item is Codex starting its
// own shell for the session rather than a command the model ran. Only a
// CommandExecution whose source is exactly "unified_exec_startup" qualifies;
// any other completed item, and a CommandExecution from any other source,
// is still a tool call.
func isCodexStartupShell(item map[string]any) bool {
	return strings.EqualFold(strings.TrimSpace(firstString(item, "type")), "commandexecution") && firstString(item, "source") == "unified_exec_startup"
}

// isSidechainRecord reports whether a native record belongs to a subagent
// rather than to the session that owns the transcript. Only an explicit true
// counts: an absent or non-boolean flag leaves the record in place.
func isSidechainRecord(record map[string]any) bool {
	for _, key := range []string{"isSidechain", "is_sidechain"} {
		if flag, ok := record[key].(bool); ok && flag {
			return true
		}
	}
	return false
}

// visibleMessage finds the record's authored message and classifies it. A
// Cursor record carries its role at the top level and its content under
// "message", which the filter-2 era parser never looked for, so every Cursor
// session derived zero turns.
func visibleMessage(record map[string]any) (string, string, archive.TurnKind, bool) {
	if role, _ := record["role"].(string); role != "" {
		content, present := record["content"]
		if !present {
			if nested, ok := record["message"].(map[string]any); ok {
				content, present = nested["content"]
			}
		}
		if present {
			if text, kind, ok := classifyContent(role, content); ok {
				return role, text, kind, true
			}
		}
		if role == "tool" {
			return role, "", archive.TurnKindToolResult, true
		}
	}
	for _, key := range []string{"message", "payload", "item", "event"} {
		if nested, ok := record[key].(map[string]any); ok {
			if role, text, kind, found := visibleMessage(nested); found {
				return role, text, kind, true
			}
		}
	}
	return "", "", "", false
}

// classifyContent separates what a person sent from what a harness wrote back
// in the user role. A user record whose content is only tool results is not a
// prompt, and counting it as one is what made a four-prompt session report
// twenty-eight turns.
func classifyContent(role string, content any) (string, archive.TurnKind, bool) {
	text, toolResults, other := visibleContent(content)
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return text, archive.TurnKindAssistant, true
	case "tool":
		return text, archive.TurnKindToolResult, true
	}
	if text != "" || other > 0 {
		return text, archive.TurnKindHumanPrompt, true
	}
	if toolResults > 0 {
		return text, archive.TurnKindToolResult, true
	}
	// A record with a role but nothing retained is not a visible message.
	return "", "", false
}

// textBlockTypes name the content blocks whose only payload is text. When the
// filter strips such a block's text — an injected <system-reminder> or
// <user_instructions> block is the common case — the block survives as a bare
// `{type: "text"}` that carries nothing a person sent.
var textBlockTypes = map[string]bool{"text": true, "input_text": true, "output_text": true, "": true}

// visibleContent reports a message's text, how many tool results it carries,
// and how many other blocks it carries. Tool-result text is deliberately not
// folded into the message text: it is the tool speaking, not the author. A
// text block left empty by the filter counts as nothing at all; otherwise a
// tool-result record with a stripped reminder beside it, or a prompt that was
// only injected instructions, would still count as a human prompt.
func visibleContent(content any) (string, int, int) {
	switch value := content.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return "", 0, 0
		}
		return value, 0, 1
	case map[string]any:
		return visibleContent([]any{value})
	case []any:
		parts := make([]string, 0, len(value))
		toolResults, other := 0, 0
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				if text := contentText(raw); text != "" {
					parts = append(parts, text)
					other++
				}
				continue
			}
			kind := strings.ToLower(strings.TrimSpace(firstString(block, "type")))
			switch {
			case toolResultTypes[kind]:
				toolResults++
			case toolInvocationTypes[kind]:
				other++
			default:
				text := contentText(block)
				if text == "" && textBlockTypes[kind] {
					continue
				}
				if text != "" {
					parts = append(parts, text)
				}
				other++
			}
		}
		return strings.Join(parts, "\n"), toolResults, other
	}
	return "", 0, 0
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := contentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, _ := v["text"].(string); text != "" {
			return text
		}
		if content, ok := v["content"]; ok {
			return contentText(content)
		}
	}
	return ""
}

// toolInvocationTypes name the shapes a harness uses to report that it called
// a tool: Claude's "tool_use", Codex's "function_call", "custom_tool_call" and
// "local_shell_call", and the generic "tool_call".
var toolInvocationTypes = map[string]bool{
	"tool_use": true, "tool_call": true, "function_call": true,
	"custom_tool_call": true, "local_shell_call": true,
}

// completedItemTypes name the Codex item.type values which report finished
// tool work inside an item_completed event.
var completedItemTypes = map[string]bool{
	"commandexecution": true, "mcptoolcall": true, "extension": true,
}

// toolResultTypes name the shapes which carry a tool's output back.
var toolResultTypes = map[string]bool{
	"tool_result": true, "function_call_output": true, "custom_tool_call_output": true,
	"local_shell_call_output": true,
}

// toolCallCandidate carries whether a call came from a completion event, which
// dedupeToolCalls needs and the published view does not.
type toolCallCandidate = archive.ToolCallCandidate

// toolActivity walks one native record once, extracting its normalized
// tool-call and tool-result entries together with any native skill
// invocation/read-inference signal found along the way — a single pass shared
// by archive.ParseNormalized's ToolCalls and deriveSkills' native-record evidence,
// rather than each doing its own separate recursive walk over the same
// structure.
func toolActivity(record map[string]any, index int, model, reasoning string) ([]toolCallCandidate, []archive.NormalizedToolResult, []archive.SkillUse) {
	var calls []toolCallCandidate
	var results []archive.NormalizedToolResult
	var skillUses []archive.SkillUse
	parent := firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid")
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			kind := strings.ToLower(strings.TrimSpace(firstString(item, "type")))
			isToolInvocation := toolInvocationTypes[kind]
			tool := firstString(item, "name", "tool_name")
			arguments := toolArguments(item)
			if isToolInvocation {
				calls = append(calls, toolCallCandidate{Call: observedTool(archive.NormalizedToolCall{
					RecordIndex: index, CallID: firstString(item, "call_id", "id"), ParentID: parent,
					Model: model, Reasoning: reasoning, Name: tool, Input: arguments,
				}, item)})
			}
			if kind == "item_completed" {
				if completed, ok := item["item"].(map[string]any); ok && completedItemTypes[strings.ToLower(strings.TrimSpace(firstString(completed, "type")))] && !isCodexStartupShell(completed) {
					calls = append(calls, toolCallCandidate{CompletionEcho: true, Call: observedTool(archive.NormalizedToolCall{
						RecordIndex: index, CallID: firstString(completed, "call_id", "id"), ParentID: parent,
						Model: model, Reasoning: reasoning, Name: firstString(completed, "name", "tool_name"),
						Input: toolArguments(completed),
					}, completed)})
				}
			}
			if toolResultTypes[kind] {
				isError, _ := item["is_error"].(bool)
				output := toolResultOutput(item)
				results = append(results, archive.NormalizedToolResult{
					RecordIndex: index, CallID: firstString(item, "call_id", "tool_use_id"),
					IsError: isError, OutputBytes: len(output), Text: output,
				})
			}
			if isToolInvocation && strings.EqualFold(tool, "skill") {
				if name := firstString(arguments, "skill", "name"); name != "" {
					skillUses = append(skillUses, archive.SkillUse{Name: name, Evidence: archive.SkillUseEvidenceNativeInvocation})
				}
			}
			path := firstString(item, "file_path", "path")
			if path == "" {
				// A harness may carry its arguments as a nested object
				// (Claude's "input") or as a JSON-encoded string (Codex's
				// "arguments"); toolArguments has already decoded either.
				path = firstString(arguments, "file_path", "path")
			}
			command := firstString(item, "command", "arguments")
			readTool := strings.EqualFold(tool, "read") || strings.EqualFold(tool, "read_file")
			catRead := strings.HasPrefix(strings.TrimSpace(command), "cat ")
			if readTool || catRead {
				if name := archive.SkillNameFromPath(path); name != "" {
					skillUses = append(skillUses, archive.SkillUse{Name: name, Evidence: archive.SkillUseEvidenceReadInference})
				} else if catRead {
					if name := archive.SkillNameFromPath(command); name != "" {
						skillUses = append(skillUses, archive.SkillUse{Name: name, Evidence: archive.SkillUseEvidenceReadInference})
					}
				}
			}
			for _, key := range walkKeys(item, toolResultTypes[kind]) {
				walk(item[key])
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(record)
	return calls, results, skillUses
}

// walkKeys are the keys of item toolActivity descends into, in sorted order
// so the calls of one record always come out in the same order. It never
// descends into a call's arguments (toolArgumentKeys: a tool's input may
// itself contain {"type":"tool_use"} shapes, which are data, not calls) or,
// for a result, into its output.
func walkKeys(item map[string]any, isResult bool) []string {
	keys := make([]string, 0, len(item))
	for key := range item {
		//lint:ignore LV1001 keys of an external JSON record are an open domain
		if toolArgumentKeys[key] || (isResult && (key == "content" || key == "output")) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// toolArguments returns a tool call's retained arguments as an object. Claude
// nests them under "input"; Codex encodes them as a JSON string under
// "arguments" or "input". Anything which does not decode to an object yields
// nil rather than an invented shape.
func toolArguments(item map[string]any) map[string]any {
	for _, key := range []string{"input", "arguments", "tool_input"} {
		switch value := item[key].(type) {
		case map[string]any:
			return value
		case string:
			var decoded map[string]any
			if value != "" && json.Unmarshal([]byte(value), &decoded) == nil && len(decoded) > 0 {
				return decoded
			}
		}
	}
	return nil
}

// toolResultOutput returns the retained output text of one tool result, whose
// length is what OutputBytes reports. Current Codex writes a function or custom
// tool's output as a list of {type: input_text, text} blocks rather than one
// string; their text is joined the same way message content is.
func toolResultOutput(item map[string]any) string {
	switch output := item["output"].(type) {
	case string:
		return output
	case []any, map[string]any:
		return contentText(output)
	}
	if content, present := item["content"]; present {
		return contentText(content)
	}
	if result, ok := item["result"].(string); ok {
		return result
	}
	return ""
}

func nestedMessageID(record map[string]any) string {
	if message, ok := record["message"].(map[string]any); ok {
		return firstString(message, "id")
	}
	return ""
}

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := record[key].(string); value != "" {
			return value
		}
	}
	return ""
}

func firstMapDeepOwner(record map[string]any, key string) (map[string]any, map[string]any) {
	if value, ok := record[key].(map[string]any); ok {
		return value, record
	}
	for _, nested := range []string{"message", "payload", "item", "event"} {
		if child, ok := record[nested].(map[string]any); ok {
			if value, owner := firstMapDeepOwner(child, key); value != nil {
				return value, owner
			}
		}
	}
	return nil, nil
}

func firstStringDeep(record map[string]any, keys ...string) string {
	if value := firstString(record, keys...); value != "" {
		return value
	}
	for _, key := range []string{"message", "payload", "item", "event"} {
		if nested, ok := record[key].(map[string]any); ok {
			if value := firstStringDeep(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}

func isCompactBoundary(record map[string]any) bool {
	kind, _ := record["type"].(string)
	subtype, _ := record["subtype"].(string)
	return kind == "system" && subtype == "compact_boundary"
}

func isMetaRecord(record map[string]any) bool {
	flag, ok := record["isMeta"].(bool)
	return ok && flag
}

func parseNativeTimestamp(record map[string]any) time.Time {
	for _, key := range []string{"timestamp", "created_at"} {
		value, _ := record[key].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func isHiddenRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "system", "developer", "reasoning", "analysis", "thinking", "chain_of_thought", "agent_reasoning", "agent_reasoning_delta", "raw_agent_reasoning":
		return true
	default:
		return false
	}
}

func tokenCount(raw any) (int, bool) {
	value, ok := raw.(float64)
	if !ok || value < 0 || value > maxTokenCount || value != math.Trunc(value) {
		return 0, false
	}
	return int(value), true
}

func reasoningTokenCount(source map[string]any) (int, bool) {
	if value, ok := tokenCount(source["reasoning_output_tokens"]); ok {
		return value, true
	}
	if details, ok := source["output_tokens_details"].(map[string]any); ok {
		for _, key := range []string{"thinking_tokens", "reasoning_tokens"} {
			if value, ok := tokenCount(details[key]); ok {
				return value, true
			}
		}
	}
	return 0, false
}

func parsePRNumber(digits string) (int, bool) {
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n >= 1 && n <= maxPRNumber
}

func splitRepository(repository string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || !repoPartPattern.MatchString(owner) || !repoPartPattern.MatchString(name) {
		return "", "", false
	}
	return owner, name, true
}

const maxTokenCount = 1 << 53

const maxPRNumber = 1 << 30

var toolArgumentKeys = map[string]bool{"input": true, "arguments": true, "tool_input": true, "args": true, "toolcall": true}

var repoPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func tokenObservation(source map[string]any) archive.TokenObservation {
	read := func(keys ...string) archive.TokenCount {
		for _, key := range keys {
			if value, ok := tokenCount(source[key]); ok {
				return archive.TokenCount{Value: value, Recorded: true}
			}
		}
		return archive.TokenCount{}
	}
	var reasoning archive.TokenCount
	if value, ok := reasoningTokenCount(source); ok {
		reasoning = archive.TokenCount{Value: value, Recorded: true}
	}
	out := archive.TokenObservation{Reasoning: reasoning, Input: read("input_tokens", "prompt_tokens"), Output: read("output_tokens", "completion_tokens"), CacheRead: read("cache_read_input_tokens", "cached_input_tokens"), CacheWrite: read("cache_creation_input_tokens", "cache_write_input_tokens")}
	return out
}
