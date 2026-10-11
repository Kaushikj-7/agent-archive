package archive

import (
	"strconv"
	"strings"
)

// handoffFitStage is one FitHandoff budget step. plan looks at the handoff as
// the step starts and returns what the step does to one exchange: it changes
// the exchange in place and returns how many items it removed or shortened.
type handoffFitStage struct {
	kind HandoffElisionKind
	plan func(h Handoff) func(exchange *HandoffExchange, i int) int
}

// FitHandoff returns a copy of h trimmed until measure(copy) is at most
// maxBytes, applying the budget steps in order and recording each in
// Elisions. Each step is applied to the oldest exchanges first, and to no more
// of them than needed. The steps are:
//
//  1. drop tool output,
//  2. collapse tool calls to per-tool counts,
//  3. shorten agent text,
//
// none of which touches the protected tail — steps in the last
// handoffKeptExchanges exchanges, but no more than the last handoffKeptSteps
// steps, so one long autonomous exchange can still be trimmed — then
//
//  4. truncate long prompts,
//  5. omit all but the last of each exchange's agent messages outside the
//     protected tail,
//  6. drop tool output everywhere, the protected tail included,
//  7. condense every exchange but the last to its prompt's first line (the
//     first prompt is kept whole, up to handoffPromptCap) and counts of what
//     it held,
//  8. collapse tool calls everywhere, and
//  9. shorten agent text everywhere.
//
// Prompts are only ever truncated, and LeftOff is never trimmed. After step 9
// what remains is the header, LeftOff, the plan and file list, one line per
// exchange, and the last exchange's capped steps, so fits is false only when
// one of those is over budget by itself or the exchanges are so many that
// their lines together are. maxBytes <= 0 means no budget.
//
// Each step is applied to an exchange only when that does not make the
// rendering longer (see fitStagePrefixes), so a step's result only shrinks as
// it reaches more exchanges and the smallest sufficient prefix is found by
// binary search: a handful of full measurements per step rather than one per
// exchange, which matters when measure renders a multi-megabyte session.
func FitHandoff(h Handoff, maxBytes int, measure func(Handoff) int) (Handoff, bool) {
	out := cloneHandoff(h)
	if maxBytes <= 0 || measure(out) <= maxBytes {
		return out, true
	}
	for _, stage := range handoffFitStages() {
		apply := fitStagePrefixes(out, stage, measure)
		trial, elision, fits := smallestFittingPrefix(len(out.Exchanges), maxBytes, measure, apply)
		out = trial
		if elision.Count > 0 {
			out.Elisions = append(out.Elisions, elision)
		}
		if fits {
			return out, true
		}
	}
	return out, false
}

// fitStagePrefixes decides, oldest exchange first, what stage does to each
// of out's exchanges, and returns apply(k): out with the first k exchanges
// changed, and the elision that records it.
//
// A change is kept only when it does not make the rendering longer, measured
// on a handoff holding just that exchange and the elisions recorded so far,
// since an exchange renders the same whatever surrounds it. A step can make a
// small exchange longer: a condensed exchange's counts line can outweigh the
// one short reply it replaces. From the first kept change on, each one also
// pays for what it adds to the stage's footer description, so the rendering
// of apply(k) never grows with k past the first change; before that, apply(k)
// is out unchanged, which is over budget. That is what smallestFittingPrefix
// needs to find the smallest prefix that fits.
func fitStagePrefixes(out Handoff, stage handoffFitStage, measure func(Handoff) int) func(k int) (Handoff, HandoffElision) {
	do := stage.plan(out)
	changed := make([]*HandoffExchange, len(out.Exchanges))
	counts := make([]int, len(out.Exchanges))
	elision := HandoffElision{Kind: stage.kind}
	sized := func(exchange HandoffExchange, elision HandoffElision) int {
		single := Handoff{FullRecordPath: out.FullRecordPath, Elisions: out.Elisions, Exchanges: []HandoffExchange{exchange}}
		return measure(withElision(single, elision))
	}
	for i, exchange := range out.Exchanges {
		candidate := cloneHandoffExchange(exchange)
		n := do(&candidate, i)
		if n <= 0 {
			continue
		}
		next := elision
		if next.First == 0 {
			next.First = i + 1
		}
		next.Last, next.Count = i+1, next.Count+n
		// The first change adds the stage's footer description, which only
		// matters to prefixes that are over budget anyway.
		after := next
		if elision.Count == 0 {
			after = elision
		}
		if sized(candidate, after) > sized(exchange, elision) {
			continue
		}
		changed[i], counts[i], elision = &candidate, n, next
	}
	return func(k int) (Handoff, HandoffElision) {
		trial := cloneHandoff(out)
		elision := HandoffElision{Kind: stage.kind}
		for i := range k {
			if changed[i] == nil {
				continue
			}
			trial.Exchanges[i] = cloneHandoffExchange(*changed[i])
			if elision.First == 0 {
				elision.First = i + 1
			}
			elision.Last, elision.Count = i+1, elision.Count+counts[i]
		}
		return trial, elision
	}
}

func handoffFitStages() []handoffFitStage {
	return []handoffFitStage{
		{HandoffElisionToolOutput, protectedStepStage(dropToolOutput)},
		{HandoffElisionToolCalls, protectedStepStage(collapseToolCalls)},
		{HandoffElisionAssistantText, protectedStepStage(shortenAssistantText)},
		{HandoffElisionPromptText, func(Handoff) func(*HandoffExchange, int) int { return truncatePrompt }},
		{HandoffElisionAgentMessages, protectedStepStage(omitAgentMessages)},
		{HandoffElisionToolOutput, everyStepStage(dropToolOutput)},
		{HandoffElisionExchanges, condenseStage},
		{HandoffElisionToolCalls, everyStepStage(collapseToolCalls)},
		{HandoffElisionAssistantText, everyStepStage(shortenAssistantText)},
	}
}

// protectedStepStage applies a step function to the steps before the
// protected tail. Protection is computed once, before the step: a step only
// changes steps outside the protected tail, so the tail it computes stays the
// same while the step runs.
func protectedStepStage(apply func(steps []HandoffStep, limit int) ([]HandoffStep, int)) func(Handoff) func(*HandoffExchange, int) int {
	return func(h Handoff) func(*HandoffExchange, int) int {
		limits := protectedStart(h.Exchanges, handoffKeptExchanges, handoffKeptSteps)
		return func(exchange *HandoffExchange, i int) int {
			var n int
			exchange.Steps, n = apply(exchange.Steps, limits[i])
			return n
		}
	}
}

// everyStepStage applies a step function to every step.
func everyStepStage(apply func(steps []HandoffStep, limit int) ([]HandoffStep, int)) func(Handoff) func(*HandoffExchange, int) int {
	return func(Handoff) func(*HandoffExchange, int) int {
		return func(exchange *HandoffExchange, _ int) int {
			var n int
			exchange.Steps, n = apply(exchange.Steps, len(exchange.Steps))
			return n
		}
	}
}

// truncatePrompt caps a long prompt; prompts are truncated, never dropped.
func truncatePrompt(exchange *HandoffExchange, _ int) int {
	if len(exchange.Prompt) <= handoffPromptCap {
		return 0
	}
	exchange.Prompt = TruncateUTF8(exchange.Prompt, handoffPromptCap)
	exchange.PromptTruncated = true
	return 1
}

// omitAgentMessages keeps the last agent message before limit and replaces
// the others with one omitted step, at the position of the first, that
// counts them. Compaction summaries are kept.
func omitAgentMessages(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	last := -1
	for i := range steps[:limit] {
		if steps[i].Kind == HandoffStepText {
			last = i
		}
	}
	out := make([]HandoffStep, 0, len(steps))
	at, total, added := -1, 0, 0
	for i, step := range steps {
		switch {
		case i < limit && step.Kind == HandoffStepOmitted:
			// A run omitted by an earlier pass joins this one.
			total += step.Count
		case i < limit && step.Kind == HandoffStepText && i != last:
			total, added = total+1, added+1
		default:
			out = append(out, step)
			continue
		}
		if at < 0 {
			at = len(out)
			out = append(out, HandoffStep{Kind: HandoffStepOmitted})
		}
	}
	if at < 0 {
		return steps, 0
	}
	out[at].Count = total
	return out, added
}

// condenseStage condenses every exchange but the last. The first exchange
// with a prompt keeps its prompt as it is; the others keep their first line.
func condenseStage(h Handoff) func(*HandoffExchange, int) int {
	first := -1
	for i, exchange := range h.Exchanges {
		if exchange.Prompt != "" {
			first = i
			break
		}
	}
	return func(exchange *HandoffExchange, i int) int {
		if i == len(h.Exchanges)-1 {
			return 0
		}
		return condenseExchange(exchange, i != first)
	}
}

// condenseExchange replaces an exchange's steps with counts of what they
// held and, when shortenPrompt is set, its prompt with the prompt's first
// line. It returns 1 when it changed the exchange.
func condenseExchange(exchange *HandoffExchange, shortenPrompt bool) int {
	if exchange.Condensed != nil {
		return 0
	}
	prompt := exchange.Prompt
	if shortenPrompt {
		prompt = promptFirstLine(prompt)
	}
	if len(exchange.Steps) == 0 && prompt == exchange.Prompt {
		return 0
	}
	condensed := &HandoffCondensed{}
	for _, step := range exchange.Steps {
		switch step.Kind {
		case HandoffStepText, HandoffStepSummary:
			condensed.AgentMessages++
		case HandoffStepOmitted:
			condensed.AgentMessages += step.Count
		case HandoffStepTool:
			condensed.ToolCalls++
		case HandoffStepCollapsed:
			condensed.ToolCalls += step.Count
		case HandoffStepShell:
			condensed.ShellCommands++
		}
	}
	if prompt != exchange.Prompt {
		exchange.PromptTruncated = exchange.PromptTruncated || prompt != strings.TrimSpace(exchange.Prompt)
		exchange.Prompt = prompt
	}
	exchange.Steps, exchange.Condensed = nil, condensed
	return 1
}

// promptFirstLine is a prompt's first non-empty line, capped at
// handoffSummaryCap bytes without splitting a character.
func promptFirstLine(prompt string) string {
	line := strings.TrimSpace(prompt)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return TruncateUTF8(line, handoffSummaryCap)
}

// CutHandoffMarkdown bounds a markdown rendering of h that FitHandoff could
// not fit, which happens when one part (LeftOff, the plan, the file list, the
// last exchange) is over budget by itself, or when there are so many
// exchanges that their condensed prompts are. It keeps whole lines from the
// start, never stops inside a code fence, and adds a marker saying where the
// rest is, then the last exchange when it takes at most half the budget, then
// h's footer, so what was omitted is still listed. When the limit leaves no
// room for that marker and the footer, a compact marker that names the full
// record takes their place, and when even that does not fit, the output is
// the compact marker cut to the limit. A rendering within maxBytes, or any
// rendering when maxBytes <= 0, is returned as it is.
func CutHandoffMarkdown(rendered []byte, h Handoff, maxBytes int) []byte {
	if maxBytes <= 0 || len(rendered) <= maxBytes {
		return rendered
	}
	var footerText strings.Builder
	renderHandoffFooter(&footerText, displayHandoff(h))
	footer := footerText.String()
	body := string(rendered)
	if !strings.HasSuffix(body, footer) {
		footer = ""
	}
	body = strings.TrimSuffix(body, footer)
	fullRecord := h.FullRecordPath != "" && len(h.Elisions) > 0
	rest := "rerun with `--max-bytes 0` for the rest"
	if fullRecord {
		rest = "the full record named below has the rest"
	}
	marker := "\n**Cut here: this handoff was still over its " + strconv.Itoa(maxBytes) + "-byte limit after every trimming step; " + rest + ".**\n"
	budget := maxBytes - len(marker) - len(footer)
	if budget <= 0 {
		// Too small a limit for the marker and footer: a compact marker
		// that still says where the rest is replaces them both.
		compact := "\n**Cut to " + strconv.Itoa(maxBytes) + " bytes; rerun with `--max-bytes 0` for the rest.**\n"
		if fullRecord {
			compact = "\n**Cut to " + strconv.Itoa(maxBytes) + " bytes; full record: " + oneLine(displayText(h.FullRecordPath)) + "**\n"
		}
		if len(compact) > maxBytes {
			return []byte(TruncateUTF8(strings.TrimPrefix(compact, "\n"), maxBytes))
		}
		return []byte(wholeLines(body, maxBytes-len(compact)) + compact)
	}
	// With many exchanges, their prompts alone can be over the limit; the
	// last exchange then goes after the marker rather than being cut off,
	// as long as it leaves most of the budget to what comes before it.
	if last := lastExchangeMarkdown(h); last != "" && len(last) <= budget/2 && strings.HasSuffix(body, last) {
		return []byte(wholeLines(strings.TrimSuffix(body, last), budget-len(last)) + marker + last + footer)
	}
	return []byte(wholeLines(body, budget) + marker + footer)
}

// lastExchangeMarkdown is how h's last exchange renders, which is how its
// markdown conversation ends, or "" when h has no exchanges.
func lastExchangeMarkdown(h Handoff) string {
	if len(h.Exchanges) == 0 {
		return ""
	}
	h = displayHandoff(h)
	var b strings.Builder
	renderHandoffExchange(&b, len(h.Exchanges)-1, h.Exchanges[len(h.Exchanges)-1])
	return b.String()
}

// wholeLines returns the longest prefix of s, at most n bytes, that ends at
// a line break and is not inside a code fence; when no line break fits, it
// cuts at a character boundary.
func wholeLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], '\n') + 1
	if cut == 0 {
		return TruncateUTF8(s, n)
	}
	s = s[:cut]
	if open := openFenceStart(s); open >= 0 {
		s = s[:open]
	}
	return s
}

// openFenceStart returns where the code fence still open at the end of s
// starts, or -1. A fence opens on a line that is, after indentation, a run of
// three or more backticks or tildes, and closes on a line that is a run of
// the same character at least as long.
func openFenceStart(s string) int {
	start, fence := -1, ""
	offset := 0
	for line := range strings.SplitSeq(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence == "":
			if run := fenceRun(trimmed); len(run) >= 3 {
				start, fence = offset, run
			}
		case strings.Trim(trimmed, fence[:1]) == "" && len(trimmed) >= len(fence):
			start, fence = -1, ""
		}
		offset += len(line) + 1
	}
	return start
}

// fenceRun is the run of backticks or tildes that starts line.
func fenceRun(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	end := len(line) - len(strings.TrimLeft(line, line[:1]))
	return line[:end]
}
