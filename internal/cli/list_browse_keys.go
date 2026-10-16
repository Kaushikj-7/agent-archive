package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// browserKeys turns key mode on for the browser's screens when they are on
// the alternate screen and stdin is a terminal that can be read a key at a
// time; otherwise it returns nil and the browser reads lines. Leaving the
// screen, however the browser ends, restores line input.
func browserKeys(env sessionBrowserDependencies, p *prompter, screen *altScreen) *keyInput {
	if !screen.clears() {
		return nil
	}
	term, ok := env.openKeyTerminal(p.source)
	if !ok {
		return nil
	}
	return attachKeys(term, screen)
}

// attachKeys turns key mode on for term and ties it to screen: Ctrl-Z shows
// the normal screen while the process is stopped, and leaving the screen,
// however it ends, restores line input. It returns nil when key mode cannot
// be turned on.
func attachKeys(term keyTerminal, screen *altScreen) *keyInput {
	keys := startKeys(term)
	if keys != nil {
		keys.hide, keys.show = screen.hide, screen.reenter
		screen.restoreOnLeave(keys.close)
	}
	return keys
}

// maxTypedAnswer bounds what can be typed at the list's prompt.
const maxTypedAnswer = 64

// pickerBottom is the scroll position of the list's last screen for one
// terminal size.
type pickerBottom struct {
	width  int
	budget int
	rows   int
	start  int
}

// listScreen is the list as one screen draws it, reading keys: rows
// [start, end) of the table fill the lines left above the detail strip,
// the footer's notes, the footer line, and the prompt.
type listScreen struct {
	groups []sessionTableGroup
	format listFormatOptions
	n      int
	width  int
	// budget is the lines left for the table; 0 when the terminal's size
	// is unknown, and every row is shown.
	budget int
	start  int
	end    int
	bottom int
	// cursor is the highlighted row the screen was measured for.
	cursor int
	// strip is set when the detail strip of the highlighted row fits under
	// the table; a very short terminal gives its lines to the rows.
	strip bool
}

// pickKeys is pick reading a key at a time. A highlight marks the row
// Enter opens: ↑ and ↓ (and the mouse wheel, which the terminal sends as
// arrows on the alternate screen) move it a row, PgUp and PgDn (and space,
// n and p) a screen, Home and End to the top and bottom, and the table
// scrolls to keep it in view. Under the table, the detail strip spells out
// the highlighted row. A group scrolled into keeps its heading, marked
// continued. Typed characters collect at the prompt until Enter chooses
// the row they name; Enter with nothing typed opens the highlighted row.
// When older rows can load, a move down past the last row loads them and
// goes on into them (olderMoveFor); o loads them too.
func (l *sessionPicker) pickKeysContext(ctx context.Context, p *prompter, stdout io.Writer, rows []listRow, totalMatched int, truncated bool, format listFormatOptions, action string) (listRow, bool, error) {
	question := p.promptText("Number or ID to "+action, true, nil, -1, ": ")
	if l.emptyWithoutAction(rows) {
		return listRow{}, false, l.printEmptyKeys(ctx, stdout, rows, totalMatched, truncated, format)
	}
	typed, notice := "", ""
	prompt := func() string {
		if l.filtering {
			return "/" + l.filter
		}
		return question + typed
	}
	l.loadingFrame = nil
	for first := true; ; first = false {
		if !first && l.clear != nil {
			l.clear()
		}
		l.prepareSearchContext(ctx, rows)
		if l.invalidView != nil {
			return listRow{}, false, l.invalidView
		}
		v := l.view(rows, totalMatched, truncated, format)
		screen := l.filterScreen(stdout, v, prompt())
		screen, notice = l.continueOlder(stdout, v, screen, prompt(), notice)
		bottom := l.footerLine(screen, action)
		if notice != "" {
			bottom = oneRow(notice, screen.width)
		}
		frame, err := l.keysFrame(v, screen, format, bottom, prompt())
		if err != nil {
			return listRow{}, false, err
		}
		terminal.Print(stdout, frame)
		notice = ""
		for {
			k, err := l.nextBrowserKey()
			l.prepareSearchContext(ctx, rows)
			if err == nil {
				err = l.invalidView
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return listRow{}, false, nil
				}
				return listRow{}, false, err
			}
			if l.loadsOlder(k, typed, v, screen) {
				l.loadedOlder = true
				l.loadingFrame = l.redrawWith(stdout, v, screen, format, prompt())
				return listRow{}, false, nil
			}
			var result listKeyResult
			if l.filtering {
				result = l.filterKey(k, screen)
			} else {
				typed, result = l.listKey(k, screen, typed)
			}
			if l.invalidView != nil {
				return listRow{}, false, l.invalidView
			}
			if again := l.view(rows, totalMatched, truncated, format); l.viewChanged(again, v, screen) {
				// Measured again, so the next key of a burst (PgDn after
				// PgDn, or after ↓) moves on from here.
				v = again
				screen = l.filterScreen(stdout, v, prompt())
			}
			switch result {
			case listQuit:
				return listRow{}, false, nil
			case listToggle:
				l.toggled = true
				l.loadingFrame = l.redrawWith(stdout, v, screen, format, prompt())
				return listRow{}, false, nil
			case listSubmit:
				if row, ok := matchBrowseRow(typed, rows); ok {
					return row, true, nil
				}
				typed, notice = "", "Enter a listed number or unique short SESSION_ID, or q to quit."
			case listPick:
				if l.cursor < len(v.order) {
					return v.order[l.cursor], true, nil
				}
				notice = "No session to open; q quits."
				if l.filtering {
					notice = "Nothing matches the filter; Esc clears it."
				}
			case listStay:
			}
			if !l.keys.buffered() {
				break
			}
		}
	}
}

// keysFrame is the list's screen, drawn for the table v as measured by
// screen: the heading, the rows on screen, the detail strip, the footer,
// bottom (the footer line, or a message in its place), and the prompt.
func (l *sessionPicker) keysFrame(v *pickerView, screen listScreen, format listFormatOptions, bottom, prompt string) (string, error) {
	var frame bytes.Buffer
	if v.heading != "" {
		frame.WriteString(oneRow(v.heading, screen.width) + "\n")
	}
	groups := highlightGroups(v.groups, l.cursor)
	if err := printSessionGroups(&frame, pageSessionGroups(groups, pickerPage{screen.start, screen.end}), v.formatFor(screen.width)); err != nil {
		return "", err
	}
	if screen.strip {
		for _, line := range detailStrip(v.order[screen.cursor], screen.width, format.Style) {
			frame.WriteString(line + "\n")
		}
	}
	frame.WriteString(l.displayFooter(v.footer, screen.width))
	frame.WriteString(bottom + "\n")
	frame.WriteString(prompt)
	return frame.String(), nil
}

// redrawWith returns what draws the list as it is now, the table v as
// measured by screen, again with a message in place of its footer line
// (loadingFrame). The screen is cleared first, so the message never
// pushes the list up a line.
func (l *sessionPicker) redrawWith(stdout io.Writer, v *pickerView, screen listScreen, format listFormatOptions, prompt string) func(string) {
	return func(message string) {
		frame, err := l.keysFrame(v, screen, format, oneRow(message, screen.width), prompt)
		if err != nil {
			return
		}
		if l.clear != nil {
			l.clear()
		}
		terminal.Print(stdout, frame)
	}
}

// printEmptyKeys prints a table with no row to choose and nothing to load:
// no keys act on it, so it ends with the count a table read by lines ends
// with.
func (l *sessionPicker) printEmptyKeys(ctx context.Context, stdout io.Writer, rows []listRow, totalMatched int, truncated bool, format listFormatOptions) error {
	l.prepareSearchContext(ctx, rows)
	if l.invalidView != nil {
		return l.invalidView
	}
	v := l.view(rows, totalMatched, truncated, format)
	width, _, _ := l.env.terminalSize(stdout)
	if err := printSessionGroups(stdout, v.groups, v.formatFor(width)); err != nil {
		return err
	}
	printListFooter(stdout, len(rows), totalMatched, truncated, format)
	if l.boundedNotice != "" {
		terminal.Println(stdout, l.boundedNotice)
	}
	return nil
}

// listKeyResult is what a key at the list asks for besides moving or
// typing.
type listKeyResult int

const (
	listStay listKeyResult = iota
	listQuit
	// listSubmit asks for the row typed.
	listSubmit
	// listToggle asks for the other scope.
	listToggle
	// listPick asks for the highlighted row.
	listPick
)

// listKey applies one key to the list drawn as screen: it moves the
// highlight (and the list with it) or edits what is typed. Enter opens
// the row typed, or the highlighted one when nothing is. a (when the scope
// can change), n, p, q, and / (which opens the filter) act only when
// nothing is typed.
func (l *sessionPicker) listKey(k key, screen listScreen, typed string) (string, listKeyResult) {
	if move, ok := scrollKeys[k.kind]; ok {
		l.move(move, screen)
		return typed, listStay
	}
	switch k.kind {
	case keyEnter:
		if typed == "" {
			return "", listPick
		}
		return typed, listSubmit
	case keyEndOfInput:
		if typed == "" {
			return "", listQuit
		}
	case keyBackspace:
		_, size := utf8.DecodeLastRuneInString(typed)
		typed = typed[:len(typed)-size]
	case keyEscape:
		typed = ""
	case keyRune:
		return l.listRune(k.r, screen, typed)
	case keyUp, keyDown, keyPageUp, keyPageDown, keyHome, keyEnd, keyLeft, keyRight, keyResize:
		// Moved above, or nothing to do but draw the list again.
	}
	return typed, listStay
}

// listRune applies a typed character to the list.
func (l *sessionPicker) listRune(r rune, screen listScreen, typed string) (string, listKeyResult) {
	switch {
	case r == ' ':
		l.move(scrollPageDown, screen)
	case typed == "" && (r == 'q' || r == 'Q'):
		return "", listQuit
	case typed == "" && l.toggles && (r == 'a' || r == 'A'):
		return "", listToggle
	case typed == "" && r == '/' && l.contentSearch != nil:
		return "", listStay
	case typed == "" && r == '/':
		// The highlight stays where it is until words are typed, so Esc
		// with none typed comes back to the same row. Moving past the end
		// loads older rows again afterwards, even where it found none.
		l.filtering, l.moved, l.olderSpent = true, true, ""
	case typed == "" && (r == 'n' || r == 'N'):
		l.move(scrollPageDown, screen)
	case typed == "" && (r == 'p' || r == 'P'):
		l.move(scrollPageUp, screen)
	case unicode.IsPrint(r) && len(typed) < maxTypedAnswer:
		typed += string(r)
	}
	return typed, listStay
}

// move moves the highlight, and the list with it: a row up or down; a
// screen, where the list scrolls a screen and the highlight keeps its
// place on it, or goes to the first or last row when the list cannot
// scroll further; or to the top or the bottom. The screen drawn next
// scrolls to keep the highlight in view.
func (l *sessionPicker) move(move scrollMove, screen listScreen) {
	last := max(screen.n-1, 0)
	l.moved = true
	switch move {
	case scrollLineUp:
		l.cursor = max(l.cursor-1, 0)
	case scrollLineDown:
		l.cursor = min(l.cursor+1, last)
	case scrollPageUp, scrollPageDown:
		l.scroll(move, screen)
		switch shift := l.start - screen.start; {
		case shift != 0:
			l.cursor = min(max(l.cursor+shift, 0), last)
		case move == scrollPageDown:
			l.cursor = last
		default:
			l.cursor = 0
		}
	case scrollTop:
		l.scroll(move, screen)
		l.cursor = 0
	case scrollBottom:
		l.scroll(move, screen)
		l.cursor = last
	}
}

// scroll moves the list's first row. The screen drawn next clamps it to
// the rows there are.
func (l *sessionPicker) scroll(move scrollMove, screen listScreen) {
	switch move {
	case scrollLineUp:
		l.start = max(l.start-1, 0)
	case scrollLineDown:
		l.start = min(l.start+1, screen.bottom)
	case scrollPageDown:
		l.start = min(screen.end, screen.bottom)
	case scrollPageUp:
		l.start = l.previousScreen(screen)
	case scrollTop:
		l.start = 0
	case scrollBottom:
		l.start = screen.bottom
	}
}

// detailStripLines is how many lines the detail strip takes: a rule, the
// title, the details, and a rule.
const detailStripLines = 4

// minStripListLines is the fewest lines the table keeps with the detail
// strip under it; a terminal shorter than that gives the strip's lines to
// the table.
const minStripListLines = 5

// screen measures the list for the terminal's size now: the lines left for
// the table, the rows that fill them from l.start, and the last screen's
// first row.
//
// The detail strip is left out first: on a terminal too short for it and
// minStripListLines of the table, and when fewer than minPickerPageRows
// rows (with their day headings) would fit above it. Only then does a
// screen show fewer rows, down to one, so it never takes more lines than
// the terminal has while a row fits.
func (l *sessionPicker) screen(stdout io.Writer, groups []sessionTableGroup, format listFormatOptions, n int, footer, prompt string) listScreen {
	s := l.measureScreen(stdout, groups, format, n, footer, prompt, n > 0)
	if s.strip && s.width > 0 && s.end-s.start < min(minPickerPageRows, s.n-s.start) {
		s = l.measureScreen(stdout, groups, format, n, footer, prompt, false)
	}
	return s
}

// minKeyScreenRows is the fewest rows a screen of the key browser shows,
// however short the terminal: unlike a page read by lines, which keeps
// minPickerPageRows, a screen that took more lines than the terminal has
// would scroll its heading away.
const minKeyScreenRows = 1

// measureScreen is screen with the detail strip under the table when strip
// is set and the terminal has lines for it.
func (l *sessionPicker) measureScreen(stdout io.Writer, groups []sessionTableGroup, format listFormatOptions, n int, footer, prompt string, strip bool) listScreen {
	s := listScreen{groups: groups, format: format, n: n, end: n, strip: strip}
	width, height, ok := l.env.terminalSize(stdout)
	if !ok {
		l.start = 0
		return s
	}
	s.width = width
	// Below the table: the detail strip, the footer's notes, the footer
	// line (or a message in its place), and the prompt with what is typed.
	chrome := l.headRows() + displayLines(l.displayFooter(footer, width), width) + 1 + displayLines(prompt, width)
	if s.strip && height-chrome-detailStripLines < minStripListLines {
		s.strip = false
	}
	if s.strip {
		chrome += detailStripLines
	}
	s.budget = max(height-chrome, minKeyScreenRows)
	s.bottom = l.lastScreen(s)
	s.start = min(l.start, s.bottom)
	l.start = s.start
	s.end = s.start + l.fit(s, s.start)
	return s
}

// fit is how many rows, from start, fill the screen's lines: as many as
// fit, but at least one.
func (l *sessionPicker) fit(s listScreen, start int) int {
	lo, hi := 1, min(s.n-start, max(s.budget, minKeyScreenRows))
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if l.tableLines(s.groups, s.format, pickerPage{start, start + mid}, s.width) <= s.budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// reachesEnd reports whether the screen starting at start shows the last
// row.
func (l *sessionPicker) reachesEnd(s listScreen, start int) bool {
	return s.n-start <= minKeyScreenRows || s.n-start <= s.budget && l.tableLines(s.groups, s.format, pickerPage{start, s.n}, s.width) <= s.budget
}

// lastScreen is the first row of the screen that shows the last row, the
// farthest the list scrolls. It is kept for the terminal's size.
func (l *sessionPicker) lastScreen(s listScreen) int {
	if b := l.bottom; b != nil && b.width == s.width && b.budget == s.budget && b.rows == s.n {
		return b.start
	}
	// Only a screen with fewer rows than lines can reach the end, and one
	// reaching it still does from a later row.
	lo, hi := max(s.n-max(s.budget, minKeyScreenRows), 0), s.n-1
	for lo < hi {
		mid := (lo + hi) / 2
		if l.reachesEnd(s, mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	l.bottom = &pickerBottom{width: s.width, budget: s.budget, rows: s.n, start: lo}
	return lo
}

// previousScreen is the first row of the screen that ends where screen
// starts: PgUp shows the rows above as PgDn showed these.
func (l *sessionPicker) previousScreen(s listScreen) int {
	if s.budget == 0 {
		return 0
	}
	lo, hi := max(s.start-max(s.budget, minKeyScreenRows), 0), s.start
	for lo < hi {
		mid := (lo + hi) / 2
		if mid+l.fit(s, mid) >= s.start {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// detailsScreen is a session's summary as one screen draws it, reading
// keys: lines [top, top+shown) of the summary fill the lines left above
// the scroll indicator, the message line, the hint, and the prompt.
type detailsScreen struct {
	lines  []string
	width  int
	budget int
	top    int
	shown  int
	bottom int
	// cut is set when the summary does not fit and scrolls.
	cut bool
}

// detailsKeys is details reading a key at a time: ↑ ↓ PgUp PgDn space Home
// and End scroll a summary taller than the window; t, m, b, and q act at
// once, and Enter and Backspace go back to the list as b does.
func (b *sessionBrowser) detailsKeys(ctx context.Context, view sessionView, row listRow) (browseAction, error) {
	var summary bytes.Buffer
	renderSessionSummary(&summary, view, b.summaryOptions(false))
	lines := strings.Split(strings.TrimSuffix(summary.String(), "\n"), "\n")
	_, _, pager := resolvePagerCommand(b.env, b.noPager, b.stdout)
	top := 0
	var notice browseNotice
	for {
		b.screen.clear()
		screen := b.drawDetailsKeys(lines, top, pager, notice)
		notice = browseNotice{}
		for {
			k, err := b.keys.next()
			if err != nil {
				return endOfInput(err)
			}
			action, message, err := b.detailsKey(ctx, k, &screen, row, summary.Bytes(), pager)
			if err != nil || action == browseBack || action == browseQuit {
				return action, err
			}
			top = screen.top
			if action == browseRedraw || message.text != "" {
				notice = message
				break
			}
			if !b.keys.buffered() {
				break
			}
		}
	}
}

// detailsKey applies one key to the details drawn as screen. It scrolls
// by moving screen.top, or returns what the browser does next: browseStay
// with a message to show, or browseRedraw after a pager.
func (b *sessionBrowser) detailsKey(ctx context.Context, k key, screen *detailsScreen, row listRow, summary []byte, pager bool) (browseAction, browseNotice, error) {
	if move, ok := scrollKeys[k.kind]; ok {
		screen.scroll(move)
		return browseStay, browseNotice{}, nil
	}
	switch k.kind {
	case keyEnter, keyBackspace:
		return browseBack, browseNotice{}, nil
	case keyEndOfInput:
		return browseQuit, browseNotice{}, nil
	case keyResize:
		return browseRedraw, browseNotice{}, nil
	case keyRune:
		return b.detailsRune(ctx, k.r, screen, row, summary, pager)
	case keyEscape, keyUp, keyDown, keyPageUp, keyPageDown, keyHome, keyEnd, keyLeft, keyRight:
		// Scrolled above, or nothing to do. Esc does not go back: it may
		// be the start of a wheel's arrow split from the rest.
	}
	return browseStay, browseNotice{}, nil
}

// detailsRune applies a typed character to the details.
func (b *sessionBrowser) detailsRune(ctx context.Context, r rune, screen *detailsScreen, row listRow, summary []byte, pager bool) (browseAction, browseNotice, error) {
	switch unicode.ToLower(r) {
	case 'b':
		return browseBack, browseNotice{}, nil
	case 'q':
		return browseQuit, browseNotice{}, nil
	case ' ':
		screen.scroll(scrollPageDown)
		return browseStay, browseNotice{}, nil
	case 't':
		action, failure, err := b.transcript(ctx, row)
		if action == browseStay {
			return action, browseNotice{text: failure, error: true}, err
		}
		return action, browseNotice{}, err
	case 'm':
		if screen.cut && pager {
			action, err := b.page(ctx, summary)
			return action, browseNotice{}, err
		}
	}
	message := "Press t for the transcript, b (or Enter) for the list, or q to quit."
	if screen.cut && pager {
		message = "Press t for the transcript, m for the whole summary, b (or Enter) for the list, or q to quit."
	}
	return browseStay, browseNotice{text: message}, nil
}

// scroll moves the summary's top line.
func (s *detailsScreen) scroll(move scrollMove) {
	switch move {
	case scrollLineUp:
		s.top = max(s.top-1, 0)
	case scrollLineDown:
		s.top = min(s.top+1, s.bottom)
	case scrollPageDown:
		s.top = min(s.top+s.shown, s.bottom)
	case scrollPageUp:
		used := 0
		for s.top > 0 && used+lineRows(s.lines[s.top-1], s.width) <= s.budget {
			s.top--
			used += lineRows(s.lines[s.top], s.width)
		}
	case scrollTop:
		s.top = 0
	case scrollBottom:
		s.top = s.bottom
	}
	if s.cut {
		// Measured again, so the next key of a burst moves on from here.
		s.shown = detailsLinesFrom(s.lines[s.top:], s.width, s.budget)
	}
}

// detailsKeysQuestion is the details' prompt reading keys: m and scrolling
// are offered when the summary does not fit.
func detailsKeysQuestion(cut, pager bool) string {
	switch {
	case cut && pager:
		return "t transcript · m more · ↑↓ scroll · b back · q quit"
	case cut:
		return "t transcript · ↑↓ scroll · b back · q quit"
	}
	return "t transcript · b back · q quit"
}

// drawDetailsKeys draws the summary from line top, as much as fits above
// the scroll indicator, the notice (or a blank line), the hint, and the
// prompt. A notice that would leave the summary fewer than minDetailLines
// is cut to one row unless it is an error.
func (b *sessionBrowser) drawDetailsKeys(lines []string, top int, pager bool, notice browseNotice) detailsScreen {
	hint := b.transcriptHint()
	s, notice := b.measureDetails(lines, top, pager, hint, notice)
	var frame bytes.Buffer
	for _, line := range lines[s.top : s.top+s.shown] {
		frame.WriteString(line + "\n")
	}
	if s.cut {
		frame.WriteString(b.format.Style.dim(scrollIndicator(s.top, len(lines)-s.top-s.shown)) + "\n")
	}
	terminal.Print(b.stdout, frame.String())
	if notice.text != "" {
		notice.print(b.stdout, b.stderr)
	} else {
		terminal.Println(b.stdout)
	}
	if hint != "" {
		terminal.Println(b.stdout, b.format.Style.dim(hint))
	}
	terminal.Print(b.stdout, b.prompt.promptText(detailsKeysQuestion(s.cut, pager), false, nil, -1, ": "))
	return s
}

// measureDetails fits the summary from line top to the terminal, and cuts
// a notice that would leave it fewer than minDetailLines to one row,
// unless it is an error.
func (b *sessionBrowser) measureDetails(lines []string, top int, pager bool, hint string, notice browseNotice) (detailsScreen, browseNotice) {
	width, height, ok := b.env.terminalSize(b.stdout)
	if !ok {
		return detailsScreen{lines: lines, shown: len(lines)}, notice
	}
	question := detailsKeysQuestion(true, pager)
	if b.search != nil {
		question = searchDetailsQuestion
	}
	below := displayLines(hint, width) + displayLines(b.prompt.promptText(question, false, nil, -1, ": "), width)
	chrome := max(displayLines(notice.text, width), 1) + below
	summaryRows := 0
	for _, line := range lines {
		summaryRows += lineRows(line, width)
	}
	if summaryRows+chrome <= height {
		return detailsScreen{lines: lines, width: width, shown: len(lines)}, notice
	}
	if !notice.error && height-chrome-1 < minDetailLines {
		notice.text = oneRow(notice.text, width)
		chrome = 1 + below
	}
	// One row is kept for the scroll indicator.
	budget := max(height-chrome-1, minDetailLines)
	bottom := lastDetailsTop(lines, width, budget)
	top = min(max(top, 0), bottom)
	return detailsScreen{
		lines: lines, width: width, budget: budget, top: top, bottom: bottom, cut: true,
		shown: detailsLinesFrom(lines[top:], width, budget),
	}, notice
}

// lastDetailsTop is the first line of the summary's last screen, the
// farthest it scrolls.
func lastDetailsTop(lines []string, width, budget int) int {
	top, used := len(lines), 0
	for top > 0 && used+lineRows(lines[top-1], width) <= budget {
		top--
		used += lineRows(lines[top], width)
	}
	return min(top, len(lines)-1)
}

// detailsLinesFrom is how many of lines fill budget rows, at least one.
func detailsLinesFrom(lines []string, width, budget int) int {
	count, used := 0, 0
	for count < len(lines) && used+lineRows(lines[count], width) <= budget {
		used += lineRows(lines[count], width)
		count++
	}
	return max(count, 1)
}

// scrollIndicator says how many lines of the summary are above and below
// the screen.
func scrollIndicator(above, below int) string {
	var parts []string
	if above > 0 {
		parts = append(parts, "↑ "+plural(above, "line")+" above")
	}
	if below > 0 {
		parts = append(parts, "↓ "+strings.Replace(plural(below, "line"), " ", " more ", 1))
	}
	return strings.Join(parts, " · ")
}

// transcriptKeys is transcriptPrompt reading a key at a time: Enter, b, or
// Backspace return to the details, and q quits.
func (b *sessionBrowser) transcriptKeys() (browseAction, error) {
	terminal.Println(b.stdout)
	terminal.Print(b.stdout, b.prompt.promptText("[Enter/b] back to details  [q] quit", false, nil, -1, ": "))
	for {
		k, err := b.keys.next()
		if err != nil {
			return endOfInput(err)
		}
		switch {
		case k.kind == keyEnter || k.kind == keyBackspace || k.kind == keyRune && unicode.ToLower(k.r) == 'b':
			return browseRedraw, nil
		case k.kind == keyEndOfInput || k.kind == keyRune && unicode.ToLower(k.r) == 'q':
			return browseQuit, nil
		}
	}
}

func (l *sessionPicker) emptyWithoutAction(rows []listRow) bool {
	return len(rows) == 0 && l.words() == "" && l.older == nil
}

func (l *sessionPicker) activatesOlder(k key, typed string) bool {
	if l.contentSearch != nil && typed == "" && k.kind == keyRune && unicode.ToLower(k.r) == 'r' {
		l.contentSearch.rerun = true
		return true
	}
	return l.older != nil && !l.filtering && typed == "" && k.kind == keyRune && unicode.ToLower(k.r) == 'o'
}

// olderMove is a move down that the rows loaded could not take, kept
// while older rows load so that it goes on into them.
type olderMove struct {
	move scrollMove
	// rows is how many rows the table had, and keep the identity
	// (rowIdentity) of the row highlighted.
	rows int
	keep string
}

// canLoadOlder reports whether older rows (or a content search's next
// results) can load: the footer's position then ends in "+", and a move down
// past the last row loads them. A move past an end where a load found none
// loads nothing (olderSpent).
func (l *sessionPicker) canLoadOlder() bool {
	if l.older == nil || l.contentSearch != nil && !l.contentSearch.hasNextPage() {
		return false
	}
	return l.olderSpent == "" || l.memo == nil || olderEnd(l.memo) != l.olderSpent
}

// olderEnd names the end of the table v: how many rows it has, and its last.
func olderEnd(v *pickerView) string {
	if len(v.order) == 0 {
		return "0"
	}
	return strconv.Itoa(len(v.order)) + " " + rowIdentity(v.order[len(v.order)-1])
}

// olderMoveFor is the move k asks for when it cannot go past the rows
// loaded and older rows can load, with the filter line closed and nothing
// typed: ↓ (or the wheel) and End on the last row, and PgDn, space, or n
// with the last row on screen.
func (l *sessionPicker) olderMoveFor(k key, typed string, screen listScreen) (scrollMove, bool) {
	if l.filtering || typed != "" || !l.canLoadOlder() {
		return 0, false
	}
	move, ok := scrollKeys[k.kind]
	if k.kind == keyRune && (k.r == ' ' || unicode.ToLower(k.r) == 'n') {
		move, ok = scrollPageDown, true
	}
	switch {
	case !ok:
		return 0, false
	case move == scrollLineDown || move == scrollBottom:
		return move, l.cursor >= screen.n-1
	case move == scrollPageDown:
		return move, screen.end >= screen.n
	}
	return 0, false
}

// continueOlder goes on with the move that loaded older rows, now that the
// table v has them: ↓ to the first row loaded, a screen down as if the rows
// had been there, and End to the new last row. When the load added no rows,
// the highlight stays and the message to show says so. screen is the list
// as it is drawn; the screen returned is measured again after the move.
// With no move to go on with, it returns screen and notice, the message
// already to show.
func (l *sessionPicker) continueOlder(stdout io.Writer, v *pickerView, screen listScreen, prompt, notice string) (listScreen, string) {
	m := l.pendingOlder
	l.pendingOlder = nil
	if m == nil {
		return screen, notice
	}
	if len(v.order) <= m.rows {
		// Moving past this end again loads nothing more.
		l.olderSpent = olderEnd(v)
		if l.contentSearch != nil {
			return screen, "No more results."
		}
		return screen, "No older sessions."
	}
	if m.rows == 0 && m.move != scrollBottom {
		// Nothing was listed: the first row loaded is the first row.
		l.cursor, l.start = 0, 0
		return l.filterScreen(stdout, v, prompt), ""
	}
	if at := slices.IndexFunc(v.order, func(r listRow) bool { return rowIdentity(r) == m.keep }); m.keep != "" && at >= 0 && at != l.cursor {
		l.cursor = at
		screen = l.filterScreen(stdout, v, prompt)
	}
	l.move(m.move, screen)
	return l.filterScreen(stdout, v, prompt), ""
}

// loadsOlder reports whether k loads older rows: o, or a move down past
// the rows loaded (olderMoveFor), which is kept to go on into them
// (continueOlder).
func (l *sessionPicker) loadsOlder(k key, typed string, v *pickerView, screen listScreen) bool {
	move, ok := l.olderMoveFor(k, typed, screen)
	if !ok {
		return l.activatesOlder(k, typed)
	}
	keep := ""
	if l.cursor >= 0 && l.cursor < len(v.order) {
		keep = rowIdentity(v.order[l.cursor])
	}
	l.pendingOlder = &olderMove{move: move, rows: len(v.order), keep: keep}
	return true
}

func (l *sessionPicker) viewChanged(next, prior *pickerView, screen listScreen) bool {
	return next != prior || l.start != screen.start || l.cursor != screen.cursor
}

// detailStrip is the strip under the key browser's table that spells out
// the highlighted row: a rule, its full display title, then dim the agent,
// the project, the PR, its subagents, its note, when it was last active,
// and its short ID, each line cut to the terminal's width (0 when unknown,
// which cuts nothing), and a rule.
func detailStrip(r listRow, width int, style textStyle) []string {
	_, agent := agentLabel(rowHarness(r))
	details := []string{agent}
	if r.Project != "" && r.Project != "-" {
		details = append(details, r.Project)
	}
	if r.PR != "" {
		details = append(details, "PR "+r.PR)
	}
	switch {
	case r.Parent != "":
		// Said even under its parent, where the table only indents it.
		details = append(details, "subagent of "+r.Parent)
	case r.Children > 0:
		details = append(details, plural(r.Children, "subagent"))
	}
	if r.Hint != "" {
		details = append(details, r.Hint)
	}
	if r.When != "" {
		details = append(details, "last active "+r.When)
	}
	if id := archive.DisplayLine(r.ShortID); id != "" {
		details = append(details, "id "+id)
	}
	ruleWidth := width
	if ruleWidth <= 0 {
		ruleWidth = unknownRuleWidth
	}
	rule := style.dim(strings.Repeat("─", ruleWidth))
	return []string{
		rule,
		stripLine(r.Title, width),
		style.dim(stripLine(strings.Join(details, " · "), width)),
		rule,
	}
}

// stripLine is a line of the detail strip, cut at a word with "…" to a
// terminal width columns wide; 0 (unknown) cuts nothing.
func stripLine(text string, width int) string {
	if width <= 0 {
		return text
	}
	return cutAtWord(text, width)
}

// unknownRuleWidth is how wide the detail strip's rules are on a terminal
// whose width is unknown.
const unknownRuleWidth = 40

// footerKey is a key the footer line names, with how much it matters:
// keys that do not fit go, least first.
type footerKey struct {
	text string
	rank int
}

// footerLine is the line under the key browser's table: the keys, and on
// the right which rows are on screen ("1–14 of 86", "1–14 of 50+" when
// older rows load past the last one). Keys that do not fit
// the width go, least needed first; the position stays.
func (l *sessionPicker) footerLine(s listScreen, action string) string {
	var keys []footerKey
	if l.filtering {
		keys = []footerKey{{"↑↓ move", 2}, {"Enter " + action, 0}, {"Esc clear", 3}}
	} else {
		keys = []footerKey{{"↑↓ move", 2}, {"PgUp/PgDn page", 5}, {"Enter " + action, 0}}
		if l.contentSearch == nil {
			keys = append(keys, footerKey{"/ search", 4})
		} else {
			keys = append(keys, footerKey{"r rerun", 4})
		}
		keys = append(keys, footerKey{"number + Enter jump", 6}, footerKey{"q quit", 3})
	}
	position := ""
	if s.n > 0 {
		position = fmt.Sprintf("%d–%d of %d", s.start+1, s.end, s.n)
		// More rows load past the last one (olderMoveFor), which the
		// filter line does not do.
		if l.canLoadOlder() && !l.filtering {
			position += "+"
		}
	}
	const gap = "   "
	for {
		texts := make([]string, len(keys))
		for i, k := range keys {
			texts[i] = k.text
		}
		left := strings.Join(texts, gap)
		switch {
		case s.width <= 0:
			return strings.TrimSpace(left + gap + position)
		case position == "" && visibleWidth(left) <= s.width:
			return left
		case visibleWidth(left)+len(gap)+visibleWidth(position) <= s.width:
			return left + strings.Repeat(" ", s.width-visibleWidth(left)-visibleWidth(position)) + position
		case len(keys) == 0:
			return oneRow(position, s.width)
		}
		least := 0
		for i, k := range keys {
			if k.rank > keys[least].rank {
				least = i
			}
		}
		keys = slices.Delete(keys, least, least+1)
	}
}
