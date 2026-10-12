package archive

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// longCodexHandoff is shaped like the three-day Codex session that rendered
// to 215 KB after the first four budget steps: 35 exchanges, 717 agent
// messages (many short "still in progress" notes, ~30 in the last
// exchange), 1,903 tool calls with JSON output, four prompts over
// handoffPromptCap, and a plan and file list.
func longCodexHandoff() Handoff {
	h := Handoff{
		Version:        HandoffVersion,
		Session:        HandoffSession{Harness: "codex", ArchiveSessionID: "97636671", Source: "archive"},
		Workspace:      HandoffWorkspace{Directory: "agent-archive", Branch: "main"},
		LeftOff:        "Merged the last PR; CI is green. " + strings.Repeat("Next: verify the release notes. ", 40),
		FullRecordPath: "/data/handoffs/97636671.md",
	}
	for i := range 12 {
		h.Plan = append(h.Plan, HandoffPlanItem{Text: fmt.Sprintf("step %d of the plan", i), Status: "completed"})
	}
	for i := range 80 {
		h.FilesTouched = append(h.FilesTouched, fmt.Sprintf("internal/pkg%d/file%d.go", i%9, i))
	}
	for i := range 35 {
		prompt := fmt.Sprintf("prompt %d: continue with the next PR", i)
		if i%9 == 0 {
			prompt += "\n" + strings.Repeat("Details — with é and 字 to cross character boundaries. ", 120)
		}
		exchange := HandoffExchange{Prompt: prompt}
		n := 20
		if i == 34 {
			n = 37
		}
		for j := range n {
			text := fmt.Sprintf("Exchange %d message %d: the check is still in progress; waiting on CI. "+strings.Repeat("Polling gh pr checks again. ", 6), i, j)
			if j%5 == 0 {
				text += " " + strings.Repeat("Longer reasoning about the change. ", 20)
			}
			exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepText, Text: text})
			calls := 3
			if j%3 == 2 {
				calls = 2
			}
			for k := range calls {
				// Backtick runs in the output make its fence longer.
				json := fmt.Sprintf(`{"statusCheckRollup":[%s],"agents":"list_agents %d"}`, strings.Repeat("{\"name\":\"ci\",\"conclusion\":\"SUCCESS\",\"detail\":\"````x\"},\n", 30), k)
				result := trimResult(json, 12, 2000)
				exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{
					Name: []string{"exec_command", "gh", "list_agents"}[k], Summary: fmt.Sprintf("gh pr view %d --json statusCheckRollup", j),
					Result: result, ResultLines: lineCount(json), ResultBytes: len(json),
				}})
			}
		}
		h.Exchanges = append(h.Exchanges, exchange)
	}
	return h
}

func TestFitHandoffBoundsALongCodexSession(t *testing.T) {
	t.Parallel()
	h := longCodexHandoff()
	full := markdownSize(h)
	for _, limit := range []int{DefaultHandoffMaxBytes, 60_000, 30_000, 14_000} {
		fit, ok := FitHandoff(h, limit, markdownSize)
		out := string(RenderHandoffMarkdown(fit, HandoffRenderOptions{Preamble: true}))
		t.Logf("limit %d: %d bytes → %d bytes", limit, full, len(out))
		if !ok || len(out) > limit {
			t.Fatalf("limit %d: fits=%v, %d bytes", limit, ok, len(out))
		}
		if !utf8.ValidString(out) || openFenceStart(out) >= 0 {
			t.Fatalf("limit %d: invalid UTF-8 or an unclosed fence", limit)
		}
		for _, want := range []string{
			"# Handoff: continuing a Codex session",
			"## Where it left off\n" + quote(h.LeftOff),
			"Omitted to fit the size limit:",
			"Full record: /data/handoffs/97636671.md",
			// The last exchange's final message is kept, if shortened.
			"Exchange 34 message 36: the check is still in progress",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("limit %d: missing %q", limit, want)
			}
		}
		for i := range h.Exchanges {
			if !strings.Contains(out, fmt.Sprintf("prompt %d: continue with the next PR", i)) {
				t.Fatalf("limit %d: prompt %d lost", limit, i)
			}
		}
		if fit.Exchanges[len(fit.Exchanges)-1].Condensed != nil {
			t.Fatalf("limit %d: the last exchange was condensed", limit)
		}
		// The first prompt, which is long, keeps its body up to the cap.
		first := fit.Exchanges[0].Prompt
		if len(first) < handoffPromptCap-3 || !strings.Contains(out, quote(first+" …(truncated)")) {
			t.Fatalf("limit %d: the first prompt lost its body: %d bytes", limit, len(first))
		}
	}
}

// At the default limit the session keeps more than the bare minimum: the
// later stages stop once it fits, and the last exchange keeps its tool
// output.
func TestFitHandoffStopsAtTheFirstStageThatFits(t *testing.T) {
	t.Parallel()
	fit, ok := FitHandoff(longCodexHandoff(), DefaultHandoffMaxBytes, markdownSize)
	if !ok {
		t.Fatal("did not fit")
	}
	var kinds []HandoffElisionKind
	for _, e := range fit.Elisions {
		kinds = append(kinds, e.Kind)
	}
	want := []HandoffElisionKind{HandoffElisionToolOutput, HandoffElisionToolCalls, HandoffElisionAssistantText, HandoffElisionPromptText, HandoffElisionAgentMessages}
	if !slices.Equal(kinds, want) {
		t.Fatalf("elisions = %v, want %v", kinds, want)
	}
	kept := 0
	for _, step := range fit.Exchanges[len(fit.Exchanges)-1].Steps {
		if step.Tool != nil && step.Tool.Result != "" {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("the last exchange lost all its tool output")
	}
	condensed, _ := FitHandoff(longCodexHandoff(), 30_000, markdownSize)
	if !slices.ContainsFunc(condensed.Elisions, func(e HandoffElision) bool { return e.Kind == HandoffElisionExchanges }) {
		t.Fatalf("a 30,000-byte limit condensed no exchanges: %v", condensed.Elisions)
	}
}

// When the recent steps' own tool output is what is over budget, it is
// dropped before any exchange is condensed.
func TestFitHandoffDropsRecentToolOutputBeforeCondensing(t *testing.T) {
	t.Parallel()
	h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "codex"}, LeftOff: "done"}
	for i := range 3 {
		exchange := HandoffExchange{Prompt: fmt.Sprintf("prompt %d", i)}
		for j := range 6 {
			exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "gh", Summary: strconv.Itoa(j), Result: strings.Repeat("{}\n", 600)}})
		}
		h.Exchanges = append(h.Exchanges, exchange)
	}
	fit, ok := FitHandoff(h, 20_000, markdownSize)
	if !ok || markdownSize(fit) > 20_000 {
		t.Fatalf("fits=%v size=%d", ok, markdownSize(fit))
	}
	last := fit.Elisions[len(fit.Elisions)-1]
	if last.Kind != HandoffElisionToolOutput || slices.ContainsFunc(fit.Exchanges, func(e HandoffExchange) bool { return e.Condensed != nil }) {
		t.Fatalf("elisions = %v", fit.Elisions)
	}
}

func TestFitHandoffIsStable(t *testing.T) {
	t.Parallel()
	h := longCodexHandoff()
	first, _ := FitHandoff(h, 30_000, markdownSize)
	second, _ := FitHandoff(h, 30_000, markdownSize)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("fitting the same handoff twice gave different results")
	}
	again, ok := FitHandoff(first, 30_000, markdownSize)
	if !ok || !reflect.DeepEqual(again, first) {
		t.Fatal("refitting a fitted handoff changed it")
	}
	if !reflect.DeepEqual(h, longCodexHandoff()) {
		t.Fatal("FitHandoff mutated its input")
	}
}

func TestOmitAgentMessagesKeepsTheLastBeforeTheTail(t *testing.T) {
	t.Parallel()
	text := func(s string) HandoffStep { return HandoffStep{Kind: HandoffStepText, Text: s} }
	tool := HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Bash"}}
	steps := []HandoffStep{text("a"), tool, text("b"), {Kind: HandoffStepSummary, Text: "s"}, text("c"), tool, text("tail")}
	got, n := omitAgentMessages(steps, 6)
	if n != 2 {
		t.Fatalf("omitted %d, want 2", n)
	}
	var kinds []string
	for _, step := range got {
		kinds = append(kinds, string(step.Kind)+":"+step.Text)
	}
	want := []string{"omitted:", "tool:", "summary:s", "text:c", "tool:", "text:tail"}
	if !slices.Equal(kinds, want) || got[0].Count != 2 {
		t.Fatalf("steps = %v (count %d), want %v", kinds, got[0].Count, want)
	}
	if again, n := omitAgentMessages(got, 5); n != 0 || len(again) != len(got) {
		t.Fatalf("second pass omitted %d", n)
	}
	if _, n := omitAgentMessages([]HandoffStep{text("only")}, 1); n != 0 {
		t.Fatal("a lone message was omitted")
	}
	// A run omitted earlier joins the new one rather than standing apart.
	merged, n := omitAgentMessages([]HandoffStep{{Kind: HandoffStepOmitted, Count: 3}, text("d"), text("e")}, 3)
	if n != 1 || len(merged) != 2 || merged[0].Kind != HandoffStepOmitted || merged[0].Count != 4 || merged[1].Text != "e" {
		t.Fatalf("merged = %+v, %d", merged, n)
	}
}

func TestCondenseExchangeCountsEverythingItHeld(t *testing.T) {
	t.Parallel()
	exchange := HandoffExchange{Prompt: "first line\nsecond line", Steps: []HandoffStep{
		{Kind: HandoffStepOmitted, Count: 4},
		{Kind: HandoffStepText, Text: "kept"},
		{Kind: HandoffStepCollapsed, Count: 9, Text: "9 tool calls: `Bash` ×9"},
		{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Read"}},
		{Kind: HandoffStepShell, Text: "ls"},
		{Kind: HandoffStepSummary, Text: "compacted"},
	}}
	if condenseExchange(&exchange, true) != 1 {
		t.Fatal("not condensed")
	}
	want := HandoffCondensed{AgentMessages: 6, ToolCalls: 10, ShellCommands: 1}
	if *exchange.Condensed != want || exchange.Prompt != "first line" || !exchange.PromptTruncated || exchange.Steps != nil {
		t.Fatalf("condensed = %#v", exchange)
	}
	out := string(RenderHandoffMarkdown(Handoff{Exchanges: []HandoffExchange{exchange}}, HandoffRenderOptions{}))
	if !strings.Contains(out, "> first line …(truncated)\n\n- 6 agent messages, 10 tool calls, 1 shell command omitted\n") {
		t.Fatalf("rendered:\n%s", out)
	}
	if condenseExchange(&exchange, true) != 0 {
		t.Fatal("condensed twice")
	}
	whole := HandoffExchange{Prompt: "keep\nall of it", Steps: []HandoffStep{{Kind: HandoffStepText, Text: "x"}}}
	condenseExchange(&whole, false)
	if whole.Prompt != "keep\nall of it" || whole.PromptTruncated {
		t.Fatalf("the first prompt was shortened: %#v", whole)
	}
}

func TestHandoffFooterDescribesTheNewSteps(t *testing.T) {
	t.Parallel()
	h := Handoff{Elisions: []HandoffElision{
		{Kind: HandoffElisionAgentMessages, First: 1, Last: 30, Count: 600},
		{Kind: HandoffElisionExchanges, First: 1, Last: 34, Count: 34},
	}}
	got := handoffFooterLines(h)[0]
	want := "Omitted to fit the size limit: 600 agent messages in exchanges 1–30 omitted, keeping each exchange's last; 34 exchanges in exchanges 1–34 condensed to the prompt's first line and counts."
	if got != want {
		t.Fatalf("footer = %q", got)
	}
}

// One part over budget by itself cannot be fitted; the rendering is then
// cut at a line, outside any fence, with a marker and the footer kept.
func TestCutHandoffMarkdownBoundsAPathologicalHandoff(t *testing.T) {
	t.Parallel()
	h := longCodexHandoff()
	h.LeftOff = strings.Repeat("字字字 a long final message\n", 4000)
	fit, ok := FitHandoff(h, DefaultHandoffMaxBytes, markdownSize)
	if ok {
		t.Fatal("an oversized LeftOff fitted")
	}
	rendered := RenderHandoffMarkdown(fit, HandoffRenderOptions{Preamble: true})
	out := string(CutHandoffMarkdown(rendered, fit, DefaultHandoffMaxBytes))
	if len(out) > DefaultHandoffMaxBytes || !utf8.ValidString(out) || openFenceStart(out) >= 0 {
		t.Fatalf("cut to %d bytes", len(out))
	}
	for _, want := range []string{"# Handoff:", "**Cut here: this handoff was still over its 120000-byte limit", "the full record named below has the rest", "Full record: /data/handoffs/97636671.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if got := CutHandoffMarkdown(rendered[:100], fit, DefaultHandoffMaxBytes); string(got) != string(rendered[:100]) {
		t.Fatal("a rendering within the limit was changed")
	}
	if tiny := CutHandoffMarkdown(rendered, fit, 50); len(tiny) > 50 || !utf8.Valid(tiny) {
		t.Fatalf("tiny limit: %d bytes", len(tiny))
	}
}

func TestWholeLinesNeverStopsInsideAFence(t *testing.T) {
	t.Parallel()
	text := "intro\n- `Bash` go test\n  ````\n  ```\n  inner\n  ````\nafter\n"
	for n := range len(text) + 1 {
		got := wholeLines(text, n)
		if !strings.HasPrefix(text, got) || len(got) > n || openFenceStart(got) >= 0 {
			t.Fatalf("n=%d: %q", n, got)
		}
	}
	if got := wholeLines(text, strings.Index(text, "inner")+3); got != "intro\n- `Bash` go test\n" {
		t.Fatalf("cut inside the fence: %q", got)
	}
	// A tilde fence opens one too, and a backtick line does not close it.
	tilde := "intro\n~~~\n```\ninner\n~~~\nafter\n"
	if got := wholeLines(tilde, strings.Index(tilde, "inner")+3); got != "intro\n" {
		t.Fatalf("cut inside the tilde fence: %q", got)
	}
	// Nor does a tilde line close a backtick fence.
	backtick := "intro\n```\n~~~\ninner\n```\nafter\n"
	if got := wholeLines(backtick, strings.Index(backtick, "inner")+3); got != "intro\n" {
		t.Fatalf("cut inside the backtick fence: %q", got)
	}
}

func TestPromptFirstLineIsCapped(t *testing.T) {
	t.Parallel()
	got := promptFirstLine("\n  " + strings.Repeat("字", 100) + "\nsecond")
	if len(got) > handoffSummaryCap || !utf8.ValidString(got) || !strings.HasPrefix(got, "字") {
		t.Fatalf("first line = %q (%d bytes)", got, len(got))
	}
}

// Hundreds of exchanges can be over the limit even condensed to a line
// each; the cut then keeps the last exchange after its marker.
func TestCutHandoffMarkdownKeepsTheLastOfManyExchanges(t *testing.T) {
	t.Parallel()
	h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "claude", ArchiveSessionID: "x"}, LeftOff: "left off here", FullRecordPath: "/data/handoffs/x.md"}
	for i := range 700 {
		h.Exchanges = append(h.Exchanges, HandoffExchange{Prompt: fmt.Sprintf("prompt %d ", i) + strings.Repeat("words ", 25), Steps: []HandoffStep{{Kind: HandoffStepText, Text: fmt.Sprintf("done %d", i) + strings.Repeat(" and more", 8)}}})
	}
	fit, ok := FitHandoff(h, DefaultHandoffMaxBytes, markdownSize)
	if ok {
		t.Fatal("700 exchanges fitted; the fixture no longer exercises the cut")
	}
	rendered := RenderHandoffMarkdown(fit, HandoffRenderOptions{Preamble: true})
	out := string(CutHandoffMarkdown(rendered, fit, DefaultHandoffMaxBytes))
	if len(out) > DefaultHandoffMaxBytes || !utf8.ValidString(out) || openFenceStart(out) >= 0 {
		t.Fatalf("cut to %d bytes", len(out))
	}
	marker, last := strings.Index(out, "**Cut here:"), strings.Index(out, "### 700 · Person\n> prompt 699 ")
	if marker < 0 || last < marker || !strings.Contains(out[last:], "> done 699 and more") || !strings.Contains(out, "left off here") || !strings.HasSuffix(out, "Full record: /data/handoffs/x.md (read it for anything omitted here).\n") {
		t.Fatalf("marker at %d, last exchange at %d:\n%s", marker, last, out[len(out)-800:])
	}
	if !strings.Contains(out, "### 2 · Person") {
		t.Fatal("the earliest exchanges were not kept before the marker")
	}
	// A last exchange over half the budget is cut like everything else.
	fit.Exchanges[len(fit.Exchanges)-1].Steps[0].Text = strings.Repeat("a long final message\n", 300)
	rendered = RenderHandoffMarkdown(fit, HandoffRenderOptions{Preamble: true})
	for _, limit := range []int{4000, 8000, 12000} {
		out := string(CutHandoffMarkdown(rendered, fit, limit))
		if len(out) > limit || strings.Contains(out, "### 700 ·") || !strings.Contains(out, "**Cut here:") {
			t.Fatalf("limit %d: %d bytes", limit, len(out))
		}
	}
}

// A limit too small for the marker and footer still names the full record,
// and a limit too small for even that is filled with as much of it as fits.
func TestCutHandoffMarkdownNamesTheFullRecordAtSmallLimits(t *testing.T) {
	t.Parallel()
	h := longCodexHandoff()
	h.LeftOff = strings.Repeat("字字字 a long final message\n", 4000)
	fit, _ := FitHandoff(h, DefaultHandoffMaxBytes, markdownSize)
	rendered := RenderHandoffMarkdown(fit, HandoffRenderOptions{Preamble: true})
	for _, limit := range []int{1, 10, 50, 100, 500, 1000, 2000} {
		compact := "**Cut to " + strconv.Itoa(limit) + " bytes; full record: /data/handoffs/97636671.md**\n"
		out := string(CutHandoffMarkdown(rendered, fit, limit))
		if len(out) > limit || !utf8.ValidString(out) || openFenceStart(out) >= 0 {
			t.Fatalf("limit %d: cut to %d bytes", limit, len(out))
		}
		switch {
		case limit > len(compact):
			// The compact marker, or the full marker and footer when they fit.
			if !strings.HasSuffix(out, "\n"+compact) && !strings.HasSuffix(out, "Full record: /data/handoffs/97636671.md (read it for anything omitted here).\n") {
				t.Fatalf("limit %d: no full record named:\n%s", limit, out)
			}
		case !strings.HasPrefix(compact, out) || len(out) < limit-utf8.UTFMax:
			t.Fatalf("limit %d: %q is not the compact marker cut to the limit", limit, out)
		}
	}
	// Without a saved full record, the marker says how to get the rest.
	fit.FullRecordPath = ""
	if out := string(CutHandoffMarkdown(rendered, fit, 500)); len(out) > 500 || !strings.HasSuffix(out, "rerun with `--max-bytes 0` for the rest.**\n") {
		t.Fatalf("no full record: %q", out)
	}
}

// Condensing a tiny exchange makes it longer, so it is left as it is, and a
// limit met by condensing only the long exchange before it is met without
// cutting.
func TestFitHandoffSkipsChangesThatGrowAnExchange(t *testing.T) {
	t.Parallel()
	h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "claude", ArchiveSessionID: "x"}, FullRecordPath: "/data/handoffs/x.md", Exchanges: []HandoffExchange{
		{Prompt: "start", Steps: []HandoffStep{{Kind: HandoffStepText, Text: strings.Repeat("a long reply ", 30)}}},
		{Prompt: "ok?", Steps: []HandoffStep{{Kind: HandoffStepText, Text: "ok"}}},
		{Prompt: "last", Steps: []HandoffStep{{Kind: HandoffStepText, Text: "done"}}},
	}}
	first := cloneHandoff(h)
	condenseExchange(&first.Exchanges[0], false)
	first.Elisions = []HandoffElision{{Kind: HandoffElisionExchanges, First: 1, Last: 1, Count: 1}}
	both := cloneHandoff(first)
	condenseExchange(&both.Exchanges[1], true)
	both.Elisions = []HandoffElision{{Kind: HandoffElisionExchanges, First: 1, Last: 2, Count: 2}}
	limit := markdownSize(first)
	if markdownSize(h) <= limit || markdownSize(both) <= limit {
		t.Fatalf("fixture: %d uncondensed, %d condensing the first, %d condensing both", markdownSize(h), limit, markdownSize(both))
	}
	fit, ok := FitHandoff(h, limit, markdownSize)
	if !ok || markdownSize(fit) > limit || fit.Exchanges[0].Condensed == nil || fit.Exchanges[1].Condensed != nil {
		t.Fatalf("fits=%v, %d bytes over a %d-byte limit, elisions %#v", ok, markdownSize(fit), limit, fit.Elisions)
	}
}

// Past its first change, every stage's rendering shrinks or stays the same
// as it reaches more exchanges, in markdown and JSON: the binary search in
// smallestFittingPrefix depends on it.
func TestFitStagesNeverGrowWithThePrefix(t *testing.T) {
	t.Parallel()
	jsonSize := func(h Handoff) int {
		data, err := json.MarshalIndent(h, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return len(DisplayJSON(data)) + 1
	}
	rng := rand.New(rand.NewPCG(414, 1))
	words := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = []string{"ok", "go", "test", "字", "fix", "\n", "done"}[rng.IntN(7)]
		}
		return strings.Join(parts, " ")
	}
	text := func() string {
		if rng.IntN(4) == 0 {
			return words(rng.IntN(400))
		}
		return words(rng.IntN(4))
	}
	cases := 300
	if testing.Short() {
		// Keep every stage and prefix in both formats for a fixed seed prefix.
		cases = 60
	}
	for n := range cases {
		h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "codex"}, FullRecordPath: "/data/handoffs/p.md"}
		for range 1 + rng.IntN(8) {
			prompt := text()
			if rng.IntN(10) == 0 {
				prompt = strings.Repeat("p", handoffPromptCap+rng.IntN(50))
			}
			exchange := HandoffExchange{Prompt: prompt}
			for range rng.IntN(6) {
				switch rng.IntN(5) {
				case 0, 1:
					exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepText, Text: text()})
				case 2:
					result := text()
					exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Bash", Summary: text(), Result: result, ResultLines: strings.Count(result, "\n") + 1}})
				case 3:
					exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepShell, Text: text()})
				default:
					exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepSummary, Text: text()})
				}
			}
			h.Exchanges = append(h.Exchanges, exchange)
		}
		for format, measure := range map[string]func(Handoff) int{"markdown": markdownSize, "json": jsonSize} {
			out := cloneHandoff(h)
			for s, stage := range handoffFitStages() {
				apply := fitStagePrefixes(out, stage, measure)
				previous, started := 0, false
				for k := range len(out.Exchanges) + 1 {
					trial, elision := apply(k)
					size := measure(withElision(trial, elision))
					if started && size > previous {
						t.Fatalf("handoff %d, %s, stage %d: %d bytes at prefix %d, %d at %d", n, format, s+1, size, k, previous, k-1)
					}
					previous, started = size, started || elision.Count > 0
				}
				// The next stage starts from this one applied everywhere.
				all, elision := apply(len(out.Exchanges))
				out = withElision(all, elision)
			}
		}
	}
}
