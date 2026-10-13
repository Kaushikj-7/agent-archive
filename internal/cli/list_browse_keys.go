package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

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
// [start, end) of the table fill the lines left above the footer, the
// status line, the message line, and the prompt.
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
}

// pickKeys is pick reading a key at a time. The table scrolls: ↑ and ↓ (and
// the mouse wheel, which the terminal sends as arrows on the alternate
// screen) by a row, PgUp and PgDn (and space, n and p) by a screen, Home
// and End to the top and bottom. A group scrolled into keeps its heading,
// marked continued, and its column header. Typed characters collect at the
// prompt until Enter chooses the row they name.
func (l *sessionPicker) pickKeys(p *prompter, stdout io.Writer, rows []listRow, totalMatched int, truncated bool, format listFormatOptions, action string) (listRow, bool, error) {
	question := p.promptText("Enter number (or unique short SESSION_ID) to "+action+", or q to quit", true, nil, -1, ": ")
	if l.emptyWithoutAction(rows) {
		l.prepareSearch(rows)
		if l.invalidView != nil {
			return listRow{}, false, l.invalidView
		}
		v := l.view(rows, totalMatched, truncated, format)
		if err := printSessionGroups(stdout, v.groups, v.format); err != nil {
			return listRow{}, false, err
		}
		terminal.Print(stdout, v.footer)
		return listRow{}, false, nil
	}
	typed, notice := "", ""
	prompt := func() string {
		if l.filtering {
			return "/" + l.filter
		}
		return question + typed
	}
	for first := true; ; first = false {
		if !first && l.clear != nil {
			l.clear()
		}
		l.prepareSearch(rows)
		if l.invalidView != nil {
			return listRow{}, false, l.invalidView
		}
		v := l.view(rows, totalMatched, truncated, format)
		screen := l.filterScreen(stdout, v, prompt())
		var frame bytes.Buffer
		if v.heading != "" {
			frame.WriteString(oneRow(v.heading, screen.width) + "\n")
		}
		groups := v.groups
		if l.filtering {
			groups = highlightGroups(groups, l.cursor)
		}
		if err := printSessionGroups(&frame, pageSessionGroups(groups, pickerPage{screen.start, screen.end}), v.format); err != nil {
			return listRow{}, false, err
		}
		frame.WriteString(v.footer)
		status := screen.status()
		if l.filtering {
			status = filterStatus(screen, action)
		}
		frame.WriteString(oneRow(status, screen.width) + "\n")
		frame.WriteString(oneRow(notice, screen.width) + "\n")
		frame.WriteString(prompt())
		terminal.Print(stdout, frame.String())
		notice = ""
		for {
			k, err := l.nextBrowserKey()
			l.prepareSearch(rows)
			if err == nil {
				err = l.invalidView
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return listRow{}, false, nil
				}
				return listRow{}, false, err
			}
			if l.activatesOlder(k, typed) {
				l.loadedOlder = true
				return listRow{}, false, nil
			}
			var result listKeyResult
			if l.filtering {
				result = l.filterKey(k, screen, v)
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
				notice = "Nothing matches the filter; Esc clears it."
			case listStay:
			}
			if !l.keys.buffered() {
				break
			}
		}
	}
}

// listKeyResult is what a key at the list asks for besides scrolling or
// typing.
type listKeyResult int

const (
	listStay listKeyResult = iota
	listQuit
	// listSubmit asks for the row typed.
	listSubmit
	// listToggle asks for the other scope.
	listToggle
	// listPick asks for the row the filter highlights.
	listPick
)

// listKey applies one key to the list drawn as screen: it scrolls (moving
// l.start) or edits what is typed. a (when the scope can change), n, p, q,
// and / (which opens the filter) act only when nothing is typed.
func (l *sessionPicker) listKey(k key, screen listScreen, typed string) (string, listKeyResult) {
	if move, ok := scrollKeys[k.kind]; ok {
		l.scroll(move, screen)
		return typed, listStay
	}
	switch k.kind {
	case keyEnter:
		if typed == "" {
			return "", listQuit
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
		// Scrolled above, or nothing to do but draw the list again.
	}
	return typed, listStay
}

// listRune applies a typed character to the list.
func (l *sessionPicker) listRune(r rune, screen listScreen, typed string) (string, listKeyResult) {
	switch {
	case r == ' ':
		l.scroll(scrollPageDown, screen)
	case typed == "" && (r == 'q' || r == 'Q'):
		return "", listQuit
	case typed == "" && l.toggles && (r == 'a' || r == 'A'):
		return "", listToggle
	case typed == "" && r == '/':
		l.filtering, l.cursor, l.moved = true, 0, false
	case typed == "" && (r == 'n' || r == 'N'):
		l.scroll(scrollPageDown, screen)
	case typed == "" && (r == 'p' || r == 'P'):
		l.scroll(scrollPageUp, screen)
	case unicode.IsPrint(r) && len(typed) < maxTypedAnswer:
		typed += string(r)
	}
	return typed, listStay
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

// screen measures the list for the terminal's size now: the lines left for
// the table, the rows that fill them from l.start, and the last screen's
// first row.
func (l *sessionPicker) screen(stdout io.Writer, groups []sessionTableGroup, format listFormatOptions, n int, footer, prompt string) listScreen {
	s := listScreen{groups: groups, format: format, n: n, end: n}
	width, height, ok := l.env.terminalSize(stdout)
	if !ok {
		l.start = 0
		return s
	}
	s.width = width
	// Below the table: the footer, the status line, the message line (or a
	// blank one), and the prompt with what is typed.
	chrome := l.headRows() + displayLines(footer, width) + 1 + 1 + displayLines(prompt, width)
	s.budget = max(height-chrome, minPickerPageRows)
	s.bottom = l.lastScreen(s)
	s.start = min(l.start, s.bottom)
	l.start = s.start
	s.end = s.start + l.fit(s, s.start)
	return s
}

// fit is how many rows, from start, fill the screen's lines: as many as
// fit, but at least a few.
func (l *sessionPicker) fit(s listScreen, start int) int {
	lo, hi := 1, min(s.n-start, max(s.budget, minPickerPageRows))
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if l.tableLines(s.groups, s.format, pickerPage{start, start + mid}, s.width) <= s.budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return max(lo, min(minPickerPageRows, s.n-start))
}

// reachesEnd reports whether the screen starting at start shows the last
// row.
func (l *sessionPicker) reachesEnd(s listScreen, start int) bool {
	return s.n-start <= minPickerPageRows || s.n-start <= s.budget && l.tableLines(s.groups, s.format, pickerPage{start, s.n}, s.width) <= s.budget
}

// lastScreen is the first row of the screen that shows the last row, the
// farthest the list scrolls. It is kept for the terminal's size.
func (l *sessionPicker) lastScreen(s listScreen) int {
	if b := l.bottom; b != nil && b.width == s.width && b.budget == s.budget && b.rows == s.n {
		return b.start
	}
	// Only a screen with fewer rows than lines can reach the end, and one
	// reaching it still does from a later row.
	lo, hi := max(s.n-max(s.budget, minPickerPageRows), 0), s.n-1
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
	lo, hi := max(s.start-max(s.budget, minPickerPageRows), 0), s.start
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

// position is where the screen is in the list: Top, a percentage, Bottom, or
// All when every row fits.
func (s listScreen) position() string {
	switch {
	case s.bottom == 0:
		return "All"
	case s.start == 0:
		return "Top"
	case s.start >= s.bottom:
		return "Bottom"
	}
	return fmt.Sprintf("%d%%", s.end*100/s.n)
}

// status is the line below the list's footer: where the screen is in the
// list and the keys.
func (s listScreen) status() string {
	const choose = "/ filter · type a number and Enter · q quit"
	if s.bottom == 0 {
		return "All · " + choose
	}
	return s.position() + " · ↑↓ scroll · PgUp/PgDn page · " + choose
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
	below := displayLines(hint, width) + displayLines(b.prompt.promptText(detailsKeysQuestion(true, pager), false, nil, -1, ": "), width)
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
	return l.older != nil && !l.filtering && typed == "" && k.kind == keyRune && unicode.ToLower(k.r) == 'o'
}

func (l *sessionPicker) viewChanged(next, prior *pickerView, screen listScreen) bool {
	return next != prior || l.start != screen.start || l.filtering && l.cursor != screen.cursor
}
