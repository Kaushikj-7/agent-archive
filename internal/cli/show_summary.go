package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// sessionView is what `show` reads for one session: its metadata sidecar and
// the live availability of each linked session. `show --json` prints it as
// the sidecar's own fields plus `linked_session_availability`.
type sessionView struct {
	archive.Metadata
	SelectedRevisionID string                      `json:"selected_revision_id,omitempty"`
	LinkedAvailability []reader.LinkedAvailability `json:"linked_session_availability,omitempty"`
}

// summaryOptions controls renderSessionSummary.
type summaryOptions struct {
	Now      time.Time
	Location *time.Location // times are shown in it; nil means time.Local
	Style    textStyle
	// Projects labels a project ID when the sidecar has no project_name.
	Projects map[string]string
	// Hints adds the lines naming the transcript and JSON commands. They
	// name the full session ID and its harness, which together are never
	// ambiguous.
	Hints bool
}

// summaryWidth is the column the header's status is aligned to, and the
// widest a line gets when the terminal's width is not known.
const summaryWidth = 80

// renderSessionSummary writes the human summary `show SESSION_ID` prints: a
// title, one line of context, labelled rows for what the metadata records
// (with what the archive omitted by design as one row), the capture gaps
// that mean content is missing, and identifiers. A row whose data is absent is left out;
// a nil count is unknown, never zero. It prints metadata only, never
// conversation content, and every metadata string goes through
// archive.DisplayLine.
func renderSessionSummary(w io.Writer, view sessionView, opts summaryOptions) {
	m := view.Metadata
	s := opts.Style
	width := lineWidth(s)
	terminal.Println(w, s.bold(ellipsize(summaryTitle(m), width)))
	if view.SelectedRevisionID != "" {
		terminal.Println(w, s.dim("Parent revision: "+archive.DisplayLine(view.SelectedRevisionID)))
	}
	context := ellipsize(strings.Join(summaryContext(m, opts), " · "), width-16)
	if status := summaryStatus(m, s); status != "" {
		gap := max(width-visibleWidth(context)-visibleWidth(status), 2)
		terminal.Println(w, s.dim(context)+strings.Repeat(" ", gap)+status)
	} else {
		terminal.Println(w, s.dim(context))
	}
	terminal.Println(w)

	for _, row := range summaryRows(view, opts, width-12) {
		for i, value := range row.values {
			label := strings.Repeat(" ", 9)
			if i == 0 {
				label = s.dim(fmt.Sprintf("%-9s", row.label))
			}
			value = ellipsize(value, width-12)
			if row.quiet {
				value = s.dim(value)
			}
			terminal.Printf(w, "  %s %s\n", label, value)
		}
	}

	if gaps := summaryGaps(m.CaptureGaps, width); len(gaps) > 0 {
		terminal.Println(w)
		terminal.Printf(w, "  %s %s\n", s.warnMark(), s.bold("Incomplete capture"))
		for _, line := range gaps {
			terminal.Println(w, "    "+line)
		}
	}

	terminal.Println(w)
	// The ID and the commands are meant to be copied, so they are cut only
	// to fit a terminal narrower than they are.
	cut := func(text string, indent int) string {
		if s.width > 0 {
			return ellipsize(text, s.width-indent)
		}
		return text
	}
	id, provenance := archive.DisplayLine(m.SessionID), strings.Join(summaryProvenance(m), " · ")
	if 5+visibleWidth(id)+3+visibleWidth(provenance) <= width {
		terminal.Println(w, "  "+s.dim("ID")+" "+id+"   "+s.dim(provenance))
	} else {
		terminal.Println(w, "  "+s.dim("ID")+" "+cut(id, 5))
		terminal.Println(w, "     "+s.dim(ellipsize(provenance, width-5)))
	}
	if opts.Hints {
		command := "agent-archive show " + shellWord(id)
		if harness := archive.DisplayLine(m.Harness.Name); harness != "" {
			command += " --harness " + shellWord(harness)
		}
		if view.SelectedRevisionID != "" {
			command += " --revision " + shellWord(archive.DisplayLine(view.SelectedRevisionID))
		}
		terminal.Println(w)
		terminal.Printf(w, "  %s %s\n", s.dim("Transcript:"), s.cmd(cut(command+" --transcript", 14)))
		terminal.Printf(w, "  %s %s\n", s.dim("JSON:      "), s.cmd(cut(command+" --json", 14)))
	}
}

// summaryTitle is the session's display title (the name its agent gave it,
// else the first prompt) as one display line, or its short ID when it has
// neither.
func summaryTitle(m archive.Metadata) string {
	if title := strings.TrimSpace(archive.DisplayLine(archive.DisplayTitle(m))); title != "" {
		return title
	}
	return archive.DisplayLine(shortSessionID(m.SessionID))
}

// summaryRow is one labelled row; values after the first continue it on
// lines of their own.
type summaryRow struct {
	label  string
	values []string
	quiet  bool // values are dimmed
}

// summaryRows builds the labelled rows; a list too long for width
// continues on further lines.
func summaryRows(view sessionView, opts summaryOptions, width int) []summaryRow {
	m := view.Metadata
	var rows []summaryRow
	add := func(label string, values ...string) {
		if len(values) > 0 && values[0] != "" {
			rows = append(rows, summaryRow{label: label, values: values})
		}
	}
	// The heading is the name when the session has one, so the first prompt,
	// which was the heading before, is a row of its own.
	if m.Name != "" {
		add("Prompt", archive.DisplayLine(m.Title))
	}
	add("When", summaryWhen(m, opts))
	agent := appName(archive.DisplayLine(m.Harness.Name))
	if v := archive.DisplayLine(m.Harness.Version); v != "" {
		agent += " " + v
	}
	add("Agent", strings.TrimSpace(agent))
	add("Model", summaryModels(m.Models)...)
	add("Activity", wrapList(summaryActivity(m.Counts), " · ", width)...)
	add("Tools", wrapList(summaryTools(m.ToolsUsed), " · ", width)...)
	add("Branch", archive.DisplayLine(m.Branch))
	add("Git", wrapList(summaryGit(m), " · ", width)...)
	add("Commit", wrapList(summaryCommit(m.GitHead), " · ", width)...)
	add("PRs", wrapList(summaryLinkedPRs(m.PullRequests), " · ", width)...)
	add("Skills", wrapList(displayAll(skillNames(m)), ", ", width)...)
	add("Subagents", summarySubagents(m, view.LinkedAvailability))
	if m.NativeChild && m.ParentSessionID == "" {
		add("Parent", "archive link pending")
	}
	if m.ParentSessionID != "" {
		add("Parent", archive.DisplayLine(shortSessionID(m.ParentSessionID)))
	}
	if m.Origin == archive.SessionOriginImport {
		imported := "by backfill"
		if m.ImportedAt != nil {
			imported = formatSummaryTime(*m.ImportedAt, opts) + " by backfill"
		}
		add("Imported", imported)
	}
	if omitted := summaryOmitted(m.CaptureGaps); len(omitted) > 0 {
		add("Omitted", wrapList(omitted, ", ", width)...)
		rows[len(rows)-1].quiet = true
	}
	return rows
}

// summaryContext is the line under the title: harness, project, and how
// long ago the session was last active, as list's WHEN says.
func summaryContext(m archive.Metadata, opts summaryOptions) []string {
	var parts []string
	if name := archive.DisplayLine(m.Harness.Name); name != "" {
		parts = append(parts, name)
	}
	project := m.ProjectName
	if project == "" {
		project = opts.Projects[m.ProjectID]
	}
	if project = archive.DisplayLine(project); project != "" {
		parts = append(parts, project)
	}
	if active := lastActivity(m); !active.IsZero() {
		parts = append(parts, compactAge(opts.Now, active))
	}
	return parts
}

// compactAge is relativeAge in the header's short form ("2h ago").
func compactAge(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// summaryStatus is the header's right-hand status: how the last turn ended,
// or that a turn may still be running.
func summaryStatus(m archive.Metadata, s textStyle) string {
	if m.State == archive.MetadataStateActive {
		return s.warnMark() + " active"
	}
	switch m.TurnOutcome {
	case archive.TurnOutcomeCompleted:
		return s.okMark() + " completed"
	case archive.TurnOutcomeInterrupted:
		return s.warnMark() + " interrupted"
	case archive.TurnOutcomeError:
		return s.failMark() + " error"
	case archive.TurnOutcomeUnknown:
	}
	return ""
}

// sessionEnd is when the session's recorded activity ends, and whether
// that is the recorded end of the session: ended_at, the latest record
// timestamp, when the parser found one. Otherwise it is lastActivity, as
// the header and list's WHEN say: for a hook capture the capture time, the
// latest activity the capture can include, shown as a span rather than a
// duration; for an import its start, since its capture time is when
// backfill ran.
func sessionEnd(m archive.Metadata) (end time.Time, exact bool) {
	if m.EndedAt != nil {
		return *m.EndedAt, true
	}
	return lastActivity(m), false
}

// summaryWhen is "Sep 29, 10:14 → 11:02 (48m span)" in opts' location.
func summaryWhen(m archive.Metadata, opts summaryOptions) string {
	end, exact := sessionEnd(m)
	switch {
	case m.StartedAt.IsZero() && end.IsZero():
		return ""
	case m.StartedAt.IsZero():
		return "captured " + formatSummaryTime(end, opts)
	case end.IsZero() || !end.After(m.StartedAt):
		return formatSummaryTime(m.StartedAt, opts)
	}
	loc := summaryLocation(opts)
	start, stop := m.StartedAt.In(loc), end.In(loc)
	endText := stop.Format("15:04")
	if start.YearDay() != stop.YearDay() || start.Year() != stop.Year() {
		endText = formatSummaryTime(end, opts)
	}
	length := formatSpan(end.Sub(m.StartedAt))
	if !exact {
		length += " span"
	}
	return fmt.Sprintf("%s → %s (%s)", formatSummaryTime(m.StartedAt, opts), endText, length)
}

func summaryLocation(opts summaryOptions) *time.Location {
	if opts.Location != nil {
		return opts.Location
	}
	return time.Local
}

// formatSummaryTime is "Sep 29, 10:14", with the year when it is not the
// current one.
func formatSummaryTime(t time.Time, opts summaryOptions) string {
	loc := summaryLocation(opts)
	local := t.In(loc)
	if !opts.Now.IsZero() && local.Year() != opts.Now.In(loc).Year() {
		return local.Format("Jan 2 2006, 15:04")
	}
	return local.Format("Jan 2, 15:04")
}

// formatSpan is a duration in whole minutes: "48m", "3h 05m", "2d 4h".
func formatSpan(d time.Duration) string {
	minutes := int(d / time.Minute)
	switch {
	case minutes < 1:
		return "<1m"
	case minutes < 60:
		return fmt.Sprintf("%dm", minutes)
	case minutes < 48*60:
		return fmt.Sprintf("%dh %02dm", minutes/60, minutes%60)
	default:
		return fmt.Sprintf("%dd %dh", minutes/(24*60), minutes%(24*60)/60)
	}
}

// summaryModels is one line per model: its name, reasoning level, and how
// many turns it answered when counted. A hook-reported model that repeats a
// transcript one adds nothing.
func summaryModels(models []archive.ModelSummary) []string {
	type entry struct {
		name      string
		reasoning string
		turns     *int
	}
	var entries []*entry
	byKey := map[string]*entry{}
	for _, model := range models {
		name := ""
		for _, key := range []string{"gen_ai.response.model", "gen_ai.request.model", "agent_archive.request.model_label"} {
			if name = archive.DisplayLine(model.Attributes[key]); name != "" {
				break
			}
		}
		if name == "" || (strings.HasPrefix(name, "<") && strings.HasSuffix(name, ">")) {
			continue
		}
		reasoning := archive.DisplayLine(model.Attributes["agent_archive.request.reasoning_level"])
		key := name + "\x00" + reasoning
		existing := byKey[key]
		if existing == nil {
			existing = &entry{name: name, reasoning: reasoning}
			byKey[key] = existing
			entries = append(entries, existing)
		}
		if model.TurnCount != nil {
			total := *model.TurnCount
			if existing.turns != nil {
				total += *existing.turns
			}
			existing.turns = &total
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ti, tj := -1, -1
		if entries[i].turns != nil {
			ti = *entries[i].turns
		}
		if entries[j].turns != nil {
			tj = *entries[j].turns
		}
		return ti > tj
	})
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		line := e.name
		if e.reasoning != "" {
			line += " (" + e.reasoning + " reasoning)"
		}
		if e.turns != nil {
			line += " · " + plural(*e.turns, "response")
		}
		lines = append(lines, line)
	}
	return lines
}

// summaryActivity lists the counts the metadata knows, skipping unknown
// (nil) ones rather than showing them as zero. Shell commands and
// compactions are occasional, and many sessions edit no files, so a known
// zero of those is left out too; turns, messages, and tool calls are shown
// even when zero.
func summaryActivity(c archive.Counts) []string {
	var parts []string
	add := func(n *int, unit string, showZero bool) {
		if n != nil && (showZero || *n != 0) {
			parts = append(parts, plural(*n, unit))
		}
	}
	add(c.Turns, "turn", true)
	add(c.Messages, "message", true)
	add(c.ToolCalls, "tool call", true)
	add(c.UserShellCommands, "shell command", false)
	add(c.Compactions, "compaction", false)
	if c.FilesTouched != nil && *c.FilesTouched != 0 {
		parts = append(parts, plural(*c.FilesTouched, "file")+" edited")
	}
	return parts
}

// summaryTools lists the most-called tools with their counts, most-called
// first, as metadata orders them: "Bash 42 · Edit 18".
func summaryTools(tools []archive.ToolUsage) []string {
	items := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := archive.DisplayLine(tool.Name); name != "" {
			items = append(items, fmt.Sprintf("%s %d", name, tool.Count))
		}
	}
	return items
}

// summaryGit lists the session's git work: how many commits and pushes,
// then each pull request opened and merged by number. When the event list
// was capped and holds fewer pull requests than were counted, the count is
// shown instead. Nothing when the session did none or the counts are
// unknown.
func summaryGit(m archive.Metadata) []string {
	var items []string
	if c := m.Counts.Commits; c != nil && *c > 0 {
		items = append(items, plural(*c, "commit"))
	}
	if c := m.Counts.Pushes; c != nil && *c > 0 {
		if *c == 1 {
			items = append(items, "1 push")
		} else {
			items = append(items, fmt.Sprintf("%d pushes", *c))
		}
	}
	items = append(items, summaryPullRequests(m.GitActivity, archive.GitEventPRCreated, m.Counts.PRsCreated, "opened")...)
	items = append(items, summaryPullRequests(m.GitActivity, archive.GitEventPRMerged, m.Counts.PRsMerged, "merged")...)
	return items
}

// summaryCommit is the commit the session started on, with whether its tree
// was dirty, and the last one a stop hook saw when that differs, each
// abbreviated to 12 digits. Empty when no hook recorded either.
func summaryCommit(head *archive.SessionGitHead) []string {
	if head == nil {
		return nil
	}
	var items []string
	if start := head.Start; start != nil {
		item := "started on " + shortCommit(start.SHA)
		if start.Dirty != nil && *start.Dirty {
			item += " with uncommitted changes"
		}
		items = append(items, item)
	}
	if last := head.Last; last != nil && (head.Start == nil || last.SHA != head.Start.SHA) {
		items = append(items, "last seen on "+shortCommit(last.SHA))
	}
	return items
}

// summaryLinkedPRs is owner/repo#n for each pull request the session was
// linked to, as many as the metadata holds.
func summaryLinkedPRs(links []archive.PullRequestLink) []string {
	items := make([]string, 0, len(links))
	for _, link := range links {
		items = append(items, fmt.Sprintf("%s#%d", archive.DisplayLine(link.Repository), link.Number))
	}
	return items
}

// shortCommit is a commit's first 12 hex digits, for reading, not for
// checking out: show --json has the full name.
func shortCommit(sha string) string {
	return archive.DisplayLine(sha[:min(12, len(sha))])
}

// summaryPullRequests is "PR #n <verb>" for each pull request of kind in
// events, or "N PRs <verb>" when count says events do not hold them all.
func summaryPullRequests(events []archive.GitEvent, kind archive.GitEventKind, count *int, verb string) []string {
	var items []string
	listed := 0
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		listed++
		if event.PRNumber > 0 {
			items = append(items, fmt.Sprintf("PR #%d %s", event.PRNumber, verb))
		}
	}
	if count != nil && *count > 0 && (*count > listed || len(items) < listed) {
		return []string{plural(*count, "PR") + " " + verb}
	}
	return items
}

// wrapList joins items with sep into lines at most width columns wide,
// breaking only between items. A line that breaks ends with sep's mark (" ·"
// ends in "·"), which counts toward its width, so no line is cut later. An
// item too wide for a line by itself is cut to fit one of its own.
func wrapList(items []string, sep string, width int) []string {
	mark := strings.TrimRight(sep, " ")
	var lines []string
	line := ""
	for i, item := range items {
		// The room this item leaves for the mark that follows it when the
		// line breaks after it.
		end := ""
		if i < len(items)-1 {
			end = mark
		}
		switch {
		case line == "":
			line = ellipsize(item, width-visibleWidth(end))
		case visibleWidth(line+sep+item+end) <= width:
			line += sep + item
		default:
			lines = append(lines, line+mark)
			line = ellipsize(item, width-visibleWidth(end))
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// summarySubagents is "2 linked (1 available, 1 expired)". Without live
// availability it counts the recorded links only.
func summarySubagents(m archive.Metadata, links []reader.LinkedAvailability) string {
	if len(m.LinkedSessions) == 0 && len(links) == 0 {
		return ""
	}
	if len(links) == 0 {
		return fmt.Sprintf("%d linked", len(m.LinkedSessions))
	}
	counts := map[string]int{}
	var order []string
	for _, link := range links {
		label := linkedStateLabel(link.State)
		if counts[label] == 0 {
			order = append(order, label)
		}
		counts[label]++
	}
	parts := make([]string, len(order))
	for i, label := range order {
		parts[i] = fmt.Sprintf("%d %s", counts[label], label)
	}
	return fmt.Sprintf("%d linked (%s)", len(links), strings.Join(parts, ", "))
}

func linkedStateLabel(state reader.LinkedState) string {
	switch state {
	case reader.LinkedStateMetadataAvailable:
		return "available"
	case reader.LinkedStatePending:
		return "pending"
	case reader.LinkedStateUnavailableOrExpired:
		return "expired"
	case reader.LinkedStateIdentityMismatch:
		return "mismatched"
	case reader.LinkedStateLookupFailed:
		return "unreadable"
	case reader.LinkedStateUnavailable:
		return "unavailable"
	}
	return archive.DisplayLine(string(state))
}

// routineGaps names, in the order the Omitted row lists them, the capture
// gap codes that record something the archive leaves out by design (the
// privacy filter, size caps) or because the parser does not recognize it.
// Neither means the capture went wrong, so the summary names them in one
// quiet row instead of warning about them. Codes that share a phrase are
// listed once.
var routineGaps = []routineGapPhrase{
	{"hidden_instruction_omitted", "injected instructions"},
	{"hidden_or_unknown_nested_content_omitted", "hidden fields"},
	{"sensitive_or_hidden_field_omitted", "sensitive fields"},
	{"sensitive_content_redacted", "redacted secrets"},
	{"binary_content_omitted", "binary content"},
	{"content_truncated", "truncated long content"},
	{"record_without_allowed_fields_omitted", "records with nothing kept"},
	{"cursor_context_omitted", "attached context"},
	{"cursor_tool_argument_omitted", "tool arguments"},
	{"supplemental_evidence_omitted", "supplemental evidence"},
	{archive.CaptureGapImportedWithoutHookEvidence, "hook events before import"},
	{"unknown_field_omitted", "unrecognized fields"},
	{"unknown_record_type", "unrecognized records"},
	{"cursor_message_type_unknown", "unrecognized messages"},
	{"unsupported_value_omitted", "unsupported values"},
}

type routineGapPhrase struct {
	code   string
	phrase string
}

func routineGap(code string) bool {
	for _, gap := range routineGaps {
		if gap.code == code {
			return true
		}
	}
	return false
}

// summaryOmitted is the Omitted row: a phrase for each routine gap the
// session has.
func summaryOmitted(gaps []archive.CaptureGap) []string {
	present := map[string]bool{}
	for _, gap := range gaps {
		present[gap.Code] = true
	}
	var phrases []string
	listed := map[string]bool{}
	for _, gap := range routineGaps {
		if present[gap.code] && !listed[gap.phrase] {
			listed[gap.phrase] = true
			phrases = append(phrases, gap.phrase)
		}
	}
	return phrases
}

// summaryGapLimit bounds how many distinct gap codes the summary lists.
const summaryGapLimit = 6

// summaryGaps is one line per capture gap code that may mean content is
// missing (every code routineGaps does not name, including ones this
// version does not know): the code, how many times it occurs, and the
// first detail recorded for it, cut to one line.
func summaryGaps(gaps []archive.CaptureGap, width int) []string {
	counts := map[string]int{}
	details := map[string]string{}
	var codes []string
	for _, gap := range gaps {
		code := archive.DisplayLine(gap.Code)
		if code == "" || routineGap(gap.Code) {
			continue
		}
		if counts[code] == 0 {
			codes = append(codes, code)
		}
		counts[code]++
		if details[code] == "" {
			details[code] = archive.DisplayLine(gap.Detail)
		}
	}
	var lines []string
	for i, code := range codes {
		if i == summaryGapLimit {
			lines = append(lines, fmt.Sprintf("… %d more (see --json)", len(codes)-i))
			break
		}
		line := code
		if counts[code] > 1 {
			line += fmt.Sprintf(" ×%d", counts[code])
		}
		if details[code] != "" {
			line += " — " + details[code]
		}
		lines = append(lines, ellipsize(line, width-4))
	}
	return lines
}

// summaryProvenance is the footer's origin, parser, and filter versions.
func summaryProvenance(m archive.Metadata) []string {
	parts := []string{"origin " + sessionOrigin(m)}
	if v := archive.DisplayLine(m.Parser.Version); v != "" {
		parser := "parser " + v
		if status := archive.DisplayLine(string(m.Parser.Status)); status != "" && m.Parser.Status != archive.ParserStatusComplete {
			parser += " (" + status + ")"
		}
		parts = append(parts, parser)
	}
	if v := archive.DisplayLine(m.FilterVersion); v != "" {
		parts = append(parts, "filter "+v)
	}
	return parts
}

// lineWidth is the width summary lines are cut to.
func lineWidth(s textStyle) int {
	if s.width > 0 && s.width < summaryWidth {
		return s.width
	}
	return summaryWidth
}

// ellipsize cuts text to at most limit columns, ending a cut with "…".
func ellipsize(text string, limit int) string {
	if limit < 2 || visibleWidth(text) <= limit {
		return text
	}
	return truncateVisible(text, limit-1) + "…"
}

func displayAll(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = archive.DisplayLine(name)
	}
	return out
}
