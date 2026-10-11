package nativecodec

import (
	"bytes"
	"encoding/json"

	"os"
	"path/filepath"

	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// handoffBundle filters a handoff fixture through the harness's adapter, the
// same path the collector and `handoff --source local` take.
func handoffBundle(t *testing.T, harness string) SourceBundle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "archive", "testdata", "handoff", harness+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(harness)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := adapter.FilterJSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: adapter.Name()}
	bundle, err := NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// Each harness renders to a checked-in golden document. Regenerate with
// `go test ./internal/archive -run TestHandoffGolden -update` and review the
// diff.
func TestHandoffGolden(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "cursor"} {
		t.Run(harness, func(t *testing.T) {
			h, err := BuildHandoff(handoffBundle(t, harness), nil, HandoffOptions{Source: "local"})
			if err != nil {
				t.Fatal(err)
			}
			got := RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true})
			golden.Check(t, filepath.Join("..", "..", "archive", "testdata", "handoff", harness+".md"), got)
		})
	}
}

// A session already under the limit is fitted to exactly its golden
// rendering: no step runs and nothing is cut.
func TestHandoffGoldenUnderTheLimitIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude", "codex", "cursor"} {
		h, err := BuildHandoff(handoffBundle(t, harness), nil, HandoffOptions{Source: "local"})
		if err != nil {
			t.Fatal(err)
		}
		render := func(h Handoff) []byte { return RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true}) }
		fit, ok := FitHandoff(h, DefaultHandoffMaxBytes, func(h Handoff) int { return len(render(h)) })
		if !ok || len(fit.Elisions) != 0 {
			t.Fatalf("%s: fits=%v elisions=%v", harness, ok, fit.Elisions)
		}
		got := CutHandoffMarkdown(render(fit), fit, DefaultHandoffMaxBytes)
		want, err := os.ReadFile(filepath.Join("..", "..", "archive", "testdata", "handoff", harness+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: fitted rendering differs from the golden", harness)
		}
	}
}

func TestHandoffClaudeContent(t *testing.T) {
	t.Parallel()
	h, err := BuildHandoff(handoffBundle(t, "claude"), nil, HandoffOptions{Source: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 2 {
		t.Fatalf("exchanges = %d, want 2 (the task notification is not a prompt)", len(h.Exchanges))
	}
	if h.Exchanges[0].Prompt != "Why does the widget test fail?" {
		t.Fatalf("first prompt = %q (injected reminder must be stripped)", h.Exchanges[0].Prompt)
	}
	if strings.Contains(h.Exchanges[1].Prompt, "sk-abcdefghijklmnopqrstuv") || !strings.Contains(h.Exchanges[1].Prompt, "[REDACTED]") {
		t.Fatalf("credential not redacted: %q", h.Exchanges[1].Prompt)
	}
	if h.Workspace != (HandoffWorkspace{Directory: "widgets", Branch: "fix/widget-test"}) {
		t.Fatalf("workspace = %#v", h.Workspace)
	}
	if got := strings.Join(h.FilesTouched, ","); got != "widget/size.go" {
		t.Fatalf("files touched = %q", got)
	}
	if len(h.Plan) != 3 || h.Plan[1].Status != "in_progress" {
		t.Fatalf("plan = %#v", h.Plan)
	}
	if h.LeftOff != "Tests pass now. Next: add a regression test for Size." {
		t.Fatalf("left off = %q", h.LeftOff)
	}
	var bash, edit *HandoffToolCall
	for _, step := range h.Exchanges[0].Steps {
		if step.Tool != nil && step.Tool.Name == "Bash" {
			bash = step.Tool
		}
		if step.Tool != nil && step.Tool.Name == "Edit" {
			edit = step.Tool
		}
	}
	if bash == nil || !bash.IsError || bash.Summary != "go test ./widget/..." || !strings.Contains(bash.Result, "size = 3, want 4") {
		t.Fatalf("bash call = %#v", bash)
	}
	rendered := string(RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true}))
	if edit == nil || strings.Contains(rendered, "len(w.parts)\"") || strings.Contains(rendered, "new_string") {
		t.Fatalf("edit body leaked or edit missing: %#v\n%s", edit, rendered)
	}
}

func TestHandoffCodexContent(t *testing.T) {
	t.Parallel()
	h, err := BuildHandoff(handoffBundle(t, "codex"), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 1 || h.Exchanges[0].Prompt != "Rename Widget.parts to Widget.pieces everywhere." {
		t.Fatalf("exchanges = %#v", h.Exchanges)
	}
	var names []string
	for _, step := range h.Exchanges[0].Steps {
		if step.Tool != nil {
			names = append(names, step.Tool.Name+":"+step.Tool.Summary)
			if step.Tool.Name == "exec" && !strings.Contains(step.Tool.Result, "widget/widget.go:4") {
				t.Fatalf("list-shaped exec output missing: %#v", step.Tool)
			}
		}
	}
	want := "exec:rg -n 'parts' widget|apply_patch:widget/size.go, widget/widget.go|update_plan:updated the plan"
	if got := strings.Join(names, "|"); got != want {
		t.Fatalf("tool steps = %q, want %q", got, want)
	}
	if got := strings.Join(h.FilesTouched, ","); got != "widget/size.go,widget/widget.go" {
		t.Fatalf("files touched = %q", got)
	}
	if len(h.Plan) != 2 || h.Plan[0].Status != "completed" {
		t.Fatalf("plan = %#v", h.Plan)
	}
	if len(h.Session.Models) != 1 || h.Session.Models[0] != "gpt-6-astra" {
		t.Fatalf("models = %#v", h.Session.Models)
	}
}

func TestHandoffCursorContent(t *testing.T) {
	t.Parallel()
	h, err := BuildHandoff(handoffBundle(t, "cursor"), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.ToolResultsUnavailable {
		t.Fatal("Cursor records no tool results; the handoff must say so")
	}
	if len(h.Exchanges) != 1 || h.Exchanges[0].Prompt != "Tighten the intro paragraph of the megafauna post." {
		t.Fatalf("prompt wrapper not removed: %#v", h.Exchanges)
	}
	rendered := string(RenderHandoffMarkdown(h, HandoffRenderOptions{}))
	if strings.Contains(rendered, "Long ago, there were many") {
		t.Fatalf("StrReplace body leaked:\n%s", rendered)
	}
	if strings.Contains(rendered, "picking up work") {
		t.Fatalf("preamble rendered with Preamble false:\n%s", rendered)
	}
}

func TestHandoffJSONRoundTrips(t *testing.T) {
	t.Parallel()
	h, err := BuildHandoff(handoffBundle(t, "codex"), nil, HandoffOptions{Source: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back Handoff
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(RenderHandoffMarkdown(back, HandoffRenderOptions{}), RenderHandoffMarkdown(h, HandoffRenderOptions{})) {
		t.Fatal("JSON round trip changed the rendering")
	}
	if back.Version != HandoffVersion || back.Session.Source != "archive" {
		t.Fatalf("session = %#v", back.Session)
	}
}

// testBundle wraps native records in a filtered-shape bundle for one harness.
func testBundle(harness string, records ...map[string]any) SourceBundle {
	return SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: harness}, AdapterName: harness, AdapterVersion: adapterVersion, SourceFormat: harness + "-jsonl", FilterVersion: FilterVersion, CapturedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)},
		NativeRecords: records,
	}
}

func toolSteps(h Handoff) []*HandoffToolCall {
	var out []*HandoffToolCall
	for _, exchange := range h.Exchanges {
		for _, step := range exchange.Steps {
			if step.Tool != nil {
				out = append(out, step.Tool)
			}
		}
	}
	return out
}

// Calls and results without ids in one record are paired by position, each
// with its own raw item and its own output — not all with the first.
func TestHandoffPairsUnidentifiedCallsInOneRecordByPosition(t *testing.T) {
	t.Parallel()
	bundle := testBundle("cursor",
		map[string]any{"role": "user", "message": map[string]any{"content": "go"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "exec", "input": "ls widget"},
			map[string]any{"type": "tool_use", "name": "exec", "input": "go vet ./widget"},
		}}},
		map[string]any{"role": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "content": "size.go"},
			map[string]any{"type": "tool_result", "content": "vet: clean"},
		}}},
	)
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolSteps(h)
	if len(tools) != 2 || tools[0].Summary != "ls widget" || tools[1].Summary != "go vet ./widget" {
		t.Fatalf("summaries = %#v", tools)
	}
	if tools[0].Result != "size.go" || tools[1].Result != "vet: clean" {
		t.Fatalf("results = %q, %q", tools[0].Result, tools[1].Result)
	}
}

// The directory is where the session started; a later `cd` recorded in cwd
// must not make paths relative to a subdirectory.
func TestHandoffUsesTheStartingDirectory(t *testing.T) {
	t.Parallel()
	record := func(cwd string, content any) map[string]any {
		return map[string]any{"type": "assistant", "cwd": cwd, "message": map[string]any{"role": "assistant", "content": content}}
	}
	bundle := testBundle("claude",
		map[string]any{"type": "user", "cwd": "/repo", "message": map[string]any{"role": "user", "content": "fix it"}},
		record("/repo", []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Edit", "input": map[string]any{"file_path": "/repo/docs/a.md"}}}),
		record("/repo/internal/archive", []any{map[string]any{"type": "tool_use", "id": "t2", "name": "Edit", "input": map[string]any{"file_path": "/repo/internal/archive/views.go"}}}),
	)
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Workspace.Directory != "repo" || strings.Join(h.FilesTouched, ",") != "docs/a.md,internal/archive/views.go" {
		t.Fatalf("workspace = %#v files = %v", h.Workspace, h.FilesTouched)
	}
}

// A plan call whose items cannot be read leaves the earlier plan; an explicit
// empty list clears it.
func TestHandoffPlanSurvivesAnUnreadablePlanCall(t *testing.T) {
	t.Parallel()
	plan := func(id string, input map[string]any) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "TodoWrite", "input": input}}}}
	}
	prompt := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "go"}}
	first := plan("p1", map[string]any{"todos": []any{map[string]any{"content": "Ship it", "status": "pending"}}})
	h, err := BuildHandoff(testBundle("claude", prompt, first, plan("p2", map[string]any{"unknown": true})), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Plan) != 1 || h.Plan[0].Text != "Ship it" {
		t.Fatalf("plan erased: %#v", h.Plan)
	}
	h, err = BuildHandoff(testBundle("claude", prompt, first, plan("p3", map[string]any{"todos": []any{}})), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Plan) != 0 {
		t.Fatalf("explicit empty plan did not clear: %#v", h.Plan)
	}
}

// With no timestamps in the records, times come only from the caller or from
// published metadata — never from the moment the bundle was built.
func TestHandoffDoesNotInventActivityTimes(t *testing.T) {
	t.Parallel()
	bundle := testBundle("cursor", map[string]any{"role": "user", "message": map[string]any{"content": "hello"}})
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Session.StartedAt != nil || h.Session.LastActivityAt != nil {
		t.Fatalf("invented times: %v %v", h.Session.StartedAt, h.Session.LastActivityAt)
	}
	mtime := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	h, _ = BuildHandoff(bundle, nil, HandoffOptions{LastActivityAt: mtime})
	if h.Session.LastActivityAt == nil || !h.Session.LastActivityAt.Equal(mtime) {
		t.Fatalf("caller's time not used: %v", h.Session.LastActivityAt)
	}
	captured := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	h, _ = BuildHandoff(bundle, &Metadata{CapturedAt: captured}, HandoffOptions{LastActivityAt: mtime})
	if !h.Session.LastActivityAt.Equal(captured) {
		t.Fatalf("published capture time not preferred: %v", h.Session.LastActivityAt)
	}
}

func TestCleanPromptRestoresSlashCommands(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"<command-message>review-pr</command-message>\n<command-name>/review-pr</command-name>\n<command-args>12</command-args>": "/review-pr 12",
		"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>":           "/model",
		"<timestamp>Mon</timestamp>\n<user_query>\nhi\n</user_query>":                                                            "hi",
		"please explain <command-name> tags": "please explain <command-name> tags",
	} {
		if got := cleanPrompt(in); got != want {
			t.Errorf("cleanPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Cursor text transcript has role sections instead of records; it still
// renders its exchanges.
func TestHandoffRendersCursorTextTranscripts(t *testing.T) {
	t.Parallel()
	filtered, err := CursorAdapter{}.FilterText(strings.NewReader("user: Tighten the intro.\nassistant: Reading it.\ntool: intro.md: 3 lines\nassistant: Done; the first sentence is shorter.\nuser: thanks\n"), time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 2 || h.Exchanges[0].Prompt != "Tighten the intro." || h.Exchanges[1].Prompt != "thanks" {
		t.Fatalf("exchanges = %#v", h.Exchanges)
	}
	if steps := h.Exchanges[0].Steps; len(steps) != 3 || steps[1].Tool == nil || steps[1].Tool.Result != "intro.md: 3 lines" {
		t.Fatalf("steps = %#v", steps)
	}
	if h.LeftOff != "Done; the first sentence is shorter." {
		t.Fatalf("left off = %q", h.LeftOff)
	}
}

// A compaction summary is shown where it happened: after /compact the agent
// worked from it, not from the turns before it.
func TestHandoffShowsCompactionSummaries(t *testing.T) {
	t.Parallel()
	filtered, err := ClaudeAdapter{}.FilterJSONL(bytes.NewReader(fixture(t, "claude-compaction.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, reg.SessionStartedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var summaries int
	for _, exchange := range h.Exchanges {
		for _, step := range exchange.Steps {
			if step.Kind == HandoffStepSummary && step.Text != "" {
				summaries++
			}
		}
		if strings.Contains(exchange.Prompt, "This session is being continued") {
			t.Fatalf("compaction summary rendered as a prompt: %q", exchange.Prompt)
		}
	}
	if summaries == 0 {
		t.Fatalf("no compaction summary in the handoff: %#v", h.Exchanges)
	}
	if !strings.Contains(string(RenderHandoffMarkdown(h, HandoffRenderOptions{})), "**Conversation compacted.**") {
		t.Fatal("summary not rendered")
	}
}

// Calls a record holds under different keys come out in one fixed order,
// the keys' sorted order, whatever order the map yields them in.
//
// Regression: review of #44, 2026-09 (1b0135e).
func TestToolCallsUnderSiblingKeysComeOutInKeyOrder(t *testing.T) {
	t.Parallel()
	record := map[string]any{
		"type": "wrapper",
		"zeta": map[string]any{"type": "tool_use", "id": "z", "name": "Write"},
		"alfa": map[string]any{"type": "tool_use", "id": "a", "name": "Read"},
		"mike": []any{map[string]any{"type": "function_call", "call_id": "m1", "name": "Bash"}, map[string]any{"type": "function_call", "call_id": "m2", "name": "Grep"}},
	}
	for range 50 {
		calls, _, _ := toolActivity(record, 0, "", "")
		var ids []string
		for _, call := range calls {
			ids = append(ids, call.Call.CallID)
		}
		if got := strings.Join(ids, ","); got != "a,m1,m2,z" {
			t.Fatalf("call order %s, want a,m1,m2,z", got)
		}
	}
}

// Claude Code labels messages it synthesizes itself "<synthetic>". That is
// not a model: metadata and the handoff name only real models, and the
// synthetic message's usage is not counted.
//
// Regression: review of #44, 2026-09 (1b0135e).
func TestSyntheticModelIsNotAModel(t *testing.T) {
	t.Parallel()
	filtered, err := ClaudeAdapter{}.FilterJSONL(bytes.NewReader(fixture(t, "claude-synthetic-model.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "claude", ClaudeAdapter{}, filtered)
	m := parserTestMetadata(t, bundle)
	if len(m.Models) != 1 || m.Models[0].Attributes["gen_ai.response.model"] != "claude-opus-5" {
		t.Fatalf("models = %+v", m.Models)
	}
	if m.Counts.InputTokens == nil || *m.Counts.InputTokens != 120 || *m.Counts.OutputTokens != 30 {
		t.Fatalf("tokens = %v/%v, want only the real message's 120/30", m.Counts.InputTokens, m.Counts.OutputTokens)
	}
	h, err := BuildHandoff(bundle, &m, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.Session.Models, ",") != "claude-opus-5" {
		t.Fatalf("handoff models = %v", h.Session.Models)
	}
}
