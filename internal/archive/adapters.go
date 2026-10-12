package archive

import (
	"bytes"
	"encoding/json"
	"errors"

	"io"
	"reflect"

	"sort"
	"strings"
)

// Adapter identifies the codec which produced retained source evidence.
type Adapter interface {
	Name() string
	Version() string
}

// FilterError means no new source bundle may be made from this input. It is
// intentionally distinct from ParseError, where a filtered source is still
// safe and valuable to retain.
type FilterError struct{ Reason string }

// Error names the refusal's reason.
func (e *FilterError) Error() string { return "unsafe source format: " + e.Reason }

// ErrUnsafeSourceFormat means the input held no record of a type the adapter
// recognizes, so nothing in it is known to be safe to retain.
var ErrUnsafeSourceFormat = &FilterError{Reason: "no recognized safe records"}

// ErrRelatedHistory refuses capture until all related native history can be read.
var ErrRelatedHistory = &FilterError{Reason: "related history capture is not yet supported"}

// ErrRecordTooLarge means one JSONL record is longer than MaxRecordBytes. It is
// a FilterError like any other refusal, distinct so a caller can record it as
// the capture gap it is rather than a malformed transcript.
var ErrRecordTooLarge = &FilterError{Reason: "record exceeds the record size limit"}

// MaxRecordBytes is the largest single JSONL record the filter reads, and the
// collector's transcript size ceiling is defined from it (see
// collector.DefaultMaxTranscriptBytes), so any record inside a transcript the
// collector accepts can be read. Filter 4 and earlier stopped at 2 MB, which
// refused whole Claude Code sessions whose tool results are a few megabytes
// before filtering (the bulk is in toolUseResult, which the filter drops).
//
// Reading one record costs memory in proportion to its size: the scanner's
// buffer and the decoded JSON value both hold it, several times over at the
// limit: TestLargeRecordMemoryCeiling measured a peak of roughly 3x the
// record's size (about 175 MiB for a 63 MiB record).
const MaxRecordBytes = 64 * 1024 * 1024

// MaxRawTranscriptBytes is the raw transcript size past which the collector,
// with its default limits, records a transcript as too large without reading
// it: four times the filtered ceiling (collector.DefaultMaxRawTranscriptBytes).
const MaxRawTranscriptBytes int64 = 4 * MaxRecordBytes

// MaxCodexRawTranscriptBytes is the same ceiling for one Codex thread, a
// single rollout or every rollout its history spans: twice the general one.
// A Codex rollout keeps every tool call's full output, which the filter
// drops, so a multi-day thread passes 256 MiB within days while what the
// archive keeps of it stays under the filtered ceiling (a 190 MB rollout on
// the owner's machine, growing about 27 MB a day, kept 31 MB). At that
// ratio the filtered ceiling (64 MiB) is reached first, near 400 MB of
// rollout, so this limit stops only threads unusually heavy in tool
// output. Reading streams a record at a time, so memory follows the longest
// record and what is kept, not this size; time follows what is kept too
// (the redaction of kept text dominates: 32 s for that rollout).
const MaxCodexRawTranscriptBytes int64 = 2 * MaxRawTranscriptBytes

// MaxRawTranscriptBytesFor is the default raw transcript limit for harness:
// MaxCodexRawTranscriptBytes for Codex, MaxRawTranscriptBytes otherwise.
// Every reader that admits, plans or captures a transcript uses it, so none
// refuses what capture accepts.
func MaxRawTranscriptBytesFor(harness string) int64 {
	if harness == HarnessCodex {
		return MaxCodexRawTranscriptBytes
	}
	return MaxRawTranscriptBytes
}

// DefaultParserVersion is the source parser version reported by this bounded
// foundation. The parser is intentionally partial until fixture coverage proves
// a given native format more completely.
const DefaultParserVersion = "0.21.0"

var supplementalAllowedKeys = map[string]bool{
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
	// are retained as numbers only (see supplementalNumericSubtreeKeys).
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
	// are admitted as booleans only (see supplementalBooleanFlagKeys).
	"iscompactsummary": true, "isvisibleintranscriptonly": true,
	// Filter 6: Claude Code says who produced a user record. A person's prompt
	// carries origin.kind "human"; a background-task completion carries
	// "task-notification" and promptSource "system". origin is retained only
	// as its kind string (see originKindOnly) and promptSource only as a
	// string, so a parser can tell a notification from a prompt.
	"origin": true, "promptsource": true,
}

// supplementalBooleanFlagKeys are allowed only as the boolean flag the harness writes. Any
// other value under one of these names is prose the allowlist never retained.
var supplementalBooleanFlagKeys = map[string]bool{"ismeta": true, "iscompactsummary": true, "isvisibleintranscriptonly": true}

// toolArgumentKeys name the subtrees which carry a tool call's own arguments.
// Filter 2 applied supplementalAllowedKeys recursively inside them, which dropped every
// Edit old_string/new_string, Agent prompt, Grep pattern, and MCP argument and
// left tool evidence unusable. Inside these subtrees every argument name is
// retained; blockedKeys, value redaction (redactSensitive), the string cap, and the
// hidden role/channel rules all still apply to the values. Codex's
// payload.input is covered by the same "input" entry.
var toolArgumentKeys = map[string]bool{"input": true, "arguments": true, "tool_input": true}

// Retaining tool arguments wholesale has two exceptions, applied at every
// depth of a tool-argument subtree. The argument's key name is recorded in a
// gap; its value is never retained.
//
// typedInputArgumentKeys are the arguments which carry text a tool types or
// submits outward — into a browser field, a form, a terminal, or a device.
// They are dropped when the tool's name (`name` or `tool_name` beside the
// subtree) says it types (see isTypedInputTool), and, for any tool, when a
// label beside the value says it is a secret (see hasSensitiveLabel). What
// was typed into a login form is exactly the value a transcript must not
// keep.
//
// Any argument whose key names a credential (isCredentialKey, from the one
// credentialVocabulary the text patterns use too) is dropped for every
// tool.
var (
	typedInputArgumentKeys = map[string]bool{"text": true, "value": true, "values": true, "keys": true, "chars": true}
)

// typedInputToolWords are the words of a tool name that say the tool types
// or submits text: Playwright's browser_type, browser_fill_form, and
// browser_press_key, chrome-devtools' fill and fill_form, a computer-use
// tool's type action, form_input, select_option, send_keys, Codex's
// write_stdin, enter_verification_code, autofill_credential. Filter 10
// matched a few whole names and suffixes, and missed every fill_form tool.
var typedInputToolWords = map[string]bool{
	"type": true, "typing": true, "fill": true, "form": true, "input": true, "keyboard": true,
	"key": true, "keys": true, "press": true, "autofill": true, "credential": true, "credentials": true,
	"verification": true, "otp": true, "password": true, "select": true, "paste": true,
	"computer": true, "stdin": true,
}

// isTypedInputTool reports whether a tool's name, split into words (see
// splitNameWords), holds one of typedInputToolWords.
func isTypedInputTool(name string) bool {
	for _, word := range splitNameWords(name) {
		if typedInputToolWords[word] {
			return true
		}
	}
	return false
}

// deniedToolArgument reports whether a tool argument's value must be dropped
// even though the subtree otherwise retains every key: a credential-named
// key, or a typed-input key of a typing tool or beside a sensitive label.
func deniedToolArgument(key, toolName string, sensitiveLabelled bool) bool {
	if isCredentialKey(key) {
		return true
	}
	return typedInputArgumentKeys[strings.ToLower(key)] && (sensitiveLabelled || isTypedInputTool(toolName))
}

var supplementalNumericSubtreeKeys = map[string]bool{
	"usage": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true,
}

// captureGapKeys are additionally allowed inside a capture_gap evidence
// payload, whose whole content is an archive-authored code and its fixed
// description. They are deliberately not in supplementalAllowedKeys: `detail` is a
// common free-text field name in native transcripts, and sanitizeObject
// recurses, so allowing it globally would retain arbitrary nested prose.
var captureGapKeys = map[string]bool{"code": true, "detail": true}

var blockedKeys = map[string]bool{
	"api_key": true, "apikey": true, "access_key": true, "secret": true,
	"secret_key": true, "password": true, "authorization": true, "token": true,
	"cookie": true, "set_cookie": true, "system": true, "developer": true,
	"instructions": true, "reasoning": true, "analysis": true, "encrypted_content": true,
	"image": true, "images": true, "audio": true, "binary": true, "attachment": true,
}

// PrivacyState carries shared mandatory redaction decisions and omission accounting.
type PrivacyState struct {
	Record int
	AddGap func(string, int, string)
	// extraAllowed widens the key allowlist for one archive-authored payload
	// shape. It applies at every depth of that payload, which is safe only
	// because such payloads are flat maps this repository writes itself.
	ExtraAllowed map[string]bool
	// retainAllKeys is set while sanitizing a tool-argument subtree, where the
	// argument names are the tool's own vocabulary and no allowlist can
	// anticipate them. Value sanitization is unchanged.
	RetainAllKeys bool
	// numericOnly is set while sanitizing a token-accounting subtree.
	NumericOnly bool
	// toolName is the name of the tool whose argument subtree is being
	// sanitized, read from the `name` or `tool_name` beside that subtree. It
	// decides whether typed-input arguments are denied.
	ToolName string
	// omittedKey, when set, receives the name of each key the filter could not
	// keep so the caller can report the distinct names once. Without it an
	// omission falls back to the content-free unknown_field_omitted gap.
	OmittedKey func(string)
	// deniedKey, when set, receives the name of each tool argument dropped by
	// the deny list. Without it the drop falls back to the content-free
	// sensitive_or_hidden_field_omitted gap.
	DeniedKey func(string)
}

func (s *PrivacyState) omitField(key string) {
	if s.OmittedKey != nil {
		s.OmittedKey(key)
		return
	}
	s.AddGap("unknown_field_omitted", s.Record, "field omitted")
}

func (s *PrivacyState) denyArgument(key string) {
	if s.DeniedKey != nil {
		s.DeniedKey(key)
		return
	}
	s.AddGap("sensitive_or_hidden_field_omitted", s.Record, "field omitted")
}

// isHiddenObject reports whether an object is a system, developer, or
// reasoning record or block, by its role, channel, or type.
func isHiddenObject(in map[string]any) bool {
	role, _ := in["role"].(string)
	channel, _ := in["channel"].(string)
	kind, _ := in["type"].(string)
	return isHiddenRole(role) || isHiddenChannel(channel) || isHiddenRole(kind)
}

func sanitizeObject(in map[string]any, state *PrivacyState) (map[string]any, bool) {
	if isHiddenObject(in) {
		state.AddGap("hidden_instruction_omitted", state.Record, "record omitted")
		return nil, false
	}
	// Filter 9: a pasted screenshot, a PDF, or an image a tool read arrives
	// as a content block whose data is base64. Its key names pass the
	// allowlist (type, source), so the block is recognized by shape and
	// dropped whole, wherever it sits, rather than kept key by key.
	if detail, binary := binaryContentBlock(in); binary && !state.NumericOnly {
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
	sensitiveLabelled := state.RetainAllKeys && hasSensitiveLabel(in)
	for _, key := range keys {
		value := in[key]
		lower := strings.ToLower(key)
		if blockedKeys[lower] {
			state.AddGap("sensitive_or_hidden_field_omitted", state.Record, "field omitted")
			continue
		}
		switch {
		case state.NumericOnly:
			if !isNumericSubtreeValue(value) {
				state.omitField(key)
				continue
			}
		case state.RetainAllKeys:
			// A tool argument's own name is retained; its value is not trusted,
			// and a typed-input or credential-named argument is dropped whole.
			if deniedToolArgument(key, state.ToolName, sensitiveLabelled) {
				state.denyArgument(key)
				continue
			}
		case !supplementalAllowedKeys[lower] && !state.ExtraAllowed[lower]:
			state.omitField(key)
			continue
		case lower == "origin":
			// Filter 6 admits origin only as {kind: <string>}. Every other
			// member is omitted by name without being inspected further.
			kind, ok := originKindOnly(value, state)
			if !ok {
				state.omitField(key)
				continue
			}
			out[key] = kind
			continue
		case lower == "promptsource":
			if _, isString := value.(string); !isString {
				state.omitField(key)
				continue
			}
		case supplementalBooleanFlagKeys[lower]:
			// Filter 4 admits isMeta, and filter 5 isCompactSummary and
			// isVisibleInTranscriptOnly, only as the boolean flag Claude Code
			// writes. Any other value under those names is prose the allowlist
			// never retained, and stays omitted.
			if _, isFlag := value.(bool); !isFlag {
				state.omitField(key)
				continue
			}
		}
		retainAll, numericOnly, toolName := state.RetainAllKeys, state.NumericOnly, state.ToolName
		switch {
		case supplementalNumericSubtreeKeys[lower]:
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
func originKindOnly(value any, state *PrivacyState) (map[string]any, bool) {
	origin, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range origin {
		if key != "kind" {
			state.omitField(key)
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

func isHiddenChannel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "analysis", "reasoning", "thinking", "chain_of_thought":
		return true
	}
	return false
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

// sanitizeNestedJSON sanitizes a string that holds a JSON object or array
// (Codex's function_call arguments, a tool result an MCP server returned as
// JSON text, a Cursor result stored as a string) structurally, as the
// object or array it is, rather than as opaque text. Filter 10 saw such a
// string only through the text patterns, so the key rules never ran on it:
// a Codex browser_type call kept the password it typed, which the same call
// from Claude Code (whose arguments are an object) dropped.
//
// The decoded value is sanitized as a tool-argument subtree is: every key
// name kept, credential-named keys and typed input dropped, every string
// redacted, binary blocks dropped, and nested JSON strings decoded again.
// Inside a tool call's arguments the tool name still decides what counts as
// typed input. The result is re-encoded (compact, keys sorted, no HTML
// escaping) only when sanitizing changed something; otherwise the string is
// kept byte for byte. A value the sanitizer keeps nothing of becomes `{}` or
// `[]`. ok is false when the string is not a JSON object or array.
func sanitizeNestedJSON(v string, state *PrivacyState) (out string, ok bool) {
	trimmed := strings.TrimSpace(v)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return v, false
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var decoded any
	if decoder.Decode(&decoded) != nil {
		return v, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return v, false
	}
	if !nonEmptyValue(decoded) {
		return v, true
	}
	retainAll, deniedKey := state.RetainAllKeys, state.DeniedKey
	if !retainAll {
		// Outside a tool call's arguments a dropped key is not a tool
		// argument, so it is not named as one.
		state.RetainAllKeys, state.DeniedKey = true, nil
	}
	safe, keep := sanitizeValue(decoded, state)
	state.RetainAllKeys, state.DeniedKey = retainAll, deniedKey
	if !keep {
		return emptyJSONContainer(trimmed[0]), true
	}
	if reflect.DeepEqual(safe, decoded) && !jsonHasDuplicateKeys(trimmed) {
		return v, true
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(safe) != nil {
		return emptyJSONContainer(trimmed[0]), true
	}
	return strings.TrimSuffix(encoded.String(), "\n"), true
}

// jsonHasDuplicateKeys reports whether an object anywhere in the JSON text s
// names a key twice. Decoding keeps only the last, so the text holds a value
// the sanitizer never saw (`{"text": "S3cret", "text": ""}`), and must not
// be kept byte for byte even when the decoded value needs no change.
func jsonHasDuplicateKeys(s string) bool {
	type frame struct {
		keys      map[string]bool
		expectKey bool
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	var stack []*frame // nil frames are arrays
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		top := len(stack) - 1
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				stack = append(stack, &frame{keys: map[string]bool{}, expectKey: true})
				continue
			case '[':
				stack = append(stack, nil)
				continue
			default:
				stack = stack[:top]
			}
		} else if top >= 0 && stack[top] != nil && stack[top].expectKey {
			key, _ := token.(string)
			if stack[top].keys[key] {
				return true
			}
			stack[top].keys[key], stack[top].expectKey = true, false
			continue
		}
		// A value ended; in an object, a key comes next.
		if top = len(stack) - 1; top >= 0 && stack[top] != nil {
			stack[top].expectKey = true
		}
	}
}

// maxSanitizeStringPasses bounds how often sanitizeValue repeats the string
// passes looking for a stable result. Every pass is a complete sanitize, so
// the result is filtered however many ran.
const maxSanitizeStringPasses = 4

// maxTextBytes is the longest string the filter retains.
const maxTextBytes = 64 * 1024

// sanitizeStringOnce is one pass of the string rules: JSON inside the
// string, injected instruction blocks, base64 data URLs, credential
// redaction, and the length cap. keep is false when nothing is left.
func sanitizeStringOnce(v string, state *PrivacyState) (string, bool) {
	if nested, isJSON := sanitizeNestedJSON(v, state); isJSON {
		v = nested
	}
	if injected, stripped := stripInjectedInstructions(v); injected {
		state.AddGap("hidden_instruction_omitted", state.Record, "injected instruction block omitted")
		if stripped == "" {
			return "", false
		}
		v = stripped
	}
	if base64DataURL.MatchString(v) {
		state.AddGap("binary_content_omitted", state.Record, "base64 data URL omitted")
		v = base64DataURL.ReplaceAllString(v, "data:${1}${2};base64,[OMITTED]")
	}
	// One redaction pass: sanitizeValue repeats this function until the
	// string is stable, which repeats the redaction as redactSensitive would.
	if redacted, hit := redactSensitiveOnce(v); hit {
		state.AddGap("sensitive_content_redacted", state.Record, "content redacted")
		v = redacted
	}
	if len(v) > maxTextBytes {
		// Cut on a character boundary, so a retained string stays valid
		// UTF-8 (filter 8 could split a multi-byte character).
		state.AddGap("content_truncated", state.Record, "content truncated")
		v = TruncateUTF8(v, maxTextBytes)
	}
	return v, true
}

// emptyJSONContainer is an empty object or array, by its opening bracket.
func emptyJSONContainer(open byte) string {
	if open == '[' {
		return "[]"
	}
	return "{}"
}

func sanitizeValue(value any, state *PrivacyState) (any, bool) {
	switch v := value.(type) {
	case nil, bool, float64, json.Number:
		return v, true
	case string:
		// One pass can make a string another pass would change: redacting
		// `{"pAss":"0"""}` as text leaves valid JSON whose key the next pass
		// drops, and a cut at the length cap can end a string mid-shape. So
		// the passes repeat until the string is stable (in practice once, or
		// twice when something was changed), which keeps sanitizing
		// idempotent: a republished snapshot is byte-identical.
		for range maxSanitizeStringPasses {
			next, keep := sanitizeStringOnce(v, state)
			if !keep {
				return nil, false
			}
			if next == v {
				break
			}
			v = next
		}
		return v, true
	case map[string]any:
		return sanitizeObject(v, state)
	case []any:
		// Filter 11: an argument vector's secret values (`-pS3cret` after
		// mysql, the word after `--token`) are redacted by their position.
		if redacted, hit := redactArgv(v); hit && !state.NumericOnly {
			state.AddGap("sensitive_content_redacted", state.Record, "content redacted")
			v = redacted
		}
		out := make([]any, 0, len(v))
		for _, item := range v {
			// An array inside a numbers-only subtree is filtered per element,
			// since only sanitizeObject sees the key that admitted it.
			if state.NumericOnly && !isNumericSubtreeValue(item) {
				state.AddGap("unsupported_value_omitted", state.Record, "value omitted")
				continue
			}
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
		state.AddGap("unsupported_value_omitted", state.Record, "value omitted")
		return nil, false
	}
}

func isHiddenRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "system", "developer", "reasoning", "analysis", "thinking", "chain_of_thought", "agent_reasoning", "agent_reasoning_delta", "raw_agent_reasoning":
		return true
	default:
		return false
	}
}

func nonEmptyValue(raw any) bool {
	switch value := raw.(type) {
	case nil:
		return false
	case bool:
		return value
	case float64:
		return value != 0
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	}
	return true
}
