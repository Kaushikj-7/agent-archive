package archive

import (
	"encoding/json"
	"fmt"

	"slices"
	"strconv"
	"strings"
	"testing"
)

func markdownSize(h Handoff) int {
	return len(RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true}))
}

// bigHandoff builds n exchanges, each with a long prompt, long assistant
// text, and three tool calls with long results.
func bigHandoff(n int) Handoff {
	h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "claude"}, LeftOff: "final words"}
	for i := range n {
		exchange := HandoffExchange{Prompt: fmt.Sprintf("prompt %d ", i) + strings.Repeat("p", 3000)}
		exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepText, Text: strings.Repeat("a", 2000)})
		for range 3 {
			exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Bash", Summary: "go test", Result: strings.Repeat("r", 1500), ResultLines: 1, ResultBytes: 1500}})
		}
		h.Exchanges = append(h.Exchanges, exchange)
	}
	return h
}

func TestFitHandoffAppliesStepsInOrderAndKeepsRecentExchanges(t *testing.T) {
	t.Parallel()
	h := bigHandoff(40)
	full := markdownSize(h)
	fit, ok := FitHandoff(h, 200_000, markdownSize)
	if !ok || markdownSize(fit) > 200_000 {
		t.Fatalf("fit = %v, size %d of %d", ok, markdownSize(fit), full)
	}
	if len(fit.Exchanges) != 40 {
		t.Fatalf("exchanges dropped: %d", len(fit.Exchanges))
	}
	for i, exchange := range fit.Exchanges {
		if !strings.HasPrefix(exchange.Prompt, fmt.Sprintf("prompt %d ", i)) {
			t.Fatalf("prompt %d lost", i)
		}
	}
	for _, exchange := range fit.Exchanges[len(fit.Exchanges)-handoffKeptExchanges:] {
		for _, step := range exchange.Steps {
			if step.Tool != nil && (step.Tool.Result == "" || step.Tool.ResultOmitted) {
				t.Fatal("a recent exchange lost its tool output")
			}
			if step.TextTruncated {
				t.Fatal("a recent exchange lost its assistant text")
			}
		}
	}
	if len(fit.Elisions) == 0 || fit.Elisions[0].Kind != HandoffElisionToolOutput || fit.Elisions[0].First != 1 {
		t.Fatalf("elisions = %#v", fit.Elisions)
	}
	// FitHandoff must not modify its input.
	if markdownSize(h) != full || h.Exchanges[0].Steps[1].Tool.Result == "" {
		t.Fatal("FitHandoff mutated its input")
	}
}

func TestFitHandoffRunsEveryStepUnderPressure(t *testing.T) {
	t.Parallel()
	// 40 prompts capped at handoffPromptCap still fit, so the steps after
	// prompt truncation are not reached.
	fit, _ := FitHandoff(bigHandoff(40), 130_000, markdownSize)
	var kinds []HandoffElisionKind
	for _, e := range fit.Elisions {
		kinds = append(kinds, e.Kind)
	}
	want := []HandoffElisionKind{HandoffElisionToolOutput, HandoffElisionToolCalls, HandoffElisionAssistantText, HandoffElisionPromptText}
	if !slices.Equal(kinds, want) {
		t.Fatalf("elision order = %v, want %v", kinds, want)
	}
	if steps := fit.Exchanges[0].Steps; len(steps) != 2 || steps[1].Kind != HandoffStepCollapsed || steps[1].Text != "3 tool calls: `Bash` ×3" {
		t.Fatalf("collapsed = %#v", steps)
	}
	for _, exchange := range fit.Exchanges {
		if exchange.Prompt == "" {
			t.Fatal("a prompt was dropped")
		}
	}
	if fit.LeftOff != "final words" {
		t.Fatal("left off was trimmed")
	}
}

func TestFitHandoffReportsWhenItCannotFit(t *testing.T) {
	t.Parallel()
	fit, ok := FitHandoff(bigHandoff(5), 1_000, markdownSize)
	if ok {
		t.Fatalf("fit reported success at %d bytes", markdownSize(fit))
	}
	if len(fit.Exchanges) != 5 {
		t.Fatal("exchanges dropped")
	}
}

func TestFitHandoffWithinBudgetIsUnchanged(t *testing.T) {
	t.Parallel()
	h := bigHandoff(2)
	fit, ok := FitHandoff(h, 0, markdownSize)
	if !ok || len(fit.Elisions) != 0 || markdownSize(fit) != markdownSize(h) {
		t.Fatal("no budget must mean no trimming")
	}
	fit, ok = FitHandoff(h, 1<<30, markdownSize)
	if !ok || len(fit.Elisions) != 0 {
		t.Fatal("a generous budget must mean no trimming")
	}
}

// The footer names the saved full record only when something was trimmed.
func TestHandoffFooterNamesFullRecordOnlyWhenTrimmed(t *testing.T) {
	t.Parallel()
	h := bigHandoff(10)
	h.FullRecordPath = "/data/handoffs/x.md"
	if strings.Contains(string(RenderHandoffMarkdown(h, HandoffRenderOptions{})), "Full record") {
		t.Fatal("full record named on an untrimmed handoff")
	}
	fit, _ := FitHandoff(h, 20_000, markdownSize)
	if !strings.Contains(string(RenderHandoffMarkdown(fit, HandoffRenderOptions{})), "Full record: /data/handoffs/x.md") {
		t.Fatal("trimmed handoff does not name its full record")
	}
}

func TestTrimResultKeepsHeadAndTail(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	got := trimResult(strings.Join(lines, "\n"), 4, 10_000)
	want := "line 1\nline 2\n… 46 lines omitted …\nline 49\nline 50"
	if got != want {
		t.Fatalf("got %q", got)
	}
	long := trimResult(strings.Repeat("é", 3000), 12, 100)
	if !strings.Contains(long, "bytes omitted") || !json.Valid([]byte(fmt.Sprintf("%q", long))) || len(long) > 200 {
		t.Fatalf("byte cap: %d bytes %q", len(long), long)
	}
	for _, r := range long {
		if r == '\uFFFD' {
			t.Fatal("a character was split")
		}
	}
}

func TestCodeFenceOutrunsBackticksInContent(t *testing.T) {
	t.Parallel()
	if got := codeFence("plain"); got != "```" {
		t.Fatalf("got %q", got)
	}
	if got := codeFence("has ```` inside"); got != "`````" {
		t.Fatalf("got %q", got)
	}
}

// One long autonomous exchange is still trimmed: only its last
// handoffKeptSteps steps are protected.
func TestFitHandoffTrimsASingleLongExchange(t *testing.T) {
	t.Parallel()
	h := bigHandoff(1)
	for i := range 60 {
		h.Exchanges[0].Steps = append(h.Exchanges[0].Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Read", Summary: strconv.Itoa(i), Result: strings.Repeat("r", 1500)}})
	}
	fit, ok := FitHandoff(h, 40_000, markdownSize)
	if !ok {
		t.Fatalf("a single exchange could not be fitted: %d bytes", markdownSize(fit))
	}
	steps := fit.Exchanges[0].Steps
	tail := steps[len(steps)-handoffKeptSteps:]
	for _, step := range tail {
		if step.Tool == nil || step.Tool.Result == "" {
			t.Fatalf("a protected step was trimmed: %#v", step)
		}
	}
	if steps[1].Tool == nil || !steps[1].Tool.ResultOmitted {
		t.Fatalf("an early step kept its output: %#v", steps[1])
	}
}

func TestProtectedStart(t *testing.T) {
	t.Parallel()
	ex := func(n int) HandoffExchange { return HandoffExchange{Steps: make([]HandoffStep, n)} }
	got := protectedStart([]HandoffExchange{ex(5), ex(5), ex(4), ex(30), ex(2)}, 3, 20)
	want := []int{5, 5, 4, 12, 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Fitting a large session takes a handful of measurements per step, not one
// per exchange.
func TestFitHandoffMeasuresLogarithmically(t *testing.T) {
	t.Parallel()
	h := bigHandoff(512)
	calls := 0
	fit, ok := FitHandoff(h, 2_000_000, func(x Handoff) int { calls++; return markdownSize(x) })
	if !ok {
		t.Fatalf("did not fit: %d", markdownSize(fit))
	}
	if calls > 4*(2+10)+1 {
		t.Fatalf("%d measurements for 512 exchanges", calls)
	}
	// Trimming stops partway through the conversation rather than applying
	// the last step to every exchange.
	e := fit.Elisions[len(fit.Elisions)-1]
	if e.First != 1 || e.Last >= 512-handoffKeptExchanges {
		t.Fatalf("elision = %#v", e)
	}
}
