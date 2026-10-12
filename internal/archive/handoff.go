package archive

import (
	"fmt"
	"path"

	"slices"
	"sort"
	"strings"
	"time"
)

// HandoffVersion identifies the Handoff document shape and the markdown
// layout RenderHandoffMarkdown produces from it.
const HandoffVersion = 1

// DefaultHandoffMaxBytes is the default output budget, about 30k tokens.
const DefaultHandoffMaxBytes = 120_000

// handoffKeptExchanges and handoffKeptSteps bound the protected tail the
// budget never trims: steps in the last three exchanges, but at most the last
// twenty steps, so a single long exchange can still be trimmed.
const (
	handoffKeptExchanges = 3
	handoffKeptSteps     = 20
)

const (
	handoffAssistantTextCap = 300
	handoffPromptCap        = 2000
	handoffSummaryCap       = 200
	handoffAgentPromptCap   = 120
)

// HandoffOptions controls how much of each tool result BuildHandoff keeps
// before any budget is applied.
type HandoffOptions struct {
	// ToolResultLines is how many lines of a result are kept, split between
	// its head and tail. Zero means 12.
	ToolResultLines int
	// ToolResultBytes caps a result after line trimming, split between head
	// and tail. Zero means 2,000.
	ToolResultBytes int
	// Source names where the bundle came from ("local", "archive", "file").
	Source string
	// StartedAt and LastActivityAt are fallbacks the caller knows (a
	// registration's start, a transcript's modification time), used only
	// when no retained record carries a timestamp. Neither is ever inferred
	// from the time the handoff is built.
	StartedAt      time.Time
	LastActivityAt time.Time
	// Checkout is where the receiving agent will work, when the caller
	// knows. BuildHandoff compares it with what the transcript recorded and
	// notes a difference in Workspace; it is never read from the bundle.
	Checkout HandoffCheckout
}

// HandoffCheckout is the checkout a handoff is for: what the person's
// current directory is, not what the session recorded.
type HandoffCheckout struct {
	// Directories are the checkout's absolute path as written and with
	// symlinks resolved, so a match under either spelling counts.
	Directories []string
	// Branch is the branch checked out, or "" when detached or unknown.
	Branch string
}

func (o HandoffOptions) resultLines() int {
	if o.ToolResultLines > 0 {
		return o.ToolResultLines
	}
	return 12
}

func (o HandoffOptions) resultBytes() int {
	if o.ToolResultBytes > 0 {
		return o.ToolResultBytes
	}
	return 2000
}

// Handoff is a filtered session arranged for another coding agent to continue
// from. It is built only from a filtered SourceBundle, never from a raw
// transcript, so it carries no more than the archive already retains.
type Handoff struct {
	Version   int              `json:"handoff_version"`
	Session   HandoffSession   `json:"session"`
	Workspace HandoffWorkspace `json:"workspace"`
	// LeftOff is the last assistant text in the session.
	LeftOff      string            `json:"left_off,omitempty"`
	Plan         []HandoffPlanItem `json:"plan,omitempty"`
	FilesTouched []string          `json:"files_touched,omitempty"`
	Exchanges    []HandoffExchange `json:"exchanges"`
	// ToolResultsUnavailable is true when the harness records no tool
	// results at all (Cursor), so their absence is stated once.
	ToolResultsUnavailable bool             `json:"tool_results_unavailable,omitempty"`
	Gaps                   []HandoffGap     `json:"capture_gaps,omitempty"`
	Elisions               []HandoffElision `json:"elisions,omitempty"`
	// FullRecordPath, when set, names where the untrimmed rendering was
	// saved. It is rendered only when Elisions is non-empty.
	FullRecordPath string `json:"full_record_path,omitempty"`
}

// HandoffSession identifies the session a Handoff continues: its IDs, harness,
// the models it used, when it started and was last active, and, when metadata
// was supplied, its lifecycle state and last turn outcome. Source names where
// the bundle came from (HandoffOptions.Source).
type HandoffSession struct {
	RevisionID       string        `json:"revision_id,omitempty"`
	CapturedAt       *time.Time    `json:"captured_at,omitempty"`
	FilterVersion    string        `json:"filter_version,omitempty"`
	ArchiveSessionID string        `json:"archive_session_id,omitempty"`
	NativeSessionID  string        `json:"native_session_id,omitempty"`
	Harness          string        `json:"harness"`
	HarnessVersion   string        `json:"harness_version,omitempty"`
	Models           []string      `json:"models,omitempty"`
	StartedAt        *time.Time    `json:"started_at,omitempty"`
	LastActivityAt   *time.Time    `json:"last_activity_at,omitempty"`
	State            MetadataState `json:"state,omitempty"`
	TurnOutcome      TurnOutcome   `json:"turn_outcome,omitempty"`
	Source           string        `json:"source,omitempty"`
}

// HandoffWorkspace is what the transcript recorded about where the work
// happened. Directory is a base name only. Elsewhere and CurrentBranch are
// set only when the caller gave HandoffOptions.Checkout and it differs.
type HandoffWorkspace struct {
	Directory string `json:"directory,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// Elsewhere is true when the recorded directory is neither the current
	// checkout nor a directory containing or inside it: the session ran in
	// another clone or on another machine, so its paths may not exist here.
	Elsewhere bool `json:"elsewhere,omitempty"`
	// CurrentBranch is the branch the current checkout is on, set when it
	// differs from Branch.
	CurrentBranch string `json:"current_branch,omitempty"`
}

// HandoffPlanItem is one entry of the agent's latest plan or todo list, with
// its status as the agent recorded it (for example "completed").
type HandoffPlanItem struct {
	Text string `json:"text"`
	// Status is copied verbatim from the harness's plan tool.
	//lint:ignore LV1001 the harnesses' plan-status vocabulary is external and open
	Status string `json:"status,omitempty"`
	// id is the item's ID in the harness's plan tool (Cursor's todo_write
	// writes one), by which a later partial update (merge: true) finds it.
	ID string `json:"-"`
}

// HandoffExchange is one human prompt and everything the agent did before
// the next one. A leading exchange with an empty prompt holds agent activity
// that preceded the first prompt.
type HandoffExchange struct {
	Prompt          string        `json:"prompt,omitempty"`
	PromptTruncated bool          `json:"prompt_truncated,omitempty"`
	Timestamp       string        `json:"timestamp,omitempty"`
	Steps           []HandoffStep `json:"steps,omitempty"`
	// Condensed is set when the budget replaced the exchange's steps with
	// counts of what they held, leaving only the prompt's first line.
	Condensed *HandoffCondensed `json:"condensed,omitempty"`
}

// HandoffCondensed counts what a condensed exchange held, including what
// earlier budget steps had already collapsed or omitted from it.
type HandoffCondensed struct {
	AgentMessages int `json:"agent_messages"`
	ToolCalls     int `json:"tool_calls"`
	ShellCommands int `json:"shell_commands,omitempty"`
}

// HandoffStep is assistant text, a tool call, a shell command the person ran
// directly, the summary Claude Code wrote when the session was compacted, or —
// once the budget has collapsed tool calls or omitted agent messages — a
// count of them such as "14 tool calls: Bash ×9, Read ×5".
type HandoffStep struct {
	Kind          HandoffStepKind  `json:"kind"`
	Text          string           `json:"text,omitempty"`
	TextTruncated bool             `json:"text_truncated,omitempty"`
	Tool          *HandoffToolCall `json:"tool,omitempty"`
	// Count is how many tool calls a collapsed step stands for, or how many
	// agent messages an omitted step stands for.
	Count int `json:"count,omitempty"`
}

// HandoffStepKind says which of the HandoffStep shapes a step is.
type HandoffStepKind string

// HandoffStepKind values, as written in the handoff document.
const (
	HandoffStepText      HandoffStepKind = "text"
	HandoffStepTool      HandoffStepKind = "tool"
	HandoffStepShell     HandoffStepKind = "shell"
	HandoffStepSummary   HandoffStepKind = "summary"
	HandoffStepCollapsed HandoffStepKind = "collapsed"
	// HandoffStepOmitted stands for agent messages the budget removed.
	HandoffStepOmitted HandoffStepKind = "omitted"
)

// HandoffToolCall is one tool call in a HandoffStep: the tool's name, a short
// summary of its input, whether it failed, and its trimmed result.
// ResultOmitted is true when the budget dropped the result.
type HandoffToolCall struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// Result is the trimmed retained output; ResultLines and ResultBytes
	// describe the whole retained output.
	Result        string `json:"result,omitempty"`
	ResultLines   int    `json:"result_lines,omitempty"`
	ResultBytes   int    `json:"result_bytes,omitempty"`
	ResultOmitted bool   `json:"result_omitted,omitempty"`
}

// HandoffGap counts the source bundle's capture gaps that share one code.
type HandoffGap struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// HandoffElision records one budget step: what it removed and from which
// exchanges (1-based, inclusive).
type HandoffElision struct {
	Kind  HandoffElisionKind `json:"kind"`
	First int                `json:"first_exchange"`
	Last  int                `json:"last_exchange"`
	Count int                `json:"count"`
}

// HandoffElisionKind names one FitHandoff budget step.
type HandoffElisionKind string

// HandoffElision kinds, in the order FitHandoff first applies them. The
// first three never touch the protected tail of recent steps; a handoff still
// over budget after the first five has tool output dropped, then tool calls
// collapsed and agent text shortened, everywhere (see FitHandoff).
const (
	// HandoffElisionToolOutput drops tool results.
	HandoffElisionToolOutput HandoffElisionKind = "tool_output"
	// HandoffElisionToolCalls collapses runs of tool calls to per-tool counts.
	HandoffElisionToolCalls HandoffElisionKind = "tool_calls"
	// HandoffElisionAssistantText shortens assistant text.
	HandoffElisionAssistantText HandoffElisionKind = "assistant_text"
	// HandoffElisionPromptText truncates long prompts; prompts are never
	// dropped.
	HandoffElisionPromptText HandoffElisionKind = "prompt_text"
	// HandoffElisionAgentMessages omits all but the last of each exchange's
	// agent messages outside the protected tail.
	HandoffElisionAgentMessages HandoffElisionKind = "agent_messages"
	// HandoffElisionExchanges condenses whole exchanges, never the last, to
	// their prompts and counts of what they held.
	HandoffElisionExchanges HandoffElisionKind = "exchanges"
)

// handoffSession selects recorded identity, model, and time metadata without
// consulting the clock or changing the parsed view.
func handoffSession(bundle SourceBundle, view NormalizedView, metadata *Metadata, opts HandoffOptions) HandoffSession {
	session := HandoffSession{
		ArchiveSessionID: bundle.ArchiveSessionID,
		NativeSessionID:  bundle.NativeSessionID,
		Harness:          bundle.Capture.Harness.Name,
		HarnessVersion:   bundle.Capture.Harness.Version,
		Source:           opts.Source,
	}
	seenModel := map[string]bool{}
	addModel := func(model string) {
		if model != "" && !isPlaceholderModel(model) && !seenModel[model] {
			seenModel[model] = true
			session.Models = append(session.Models, model)
		}
	}
	var first, last time.Time
	for _, turn := range view.Turns {
		addModel(turn.Model)
		addModel(turn.ResponseModel)
		if t, err := time.Parse(time.RFC3339Nano, turn.Timestamp); err == nil {
			if first.IsZero() || t.Before(first) {
				first = t
			}
			if t.After(last) {
				last = t
			}
		}
	}
	if metadata != nil {
		for _, model := range metadata.Models {
			addModel(model.Attributes["gen_ai.response.model"])
			addModel(model.Attributes["gen_ai.request.model"])
		}
		session.State, session.TurnOutcome = metadata.State, metadata.TurnOutcome
		if first.IsZero() {
			first = metadata.StartedAt
		}
		// A published capture is taken from the transcript as it stood, so
		// its capture time bounds the last activity it shows. A bundle built
		// just now has no such meaning, which is why CapturedAt itself is
		// never used.
		if last.IsZero() {
			last = metadata.CapturedAt
		}
	}
	if first.IsZero() {
		first = opts.StartedAt
	}
	if last.IsZero() {
		last = opts.LastActivityAt
	}
	if !first.IsZero() {
		t := first.UTC()
		session.StartedAt = &t
	}
	if !last.IsZero() {
		t := last.UTC()
		session.LastActivityAt = &t
	}
	return session
}

// selectHandoffExchanges groups selected turns and tool calls in record order.
// It only reads the normalized events and returns new handoff values.
func selectHandoffExchanges(events []handoffEvent, root string, opts HandoffOptions) ([]HandoffExchange, string, []HandoffPlanItem) {
	exchanges := []HandoffExchange{}
	leftOff := ""
	var plan []HandoffPlanItem
	var current *HandoffExchange
	flush := func() {
		if current != nil && (current.Prompt != "" || len(current.Steps) > 0) {
			exchanges = append(exchanges, *current)
		}
	}
	current = &HandoffExchange{}
	for _, event := range events {
		if event.turn != nil {
			turn := event.turn
			switch turn.Kind {
			case TurnKindHumanPrompt:
				flush()
				current = &HandoffExchange{Prompt: turnDisplayText(*turn), Timestamp: turn.Timestamp}
			case TurnKindAssistant:
				if text := strings.TrimSpace(turn.Text); text != "" {
					current.Steps = append(current.Steps, HandoffStep{Kind: HandoffStepText, Text: text})
					leftOff = text
				}
			case TurnKindShellCommand:
				if command := strings.TrimSpace(turnDisplayText(*turn)); command != "" {
					current.Steps = append(current.Steps, HandoffStep{Kind: HandoffStepShell, Text: command})
				}
			case TurnKindCompactSummary:
				// After /compact the agent worked from this model-written
				// summary rather than the turns before it, so the receiving
				// agent should see it where it happened.
				if text := strings.TrimSpace(turn.Text); text != "" {
					current.Steps = append(current.Steps, HandoffStep{Kind: HandoffStepSummary, Text: text})
				}
			case TurnKindToolResult, TurnKindHarnessMeta, TurnKindCommandOutput, TurnKindLocalCommand, TurnKindHarnessNotification:
				// Not part of the exchange the receiving agent reads.
			}
			continue
		}
		call := event.call
		tool := handoffToolCall(call, root, opts)
		if tool == nil {
			continue
		}
		updated := applyPlan(call.Action.Plan, plan)

		if updated != nil {
			plan = updated
		}
		current.Steps = append(current.Steps, HandoffStep{Kind: HandoffStepTool, Tool: tool})
	}
	flush()
	return exchanges, leftOff, plan
}

// handoffToolCall describes one tool call: its name, a one-line summary of
// its input, whether it failed, and its trimmed result. It returns nil for a
// call listedCall leaves out.
func handoffToolCall(call *NormalizedToolCall, root string, opts HandoffOptions) *HandoffToolCall {
	name, summary, listed := listedCall(*call, root)
	if !listed {
		return nil
	}
	var result string
	var resultLines, resultBytes int
	if call.ResultRecordIndex != nil {
		text := call.ResultText
		result, resultLines, resultBytes = trimResult(text, opts.resultLines(), opts.resultBytes()), lineCount(text), len(text)
	}
	return &HandoffToolCall{
		Name: name, Summary: summary, IsError: call.IsError != nil && *call.IsError,
		Result: result, ResultLines: resultLines, ResultBytes: resultBytes,
	}
}

// handoffEvent is one turn or one tool call, in record order.
type handoffEvent struct {
	record int
	turn   *NormalizedTurn
	call   *NormalizedToolCall
}

// handoffEvents merges turns and tool calls by record index. A tool call
// shares a record with the assistant text that introduced it (Claude) or has
// a record of its own (Codex); within one record the text comes first.
func handoffEvents(view NormalizedView) []handoffEvent {
	events := make([]handoffEvent, 0, len(view.Turns)+len(view.ToolCalls))
	for i := range view.Turns {
		events = append(events, handoffEvent{record: view.Turns[i].RecordIndex, turn: &view.Turns[i]})
	}
	for i := range view.ToolCalls {
		events = append(events, handoffEvent{record: view.ToolCalls[i].RecordIndex, call: &view.ToolCalls[i]})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].record != events[j].record {
			return events[i].record < events[j].record
		}
		return events[i].turn != nil && events[j].turn == nil
	})
	return events
}

// compareWorkspace marks where the recorded workspace differs from the
// checkout the handoff is for. root is the full recorded directory, compared
// here and never kept. A recorded directory that contains the checkout, or is
// inside it, is the same checkout (a session may start in a subdirectory).
// With no root, or no checkout given, nothing is claimed.
func compareWorkspace(ws HandoffWorkspace, root string, checkout HandoffCheckout) HandoffWorkspace {
	if root != "" && len(checkout.Directories) > 0 {
		ws.Elsewhere = !slices.ContainsFunc(checkout.Directories, func(dir string) bool {
			dir = path.Clean(dir)
			return dir == root || strings.HasPrefix(dir, strings.TrimSuffix(root, "/")+"/") || strings.HasPrefix(root, strings.TrimSuffix(dir, "/")+"/")
		})
	}
	if ws.Branch != "" && checkout.Branch != "" && ws.Branch != checkout.Branch {
		ws.CurrentBranch = checkout.Branch
	}
	return ws
}

func countGaps(gaps []CaptureGap) []HandoffGap {
	counts := map[string]int{}
	for _, gap := range gaps {
		counts[gap.Code]++
	}
	out := make([]HandoffGap, 0, len(counts))
	for code, count := range counts {
		out = append(out, HandoffGap{Code: code, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// lineCount counts visible lines, excluding trailing line separators.
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
}

// trimResult keeps the head and tail of a tool result: at most maxLines
// lines, then at most maxBytes bytes, each split evenly, with a marker where
// content was removed.
func trimResult(text string, maxLines, maxBytes int) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		head, tail := (maxLines+1)/2, maxLines/2
		omitted := len(lines) - head - tail
		kept := append(append([]string{}, lines[:head]...), fmt.Sprintf("… %d lines omitted …", omitted))
		lines = append(kept, lines[len(lines)-tail:]...)
		text = strings.Join(lines, "\n")
	}
	if len(text) > maxBytes {
		half := maxBytes / 2
		head, tail := TruncateUTF8(text, half), text[len(text)-half:]
		for len(tail) > 0 && !isRuneStart(tail[0]) {
			tail = tail[1:]
		}
		text = head + fmt.Sprintf("\n… %d bytes omitted …\n", len(text)-len(head)-len(tail)) + tail
	}
	return text
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// TruncateUTF8 returns at most n bytes of s without splitting a character:
// a cut that would land inside a UTF-8 sequence moves back to its start.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// firstLine returns the first non-empty line of s, capped at limit bytes.
func firstLine(s string, limit int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	if len(s) > limit {
		s = TruncateUTF8(s, limit) + "…"
	}
	return s
}

// workspaceFile is how a file a call names is shown and deduplicated, so the
// list reads the same on a machine where the repository lives elsewhere: a
// path under root is shown relative to it, a relative path that stays under
// root once resolved against it is shown that way, and anything else is
// cleaned. In a session rooted at /repo, "/repo/a.go", "a.go", "./a.go", and
// "../repo/a.go" are all "a.go". A relative path that leaves root
// ("../other/b.go") keeps its cleaned relative spelling: resolving it would
// print the absolute path above the workspace (a home directory), and a
// call run from a subdirectory meant it relative to that directory anyway.
// A Windows drive path (C:\repo\a.go) is read with forward slashes and an
// upper-case drive letter. root itself, ".", and "" name no file and are "".
func workspaceFile(file, root string) string {
	if file == "" {
		return ""
	}
	windows := isDrivePath(file) || isDrivePath(root)
	file, root = slashPath(file, windows), slashPath(root, windows)
	if root != "" {
		root = path.Clean(root)
	}
	if !path.IsAbs(file) && !isDrivePath(file) {
		clean := path.Clean(file)
		if clean == "." {
			return ""
		}
		if root != "" {
			if relative, under := underRoot(path.Join(root, clean), root); under {
				return relative
			}
		}
		return clean
	}
	file = path.Clean(file)
	if root != "" {
		if relative, under := underRoot(file, root); under {
			return relative
		}
	}
	return file
}

// underRoot returns clean path file relative to clean root when it is root
// ("") or inside it.
func underRoot(file, root string) (string, bool) {
	if file == root {
		return "", true
	}
	return strings.CutPrefix(file, strings.TrimSuffix(root, "/")+"/")
}

// shownFile is workspaceFile for display: a non-empty argument that names
// the workspace root itself is shown as ".".
func shownFile(file, root string) string {
	if shown := workspaceFile(file, root); shown != "" || file == "" {
		return shown
	}
	return "."
}

// isDrivePath reports whether p starts with a Windows drive (C:\ or C:/).
func isDrivePath(p string) bool {
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return false
	}
	c := p[0] | 0x20
	return c >= 'a' && c <= 'z'
}

// slashPath reads a path from a Windows session (windows) with forward
// slashes and an upper-case drive letter; any other path is unchanged, since
// a backslash is a legal file-name character there.
func slashPath(p string, windows bool) string {
	if !windows {
		return p
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if isDrivePath(p) {
		p = strings.ToUpper(p[:1]) + p[1:]
	}
	return p
}

// callToolName is the name a call is listed under: the tool's own name, or
// for a call with none the type of its record. The filter drops Codex's
// local_shell_call action, so such a call is listed as "local_shell_call".
func callToolName(call NormalizedToolCall) string {
	if call.ObservedName != "" {
		return call.ObservedName
	}
	if call.Name != "" {
		return call.Name
	}
	return call.ObservedName
}

// listedCall is how handoff lists a call and whether tools_used counts it:
// its name (callToolName, else "tool"), its one-line summary, and whether it
// is listed at all. A named call always is, and so is a nameless invocation
// (a Codex local_shell_call, whose action the filter drops). A nameless
// completion echo that no invocation reported (dedupeToolCalls already
// dropped the ones that were) is listed only when its summary says what ran,
// such as a CommandExecution's command; an Extension item with nothing to
// show says only that something finished.
func listedCall(call NormalizedToolCall, root string) (name, summary string, listed bool) {
	name = callToolName(call)
	if name == "" {
		name = "tool"
	}
	summary = actionSummary(call.Action, root)
	return name, summary, call.Name != "" || call.Invocation || summary != ""
}

// sessionFilesTouched lists, in first-touched order, the distinct files the
// editing calls among calls named (touchedFiles), keyed by workspaceFile.
// handoff lists these files and metadata publishes how many there are
// (counts.files_touched), so the two always agree.
func sessionFilesTouched(calls []NormalizedToolCall, root string) []string {
	files := fileSet{}
	for _, call := range calls {
		namedFiles := call.Action.Files

		for _, file := range namedFiles {
			files.add(workspaceFile(file, root))
		}
	}
	return files.list
}

type fileSet struct {
	seen map[string]bool
	list []string
}

func (s *fileSet) add(file string) {
	if file == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if !s.seen[file] {
		s.seen[file] = true
		s.list = append(s.list, file)
	}
}

// smallestFittingPrefix applies a step to the first k exchanges for the
// smallest k whose result fits, or to all n when none does. apply(k) must not
// modify its input, and once apply(k) fits, apply(k+1) must fit too (see
// fitStagePrefixes). The returned
// handoff does not yet carry the step's elision.
func smallestFittingPrefix(n, maxBytes int, measure func(Handoff) int, apply func(k int) (Handoff, HandoffElision)) (Handoff, HandoffElision, bool) {
	all, allElision := apply(n)
	if measure(withElision(all, allElision)) > maxBytes {
		return all, allElision, false
	}
	lo, hi := 1, n // the answer is in [lo, hi]; apply(hi) fits
	best, bestElision := all, allElision
	for lo < hi {
		mid := (lo + hi) / 2
		trial, elision := apply(mid)
		if measure(withElision(trial, elision)) <= maxBytes {
			hi, best, bestElision = mid, trial, elision
		} else {
			lo = mid + 1
		}
	}
	return best, bestElision, true
}

// withElision is h as it will render once elision is recorded, so a
// measurement includes the footer line that describes it.
func withElision(h Handoff, elision HandoffElision) Handoff {
	if elision.Count == 0 {
		return h
	}
	h.Elisions = append(append([]HandoffElision(nil), h.Elisions...), elision)
	return h
}

// protectedStart returns, for each exchange, the index of its first
// protected step: steps from there on belong to the protected tail. A step is
// protected when its exchange is among the last keptExchanges and it is among
// the last keptSteps steps of the whole conversation.
func protectedStart(exchanges []HandoffExchange, keptExchanges, keptSteps int) []int {
	starts := make([]int, len(exchanges))
	remaining := keptSteps
	for i := len(exchanges) - 1; i >= 0; i-- {
		n := len(exchanges[i].Steps)
		switch {
		case i < len(exchanges)-keptExchanges || remaining == 0:
			starts[i] = n
		case n <= remaining:
			starts[i] = 0
			remaining -= n
		default:
			starts[i] = n - remaining
			remaining = 0
		}
	}
	return starts
}

func dropToolOutput(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	n := 0
	for _, step := range steps[:limit] {
		if step.Tool != nil && step.Tool.Result != "" {
			step.Tool.Result, step.Tool.ResultOmitted = "", true
			n++
		}
	}
	return steps, n
}

// collapseToolCalls replaces the tool-call steps before limit with one
// "collapsed" step, at the position of the first, that counts them by name.
func collapseToolCalls(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	counts := map[string]int{}
	var order []string
	out := make([]HandoffStep, 0, len(steps))
	at, total := -1, 0
	for i, step := range steps {
		if i >= limit || step.Tool == nil {
			out = append(out, step)
			continue
		}
		if at < 0 {
			at = len(out)
			out = append(out, HandoffStep{Kind: HandoffStepCollapsed})
		}
		if counts[step.Tool.Name] == 0 {
			order = append(order, step.Tool.Name)
		}
		counts[step.Tool.Name]++
		total++
	}
	if total == 0 {
		return steps, 0
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	parts := make([]string, 0, len(order))
	for _, name := range order {
		// A tool name is recorded data: a code span keeps it one inert line.
		parts = append(parts, fmt.Sprintf("%s ×%d", codeSpan(name), counts[name]))
	}
	noun := "tool calls"
	if total == 1 {
		noun = "tool call"
	}
	out[at].Text = fmt.Sprintf("%d %s: %s", total, noun, strings.Join(parts, ", "))
	out[at].Count = total
	return out, total
}

func shortenAssistantText(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	n := 0
	for i := range steps[:limit] {
		step := &steps[i]
		if (step.Kind == HandoffStepText || step.Kind == HandoffStepSummary) && len(step.Text) > handoffAssistantTextCap {
			step.Text = TruncateUTF8(step.Text, handoffAssistantTextCap)
			step.TextTruncated = true
			n++
		}
	}
	return steps, n
}

// cloneHandoff copies everything FitHandoff mutates.
func cloneHandoff(h Handoff) Handoff {
	out := h
	out.Elisions = append([]HandoffElision(nil), h.Elisions...)
	out.Exchanges = make([]HandoffExchange, len(h.Exchanges))
	for i, exchange := range h.Exchanges {
		out.Exchanges[i] = cloneHandoffExchange(exchange)
	}
	return out
}

// cloneHandoffExchange returns a copy of exchange that shares nothing a
// budget step changes.
func cloneHandoffExchange(exchange HandoffExchange) HandoffExchange {
	copied := exchange
	if exchange.Steps != nil {
		copied.Steps = make([]HandoffStep, len(exchange.Steps))
	}
	for j, step := range exchange.Steps {
		if step.Tool != nil {
			tool := *step.Tool
			step.Tool = &tool
		}
		copied.Steps[j] = step
	}
	if exchange.Condensed != nil {
		condensed := *exchange.Condensed
		copied.Condensed = &condensed
	}
	return copied
}

// BuildHandoffWithAnalysis renders a handoff from one previously derived analysis.
func BuildHandoffWithAnalysis(bundle SourceBundle, analysis Analysis, metadata *Metadata, opts HandoffOptions) (Handoff, error) {
	view := analysis.View
	var exchanges []HandoffExchange
	var leftOff string
	var plan []HandoffPlanItem
	var files []string
	toolResultsUnavailable := !analysis.Observability.ToolResults.Available()
	if analysis.Facts.TextOnly {
		// A Cursor text transcript: role sections, no records to walk.
		exchanges, leftOff = textTurnsExchanges(view.Turns, opts)
		toolResultsUnavailable = false
	} else {
		root := analysis.Facts.WorkspaceRoot
		exchanges, leftOff, plan = selectHandoffExchanges(handoffEvents(view), root, opts)
		files = sessionFilesTouched(view.ToolCalls, root)
	}
	directory := path.Base(analysis.Facts.WorkspaceRoot)
	if analysis.Facts.WorkspaceRoot == "" {
		directory = ""
	}
	h := Handoff{
		Version:                HandoffVersion,
		Session:                handoffSession(bundle, view, metadata, opts),
		Workspace:              compareWorkspace(HandoffWorkspace{Directory: directory, Branch: analysis.Facts.Branch}, analysis.Facts.WorkspaceRoot, opts.Checkout),
		ToolResultsUnavailable: toolResultsUnavailable,
		Exchanges:              exchanges,
		LeftOff:                leftOff,
		Plan:                   plan,
		FilesTouched:           files,
		Gaps:                   countGaps(bundle.Capture.Gaps),
	}
	return displayHandoff(h), nil
}

func textTurnsExchanges(turns []NormalizedTurn, opts HandoffOptions) ([]HandoffExchange, string) {
	exchanges := []HandoffExchange{}
	current := &HandoffExchange{}
	leftOff := ""
	var kind TurnKind
	body := []string{}
	flushSection := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		switch kind {
		case TurnKindHumanPrompt:
			if current.Prompt != "" || len(current.Steps) > 0 {
				exchanges = append(exchanges, *current)
			}
			current = &HandoffExchange{Prompt: strings.TrimSpace(text)}
		case TurnKindAssistant:
			if text != "" {
				current.Steps = append(current.Steps, HandoffStep{Kind: HandoffStepText, Text: text})
				leftOff = text
			}
		case TurnKindToolResult:
			if text != "" {
				current.Steps = append(current.Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{
					Name: "tool", Summary: firstLine(text, handoffSummaryCap),
					Result: trimResult(text, opts.resultLines(), opts.resultBytes()), ResultLines: lineCount(text), ResultBytes: len(text),
				}})
			}
		case TurnKindCommandOutput, TurnKindShellCommand, TurnKindLocalCommand, TurnKindCompactSummary, TurnKindHarnessNotification:
		// Text formats do not establish these native structured categories.
		case TurnKindHarnessMeta:
			// FilterText omitted hidden sections, and only a visible role
			// starts one here.
		}
		kind, body = "", body[:0]
	}
	for _, turn := range turns {
		kind = turn.Kind
		body = append(body, turnDisplayText(turn))
		flushSection()
	}

	if current.Prompt != "" || len(current.Steps) > 0 {
		exchanges = append(exchanges, *current)
	}
	return exchanges, leftOff
}
