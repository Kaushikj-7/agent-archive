package nativecodec

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// parsedFixture filters a fixture with the named harness's adapter and derives
// its metadata and normalized view, the way a publish does.
func parsedFixture(t *testing.T, harness, name string) (NormalizedView, Metadata) {
	t.Helper()
	adapter, err := NewAdapter(harness)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := adapter.FilterJSONL(bytes.NewReader(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: adapter.Name()}
	captured := time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC)
	bundle, err := NewSourceBundle(reg, adapter, filtered, captured, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	reference := SourceReference{Key: "sessions/" + adapter.Name() + "/archive-123/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}
	metadata, err := BuildMetadata(bundle, "machine", reg.SessionStartedAt, captured, reference, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return view, metadata
}

func countIs(t *testing.T, label string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Errorf("%s is unknown, want %d", label, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", label, *got, want)
	}
}

// Filter 2's parser counted a user record carrying only tool results as a
// human prompt and a turn, so a session with one prompt reported many. Turns
// are human prompts; messages are prompts plus assistant records, including
// assistant records whose only content is a tool call.
func TestParserV06CountsHumanPromptsNotToolResults(t *testing.T) {
	t.Parallel()
	view, metadata := parsedFixture(t, "claude", "claude-tool-only-assistant.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 3)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 1)
	countIs(t, "tool results", metadata.Counts.ToolResults, 1)

	kinds := map[TurnKind]int{}
	for _, turn := range view.Turns {
		kinds[turn.Kind]++
	}
	if kinds[TurnKindHumanPrompt] != 1 || kinds[TurnKindAssistant] != 2 || kinds[TurnKindToolResult] != 1 {
		t.Fatalf("turn kinds = %#v", kinds)
	}

	// Token accounting is summed from every assistant record's usage.
	countIs(t, "input tokens", metadata.Counts.InputTokens, 30)
	countIs(t, "output tokens", metadata.Counts.OutputTokens, 12)
	countIs(t, "cache read tokens", metadata.Counts.CacheReadTokens, 2)
	countIs(t, "cache write tokens", metadata.Counts.CacheWriteTokens, 1)
}

func TestParserV06LinksClaudeToolCallsToTheirResults(t *testing.T) {
	t.Parallel()
	view, _ := parsedFixture(t, "claude", "claude-tool-only-assistant.jsonl")
	if len(view.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", view.ToolCalls)
	}
	call := view.ToolCalls[0]
	if call.Name != "Bash" || call.CallID != "toolu_2" {
		t.Fatalf("tool call identity = %#v", call)
	}
	if call.Input == nil || call.Input["command"] != "go test ./..." || call.Input["timeout"] != float64(120000) {
		t.Fatalf("retained arguments = %#v", call.Input)
	}
	if call.ResultRecordIndex == nil || *call.ResultRecordIndex != 2 {
		t.Fatalf("result record index = %v", call.ResultRecordIndex)
	}
	if call.IsError == nil || *call.IsError {
		t.Fatalf("is_error = %v", call.IsError)
	}
	if call.OutputBytes == nil || *call.OutputBytes != len("ok  github.com/example/widget 0.2s") {
		t.Fatalf("output bytes = %v", call.OutputBytes)
	}
}

// The B1 Edit fixture carries the argument subtree filter 3 restored; the
// parser must surface it and link the result that answers it.
func TestParserV06SurfacesEditArgumentsFromTheFilterFixture(t *testing.T) {
	t.Parallel()
	view, metadata := parsedFixture(t, "claude", "claude-tool-evidence.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 2)
	countIs(t, "tool results", metadata.Counts.ToolResults, 1)
	countIs(t, "input tokens", metadata.Counts.InputTokens, 1200)
	countIs(t, "cache write tokens", metadata.Counts.CacheWriteTokens, 40)
	if len(view.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", view.ToolCalls)
	}
	call := view.ToolCalls[0]
	if call.Name != "Edit" || call.Input == nil || call.Input["file_path"] != "/work/widget/widget.go" {
		t.Fatalf("tool call = %#v", call)
	}
	if body, _ := call.Input["new_string"].(string); !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("normalized view exposed an unredacted argument: %q", body)
	}
	if call.ResultRecordIndex == nil || *call.ResultRecordIndex != 2 {
		t.Fatalf("result linkage = %v", call.ResultRecordIndex)
	}
}

// Codex reports finished tool work twice: once as the call's own record and
// again as an item_completed event. Both must be recognized, and the same
// work must not be counted twice.
func TestParserV06CountsCodexToolEventsWithoutDoubleCounting(t *testing.T) {
	t.Parallel()
	view, metadata := parsedFixture(t, "codex", "codex-tool-events.jsonl")
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 3)
	names := map[string]bool{}
	for _, call := range view.ToolCalls {
		names[call.CallID] = true
		if call.Model != "gpt-6-astra" || call.Reasoning != "high" {
			t.Errorf("turn context attribution lost: %#v", call)
		}
	}
	for _, id := range []string{"call_shell_1", "item_mcp_1", "item_ext_1"} {
		if !names[id] {
			t.Errorf("missing tool call %q in %#v", id, view.ToolCalls)
		}
	}
	countIs(t, "turns", metadata.Counts.Turns, 0)
}

func TestParserV06CountsCodexPromptsToolCallsAndTokens(t *testing.T) {
	t.Parallel()
	view, metadata := parsedFixture(t, "codex", "codex-tool-and-usage.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 1)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 1)
	countIs(t, "tool results", metadata.Counts.ToolResults, 1)
	countIs(t, "input tokens", metadata.Counts.InputTokens, 1000)
	countIs(t, "output tokens", metadata.Counts.OutputTokens, 90)
	countIs(t, "cache read tokens", metadata.Counts.CacheReadTokens, 512)
	if metadata.Counts.CacheWriteTokens != nil {
		t.Errorf("this Codex fixture reports no cache-write accounting, got %d", *metadata.Counts.CacheWriteTokens)
	}
	call := view.ToolCalls[0]
	if call.CallID != "call_synthetic_1" || call.Name != "shell" {
		t.Fatalf("tool call = %#v", call)
	}
	if call.Input == nil || call.Input["timeout_ms"] != float64(120000) {
		t.Fatalf("JSON-encoded arguments were not decoded: %#v", call.Input)
	}
	if call.ResultRecordIndex == nil || *call.ResultRecordIndex != 2 || call.OutputBytes == nil || *call.OutputBytes == 0 {
		t.Fatalf("call_id linkage failed: %#v", call)
	}
}

// Cursor carries its role at the top level and its content under "message",
// which the previous parser never read: every Cursor session derived zero
// turns. Its end-of-turn record is the only outcome signal it has.
func TestParserV06DerivesCursorTurnsAndOutcome(t *testing.T) {
	t.Parallel()
	view, metadata := parsedFixture(t, "cursor", "cursor-turn.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 2)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 1)
	if metadata.Counts.ToolResults != nil {
		t.Fatalf("unavailable Cursor results = %v", metadata.Counts.ToolResults)
	}
	if metadata.State != MetadataStateIdle || metadata.TurnOutcome != TurnOutcomeCompleted {
		t.Fatalf("lifecycle = %q/%q", metadata.State, metadata.TurnOutcome)
	}
	if metadata.Counts.InputTokens != nil {
		t.Errorf("Cursor exposes no token accounting, got %d", *metadata.Counts.InputTokens)
	}
	if len(view.Turns) != 2 || view.Turns[0].Text != "Summarize the failing widget test." {
		t.Fatalf("turns = %#v", view.Turns)
	}
	if view.ToolCalls[0].Name != "read_file" || view.ToolCalls[0].Input["path"] != "internal/widget/widget_test.go" {
		t.Fatalf("tool call = %#v", view.ToolCalls[0])
	}
}

// An observed hook outcome is stronger evidence than a native end-of-turn
// record and must not be overwritten by it.
func TestParserV06NativeTurnEndDoesNotOverrideHookEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: "cursor"}, AdapterName: "cursor", AdapterVersion: adapterVersion, SourceFormat: "cursor-jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords: []map[string]any{{"role": "user", "message": map[string]any{"content": "hello"}}, {"type": "turn_ended", "status": "completed"}},
		SupplementalEvidence: []SupplementalEvidence{
			{Kind: EvidenceKindLifecycleHook, ObservedAt: now, Provenance: "hook:cursor:interrupt", Payload: map[string]any{"event_name": "Interrupt"}},
		},
	}
	reference := SourceReference{Key: "sessions/cursor/a/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64)}
	metadata, err := BuildMetadata(bundle, "machine", now, now, reference, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.TurnOutcome != TurnOutcomeInterrupted {
		t.Fatalf("observed interruption was overwritten: %q", metadata.TurnOutcome)
	}
}

// A tool result which identifies no call is linked by position, which is all
// a harness that names neither side offers.
func TestParserV06LinksUnidentifiedToolResultsByPosition(t *testing.T) {
	t.Parallel()
	input := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"a.go"}}]}}` + "\n" +
		`{"role":"tool","message":{"content":[{"type":"tool_result","content":"package widget"}]}}`
	filtered, err := (CursorAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ToolCalls) != 1 || view.ToolCalls[0].ResultRecordIndex == nil || *view.ToolCalls[0].ResultRecordIndex != 1 {
		t.Fatalf("positional linkage failed: %#v", view.ToolCalls)
	}
	if view.ToolCalls[0].OutputBytes == nil || *view.ToolCalls[0].OutputBytes != len("package widget") {
		t.Fatalf("output bytes = %v", view.ToolCalls[0].OutputBytes)
	}
}

// A result naming a call this bundle does not contain stays unlinked rather
// than being attached to whichever call happens to be next.
func TestParserV06DoesNotMislinkAnUnknownCallID(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: adapterVersion, SourceFormat: "claude-jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords: []map[string]any{
			{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read"}}}},
			{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_from_another_session", "content": "x"}}}},
		},
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if view.ToolCalls[0].ResultRecordIndex != nil {
		t.Fatalf("result was mislinked: %#v", view.ToolCalls[0])
	}
	if len(view.ToolResults) != 1 {
		t.Fatalf("result was not observed: %#v", view.ToolResults)
	}
}

// A bundle captured under filter 2 carries no tool arguments, no tool_use_id,
// and no usage. It must still parse and regenerate metadata; the fields this
// parser version added simply stay absent.
func TestParserV06RegeneratesFilterTwoBundles(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "0.2.0", SourceFormat: "claude-jsonl", FilterVersion: "2", CapturedAt: now},
		NativeRecords: []map[string]any{
			{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "please check it"}},
			{"type": "assistant", "uuid": "a1", "model": "claude-old", "message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "I will check it."},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read"},
			}}},
			{"type": "user", "uuid": "u2", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result"}}}},
		},
	}
	reference := SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64)}
	metadata, err := BuildMetadata(bundle, "machine", now, now, reference, ParserInfo{})
	if err != nil {
		t.Fatalf("a filter 2 bundle no longer parses: %v", err)
	}
	if metadata.FilterVersion != "2" || metadata.Parser.Version != DefaultParserVersion {
		t.Fatalf("regenerated provenance = filter %q parser %q", metadata.FilterVersion, metadata.Parser.Version)
	}
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 2)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 1)
	countIs(t, "tool results", metadata.Counts.ToolResults, 1)
	if metadata.Counts.InputTokens != nil || metadata.Counts.OutputTokens != nil {
		t.Errorf("filter 2 retained no usage, but tokens were reported: %#v", metadata.Counts)
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if view.ToolCalls[0].Input != nil {
		t.Errorf("filter 2 retained no arguments, but some appeared: %#v", view.ToolCalls[0].Input)
	}
	if view.ToolCalls[0].ResultRecordIndex != nil {
		t.Errorf("filter 2 retained no tool_use_id, so nothing can be linked: %#v", view.ToolCalls[0])
	}
}

// ParseNormalized still refuses a source which carries a hidden role: the
// parser is not a second chance for content the filter must have removed.
func TestParserV06StillRejectsHiddenRoles(t *testing.T) {
	t.Parallel()
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: adapterVersion, SourceFormat: "codex-jsonl", FilterVersion: FilterVersion, CapturedAt: time.Now()},
		NativeRecords: []map[string]any{{"role": "system", "content": "must not be normalized"}},
	}
	if _, err := ParseNormalized(bundle); !IsParseError(err) {
		t.Fatalf("hidden role accepted: %v", err)
	}
}

// Claude Code writes one JSONL record per content block of a single API
// message; every record repeats the same message.id and the same usage. The
// response's tokens must be counted once, not once per record. The same
// fixture carries the two shapes in which a stripped injected block survives
// as an empty text block: beside a tool result, and as a whole prompt.
// Neither is a human prompt.
func TestParserV06CountsStreamedUsageOncePerMessage(t *testing.T) {
	t.Parallel()
	view, metadata := parsedFixture(t, "claude", "claude-streamed-usage.jsonl")
	countIs(t, "input tokens", metadata.Counts.InputTokens, 30)
	countIs(t, "output tokens", metadata.Counts.OutputTokens, 12)
	countIs(t, "cache read tokens", metadata.Counts.CacheReadTokens, 2)
	countIs(t, "cache write tokens", metadata.Counts.CacheWriteTokens, 1)

	countIs(t, "turns", metadata.Counts.Turns, 1)
	// Parser 0.7 counts distinct assistant message ids: the two streamed
	// records of msg_1 are one message (parser 0.6 counted them as two and
	// reported 4).
	countIs(t, "messages", metadata.Counts.Messages, 3)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 1)
	countIs(t, "tool results", metadata.Counts.ToolResults, 1)
	for _, turn := range view.Turns {
		if turn.Role == "user" && turn.RecordIndex != 0 && turn.Kind == TurnKindHumanPrompt {
			t.Errorf("record %d holds no human text but was classified as a prompt: %#v", turn.RecordIndex, turn)
		}
	}
	if len(view.ToolCalls) != 1 || view.ToolCalls[0].ResultRecordIndex == nil || *view.ToolCalls[0].ResultRecordIndex != 3 {
		t.Fatalf("tool linkage = %#v", view.ToolCalls)
	}
}

// A Codex prompt that was only injected instructions is stripped to an empty
// input_text block by the filter and must not count as a prompt either.
func TestParserV06IgnoresCodexInjectedOnlyPrompts(t *testing.T) {
	t.Parallel()
	input := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<user_instructions>\nAGENTS.md: keep it small.\n</user_instructions>"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<user_instructions>\nAGENTS.md: keep it small.\n</user_instructions>\n\nAdd a test."}]}}` + "\n"
	adapter, err := NewAdapter("codex")
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := adapter.FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "codex"}
	bundle, err := NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Turns) != 1 || view.Turns[0].RecordIndex != 1 || view.Turns[0].Kind != TurnKindHumanPrompt || view.Turns[0].Text != "Add a test." {
		t.Fatalf("turns = %#v", view.Turns)
	}
}

// Native text is retained because its structure is unproven, so a bundle
// holding any leaves every structure-derived count unknown, the new token and
// tool-result counts included.
func TestParserV06TextBundlesLeaveCountsUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: adapterVersion, SourceFormat: "claude-jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords: []map[string]any{
			{"type": "user", "message": map[string]any{"role": "user", "content": "please check it"}},
			{"type": "assistant", "message": map[string]any{"id": "msg_1", "role": "assistant", "usage": map[string]any{"input_tokens": float64(3)}, "content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
		NativeText: []TextTranscript{{Format: "text", Content: "an unparsed line"}},
	}
	reference := SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64)}
	metadata, err := BuildMetadata(bundle, "machine", now, now, reference, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	counts := metadata.Counts
	if counts.Turns != nil || counts.Messages != nil || counts.ToolCalls != nil || counts.ToolResults != nil || counts.InputTokens != nil || counts.OutputTokens != nil || counts.CacheReadTokens != nil || counts.CacheWriteTokens != nil {
		t.Fatalf("a text bundle published structure-derived counts: %#v", counts)
	}
	if counts.ExplicitFeedback == nil {
		t.Fatal("explicit feedback is evidence-derived and should stay known")
	}
}
