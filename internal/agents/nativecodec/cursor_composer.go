package nativecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// CursorBubble is one message row: the bubble ID from the chat's header and
// the bubbleId:<chat>:<message> value, nil when the row is missing.
type CursorBubble struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}

// cursorComposerFormat is the source format of a chat filtered from Cursor's
// database, distinct from the hook-provided cursor-jsonl and cursor-text.
const cursorComposerFormat = "cursor-composer"

// cursorComposerVersion and cursorBubbleVersion are the only _v values this
// filter accepts on a composerData value and on a message: the versions the
// probe of a real database found on every chat and every message. Any other
// value, older or newer, or none, is refused rather than guessed at (spec
// phase 2, decision 6). Chats older than composer version 18 kept their
// messages inline in "conversation"; that shape is not read.
const (
	cursorComposerVersion = 18
	cursorBubbleVersion   = 3
)

// Cursor's message types: 1 is the person, 2 is the assistant. Any other type
// is omitted and counted.
const (
	cursorBubbleUser      = 1
	cursorBubbleAssistant = 2
)

// errCursorFormatUnknown is an archive.ErrUnsafeSourceFormat, so callers classify it
// as they do any other source they cannot safely filter.
var errCursorFormatUnknown = fmt.Errorf("cursor composer format version is not known: %w", archive.ErrUnsafeSourceFormat)

// cursorChatNameKey is the key a Cursor chat's name is written under, on
// the session record that starts a cursor-composer transcript (filter 13).
const cursorChatNameKey = "name"

// SameNativeRecord reports whether two filtered records at the same position
// of a transcript in format are the same evidence: equal, or, for a
// cursor-composer transcript, session records that differ only in the chat's
// name. Cursor names a chat after its first messages, and the person can
// rename it; the name is a label the latest one replaces, not evidence a
// later snapshot must keep, so naming a chat is not a rewrite of it.
func SameNativeRecord(format string, previous, candidate map[string]any) bool {
	if reflect.DeepEqual(previous, candidate) {
		return true
	}
	if format != cursorComposerFormat || previous["type"] != "session" || candidate["type"] != "session" {
		return false
	}
	return reflect.DeepEqual(withoutKey(previous, cursorChatNameKey), withoutKey(candidate, cursorChatNameKey))
}

// withoutKey is a shallow copy of record without key.
func withoutKey(record map[string]any, key string) map[string]any {
	out := make(map[string]any, len(record))
	for k, v := range record {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// cursorComposerConsumed are the composerData keys the filter reads for
// structure, identity, and the chat's name. They are not retained as fields
// of their own, but nothing they hold is lost either, so they are not
// reported as omitted. conversation is reported by its own gap when it holds
// anything. name is written onto the session record (filter 13); a name that
// is not a string is reported as omitted.
var cursorComposerConsumed = map[string]bool{
	"_v": true, "composerId": true, "createdAt": true,
	"fullConversationHeadersOnly": true, "conversation": true,
	"generatingBubbleIds": true, "status": true, "name": true,
}

// cursorBubbleConsumed are the message keys the filter maps onto a record
// (see bubbleRecord). richText is the editor's structured copy of text and is
// consumed only when text is present.
var cursorBubbleConsumed = map[string]bool{
	"_v": true, "bubbleId": true, "type": true, "text": true,
	"toolFormerData": true, "toolResults": true, "tokenCount": true, "modelInfo": true,
	"createdAt": true, "startedAtMs": true, "completedAtMs": true, "requestId": true,
}

// cursorToolConsumed are the toolFormerData keys mapped onto tool_use and
// tool_result blocks. Everything else in it (toolCallBinary, userDecision,
// additionalData, the tool's numeric ID) is reported as omitted.
var cursorToolConsumed = map[string]bool{
	"toolCallId": true, "name": true, "rawArgs": true, "params": true,
	"result": true, "error": true, "status": true,
}

// cursorContextKeys are the context payloads Cursor attaches to a message:
// code, files, diffs, images, and logs gathered for the model, not what the
// person or the assistant wrote. They are dropped by design (spec phase 2,
// decision 5) and reported by name under their own gap.
var cursorContextKeys = map[string]bool{
	"codebaseContextChunks": true, "attachedCodeChunks": true, "originalFileStates": true,
	"diffHistories": true, "images": true, "consoleLogs": true, "recentlyViewedFiles": true,
	"relevantFiles": true, "attachedFolders": true, "attachedFoldersNew": true,
	"attachedFileCodeChunksUris": true, "externalLinks": true, "docsReferences": true,
	"webReferences": true, "gitDiffs": true, "lints": true, "context": true,
	"contextPieces": true, "cursorRules": true, "summarizedComposers": true,
	"suggestedCodeBlocks": true, "fileDiffTrajectories": true, "workspaceUris": true,
}

// cursorHiddenKeys carry the model's reasoning, which no adapter retains.
var cursorHiddenKeys = map[string]bool{"thinking": true, "allThinkingBlocks": true}

// cursorSettledToolStatuses are the toolFormerData statuses of a tool call
// that has finished. Any other status, including none, is treated as still
// running ("loading" and "pending" are the ones Cursor is known to write), so
// its message is in flight. The probed database had only "completed" and
// "error".
var cursorSettledToolStatuses = map[string]bool{"completed": true, "error": true, "cancelled": true, "canceled": true}

// cursorSettledComposerStatuses are the composerData statuses of a chat that
// is not generating. Any other value is treated as generating its last
// message, as a backstop to generatingBubbleIds. The probed database had
// only "completed" and "none".
var cursorSettledComposerStatuses = map[string]bool{
	"": true, "completed": true, "none": true, "aborted": true,
	"cancelled": true, "canceled": true, "error": true,
}

// cursorComposerFilter accumulates one FilterComposer call.
type cursorComposerFilter struct {
	bounded        *cursorOutputBudget
	result         archive.FilteredTranscript
	gapSet         map[string]bool
	omitted        keyNameSet
	denied         keyNameSet
	context        keyNameSet
	args           keyNameSet
	argSources     keyNameSet
	resultDenied   keyNameSet
	messages       int
	missing        int
	idMismatch     int
	blobMessages   int
	unknownType    int
	incompleteTail int
	unreadableTail int
	composerBlob   bool
}

func (f *cursorComposerFilter) addGap(code string, _ int, detail string) {
	key := code + ":" + detail
	if !f.gapSet[key] {
		f.gapSet[key] = true
		f.result.Gaps = append(f.result.Gaps, archive.CaptureGap{Code: code, Detail: detail})
	}
}

// omit records an omitted key name under its level (chat, message, tool for
// toolFormerData, toolResult for toolResults entries, model, tokens, or record
// for a key the sanitizer omitted) so a reader can tell where it was.
func (f *cursorComposerFilter) omit(level, key string) { f.omitted.add(level + "." + key) }

// FilterComposerRecords consumes one composer and its ordered borrowed message rows.
func FilterComposerRecords(ctx context.Context, raw []byte, next func(context.Context) (CursorBubble, bool, error)) (archive.FilteredTranscript, error) {
	return filterComposerRecords(ctx, raw, next, nil)
}

func filterComposerRecords(ctx context.Context, raw []byte, next func(context.Context) (CursorBubble, bool, error), bounded *cursorOutputBudget) (archive.FilteredTranscript, error) {
	if len(raw) > maxRecordBytes {
		return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
	}
	var composer map[string]any
	if err := json.Unmarshal(raw, &composer); err != nil || composer == nil {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor composer is not valid JSON"}
	}
	if v, ok := cursorInt(composer["_v"]); !ok || v != cursorComposerVersion {
		return archive.FilteredTranscript{}, errCursorFormatUnknown
	}
	composerID, _ := composer["composerId"].(string)
	if strings.TrimSpace(composerID) == "" {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor composer has no composer ID"}
	}
	// createdAt is the zero time when the chat records no creation time.
	createdAt, hasCreatedAt := cursorTime(composer["createdAt"])
	f := &cursorComposerFilter{
		bounded: bounded,
		result: archive.FilteredTranscript{
			Format:              cursorComposerFormat,
			SessionIDs:          []string{composerID},
			NativeStartAt:       createdAt,
			FirstEventAt:        createdAt,
			NativeStartComplete: hasCreatedAt,
		},
		gapSet: map[string]bool{},
	}
	for _, key := range sortedKeys(composer) {
		switch {
		case cursorComposerConsumed[key]:
		case isCursorBlobKey(key):
			if key != "blobEncryptionKey" && nonEmptyValue(composer[key]) {
				f.composerBlob = true
			}
		default:
			f.omit("chat", key)
		}
	}
	// A name is text, or nothing: Cursor leaves it empty until it has named
	// the chat. Anything else is not a name, and is reported as omitted.
	chatName := ""
	switch name := composer["name"].(type) {
	case nil:
	case string:
		chatName = strings.TrimSpace(name)
	default:
		f.omit("chat", "name")
	}
	if conversation, present := composer["conversation"]; present && nonEmptyValue(conversation) {
		f.addGap("cursor_inline_conversation_omitted", 0, "inline conversation entries are not read")
	}

	headers, _ := composer["fullConversationHeadersOnly"].([]any)
	messages, err := f.filterStreamMessages(ctx, headers, next, cursorGenerating(composer))
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	if len(messages) > 0 {
		session := map[string]any{"type": "session", "session_id": composerID}
		if hasCreatedAt {
			session["timestamp"] = createdAt.Format(time.RFC3339Nano)
		}
		if chatName != "" {
			session[cursorChatNameKey] = chatName
		}
		if err := f.retain(session); err != nil {
			return archive.FilteredTranscript{}, err
		}
		f.result.Records = append(f.result.Records, messages...)
		for _, record := range messages {
			f.result.Boundary.RetainedRecords++
			f.result.Boundary.RetainedBytes += len(record)
		}
	}
	if !f.result.NativeStartComplete && !f.result.NativeStartAt.IsZero() {
		f.result.FirstEventAt = f.result.NativeStartAt
	}
	f.finishGaps()
	return f.result, nil
}

// filterHeaderMessages builds the records for the messages listed in
// fullConversationHeadersOnly, which the reader supplies as bubbles in the
// same order. Output stops at the first message in flight, and at the first
// message whose row is missing, belongs to another message, or disagrees with
// its header about who wrote it: skipping such a message and emitting later
// ones would make the output stop being a prefix of the next pass's once the
// row appears or is fixed. generating reports whether a header's message is
// one the chat is still generating. A list that does not match the headers is
// refused: the reader and the chat disagree about what the conversation is.
func (f *cursorComposerFilter) filterStreamMessages(ctx context.Context, headers []any, next func(context.Context) (CursorBubble, bool, error), generating func(index, count int, id string) bool) ([][]byte, error) {
	f.messages = len(headers)
	var records [][]byte
	stopped := false
	var lastAt time.Time
	for i, rawHeader := range headers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, more, err := next(ctx)
		if err != nil {
			return nil, err
		}
		if !more {
			return nil, &archive.FilterError{Reason: "cursor composer messages do not match its headers"}
		}
		if stopped {
			continue
		}
		header, _ := rawHeader.(map[string]any)
		headerID, _ := header["bubbleId"].(string)
		if header == nil || headerID == "" || headerID != row.ID {
			return nil, &archive.FilterError{Reason: "cursor composer messages do not match its headers"}
		}
		if generating(i, len(headers), headerID) {
			f.incompleteTail = len(headers) - i
			stopped = true
			continue
		}
		headerType, hasHeaderType := cursorInt(header["type"])
		value := bytes.TrimSpace(row.Value)
		if len(value) == 0 || string(value) == "null" {
			f.missing++
			f.unreadableTail = len(headers) - i
			stopped = true
			continue
		}
		bubble, err := decodeCursorBubble(value)
		if err != nil {
			return nil, err
		}
		if rowID, _ := bubble["bubbleId"].(string); rowID != headerID {
			// The row is some other message's: as good as missing.
			f.missing++
			f.idMismatch++
			f.unreadableTail = len(headers) - i
			stopped = true
			continue
		}
		if cursorToolRunning(bubble) {
			f.incompleteTail = len(headers) - i
			stopped = true
			continue
		}
		bubbleType, _ := cursorInt(bubble["type"])
		if hasHeaderType && bubbleType != headerType {
			// The header and the row disagree about who wrote the message.
			f.unknownType++
			f.unreadableTail = len(headers) - i
			stopped = true
			continue
		}
		var role string
		switch bubbleType {
		case cursorBubbleUser:
			role = "user"
		case cursorBubbleAssistant:
			role = "assistant"
		default:
			f.unknownType++
			continue
		}
		record, blob := f.bubbleRecord(bubble, role)
		if blob {
			f.blobMessages++
		}
		if created, ok := cursorTime(bubble["createdAt"]); ok && !f.result.NativeStartComplete && (f.result.NativeStartAt.IsZero() || created.Before(f.result.NativeStartAt)) {
			f.result.NativeStartAt = created
		}
		before := len(f.result.Records)
		if err := f.retain(record); err != nil {
			return nil, err
		}
		if len(f.result.Records) > before {
			encoded := f.result.Records[before]
			records = append(records, encoded)
			f.result.Boundary.RetainedRecords--
			f.result.Boundary.RetainedBytes -= len(encoded)
		}
		f.result.Records = f.result.Records[:before]
		// Session is emitted first by the caller, so defer byte/record accounting.
		at := cursorMessageTime(bubble)
		if at.IsZero() {
			at = cursorMessageTime(header)
		}
		if !at.IsZero() {
			lastAt = at
		}
	}
	if _, more, err := next(ctx); err != nil {
		return nil, err
	} else if more {
		return nil, &archive.FilterError{Reason: "cursor composer messages do not match its headers"}
	}
	f.result.NativeEndAt = lastAt
	return records, nil
}

// A message is in flight, and may still change, when the chat lists it in
// generatingBubbleIds; when the chat's status is not a settled one (see
// cursorSettledComposerStatuses) and it is the chat's last message; or when
// its tool call's status is not a settled one (see cursorToolRunning).
// completedAtMs is not a signal: most finished assistant messages in the
// probed database had none.

// cursorGenerating returns the chat-level half of that rule for the message
// at index of count with the given ID.
func cursorGenerating(composer map[string]any) func(index, count int, id string) bool {
	ids := map[string]bool{}
	if list, ok := composer["generatingBubbleIds"].([]any); ok {
		for _, raw := range list {
			if id, ok := raw.(string); ok {
				ids[id] = true
			}
		}
	}
	status, _ := composer["status"].(string)
	chatGenerating := !cursorSettledComposerStatuses[strings.ToLower(strings.TrimSpace(status))]
	return func(index, count int, id string) bool {
		return ids[id] || (chatGenerating && index == count-1)
	}
}

// cursorToolRunning reports whether a message's tool call has not settled.
func cursorToolRunning(bubble map[string]any) bool {
	tool, ok := bubble["toolFormerData"].(map[string]any)
	if !ok {
		return false
	}
	status, _ := tool["status"].(string)
	return !cursorSettledToolStatuses[strings.ToLower(strings.TrimSpace(status))]
}

// bubbleRecord maps one message onto the record shape FilterComposer
// documents, reporting every key it does not map, and whether the message
// references blob content.
func (f *cursorComposerFilter) bubbleRecord(bubble map[string]any, role string) (map[string]any, bool) {
	record := map[string]any{"role": role}
	if id, ok := bubble["bubbleId"].(string); ok {
		record["id"] = id
	}
	if created, ok := cursorTime(bubble["createdAt"]); ok {
		record["timestamp"] = created.Format(time.RFC3339Nano)
	}
	if requestID, ok := bubble["requestId"].(string); ok && requestID != "" {
		record["requestId"] = requestID
	}
	for key, out := range map[string]string{"startedAtMs": "started_at_ms", "completedAtMs": "completed_at_ms"} {
		if ms, ok := bubble[key].(float64); ok && ms > 0 {
			record[out] = ms
		}
	}
	if info, ok := bubble["modelInfo"].(map[string]any); ok {
		for _, key := range sortedKeys(info) {
			if name, isString := info[key].(string); key == "modelName" && isString && name != "" {
				record["model"] = name
			} else if key != "modelName" {
				f.omit("model", key)
			}
		}
	}
	if usage := f.cursorUsage(bubble["tokenCount"]); usage != nil {
		record["usage"] = usage
	}

	var content []any
	text, _ := bubble["text"].(string)
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	content = append(content, f.toolFormerBlocks(bubble["toolFormerData"])...)
	content = append(content, f.toolResultBlocks(bubble["toolResults"])...)
	if len(content) > 0 {
		record["message"] = map[string]any{"content": content}
	}

	blob := false
	for _, key := range sortedKeys(bubble) {
		switch {
		case cursorBubbleConsumed[key]:
		case key == "richText" && text != "":
		case cursorContextKeys[key]:
			if nonEmptyValue(bubble[key]) {
				f.context.add(key)
			}
		case cursorHiddenKeys[key]:
			if nonEmptyValue(bubble[key]) {
				f.addGap("hidden_instruction_omitted", 0, "reasoning omitted")
			}
		case isCursorBlobKey(key):
			if nonEmptyValue(bubble[key]) {
				blob = true
			}
		default:
			f.omit("message", key)
		}
	}
	return record, blob
}

// cursorUsage maps a message's tokenCount onto the usage shape the parser
// sums. Cursor writes zeros on messages it did not meter, so an all-zero count
// is not retained as an observed zero.
func (f *cursorComposerFilter) cursorUsage(raw any) map[string]any {
	count, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	usage := map[string]any{}
	for _, key := range sortedKeys(count) {
		value, isNumber := count[key].(float64)
		switch {
		case key == "inputTokens" && isNumber:
			usage["input_tokens"] = value
		case key == "outputTokens" && isNumber:
			usage["output_tokens"] = value
		default:
			f.omit("tokens", key)
		}
	}
	if in, out := usage["input_tokens"], usage["output_tokens"]; (in == nil || in == 0.0) && (out == nil || out == 0.0) {
		return nil
	}
	return usage
}

// toolFormerBlocks maps a message's toolFormerData (the tool call the
// assistant made and, once it ran, its result or error) onto a tool_use block
// and a tool_result block. A tool that failed reports its error, not its
// result, as the tool_result's content, with is_error set; a result recorded
// beside an error is reported as omitted.
func (f *cursorComposerFilter) toolFormerBlocks(raw any) []any {
	tool, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	callID, _ := tool["toolCallId"].(string)
	name, _ := tool["name"].(string)
	call := map[string]any{"type": "tool_use"}
	if callID != "" {
		call["id"] = callID
	}
	if name != "" {
		call["name"] = name
	}
	if input := f.toolInput(tool); input != nil {
		call["input"] = input
	}
	var blocks []any
	if len(call) > 1 {
		blocks = append(blocks, call)
	}
	status, _ := tool["status"].(string)
	output, hasOutput := f.toolOutput(tool["result"], "tool", "result")
	isError := strings.EqualFold(status, "error")
	// An error of any shape (Cursor writes a string) is kept as text.
	if errorText, hasError := f.toolOutput(tool["error"], "tool", "error"); hasError && nonEmptyValue(tool["error"]) {
		if hasOutput {
			f.omit("tool", "result")
		}
		output, hasOutput, isError = errorText, true, true
	}
	if hasOutput {
		result := map[string]any{"type": "tool_result", "content": output}
		if callID != "" {
			result["tool_use_id"] = callID
		}
		if status != "" {
			result["status"] = status
		}
		if isError {
			result["is_error"] = true
		}
		blocks = append(blocks, result)
	}
	for _, key := range sortedKeys(tool) {
		if !cursorToolConsumed[key] {
			f.omit("tool", key)
		}
	}
	return blocks
}

// toolInput returns a tool call's arguments as an object: rawArgs when it
// decodes to a non-empty one (after dropNestedJSON), else params. When
// neither yields arguments, a source that did not decode is named in its own
// gap detail, apart from the tool's argument names; a source that decoded to
// an empty object is no arguments, not an omission. A string argument that is
// itself JSON (an object or array) is dropped and named rather than retained
// as opaque text the argument rules never saw.
func (f *cursorComposerFilter) toolInput(tool map[string]any) map[string]any {
	var undecodable []string
	for _, key := range []string{"rawArgs", "params"} {
		decoded, ok := cursorArgumentObject(tool[key])
		if !ok {
			if nonEmptyValue(tool[key]) {
				undecodable = append(undecodable, key)
			}
			continue
		}
		if input, _ := f.dropNestedJSON(decoded, "").(map[string]any); len(input) > 0 {
			return input
		}
	}
	for _, key := range undecodable {
		f.argSources.add(key)
	}
	return nil
}

// dropNestedJSON removes, at every depth, each string that decodes as a JSON
// object or array, naming the nearest argument key that held it (parent). It
// also removes objects left empty, which carry nothing and which the
// sanitizer would otherwise report as a record without allowed fields, and
// arrays emptied by the removal, so no [[]] is left behind.
func (f *cursorComposerFilter) dropNestedJSON(value any, parent string) any {
	emptied := func(before, after any) bool {
		switch a := after.(type) {
		case map[string]any:
			return len(a) == 0
		case []any:
			b, _ := before.([]any)
			return len(a) == 0 && len(b) > 0
		}
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range sortedKeys(v) {
			if s, ok := v[key].(string); ok && isNestedJSON(s) {
				f.args.add(key)
				continue
			}
			if child := f.dropNestedJSON(v[key], key); !emptied(v[key], child) {
				out[key] = child
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && isNestedJSON(s) {
				f.args.add(parent)
				continue
			}
			if child := f.dropNestedJSON(item, parent); !emptied(item, child) {
				out = append(out, child)
			}
		}
		return out
	}
	return value
}

// isNestedJSON reports whether a string is a JSON object or array.
func isNestedJSON(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	return json.Valid([]byte(trimmed))
}

// toolResultBlocks maps the entries of a message's toolResults onto
// tool_result blocks. An entry keeps its call ID, tool name, and result; any
// other member is reported by name. (The probed database never had an entry;
// the shape is conservative.)
func (f *cursorComposerFilter) toolResultBlocks(raw any) []any {
	entries, _ := raw.([]any)
	var blocks []any
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			f.addGap("unsupported_value_omitted", 0, "value omitted")
			continue
		}
		block := map[string]any{"type": "tool_result"}
		for _, key := range sortedKeys(entry) {
			//lint:ignore LV1001 keys of Cursor's stored JSON are an external, open vocabulary
			switch key {
			case "toolCallId":
				if id, ok := entry[key].(string); ok && id != "" {
					block["tool_use_id"] = id
				}
			case "name", "toolName":
				if name, ok := entry[key].(string); ok && name != "" {
					block["name"] = name
				}
			case "result":
				if output, ok := f.toolOutput(entry[key], "toolResult", key); ok {
					block["content"] = output
				}
			default:
				f.omit("toolResult", key)
			}
		}
		if len(block) > 1 {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// retain sanitizes one built record exactly as filterJSONL sanitizes a native
// one and appends it.
func (f *cursorComposerFilter) retain(record map[string]any) error {
	if f.bounded != nil {
		defer func() { f.bounded.owner.Release(f.bounded.transient); f.bounded.transient = 0 }()
	}
	if f.bounded != nil && f.bounded.err != nil {
		return f.bounded.err
	}
	state := archive.PrivacyState{AddGap: f.addGap, OmittedKey: func(key string) { f.omit("record", key) }, DeniedKey: f.denied.add}
	safe, keep := sanitizeObject(record, &state)
	if !keep {
		return nil
	}
	var encoded []byte
	var err error
	if f.bounded != nil {
		encoded, err = f.bounded.encodeRecord(safe)
	} else {
		encoded, err = json.Marshal(safe)
	}
	if err != nil {
		if f.bounded != nil {
			return err
		}
		return &archive.FilterError{Reason: "safe record cannot be encoded"}
	}
	f.result.Records = append(f.result.Records, encoded)
	f.result.Boundary.RetainedRecords++
	f.result.Boundary.RetainedBytes += len(encoded)
	return nil
}

// finishGaps adds the counted and named gaps, then orders every gap by code as
// filterJSONL does.
func (f *cursorComposerFilter) finishGaps() {
	if f.missing > 0 {
		f.addGap("cursor_bubble_missing", 0, fmt.Sprintf("%d of %d messages have no message row", f.missing, f.messages))
	}
	if f.idMismatch > 0 {
		f.addGap("cursor_bubble_id_mismatch", 0, fmt.Sprintf("%d of %d message rows belong to another message", f.idMismatch, f.messages))
	}
	if f.blobMessages > 0 {
		f.addGap("cursor_blob_content_unavailable", 0, fmt.Sprintf("%d of %d messages reference content blobs, which are not read", f.blobMessages, f.messages))
	}
	if f.composerBlob {
		f.addGap("cursor_blob_content_unavailable", 0, "chat references content blobs, which are not read")
	}
	if f.unknownType > 0 {
		f.addGap("cursor_message_type_unknown", 0, fmt.Sprintf("%d of %d messages have an unknown or inconsistent type", f.unknownType, f.messages))
	}
	if f.incompleteTail > 0 {
		f.addGap("cursor_incomplete_tail_omitted", 0, fmt.Sprintf("%d of %d messages, from the first one still in flight on, are left for a later pass", f.incompleteTail, f.messages))
	}
	if f.unreadableTail > 0 {
		f.addGap("cursor_incomplete_tail_omitted", 0, fmt.Sprintf("%d of %d messages, from the first one without a usable row on, are left for a later pass", f.unreadableTail, f.messages))
	}
	if detail := f.argSources.detail("tool arguments that do not decode to an object, from Cursor's: "); detail != "" {
		f.addGap("cursor_tool_argument_omitted", 0, detail)
	}
	if detail := f.context.detail("omitted context fields: "); detail != "" {
		f.addGap("cursor_context_omitted", 0, detail)
	}
	if detail := f.args.detail("omitted tool arguments: "); detail != "" {
		f.addGap("cursor_tool_argument_omitted", 0, detail)
	}
	if detail := f.omitted.detail("omitted keys: "); detail != "" {
		f.addGap("unknown_field_omitted", 0, detail)
	}
	if detail := f.denied.detail(deniedToolArgumentIntro); detail != "" {
		f.addGap("sensitive_or_hidden_field_omitted", 0, detail)
	}
	if detail := f.resultDenied.detail("omitted tool result keys: "); detail != "" {
		f.addGap("sensitive_or_hidden_field_omitted", 0, detail)
	}
	sort.SliceStable(f.result.Gaps, func(i, j int) bool { return f.result.Gaps[i].Code < f.result.Gaps[j].Code })
}

// cursorInt reads a JSON number that is a whole number.
func cursorInt(raw any) (int, bool) {
	value, ok := raw.(float64)
	if !ok || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

// cursorTime reads a Cursor timestamp: milliseconds since the epoch (as
// composerData and the *Ms fields write it) or an RFC 3339 string (as some
// message rows write createdAt).
//
// A time before 2000 or after 9999 is not one Cursor wrote (it cannot even
// be encoded as RFC 3339), so it is unknown rather than an error later.
func cursorTime(raw any) (time.Time, bool) {
	var parsed time.Time
	switch value := raw.(type) {
	case float64:
		if value <= 0 || value > float64(cursorTimeMax.UnixMilli()) {
			return time.Time{}, false
		}
		parsed = time.UnixMilli(int64(value)).UTC()
	case string:
		var err error
		if parsed, err = time.Parse(time.RFC3339Nano, value); err != nil {
			return time.Time{}, false
		}
		parsed = parsed.UTC()
	default:
		return time.Time{}, false
	}
	if parsed.Before(cursorTimeMin) || parsed.After(cursorTimeMax) {
		return time.Time{}, false
	}
	return parsed, true
}

// cursorTimeMin and cursorTimeMax bound the timestamps cursorTime accepts.
var (
	cursorTimeMin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cursorTimeMax = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
)

// cursorMessageTime is when a message (or its header) finished: completedAtMs,
// else createdAt.
func cursorMessageTime(message map[string]any) time.Time {
	if at, ok := cursorTime(message["completedAtMs"]); ok {
		return at
	}
	at, _ := cursorTime(message["createdAt"])
	return at
}

// cursorArgumentObject decodes a tool call's arguments when they are an
// object, or a JSON string holding one.
func cursorArgumentObject(raw any) (map[string]any, bool) {
	switch value := raw.(type) {
	case map[string]any:
		return value, true
	case string:
		var decoded map[string]any
		if json.Unmarshal([]byte(value), &decoded) == nil && decoded != nil {
			return decoded, true
		}
	}
	return nil, false
}

// toolOutput reads a tool result as one string. Cursor stores most as a JSON
// string. A structured result is sanitized first, as a tool-argument subtree
// is (every key name kept, blockedKeys and the credential-named key deny list
// applied, every string redacted, binary blocks dropped), and only then
// encoded to one string, so the sanitizer treats it as the text it is rather
// than applying the key allowlist to it. Filter 8 encoded it before
// sanitizing, so a result such as {"password": …} was kept whole: value
// redaction sees `"password":"…"` as text, and the key rules never ran.
//
// An empty object or array is kept as it was (`{}`, `[]`). A structured
// result the sanitizer leaves nothing of is no output, and is named, as
// level.key, among the omitted keys; the sanitizer's own "record without
// allowed fields" gap would misdescribe it, since it is no record.
func (f *cursorComposerFilter) toolOutput(raw any, level, key string) (string, bool) {
	if f.bounded != nil && f.bounded.err != nil {
		return "", false
	}
	original := ""
	if text, isString := raw.(string); isString && isNestedJSON(text) {
		// Filter 11: a result stored as a JSON string (the common case) is
		// the structure it encodes, and goes through the structured branch
		// below. Filter 10 kept it as text, so a base64 image, a cookie, or a
		// token in it survived that the same result stored as an object lost.
		// A result the sanitizer leaves as it was keeps its own text.
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var decoded any
		if decoder.Decode(&decoded) == nil {
			raw, original = decoded, text
		}
	}
	switch value := raw.(type) {
	case nil:
		return "", false
	case string:
		return value, value != ""
	case map[string]any, []any:
		if !nonEmptyValue(value) {
			if original != "" {
				return original, true
			}
			encoded, _ := json.Marshal(value)
			return string(encoded), true
		}
	}
	state := archive.PrivacyState{
		AddGap: func(code string, record int, detail string) {
			if code != "record_without_allowed_fields_omitted" {
				f.addGap(code, record, detail)
			}
		},
		RetainAllKeys: true,
		OmittedKey:    func(key string) { f.omit("toolResult", key) }, DeniedKey: f.resultDenied.add,
	}
	safe, keep := sanitizeValue(raw, &state)
	if !keep {
		f.omit(level, key)
		return "", false
	}
	if original != "" && reflect.DeepEqual(safe, raw) && !archive.JSONHasDuplicateKeys(original) {
		return original, true
	}
	var encoded []byte
	var err error
	if f.bounded != nil {
		encoded, err = f.bounded.encodeTransient(safe)
	} else {
		encoded, err = json.Marshal(safe)
	}
	if err != nil {
		if f.bounded != nil {
			f.bounded.err = err
		}
		return "", false
	}
	return string(encoded), true
}

// isCursorBlobKey reports whether a key refers to agentKv blob content.
func isCursorBlobKey(key string) bool {
	return strings.Contains(strings.ToLower(key), "blob")
}

// nonEmptyValue reports whether a JSON value carries anything.
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

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func decodeCursorBubble(value []byte) (map[string]any, error) {
	if len(value) > maxRecordBytes {
		return nil, archive.ErrRecordTooLarge
	}
	var bubble map[string]any
	if err := json.Unmarshal(value, &bubble); err != nil || bubble == nil {
		return nil, &archive.FilterError{Reason: "cursor message is not valid JSON"}
	}
	if v, ok := cursorInt(bubble["_v"]); !ok || v != cursorBubbleVersion {
		return nil, errCursorFormatUnknown
	}
	return bubble, nil
}

// CursorFilterReservation borrows one captured resource owner; it grants no source authority.
type CursorFilterReservation interface {
	Reserve(int64) bool
	Release(int64)
	Available() int64
	Refuse(int64)
}

type cursorOutputBudget struct {
	ctx       context.Context
	owner     CursorFilterReservation
	limit     int64
	held      int64
	transient int64
	count     int
	err       error
}

// FilterComposerRecordsBounded uses the identical native filter with prospective
// output checks. The returned close ends borrowed encoded-record ownership.
func FilterComposerRecordsBounded(ctx context.Context, raw []byte, next func(context.Context) (CursorBubble, bool, error), limit int64, owner CursorFilterReservation) (archive.FilteredTranscript, func(), error) {
	if owner == nil || limit <= 0 || limit > 32<<20 {
		return archive.FilteredTranscript{}, func() {}, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	budget := &cursorOutputBudget{ctx: ctx, owner: owner, limit: limit}
	var once sync.Once
	closeOutput := func() {
		once.Do(func() { owner.Release(budget.held + budget.transient); budget.held = 0; budget.transient = 0 })
	}
	out, err := filterComposerRecords(ctx, raw, next, budget)
	if err != nil {
		closeOutput()
	}
	return out, closeOutput, err
}

func (b *cursorOutputBudget) encodedBound(value any) (int64, error) {
	const scratch int64 = 32 << 10
	if !b.owner.Reserve(scratch) {
		return 0, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := agentmeta.JSONWireBound(b.ctx, value, min(b.limit-b.held, b.owner.Available()/2))
	b.owner.Release(scratch)
	if err != nil {
		return 0, errors.Join(agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget), err)
	}
	return n, nil
}

func (b *cursorOutputBudget) encodeRecord(value any) ([]byte, error) {
	if b.count >= 65536 {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := b.encodedBound(value)
	if err != nil {
		return nil, err
	}
	if !b.owner.Reserve(2 * n) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		b.owner.Release(2 * n)
		return nil, err
	}
	b.owner.Release(2*n - int64(len(encoded)))
	b.held += int64(len(encoded))
	b.count++
	return encoded, nil
}

func (b *cursorOutputBudget) encodeTransient(value any) ([]byte, error) {
	n, err := b.encodedBound(value)
	if err != nil {
		return nil, err
	}
	if !b.owner.Reserve(2 * n) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		b.owner.Release(2 * n)
		return nil, err
	}
	b.transient += 2 * n
	return encoded, nil
}
