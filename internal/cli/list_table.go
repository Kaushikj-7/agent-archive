package cli

import (
	"io"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printSessionTable writes the human list table for rows. When GroupByProject
// is set and more than one project appears, rows are printed under project
// headings (indices stay global for the interactive picker).
func printSessionTable(w io.Writer, rows []listRow, opts listFormatOptions) error {
	return printSessionGroups(w, sessionTableGroups(rows, opts), opts)
}

// sessionTableGroup is the rows under one project heading of the human list
// table. The flat table is a single group without a heading.
type sessionTableGroup struct {
	// label is the project's heading, styled; empty for the flat table.
	label string
	// count is how many rows the whole group has.
	count int
	rows  []listRow
	// continued marks a group whose earlier rows are on a previous page.
	continued bool
}

// sessionTableGroups arranges rows as printSessionTable prints them. Their
// rows, taken in order, are the table's rows from top to bottom.
func sessionTableGroups(rows []listRow, opts listFormatOptions) []sessionTableGroup {
	if !opts.GroupByProject || distinctProjects(rows) <= 1 {
		return []sessionTableGroup{{count: len(rows), rows: rows}}
	}
	order := make([]listRow, 0)
	seen := map[string]bool{}
	labelGroups := map[string]int{}
	for _, r := range rows {
		key := projectGroupKey(r)
		if !seen[key] {
			seen[key] = true
			order = append(order, r)
			labelGroups[r.Project]++
		}
	}
	projectIDs := make([]string, len(order))
	for i, group := range order {
		projectIDs[i] = group.ProjectID
	}
	shortIDs := uniqueShortIDs(projectIDs)
	groups := make([]sessionTableGroup, 0, len(order))
	for i, group := range order {
		label := group.Project
		if label == "-" {
			label = "unknown project"
		}
		if labelGroups[group.Project] > 1 {
			if group.ProjectID != "" {
				label += " [" + archive.DisplayLine(shortIDs[i]) + "]"
			} else {
				label += " [unidentified]"
			}
		}
		if opts.Style.color {
			label = opts.Style.bold(label)
		}
		var members []listRow
		for _, r := range rows {
			if projectGroupKey(r) == projectGroupKey(group) {
				members = append(members, r)
			}
		}
		groups = append(groups, sessionTableGroup{label: label, count: len(members), rows: members})
	}
	return groups
}

// printSessionGroups writes groups as the human list table: each group's
// heading, when it has one, then its column header and rows.
func printSessionGroups(w io.Writer, groups []sessionTableGroup, opts listFormatOptions) error {
	if opts.Search {
		return printContentSearchRows(w, groups, opts.SearchWidth)
	}
	for i, group := range groups {
		if group.label != "" {
			if i > 0 {
				terminal.Println(w)
			}
			if group.continued {
				terminal.Printf(w, "%s (continued)\n", group.label)
			} else {
				terminal.Printf(w, "%s (%d)\n", group.label, group.count)
			}
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

func distinctProjects(rows []listRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[projectGroupKey(r)] = true
	}
	return len(seen)
}

func projectGroupKey(row listRow) string {
	if row.ProjectID != "" {
		return "id:" + row.ProjectID
	}
	return "name:" + row.Project
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
// hides a column.
func tableHeader(opts listFormatOptions) []string {
	var cols []string
	if opts.Numbered {
		cols = append(cols, "#")
	}
	cols = append(cols, "TITLE")
	if opts.ShowPR {
		cols = append(cols, "PR")
	}
	cols = append(cols, "WHEN")
	if !opts.HideHarness {
		cols = append(cols, "HARNESS")
	}
	if !opts.HideProject {
		cols = append(cols, "PROJECT")
	}
	return append(cols, "ID")
}

// subagentHint is what follows a title about the subagents: " · 45 subagents"
// after a parent, " · subagent of d7a77938" after a subagent.
func subagentHint(r listRow) string {
	switch {
	case r.Depth > 0:
		// Shown under its parent, which the indent says.
		return ""
	case r.Parent != "":
		return " · subagent of " + r.Parent
	case r.Children == 1:
		return " · 1 subagent"
	case r.Children > 1:
		return " · " + strconv.Itoa(r.Children) + " subagents"
	}
	return ""
}

// liveMark leads the title of a session active right now.
const liveMark = "●"

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

// indentedTitle is the title of a subagent shown under its parent, set in
// from the parents' titles.
func indentedTitle(r listRow) string {
	if r.Depth <= 0 {
		return r.Title
	}
	return strings.Repeat("  ", r.Depth) + subagentMark + " " + r.Title
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

// tableCells is one row of the plain table, in tableHeader's columns.
func tableCells(r listRow, opts listFormatOptions) []string {
	title := indentedTitle(r)
	if opts.LiveMarks {
		// Room for the dot on every row, so titles stay aligned.
		if r.Live {
			title = liveMark + " " + title
		} else {
			title = "  " + title
		}
	}
	title = cursorCell(r, opts) + title
	if hints := r.SkillHint + subagentHint(r); hints != "" {
		if opts.Style.color {
			title += opts.Style.dim(hints)
		} else {
			title += hints
		}
	}
	id := archive.DisplayLine(r.ShortID)
	if opts.DimID && opts.Style.color {
		id = opts.Style.dim(id)
	}
	var cells []string
	if opts.Numbered {
		cells = append(cells, rowNumber(r))
	}
	cells = append(cells, title)
	if opts.ShowPR {
		cells = append(cells, r.PR)
	}
	cells = append(cells, r.When)
	if !opts.HideHarness {
		cells = append(cells, r.Harness)
	}
	if !opts.HideProject {
		cells = append(cells, r.Project)
	}
	return append(cells, id)
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
