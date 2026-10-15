package cli

import (
	"io"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printSessionTable writes the human list table for rows. When GroupByDay
// is set the rows are printed under day headings (indices stay global for
// the interactive picker).
func printSessionTable(w io.Writer, rows []listRow, opts listFormatOptions) error {
	return printSessionGroups(w, sessionTableGroups(rows, opts), opts)
}

// sessionTableGroup is the rows under one day heading of the human list
// table. The flat table is a single group without a heading.
type sessionTableGroup struct {
	// label is the day's heading, styled; empty for the flat table.
	label string
	rows  []listRow
	// continued marks a group whose earlier rows are on a previous page.
	continued bool
}

// sessionTableGroups arranges rows as printSessionTable prints them. Their
// rows, taken in order, are the table's rows from top to bottom.
//
// With GroupByDay (and not Verbose), the rows go under the heading of their
// day (dayBucket of Active at Now), the days newest first and the rows in
// the order given within a day. A subagent shown under its parent (Depth)
// stays under it, in the parent's day. One day still gets its heading: it
// is the landmark the reader learns.
func sessionTableGroups(rows []listRow, opts listFormatOptions) []sessionTableGroup {
	if !opts.GroupByDay || opts.Verbose || len(rows) == 0 {
		return []sessionTableGroup{{rows: rows}}
	}
	byDay := map[dayBucketKind][]listRow{}
	day := bucketToday
	for _, r := range rows {
		if r.Depth == 0 {
			day = dayBucket(opts.Now, r.Active)
		}
		byDay[day] = append(byDay[day], r)
	}
	var groups []sessionTableGroup
	for d := bucketToday; d <= bucketOlder; d++ {
		if len(byDay[d]) == 0 {
			continue
		}
		label := d.label()
		if opts.Style.color {
			label = opts.Style.bold(label)
		}
		groups = append(groups, sessionTableGroup{label: label, rows: byDay[d]})
	}
	return groups
}

// printSessionGroups writes groups as the human list table. On a terminal of
// known width (opts.Width) each group's heading, when it has one, and each
// row take exactly one line (renderRow), with no column header. Otherwise it
// is the plain table: the column header, then a row a line, under each
// group's heading when it has one. The verbose table keeps its own columns
// and a header under each heading.
func printSessionGroups(w io.Writer, groups []sessionTableGroup, opts listFormatOptions) error {
	if opts.Width > 0 && !opts.Verbose {
		return printSessionLines(w, groups, opts)
	}
	for i, group := range groups {
		if group.label != "" {
			if i > 0 {
				terminal.Println(w)
			}
			terminal.Println(w, groupHeading(group))
		}
		tw := &columnWriter{out: w}
		printSessionHeader(tw, opts)
		for _, r := range group.rows {
			printSessionRow(tw, r, opts)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// groupHeading is a group's heading line: its label, and that it goes on
// from a previous page when it does.
func groupHeading(group sessionTableGroup) string {
	if group.continued {
		return group.label + " (continued)"
	}
	return group.label
}

// printSessionLines writes groups one terminal line a row, laid out by
// opts.layout, or by a layout of the rows given when there is none.
func printSessionLines(w io.Writer, groups []sessionTableGroup, opts listFormatOptions) error {
	layout := opts.layout
	if layout == nil || layout.width != opts.Width {
		var rows []listRow
		for _, group := range groups {
			rows = append(rows, group.rows...)
		}
		laid := newSessionLayout(rows, opts)
		layout = &laid
	}
	var b strings.Builder
	for i, group := range groups {
		if group.label != "" {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(oneRow(groupHeading(group), opts.Width))
			b.WriteString("\n")
		}
		for _, r := range group.rows {
			b.WriteString(layout.renderRow(r, opts.Style))
			b.WriteString("\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// sessionLines is how many terminal lines printSessionLines takes for
// groups: a line a heading and a row, and a blank line before each heading
// after the first group.
func sessionLines(groups []sessionTableGroup) int {
	n := 0
	for i, group := range groups {
		if group.label != "" {
			n++
			if i > 0 {
				n++
			}
		}
		n += len(group.rows)
	}
	return n
}

func printSessionHeader(tw io.Writer, opts listFormatOptions) {
	if opts.Verbose {
		if opts.Numbered {
			terminal.Println(tw, "#\tTITLE\tSESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED\tPROJECT")
			return
		}
		terminal.Println(tw, "TITLE\tSESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED\tPROJECT")
		return
	}
	terminal.Println(tw, strings.Join(tableHeader(opts), "\t"))
}

// tableHeader is the column names of the plain table, left out where opts
// hides a column: WHEN AGENT PROJECT PR ID TITLE, led by # when the rows are
// numbered. TITLE comes last, so a long one pushes no other column.
func tableHeader(opts listFormatOptions) []string {
	var cols []string
	if opts.Numbered {
		cols = append(cols, "#")
	}
	cols = append(cols, "WHEN")
	if !opts.HideHarness {
		cols = append(cols, "AGENT")
	}
	if !opts.HideProject {
		cols = append(cols, "PROJECT")
	}
	if opts.ShowPR {
		cols = append(cols, "PR")
	}
	return append(cols, "ID", "TITLE")
}

// subagentHint is the note after a title about the subagents, for a row of
// the width-aware table: "+45" after a parent, "subagent of d7a77938" after
// a subagent that is not shown under its parent.
func subagentHint(r listRow) string {
	switch {
	case r.Depth > 0:
		// Shown under its parent, which the indent says.
		return ""
	case r.Parent != "":
		return "subagent of " + r.Parent
	case r.Children > 0:
		return "+" + strconv.Itoa(r.Children)
	}
	return ""
}

// subagentNote is what follows a title about the subagents where it is
// spelled out: " · 45 subagents" after a parent, " · subagent of d7a77938"
// after a subagent.
func subagentNote(r listRow) string {
	switch {
	case r.Depth > 0:
		return ""
	case r.Parent != "":
		return " · subagent of " + r.Parent
	case r.Children > 0:
		return " · " + plural(r.Children, "subagent")
	}
	return ""
}

// liveWhen is the WHEN cell of a session active right now, in place of
// its time. A word, not a mark: the agent column's dot is a mark already.
const liveWhen = "live"

// whenText is a row's WHEN cell: liveWhen for a session active right now,
// else its short time.
func whenText(r listRow) string {
	if r.Live {
		return liveWhen
	}
	return r.WhenShort
}

// cursorMark leads the title of the row Enter acts on while the browser
// filters.
const cursorMark = "▸"

// subagentMark leads the title of a subagent shown under its parent.
const subagentMark = "↳"

// rowNumber is the number a row is chosen by, which is blank for a row that
// had none before the filter brought it in (a subagent).
func rowNumber(r listRow) string {
	if r.Index <= 0 {
		return ""
	}
	return strconv.Itoa(r.Index)
}

// cellTitle is the title a row's title cell shows: its readable list title,
// or the title itself when it is a stand-in such as a short ID.
func cellTitle(r listRow) string {
	if r.ListTitle != "" {
		return r.ListTitle
	}
	return r.Title
}

// indentedTitle is the title of a subagent shown under its parent, set in
// from the parents' titles.
func indentedTitle(r listRow) string {
	return titlePrefix(r) + r.Title
}

// titlePrefix sets a subagent shown under its parent in from the parents'
// titles; empty for every other row.
func titlePrefix(r listRow) string {
	if r.Depth <= 0 {
		return ""
	}
	return strings.Repeat("  ", r.Depth) + subagentMark + " "
}

// cursorCell leads a title with the mark of the row Enter acts on, or room
// for it, when the table has one.
func cursorCell(r listRow, opts listFormatOptions) string {
	switch {
	case !opts.Cursor:
		return ""
	case r.Highlight:
		return cursorMark + " "
	}
	return "  "
}

// tableCells is one row of the plain table, in tableHeader's columns. Its
// title is the full display title (Title, not the readable ListTitle the
// width-aware cell shows), so grep or a pager finds the session's own words,
// uncut, with the subagent note and the hint spelled out after it.
func tableCells(r listRow, opts listFormatOptions) []string {
	title := indentedTitle(r) + subagentNote(r)
	if opts.Numbered {
		// A picker whose terminal's size is unknown: its mark still shows.
		title = cursorCell(r, opts) + title
	}
	if r.Hint != "" {
		title += " · " + r.Hint
	}
	var cells []string
	if opts.Numbered {
		cells = append(cells, rowNumber(r))
	}
	cells = append(cells, whenText(r))
	if !opts.HideHarness {
		cells = append(cells, r.Harness)
	}
	if !opts.HideProject {
		cells = append(cells, r.Project)
	}
	if opts.ShowPR {
		cells = append(cells, orDash(r.PR))
	}
	return append(cells, archive.DisplayLine(r.ShortID), title)
}

// orDash is text, or "-" when it is empty, so a plain table's cell is never
// blank. The columns still do not split on whitespace: WHEN can hold a
// space ("Sep 20") and TITLE often does; scripts read list --json.
func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

func printSessionRow(tw io.Writer, r listRow, opts listFormatOptions) {
	if opts.Verbose {
		if opts.Numbered {
			terminal.Printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				rowNumber(r), cursorCell(r, opts)+indentedTitle(r), archive.DisplayLine(r.SessionID), r.Harness, r.CapturedAt, r.Origin, r.Parser, r.ModelAll, r.Skills, r.Project)
			return
		}
		terminal.Printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			cursorCell(r, opts)+indentedTitle(r), archive.DisplayLine(r.SessionID), r.Harness, r.CapturedAt, r.Origin, r.Parser, r.ModelAll, r.Skills, r.Project)
		return
	}
	terminal.Println(tw, strings.Join(tableCells(r, opts), "\t"))
}

// The title column of a width-aware row is the room the other columns leave,
// kept between minTitleWidth and maxTitleWidth so the project, PR and time
// stay next to the title on a wide terminal.
const (
	minTitleWidth = 24
	maxTitleWidth = 56
)

// maxProjectWidth is the widest the project column of a width-aware row
// gets; a longer name is cut.
const maxProjectWidth = 24

// projectTitleWidth is the narrowest title column the project column is
// kept for: the title shrinks from maxTitleWidth to this before the project
// goes, and below it the project goes first.
const projectTitleWidth = 36

// sessionLayout is how a table's rows are laid out on a terminal of known
// width, so each takes exactly one line: which columns are shown and how
// wide each is. Its columns are the number (led by the filter's cursor
// mark), the agent (a colored dot and its name), the title (led by a
// subagent's indent), the project, the PR, and the short time, or "live".
type sessionLayout struct {
	width int
	// numbered and cursor lead each row with its number, and with the
	// filter's mark or room for it; numW is the widest number.
	numbered bool
	cursor   bool
	numW     int
	// agent shows the agent's dot, and agentName its name after it, as wide
	// as the widest, agentW.
	agent     bool
	agentName bool
	agentW    int
	// title is the width of the title column.
	title int
	// project and pr show those columns, projectW and prW wide.
	project  bool
	projectW int
	pr       bool
	prW      int
	// whenW is the widest WHEN cell (whenText).
	whenW int
}

// newSessionLayout lays rows out for a terminal opts.Width columns wide,
// with the columns opts leaves in (see withColumns). The title gets the room
// the others leave, at most maxTitleWidth. The project column is kept while
// that leaves the title projectTitleWidth; then it goes, and while the room
// is under minTitleWidth the PR goes, then the agent's name (its dot
// stays).
func newSessionLayout(rows []listRow, opts listFormatOptions) sessionLayout {
	l := sessionLayout{
		width: opts.Width, numbered: opts.Numbered, cursor: opts.Cursor,
		agent: !opts.HideHarness, agentName: !opts.HideHarness,
		project: !opts.HideProject, pr: opts.ShowPR,
	}
	for _, r := range rows {
		l.numW = max(l.numW, len(rowNumber(r)))
		_, name := agentLabel(rowHarness(r))
		l.agentW = max(l.agentW, visibleWidth(name))
		l.projectW = max(l.projectW, min(visibleWidth(r.Project), maxProjectWidth))
		l.prW = max(l.prW, visibleWidth(r.PR))
		l.whenW = max(l.whenW, visibleWidth(whenText(r)))
	}
	if l.prW == 0 {
		l.pr = false
	}
	for {
		room, least := l.width-l.othersWidth(), minTitleWidth
		if l.project {
			least = projectTitleWidth
		}
		if room >= least {
			l.title = min(room, maxTitleWidth)
			return l
		}
		switch {
		case l.project:
			l.project = false
		case l.pr:
			l.pr = false
		case l.agentName:
			l.agentName = false
		default:
			// Nothing more to drop: the title takes what is left, and the
			// row is cut to the width if even that is too much.
			l.title = max(room, 1)
			return l
		}
	}
}

// othersWidth is the columns every cell but the title's takes, with the
// gaps between all the cells.
func (l sessionLayout) othersWidth() int {
	widths := []int{l.whenW}
	if lead := l.leadWidth(); lead > 0 {
		widths = append(widths, lead)
	}
	if l.agent {
		widths = append(widths, l.agentWidth())
	}
	if l.project {
		widths = append(widths, l.projectW)
	}
	if l.pr {
		widths = append(widths, l.prW)
	}
	// One more cell, the title, and a gap between each two.
	total := columnGap * len(widths)
	for _, w := range widths {
		total += w
	}
	return total
}

// leadWidth is the width of the cell before the agent: the cursor mark's
// room and the number.
func (l sessionLayout) leadWidth() int {
	w := 0
	if l.cursor {
		w += 2
	}
	if l.numbered {
		w += l.numW
	}
	return w
}

// agentWidth is the width of the agent cell: the dot, and the name after a
// space when it is shown.
func (l sessionLayout) agentWidth() int {
	if !l.agentName {
		return visibleWidth(agentDot)
	}
	return visibleWidth(agentDot) + 1 + l.agentW
}

// rowHarness is the harness a row's agent column names.
func rowHarness(r listRow) string {
	if r.HarnessKey != "" {
		return r.HarnessKey
	}
	return r.Harness
}

// renderRow is r as exactly one terminal line, at most l.width columns wide:
// its cells padded to the layout's columns, the title cut at a word with
// "…" to fit its column (see titleCell), and WHEN "live", green, for a
// session active right now.
func (l sessionLayout) renderRow(r listRow, style textStyle) string {
	var cells []string
	if lead := l.leadWidth(); lead > 0 {
		cell := ""
		switch {
		case l.cursor && r.Highlight:
			cell = cursorMark + " "
		case l.cursor:
			cell = "  "
		}
		if l.numbered {
			cell += padLeft(rowNumber(r), l.numW)
		}
		cells = append(cells, cell)
	}
	if l.agent {
		harness := rowHarness(r)
		dot, name := agentLabel(harness)
		cell := style.agent(harness, dot)
		if l.agentName {
			cell += " " + padRight(name, l.agentW)
		}
		cells = append(cells, cell)
	}
	cells = append(cells, l.titleCell(r, style))
	if l.project {
		cells = append(cells, padRight(cutAtWord(r.Project, l.projectW), l.projectW))
	}
	if l.pr {
		cells = append(cells, padRight(r.PR, l.prW))
	}
	when := padLeft(whenText(r), l.whenW)
	if r.Live {
		when = style.ok(when)
	}
	cells = append(cells, when)
	line := strings.Join(cells, strings.Repeat(" ", columnGap))
	if visibleWidth(line) > l.width {
		// Only a terminal too narrow for the fewest columns gets here.
		line = truncateVisible(line, l.width)
	}
	return line
}

// titleCell is the title column of a row, l.title wide: a subagent's
// indent, the title cut to what is left, then dim notes. The row's hint
// (rowHint) matters to choosing the row, so it always shows: the title is
// cut to leave room for it. The subagent count shows only where it fits
// after the whole title.
func (l sessionLayout) titleCell(r listRow, style textStyle) string {
	lead := titlePrefix(r)
	room := max(l.title-visibleWidth(lead), 0)
	hint := ""
	if h := rowHint(r); h != "" {
		hint = "· " + h
	}
	titleRoom := room
	if hint != "" {
		titleRoom = max(room-1-visibleWidth(hint), 0)
	}
	full := cellTitle(r)
	title := cutAtWord(full, titleRoom)
	if count := subagentHint(r); count != "" && title == full && visibleWidth(title)+1+visibleWidth(count) <= titleRoom {
		title += " " + style.dim(count)
	}
	if hint != "" {
		if title != "" {
			title += " "
		}
		// Cut only on a terminal too narrow for the hint and any title.
		title += style.dim(cutAtWord(hint, room-visibleWidth(title)))
	}
	return padRight(lead+title, l.title)
}

// rowHint is the note a width-aware row shows after its title: the hint's
// short form when it has one, else the hint.
func rowHint(r listRow) string {
	if r.ShortHint != "" {
		return r.ShortHint
	}
	return r.Hint
}

// cutAtWord cuts text to at most limit columns. A cut text ends with "…",
// after the last whole word when that keeps at least half of the room, and
// otherwise mid-word.
func cutAtWord(text string, limit int) string {
	switch {
	case visibleWidth(text) <= limit:
		return text
	case limit <= 0:
		return ""
	case limit == 1:
		return "…"
	}
	cut := truncateVisible(text, limit-1)
	if i := strings.LastIndexByte(cut, ' '); i > 0 && visibleWidth(cut[:i]) >= (limit-1)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:·-") + "…"
}

// columnGap is the room between the columns of a table.
const columnGap = 2

// columnWriter lines up tab-separated cells as text/tabwriter does with
// padding columnGap, but measures a cell by visibleWidth: tabwriter counts
// the bytes of a color code (a dimmed hint, a dimmed ID) as width, which
// pushes the rest of a colored row out of line. A line's last cell is not
// padded. Lines are held until Flush, and every line is expected to have
// the same cells, as a table's do.
type columnWriter struct {
	out io.Writer
	buf strings.Builder
}

func (c *columnWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }

// Flush writes the held lines, lined up.
func (c *columnWriter) Flush() error {
	text := c.buf.String()
	c.buf.Reset()
	if text == "" {
		return nil
	}
	ended := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	cells := make([][]string, len(lines))
	var widths []int
	for i, line := range lines {
		cells[i] = strings.Split(line, "\t")
		for j, cell := range cells[i][:len(cells[i])-1] {
			if j == len(widths) {
				widths = append(widths, 0)
			}
			widths[j] = max(widths[j], visibleWidth(cell))
		}
	}
	var b strings.Builder
	for i, row := range cells {
		if i > 0 {
			b.WriteString("\n")
		}
		for j, cell := range row[:len(row)-1] {
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[j]-visibleWidth(cell)+columnGap))
		}
		b.WriteString(row[len(row)-1])
	}
	if ended {
		b.WriteString("\n")
	}
	_, err := io.WriteString(c.out, b.String())
	return err
}
