package cli

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// maxFilterText bounds the words typed into the filter.
const maxFilterText = 128

// pickerView is the table the picker draws for its filter: the rows to show,
// laid out under their heading and footer.
type pickerView struct {
	// words and filtering say what the view was made for; the view is kept
	// until either changes.
	words     string
	filtering bool
	// rows are the rows shown, and order the same rows from the top of the
	// table down, which is where the highlight counts from.
	rows  []listRow
	order []listRow
	// first is the position in order of the first row the words match, where
	// the highlight starts: a parent shown only for a subagent under it is
	// not what Enter acts on unless the highlight is moved there.
	first int
	// matched is how many sessions match the filter's words.
	matched int
	groups  []sessionTableGroup
	format  listFormatOptions
	heading string
	footer  string
	// layout is the rows' layout for the width it was made for, kept until
	// the width changes.
	layout *sessionLayout
}

// formatFor is the view's format for a terminal width columns wide (0 when
// its size is unknown), with every row's layout for that width, so the rows
// of each page or screen line up with the rest.
func (v *pickerView) formatFor(width int) listFormatOptions {
	f := v.format
	f.Width = width
	if width <= 0 || f.Verbose {
		return f
	}
	if v.layout == nil || v.layout.width != width {
		laid := newSessionLayout(v.rows, f)
		v.layout = &laid
	}
	f.layout = v.layout
	return f
}

// words is the filter's words, single-spaced; empty when the table is not
// narrowed.
func (l *sessionPicker) words() string { return strings.Join(strings.Fields(l.filter), " ") }

// setFilter narrows the table to the words in text. The table starts again at
// its top, with the first row that matches highlighted.
func (l *sessionPicker) setFilter(text string) {
	if strings.Join(strings.Fields(text), " ") != l.words() {
		l.stopOperations()
		l.searchFailure = nil
	}
	l.filter = text
	l.cursor, l.moved, l.start, l.bottom, l.cache, l.memo = 0, false, 0, nil, nil, nil
}

// closeFilter clears the filter and puts the filter line away.
func (l *sessionPicker) closeFilter() {
	l.filtering = false
	l.setFilter("")
}

// view is the table to draw: the rows given (a table of the scope, cut to
// a limit) when the filter has no words, and otherwise the sessions of the
// scope that match them, their subagents under them. It is kept until the
// filter changes.
func (l *sessionPicker) view(rows []listRow, totalMatched int, truncated bool, format listFormatOptions) *pickerView {
	words := l.words()
	if m := l.memo; m != nil && m.words == words && m.filtering == l.filtering {
		return m
	}
	shown, matched, laid := rows, len(rows), format
	q := parseSessionQuery(words)
	// Reading keys, the footer line under the table says where the screen
	// is and which keys act (footerLine), and the heading says how many
	// subagents are hidden: the count and the flags' advice are for a table
	// read by lines.
	keyed := l.keys != nil
	var footer bytes.Buffer
	switch {
	case words == "" && keyed:
		if format.Note != "" {
			footer.WriteString(format.Note + "\n")
		}
	case words == "":
		printListFooter(&footer, len(rows), totalMatched, truncated, format)
	default:
		shown, matched = filterRows(l.universe(rows), q)
		if l.searchFailure != nil {
			fmt.Fprintf(&footer, "Search failed: %v; clear the filter and retry.\n", l.searchFailure)
		} else if !l.searchRead && (l.search != nil || l.searchContext != nil) {
			footer.WriteString("Loading complete search; Esc clears.\n")
		}
		laid = filteredFormat(format, shown)
		if l.searchRead || (l.search == nil && l.searchContext == nil) {
			footer.WriteString(l.filterFooter(matched, words))
		}
		if l.searchNotice != "" {
			footer.WriteString(l.searchNotice + "\n")
		}
	}
	if l.boundedNotice != "" {
		footer.WriteString(l.boundedNotice + "\n")
	}
	if l.older != nil && !keyed {
		footer.WriteString("o · " + l.older.Label + " (Esc first while filtering)\n")
	}
	// The mark is the key browser's highlight; a table read by lines is
	// chosen from by number.
	laid.Cursor = keyed
	groups := sessionTableGroups(shown, laid)
	var order []listRow
	for _, group := range groups {
		order = append(order, group.rows...)
	}
	first := 0
	if words != "" {
		first = max(slices.IndexFunc(order, func(r listRow) bool { return q.matches(r.fields) }), 0)
	}
	heading := l.viewHeading(words, matched, len(rows), format.HiddenSubagents)
	v := &pickerView{words: words, filtering: l.filtering, rows: shown, order: order, first: first, matched: matched, groups: groups, format: laid, heading: heading, footer: footer.String()}
	l.memo = v
	return v
}

// viewHeading is the line above the table for the filter's words, the
// sessions they match, and the rows given. Reading keys, an unfiltered
// table's heading names the subagents it hides.
func (l *sessionPicker) viewHeading(words string, matched, rows, hidden int) string {
	heading := l.heading
	if l.keys != nil && words == "" && hidden > 0 && l.headingFor == nil {
		heading = strings.TrimPrefix(heading+" · "+hiddenSubagents(hidden), " · ")
	}
	if l.headingFor != nil {
		heading = l.headingFor(words, matched)
		if words != "" && !l.searchRead && (l.search != nil || l.searchContext != nil) {
			heading = l.headingFor("", rows)
			if l.searchFailure != nil {
				heading += " · Search unavailable"
			} else {
				heading += " · Searching…"
			}
		}
	}
	if words != "" && l.searchRead && l.searchPartial {
		heading += " · local eligibility may be unknown"
	}
	return heading
}

// universe is every session the filter searches: those of the table's scope
// when it knows them (subagents too), else the table's own rows. The table's
// rows keep the numbers the table gave them (withTableNumbers).
func (l *sessionPicker) universe(rows []listRow) []listRow {
	if l.search == nil && l.searchContext == nil {
		return rows
	}
	if !l.searchRead {
		return nil
	}
	return l.searched
}

// withTableNumbers is the table's rows, as the table numbers them, then the
// sessions searched that the table does not hold, numbered on from its last
// row (a subagent keeps no number). A search reads its sessions after the
// table did, and the handoff picker's have moved since when one of this
// machine's sessions was active in between, so its own numbers would name
// other rows than the table's.
func withTableNumbers(table, searched []listRow) []listRow {
	inTable := make(map[string]bool, len(table))
	for _, r := range table {
		inTable[childKey(r.HarnessKey, r.SessionID)] = true
	}
	out := slices.Clone(table)
	full := make(map[string]listRow, len(searched))
	for _, r := range searched {
		full[childKey(r.HarnessKey, r.SessionID)] = r
	}
	for i, r := range out {
		if searchedRow, ok := full[childKey(r.HarnessKey, r.SessionID)]; ok {
			out[i].fields = searchedRow.fields
		}
	}
	next := len(table)
	for _, r := range searched {
		if inTable[childKey(r.HarnessKey, r.SessionID)] {
			continue
		}
		if r.Index != 0 {
			next++
			r.Index = next
		}
		out = append(out, r)
	}
	return out
}

// filterFooter says what the filter found. A table read by lines also says
// how to go on; one read by keys has Esc in its heading.
func (l *sessionPicker) filterFooter(matched int, words string) string {
	quoted := `"` + archive.DisplayLine(words) + `"`
	var line string
	if l.searchPartial {
		line = fmt.Sprintf("%d observed sessions match %s; local eligibility may be unknown.\n", matched, quoted)
		return line
	}
	switch {
	case l.keys != nil && matched == 0:
		line = "No sessions match " + quoted + ".\n"
	case l.keys != nil:
	case matched == 0:
		line = "No sessions match " + quoted + " · more words only narrow it; Enter for all\n"
	case matched == 1:
		line = "1 session matches " + quoted + " · a number, more words, or Enter for all\n"
	default:
		line = fmt.Sprintf("%d sessions match %s · a number, more words, or Enter for all\n", matched, quoted)
	}
	if l.noteText != "" && l.noteWords == words {
		line += l.noteText + "\n"
	}
	return line
}

// filterRows narrows rows to those q matches. A subagent that matches is
// shown under its parent (indented), and the parent is shown too when it does
// not match; a subagent whose parent is not among the rows stands on its own,
// after the rest. matched counts the rows that match, not the parents shown
// for them. The rows keep their order, and the numbers they had.
func filterRows(rows []listRow, q sessionQuery) (shown []listRow, matched int) {
	matches := make([]bool, len(rows))
	parents := map[string]int{}
	for i, r := range rows {
		if matches[i] = q.matches(r.fields); matches[i] {
			matched++
		}
		if r.parentID == "" {
			parents[childKey(r.HarnessKey, r.SessionID)] = i
		}
	}
	children := map[int][]int{}
	var orphans []int
	for i, r := range rows {
		if r.parentID == "" || !matches[i] {
			continue
		}
		if parent, ok := parents[childKey(r.HarnessKey, r.parentID)]; ok {
			children[parent] = append(children[parent], i)
		} else {
			orphans = append(orphans, i)
		}
	}
	for i, r := range rows {
		if r.parentID != "" || !matches[i] && len(children[i]) == 0 {
			continue
		}
		shown = append(shown, r)
		for _, c := range children[i] {
			child := rows[c]
			child.Depth = 1
			shown = append(shown, child)
		}
	}
	for _, i := range orphans {
		shown = append(shown, rows[i])
	}
	return shown, matched
}

// filteredFormat is format laid out for the rows a filter shows: the columns
// that were left out for having one value come back when these rows differ.
func filteredFormat(format listFormatOptions, rows []listRow) listFormatOptions {
	f := format
	same := func(value func(listRow) string) bool {
		return !slices.ContainsFunc(rows, func(r listRow) bool { return value(r) != value(rows[0]) })
	}
	if len(rows) > 0 {
		f.HideHarness = format.HideHarness && same(func(r listRow) string { return r.Harness })
		f.HideProject = format.HideProject && same(func(r listRow) string { return r.Project })
	}
	if !format.Verbose {
		f.ShowPR = slices.ContainsFunc(rows, func(r listRow) bool { return r.PR != "" })
	}
	return f
}

// highlightGroups returns groups with the row cursor places from the top of
// the table marked as the one Enter acts on. The groups given are not
// changed.
func highlightGroups(groups []sessionTableGroup, cursor int) []sessionTableGroup {
	out := slices.Clone(groups)
	offset := 0
	for i, group := range groups {
		if at := cursor - offset; at >= 0 && at < len(group.rows) {
			rows := slices.Clone(group.rows)
			rows[at].Highlight = true
			out[i].rows = rows
		}
		offset += len(group.rows)
	}
	return out
}

// filterKey applies one key to the filter line: typed characters add to the
// filter, Backspace takes one off (and closes the line when none is left),
// Esc clears the filter, and the arrows, PgUp, PgDn, Home, and End move the
// highlight as they do unfiltered (move). Enter asks for the highlighted
// row.
func (l *sessionPicker) filterKey(k key, screen listScreen) listKeyResult {
	if move, ok := scrollKeys[k.kind]; ok {
		l.move(move, screen)
		return listStay
	}
	switch k.kind {
	case keyEnter:
		return listPick
	case keyEscape:
		l.closeFilter()
	case keyBackspace:
		if l.filter == "" {
			l.closeFilter()
			break
		}
		_, size := utf8.DecodeLastRuneInString(l.filter)
		l.setFilter(l.filter[:len(l.filter)-size])
	case keyEndOfInput:
		if l.filter == "" {
			return listQuit
		}
	case keyRune:
		if unicode.IsPrint(k.r) && len(l.filter) < maxFilterText {
			l.setFilter(l.filter + string(k.r))
		}
	case keyUp, keyDown, keyPageUp, keyPageDown, keyHome, keyEnd, keyLeft, keyRight, keyResize:
		// Moved above, or nothing to do but draw the screen again.
	}
	return listStay
}

// filterScreen is the list's screen for the table v, with the highlighted row
// scrolled into view, unfiltered or not. Until a key moves it, the filter's
// highlight is on the first row its words match.
func (l *sessionPicker) filterScreen(stdout io.Writer, v *pickerView, prompt string) listScreen {
	l.shown = v.heading
	measure := func() listScreen {
		s := l.screen(stdout, v.groups, v.format, len(v.rows), v.footer, prompt)
		s.cursor = l.cursor
		return s
	}
	s := measure()
	if len(v.order) == 0 {
		return s
	}
	if l.filtering && !l.moved {
		l.cursor = v.first
	}
	l.cursor = min(max(l.cursor, 0), len(v.order)-1)
	s.cursor = l.cursor
	if s.budget == 0 || l.cursor >= s.start && l.cursor < s.end {
		return s
	}
	l.start = l.revealStart(s, l.cursor)
	return measure()
}

// revealStart is the first row of the screen that shows row at, scrolling
// as little as it can.
func (l *sessionPicker) revealStart(s listScreen, at int) int {
	if at < s.start {
		return at
	}
	// A screen starting later never ends sooner.
	lo, hi := s.start, at
	for lo < hi {
		mid := (lo + hi) / 2
		if mid+l.fit(s, mid) > at {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}
