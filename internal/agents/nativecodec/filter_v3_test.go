package nativecodec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// decodeRecords turns a filtered transcript back into decoded objects so a
// test can assert on exact retained keys rather than on substrings.
func decodeRecords(t *testing.T, filtered FilteredTranscript) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(filtered.Records))
	for index, raw := range filtered.Records {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("record %d: %v", index, err)
		}
		out = append(out, record)
	}
	return out
}

func child(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()
	current, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value is not an object: %#v", value)
	}
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("missing object at %q in %#v", key, current)
		}
		current = next
	}
	return current
}

// contentBlock returns the first block of the given type in a message's
// content array.
func contentBlock(t *testing.T, message map[string]any, blockType string) map[string]any {
	t.Helper()
	blocks, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %#v", message["content"])
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if ok && block["type"] == blockType {
			return block
		}
	}
	t.Fatalf("no %q block in %#v", blockType, blocks)
	return nil
}

func gapDetail(gaps []CaptureGap, code string) string {
	for _, gap := range gaps {
		if gap.Code == code {
			return gap.Detail
		}
	}
	return ""
}

// Filter 2 applied the key allowlist recursively inside tool arguments, so an
// Edit's old_string/new_string were dropped and a tool result could not be
// joined to the call it answered. Filter 3 keeps the whole argument subtree
// and the linkage keys, without relaxing value sanitization.
func TestFilterV3RetainsClaudeToolArgumentsAndResultLinkage(t *testing.T) {
	t.Parallel()
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-tool-evidence.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	if len(records) != 3 {
		t.Fatalf("retained %d records, want 3", len(records))
	}

	assistant := child(t, records[1], "message")
	for _, key := range []string{"sessionId", "requestId", "gitBranch"} {
		if _, kept := records[1][key]; !kept {
			t.Errorf("assistant record lost %q: %#v", key, records[1])
		}
	}
	if assistant["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %#v", assistant["stop_reason"])
	}
	usage := child(t, assistant["usage"])
	for key, want := range map[string]float64{"input_tokens": 1200, "output_tokens": 95, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 40} {
		if usage[key] != want {
			t.Errorf("usage[%q] = %#v, want %v", key, usage[key], want)
		}
	}
	if _, kept := usage["service_tier"]; kept {
		t.Errorf("non-numeric value retained in the usage subtree: %#v", usage)
	}

	call := contentBlock(t, assistant, "tool_use")
	if call["id"] != "toolu_synthetic_1" || call["name"] != "Edit" {
		t.Errorf("tool call identity = %#v", call)
	}
	input := child(t, call["input"])
	for _, key := range []string{"file_path", "old_string", "new_string", "replace_all"} {
		if _, kept := input[key]; !kept {
			t.Errorf("tool argument %q was dropped: %#v", key, input)
		}
	}
	newString, _ := input["new_string"].(string)
	if !strings.Contains(newString, "func newHelper() error {") {
		t.Errorf("edit body lost: %q", newString)
	}
	// A secret inside a retained tool argument is still redacted.
	if strings.Contains(newString, "sk-synthetic") || !strings.Contains(newString, "[REDACTED]") {
		t.Errorf("secret survived inside new_string: %q", newString)
	}

	result := contentBlock(t, child(t, records[2], "message"), "tool_result")
	if result["tool_use_id"] != call["id"] {
		t.Errorf("tool result cannot be joined to its call: %#v", result)
	}
	if result["is_error"] != false {
		t.Errorf("is_error = %#v", result["is_error"])
	}
	if _, kept := records[2]["toolUseResult"]; kept {
		t.Errorf("toolUseResult duplicate content was retained: %#v", records[2])
	}

	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, forbidden := range []string{"hidden private reasoning", "sig-synthetic", "system-reminder", "always run the linter", "userModified"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("filtered source contains %q", forbidden)
		}
	}
	if prompt, _ := child(t, records[0], "message")["content"].(string); !strings.Contains(prompt, "Please rename the helper") || strings.Contains(prompt, "linter") {
		t.Errorf("user prompt = %q", prompt)
	}
	if !hasGap(filtered.Gaps, "hidden_instruction_omitted") || !hasGap(filtered.Gaps, "sensitive_content_redacted") {
		t.Errorf("gaps = %#v", filtered.Gaps)
	}
	detail := gapDetail(filtered.Gaps, "unknown_field_omitted")
	for _, name := range []string{"toolUseResult", "service_tier"} {
		if !strings.Contains(detail, name) {
			t.Errorf("omitted key %q missing from gap detail %q", name, detail)
		}
	}

	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	if _, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("filtered tool evidence did not round-trip into a bundle: %v", err)
	}
}

// Retaining argument names must not retain a blocked key, an unbounded value,
// or a credential: the tool-argument subtree only widens the key allowlist.
func TestFilterV3ToolArgumentSubtreeKeepsValuePolicy(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 70*1024)
	input := `{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":` +
		`{"file_path":"/w/config.go","old_string":"","new_string":"const key = \"sk-synthetic-not-a-real-key-value\"","password":"hunter2-synthetic","note":"` + long + `"}}]}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	arguments := child(t, contentBlock(t, child(t, records[0], "message"), "tool_use")["input"])
	if _, kept := arguments["password"]; kept {
		t.Errorf("blocked key retained inside tool arguments: %#v", arguments)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "hunter2-synthetic") || strings.Contains(joined, "sk-synthetic") {
		t.Errorf("credential survived inside tool arguments")
	}
	if note, _ := arguments["note"].(string); len(note) != 64*1024 {
		t.Errorf("string cap not applied inside tool arguments: %d bytes", len(note))
	}
	for _, code := range []string{"sensitive_or_hidden_field_omitted", "sensitive_content_redacted", "content_truncated"} {
		if !hasGap(filtered.Gaps, code) {
			t.Errorf("missing gap %q in %#v", code, filtered.Gaps)
		}
	}
}

func TestFilterV3RetainsCodexToolCallsAndTokenUsage(t *testing.T) {
	t.Parallel()
	filtered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-tool-and-usage.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	if len(records) != 4 {
		t.Fatalf("retained %d records, want 4", len(records))
	}

	prompt, _ := contentBlock(t, child(t, records[0], "payload"), "input_text")["text"].(string)
	if !strings.Contains(prompt, "Add a test for the widget parser.") {
		t.Errorf("user text lost: %q", prompt)
	}
	for _, forbidden := range []string{"user_instructions", "AGENTS.md", "environment_context", "zsh"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("injected instruction text %q survived: %q", forbidden, prompt)
		}
	}

	call := child(t, records[1], "payload")
	if call["type"] != "custom_tool_call" || call["call_id"] != "call_synthetic_1" || call["status"] != "completed" {
		t.Errorf("custom_tool_call = %#v", call)
	}
	if arguments, _ := call["input"].(string); !strings.Contains(arguments, "go") {
		t.Errorf("custom_tool_call input = %#v", call["input"])
	}
	output := child(t, records[2], "payload")
	if output["call_id"] != "call_synthetic_1" || !strings.Contains(fmt.Sprint(output["output"]), "0.21s") {
		t.Errorf("custom_tool_call_output = %#v", output)
	}

	usage := child(t, records[3], "payload")
	if usage["last_agent_message"] != "I added the parser test." || usage["thread_id"] != "thread_synthetic_1" || usage["root_turn_id"] != "turn_synthetic_1" {
		t.Errorf("token usage identity = %#v", usage)
	}
	if usage["started_at_ms"] == nil || usage["completed_at_ms"] == nil {
		t.Errorf("token usage timestamps dropped: %#v", usage)
	}
	total := child(t, usage, "info", "total_token_usage")
	if total["input_tokens"] != float64(4100) || total["total_tokens"] != float64(4430) {
		t.Errorf("total token usage = %#v", total)
	}
	if _, kept := child(t, usage, "info")["model_context_window"]; kept {
		t.Errorf("unallowed sibling retained inside info: %#v", usage["info"])
	}
	for _, key := range []string{"turn_token_usage", "thread_token_usage"} {
		if child(t, usage, key)["output_tokens"] == nil {
			t.Errorf("%s dropped: %#v", key, usage[key])
		}
	}
	if !strings.Contains(gapDetail(filtered.Gaps, "unknown_field_omitted"), "model_context_window") {
		t.Errorf("omitted key names = %q", gapDetail(filtered.Gaps, "unknown_field_omitted"))
	}
	if !hasGap(filtered.Gaps, "hidden_instruction_omitted") {
		t.Errorf("stripped instruction blocks were not reported: %#v", filtered.Gaps)
	}
}

func TestFilterV3RetainsCursorNestedMessagesAndTurnEnded(t *testing.T) {
	t.Parallel()
	filtered, err := (CursorAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "cursor-turn.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	if len(records) != 3 {
		t.Fatalf("retained %d records, want 3", len(records))
	}
	if text, _ := contentBlock(t, child(t, records[0], "message"), "text")["text"].(string); text != "Summarize the failing widget test." {
		t.Errorf("cursor user text = %q", text)
	}
	arguments := child(t, contentBlock(t, child(t, records[1], "message"), "tool_use")["input"])
	if arguments["path"] != "internal/widget/widget_test.go" || arguments["limit"] != float64(120) {
		t.Errorf("cursor tool arguments = %#v", arguments)
	}
	if records[2]["type"] != "turn_ended" || records[2]["status"] != "completed" {
		t.Errorf("turn_ended record = %#v", records[2])
	}
}

// The omitted key names are reported once for the whole transcript, sorted and
// capped, so a reader can see what this filter version could not keep without
// the gap list growing with the transcript.
func TestFilterV3ReportsOmittedKeyNamesOnceAndCapsThem(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	builder.WriteString(`{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":"visible"}`)
	for i := range maxOmittedKeyNames + 20 {
		fmt.Fprintf(&builder, `,"unknown_%03d":"value-%03d"`, i, i)
	}
	builder.WriteString("}")
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(builder.String()))
	if err != nil {
		t.Fatal(err)
	}
	omissions := 0
	for _, gap := range filtered.Gaps {
		if gap.Code == "unknown_field_omitted" {
			omissions++
		}
	}
	if omissions != 1 {
		t.Fatalf("expected exactly one omitted-key gap, got %d: %#v", omissions, filtered.Gaps)
	}
	detail := gapDetail(filtered.Gaps, "unknown_field_omitted")
	names := strings.Split(strings.TrimPrefix(strings.TrimSuffix(detail, "; further key names omitted"), "omitted keys: "), ", ")
	if len(names) != maxOmittedKeyNames || names[0] != "unknown_000" {
		t.Fatalf("reported %d key names: %q", len(names), detail)
	}
	if !strings.HasSuffix(detail, "; further key names omitted") {
		t.Errorf("cap was not reported: %q", detail)
	}
	if strings.Contains(detail, "value-000") {
		t.Errorf("gap detail leaked a value: %q", detail)
	}
}

// A truncated or still-streaming record can carry an instruction block whose
// closing tag has not been written yet. Everything from the opening tag on is
// dropped rather than partly retained.
func TestFilterV3DropsUnterminatedInstructionBlocks(t *testing.T) {
	t.Parallel()
	input := `{"type":"user","uuid":"u","timestamp":"2026-09-20T10:00:00Z","message":{"role":"user","content":"Fix the parser.\n<system-reminder>\nmemory: never"}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	text, _ := child(t, records[0], "message")["content"].(string)
	if text != "Fix the parser." {
		t.Fatalf("unterminated instruction block was not dropped: %q", text)
	}
	if !hasGap(filtered.Gaps, "hidden_instruction_omitted") {
		t.Errorf("gaps = %#v", filtered.Gaps)
	}
}

// A message that is nothing but an injected instruction block leaves no
// content behind, and the record is recorded as omitted rather than retained
// as an empty message.
func TestFilterV3DropsMessagesThatAreOnlyInjectedInstructions(t *testing.T) {
	t.Parallel()
	input := `{"type":"user","uuid":"u","timestamp":"2026-09-20T10:00:00Z","message":{"role":"user","content":"<system-reminder>only memory</system-reminder>"}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "only memory") || strings.Contains(joined, "system-reminder") {
		t.Fatalf("injected instructions retained: %s", joined)
	}
	if !hasGap(filtered.Gaps, "hidden_instruction_omitted") {
		t.Errorf("gaps = %#v", filtered.Gaps)
	}
}

// The filter and adapter versions are part of the published provenance: a
// bundle written by this build must declare the build's own filter and adapter
// versions so a reader can tell which policy produced it. (Filter 13 and
// adapter 0.13.0 superseded the filter-3 to filter-12 values this test pinned
// before; filter 15 and adapter 0.15.0 superseded filter 14; filter 26 and
// adapter 0.26.0 superseded filter 24.)
func TestFilterVersionIsDeclaredInCaptureProvenance(t *testing.T) {
	t.Parallel()
	if FilterVersion != "26" || adapterVersion != "0.26.0" {
		t.Fatalf("FilterVersion=%q adapterVersion=%q", FilterVersion, adapterVersion)
	}
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-tool-evidence.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Capture.FilterVersion != FilterVersion || bundle.Capture.AdapterVersion != adapterVersion {
		t.Fatalf("capture = %#v", bundle.Capture)
	}
}

// A numbers-only subtree admits objects and arrays so the recursion can prune
// them; prose reaching it through either must still be omitted.
func TestFilterV3NumericSubtreeOmitsNonNumericValues(t *testing.T) {
	t.Parallel()
	input := `{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":"ok","usage":{"input_tokens":12,"service_tier":"standard","notes":["free text","more free text"],"breakdown":{"label":"free text","cached":7}}}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "free text") || strings.Contains(joined, "standard") {
		t.Fatalf("prose survived in a numbers-only subtree: %s", joined)
	}
	usage := child(t, decodeRecords(t, filtered)[0], "message", "usage")
	if usage["input_tokens"] != float64(12) || child(t, usage, "breakdown")["cached"] != float64(7) {
		t.Fatalf("numbers were not retained: %#v", usage)
	}
}

// Tool arguments are retained wholesale except for two deny rules: the text a
// typing or form-submitting tool sends outward, and any argument whose name
// looks like a credential. The key name goes into a gap; the value never does.
//
// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3DeniesTypedInputAndCredentialArguments(t *testing.T) {
	t.Parallel()
	record := func(name string, input string) string {
		return `{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"` + name + `","input":` + input + `}]}}`
	}
	lines := []string{
		record("computer", `{"action":"type","text":"typed-secret-1","coordinate":[10,20]}`),
		record("form_input", `{"ref":"ref_3","value":"typed-secret-2"}`),
		record("mcp__claude-in-chrome__form_input", `{"ref":"ref_4","values":["typed-secret-3"]}`),
		record("mcp__Claude_Browser__computer", `{"action":"key","text":"typed-secret-4"}`),
		record("browser_fill", `{"selector":"#pw","value":"typed-secret-5","nested":{"value":"typed-secret-6"}}`),
		record("Edit", `{"file_path":"/w/a.go","old_string":"x","new_string":"kept-edit-body","text":"kept-edit-text","value":"kept-edit-value"}`),
		record("mcp__github__create_issue", `{"title":"kept-title","github_token":"typed-secret-7","auth":{"CredentialsFile":"typed-secret-8","AuthorizationHeader":"typed-secret-9"},"max_tokens":512}`),
	}
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	for i := 1; i <= 9; i++ {
		if strings.Contains(joined, "typed-secret-"+string(rune('0'+i))) {
			t.Errorf("typed or credential argument %d survived", i)
		}
	}
	for _, kept := range []string{"kept-edit-body", "kept-edit-text", "kept-edit-value", "kept-title", `"ref":"ref_3"`, `"selector":"#pw"`, `"action":"type"`} {
		if !strings.Contains(joined, kept) {
			t.Errorf("ordinary argument %q was dropped", kept)
		}
	}
	records := decodeRecords(t, filtered)
	if len(records) != len(lines) {
		t.Fatalf("retained %d records, want %d", len(records), len(lines))
	}
	edit := child(t, contentBlock(t, child(t, records[5], "message"), "tool_use")["input"])
	for _, key := range []string{"text", "value", "new_string"} {
		if _, kept := edit[key]; !kept {
			t.Errorf("Edit is not a typing tool; its %q argument must stay", key)
		}
	}
	issue := child(t, contentBlock(t, child(t, records[6], "message"), "tool_use")["input"])
	// Filter 11 matches argument names by the shared credential vocabulary
	// word by word: max_tokens (a plural) is a budget and stays, and auth is
	// a credential name, dropped whole.
	if _, kept := issue["max_tokens"]; !kept {
		t.Errorf("max_tokens is not a credential name: %#v", issue)
	}
	if _, kept := issue["auth"]; kept {
		t.Errorf("auth is a credential argument: %#v", issue)
	}

	var details []string
	for _, gap := range filtered.Gaps {
		if gap.Code == "sensitive_or_hidden_field_omitted" && strings.HasPrefix(gap.Detail, deniedToolArgumentIntro) {
			details = append(details, gap.Detail)
		}
	}
	if len(details) != 1 {
		t.Fatalf("want exactly one denied-argument gap, got %d in %#v", len(details), filtered.Gaps)
	}
	if details[0] != deniedToolArgumentIntro+"auth, github_token, text, value, values" {
		t.Errorf("denied-argument gap detail = %q", details[0])
	}
	if strings.Contains(details[0], "typed-secret") {
		t.Errorf("gap detail leaked a value: %q", details[0])
	}
}

// Cursor carries the tool name in tool_name beside tool_input; the deny list
// must read that spelling too.
//
// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3DeniesTypedInputForCursorToolCalls(t *testing.T) {
	t.Parallel()
	input := `{"type":"tool_call","timestamp":"2026-09-20T10:00:00Z","tool_name":"computer","tool_input":{"action":"type","text":"typed-secret"}}`
	filtered, err := (CursorAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "typed-secret") || !strings.Contains(joined, `"action":"type"`) {
		t.Errorf("cursor typed input handling wrong: %s", joined)
	}
}

// Supplemental evidence has no per-transcript name collector; a denied
// argument there falls back to the content-free gap and is still dropped.
//
// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3DeniedArgumentsInSupplementalEvidenceFallBackToPlainGap(t *testing.T) {
	t.Parallel()
	filtered, gaps, err := FilterSupplementalEvidence([]SupplementalEvidence{{
		//lint:ignore LV1001 deliberately a kind with no declared constant
		Kind: "hook_event", ObservedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), Provenance: "test",
		Payload: map[string]any{"tool_name": "form_input", "tool_input": map[string]any{"ref": "r", "value": "typed-secret"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(filtered)
	if strings.Contains(string(encoded), "typed-secret") {
		t.Fatalf("typed value retained in supplemental evidence: %s", encoded)
	}
	if !hasGap(gaps, "sensitive_or_hidden_field_omitted") {
		t.Errorf("gaps = %#v", gaps)
	}
}

// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3RedactsCredentialShapes(t *testing.T) {
	t.Parallel()
	positive := map[string]string{
		"pem":                      "config:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7synthetic\nline2\n-----END RSA PRIVATE KEY-----\ndone",
		"pem openssh unterminated": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAAsynthetic",
		"jwt":                      "header: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.c3ludGhldGljLXNpZ25hdHVyZQ end",
		"url userinfo":             "git remote add origin https://x-access-token:ghs_synthetic@github.com/o/r.git",
		"url bare user":            "curl ftp://deploy@files.example.test/x",
		"github classic":           "export GH=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github fine grained":      "github_pat_11ABCDEFG0123456789_synthetic_value_here",
		"slack bot":                "xoxb-1234567890-synthetic-value",
		"slack user":               "xoxp-1234567890-synthetic-value",
	}
	for name, in := range positive {
		out, hit := redactSensitive(in)
		if !hit || !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s: not redacted: %q", name, out)
		}
		for _, leaked := range []string{"MIIEpAIBAAKCAQEA7synthetic", "line2", "b3BlbnNzaC1rZXktdjEAAAAAsynthetic", "eyJhbGciOiJIUzI1NiJ9", "c3ludGhldGljLXNpZ25hdHVyZQ", "x-access-token:ghs_synthetic", "deploy@", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github_pat_11ABCDEFG", "xoxb-1234567890", "xoxp-1234567890"} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s: %q survived in %q", name, leaked, out)
			}
		}
	}
	// The PEM block's END line is consumed with it; text after it survives.
	if out, _ := redactSensitive(positive["pem"]); !strings.HasPrefix(out, "config:\n[REDACTED]\ndone") {
		t.Errorf("pem redaction shape: %q", out)
	}
	// Only the userinfo of a URL is replaced; scheme and host stay legible.
	if out, _ := redactSensitive(positive["url userinfo"]); !strings.Contains(out, "https://[REDACTED]@github.com/o/r.git") {
		t.Errorf("url userinfo redaction shape: %q", out)
	}
	// The same shapes are redacted when they sit inside a retained tool argument.
	record := `{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Write","input":{"file_path":"/w/k","content":"` + strings.ReplaceAll(positive["pem"], "\n", `\n`) + `"}}]}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(record))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "MIIEpAIBAAKCAQEA7synthetic") || !hasGap(filtered.Gaps, "sensitive_content_redacted") {
		t.Errorf("pem survived inside a tool argument: %s %#v", joined, filtered.Gaps)
	}
}

// Ordinary code and prose must pass through the structural patterns. The one
// known false-positive class is the pre-existing assignment pattern, which
// this test pins so a change to it is deliberate.
//
// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3DoesNotRedactOrdinaryCode(t *testing.T) {
	t.Parallel()
	negative := []string{
		"tokens := lexer.Tokenize(src)",
		"func parseToken(t Token) error { return nil }",
		"if token.Kind == KindIdent { next() }",
		"see https://example.test/docs/path?x=1#frag and ssh://host/repo",
		"-----BEGIN CERTIFICATE-----\nMIIBsynthetic\n-----END CERTIFICATE-----",
		"eyJ is how a base64 JSON object starts; eyJhbGci.short.x is not a token",
		"ghp_short and xoxb- alone and github_pat_ alone",
		"password_policy = strong",
		"the secretary filed the report",
	}
	for _, in := range negative {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("ordinary text was redacted: %q -> %q", in, out)
		}
	}
	// Known false positive: the assignment pattern does not know the value is
	// code. Since filter 9 only the value is replaced.
	if out, hit := redactSensitive("token = parse(x)"); !hit || out != "token = [REDACTED]" {
		t.Errorf("assignment pattern behavior changed: %q (hit=%v)", out, hit)
	}
}

// Two scans of identical input must produce byte-identical records and gaps,
// otherwise a repeated background scan would look like new evidence and mint
// a new source snapshot.
//
// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3OutputIsDeterministicAcrossScans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		adapter Adapter
		fixture string
	}{
		{"claude", ClaudeAdapter{}, "claude-tool-evidence.jsonl"},
		{"codex", CodexAdapter{}, "codex-tool-and-usage.jsonl"},
		{"cursor", CursorAdapter{}, "cursor-turn.jsonl"},
	} {
		first, err := tc.adapter.FilterJSONL(bytes.NewReader(fixture(t, tc.fixture)))
		if err != nil {
			t.Fatal(err)
		}
		second, err := tc.adapter.FilterJSONL(bytes.NewReader(fixture(t, tc.fixture)))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(second)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: repeated scan differs:\n%s\n%s", tc.name, a, b)
		}
	}
}
