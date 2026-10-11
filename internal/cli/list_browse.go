package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// browseInteractive reports whether list/show should offer a session picker:
// both stdin and stdout are terminals, and the caller is not forcing JSON.
// Interactive list skips the pager so the prompt stays with the table.
func browseInteractive(env sessionBrowseDependencies, stdin io.Reader, stdout io.Writer) bool {
	return env.interactive(stdin) && env.interactive(stdout)
}

// browseAction is what the session browser does after a prompt.
type browseAction int

const (
	// browseBack returns to the list.
	browseBack browseAction = iota
	// browseQuit leaves the browser.
	browseQuit
	// browseRedraw draws the session's details again.
	browseRedraw
	// browseStay asks again without redrawing, keeping a message visible.
	browseStay
)

// sessionBrowser is interactive list and bare show: pick a row, read its
// summary, open its transcript with t, go back to the list, or quit. The
// list and the details replace each other on the terminal's alternate
// screen (altScreen), each cut to fit the window. Transcript content is
// shown only when t is pressed.
type sessionBrowser struct {
	env     sessionBrowserDependencies
	prompt  *prompter
	stdout  io.Writer
	stderr  io.Writer
	store   storage.ObjectStore
	format  listFormatOptions
	noPager bool
	screen  *altScreen
	// keys, when set, reads the list and the details a key at a time; nil
	// reads lines.
	keys *keyInput
	// list is the session table, which keeps its page between visits.
	list *sessionPicker
	// last is the session whose details were shown last, printed to the
	// normal screen on the way out so its ID stays in scrollback.
	last *sessionView
}

func (b *sessionBrowser) run(ctx context.Context, choices *scopeChoices) error {
	for {
		b.screen.clear()
		row, ok, err := b.list.pickScoped(b.prompt, b.stdout, choices, "show")
		if err != nil || !ok {
			return err
		}
		stop := startActivity(b.stdout, "Loading session…")
		view, err := readSessionView(ctx, b.store, row.HarnessKey, row.SessionID)
		stop()
		if err != nil {
			return err
		}
		b.last = &view
		action, err := b.details(ctx, view, row)
		if err != nil || action == browseQuit {
			return err
		}
	}
}

// details shows one session's summary and reads what to do next.
func (b *sessionBrowser) details(ctx context.Context, view sessionView, row listRow) (browseAction, error) {
	if b.keys != nil {
		return b.detailsKeys(ctx, view, row)
	}
	var notice browseNotice
	for {
		b.screen.clear()
		var summary bytes.Buffer
		renderSessionSummary(&summary, view, b.summaryOptions(false))
		hint := b.transcriptHint()
		var rest string
		var redraw bool
		rest, notice, redraw = b.drawDetails(summary.String(), hint, notice)
		action, next, err := b.detailsPrompt(ctx, row, summary.Bytes(), rest, hint, notice, redraw)
		if err != nil || action != browseRedraw {
			return action, err
		}
		notice = next
	}
}

// browseNotice is a message for the user about their last answer, and
// whether it is an error (printed to stderr).
type browseNotice struct {
	text  string
	error bool
}

// print writes the notice as one line to stdout, or to stderr for an
// error.
func (n browseNotice) print(stdout, stderr io.Writer) {
	if n.error {
		terminal.Println(stderr, n.text)
		return
	}
	terminal.Println(stdout, n.text)
}

// oneRow cuts text to one terminal row width columns wide, ending a cut
// with "…". A width of 0 or less (unknown) cuts nothing.
func oneRow(text string, width int) string {
	if width <= 0 || visibleWidth(text) <= width {
		return text
	}
	return truncateVisible(text, width-1) + "…"
}

// minDetailLines is the fewest summary lines the details show, however
// short the terminal.
const minDetailLines = 5

// detailsQuestion is the details' prompt; [m] is offered when the summary
// was cut to fit.
func detailsQuestion(cut bool) string {
	if cut {
		return "[t] transcript  [m] more  [Enter/b] back to list  [q] quit"
	}
	return "[t] transcript  [Enter/b] back to list  [q] quit"
}

// drawDetails prints the summary. When it does not fit the terminal above
// the notice (or a blank line), the hint, and the prompt, only the lines
// that fit are printed, then how many more there are; rest is the lines
// left out. A notice that would push the details past the terminal's
// height is cut to one row, unless it is an error, which is kept whole;
// shown is the notice as it is to be printed. redraw reports that the terminal's size is known and the
// screen can be cleared, so the details can be drawn again in place with a
// notice rather than have one printed below them.
func (b *sessionBrowser) drawDetails(summary, hint string, notice browseNotice) (rest string, shown browseNotice, redraw bool) {
	width, height, ok := b.env.terminalSize(b.stdout)
	if !ok {
		terminal.Print(b.stdout, summary)
		return "", notice, false
	}
	redraw = b.screen.clears()
	// Below the summary: the notice or a blank line, the hint, and the
	// prompt.
	below := displayLines(hint, width) + displayLines(b.prompt.promptText(detailsQuestion(true), false, nil, -1, ": "), width)
	chrome := max(displayLines(notice.text, width), 1) + below
	if displayLines(summary, width)+chrome <= height {
		terminal.Print(b.stdout, summary)
		return "", notice, redraw
	}
	if !notice.error && height-chrome-1 < minDetailLines {
		// The summary is at its fewest lines, so the notice gets one row,
		// as in the list.
		notice.text = oneRow(notice.text, width)
		chrome = 1 + below
	}
	lines := strings.Split(strings.TrimSuffix(summary, "\n"), "\n")
	// One row is kept for the line saying how many more there are.
	budget := max(height-chrome-1, minDetailLines)
	count, used := 0, 0
	for count < len(lines) && used+lineRows(lines[count], width) <= budget {
		used += lineRows(lines[count], width)
		count++
	}
	if count == len(lines) {
		terminal.Print(b.stdout, summary)
		return "", notice, redraw
	}
	count = max(count, 1)
	for _, line := range lines[:count] {
		terminal.Println(b.stdout, line)
	}
	more := len(lines) - count
	noun := "lines"
	if more == 1 {
		noun = "line"
	}
	terminal.Println(b.stdout, b.format.Style.dim(fmt.Sprintf("… %d more %s", more, noun)))
	return strings.Join(lines[count:], "\n") + "\n", notice, redraw
}

// transcriptHint says how to use and leave the pager t opens, or is empty
// when the transcript is printed without one.
func (b *sessionBrowser) transcriptHint() string {
	command, chosen, page := resolvePagerCommand(b.env, b.noPager, b.stdout)
	switch {
	case !page:
		return ""
	case !chosen:
		// The default less scrolls on the wheel (defaultPagerCommand).
		return "t opens the transcript: scroll with the wheel or arrows, q returns here."
	case isLess(command):
		// A less the user set may not take the wheel.
		return "t opens the transcript: scroll with the arrows or space, q returns here."
	}
	return "t opens the transcript in your pager; quit it to return here."
}

// detailsPrompt reads what to do with the details drawn above it. A
// message about an answer is printed below the prompt, or, when redraw is
// set, returned with browseRedraw to be shown on the details drawn again.
func (b *sessionBrowser) detailsPrompt(ctx context.Context, row listRow, summary []byte, rest, hint string, notice browseNotice, redraw bool) (browseAction, browseNotice, error) {
	for first := true; ; first = false {
		if first && notice.text != "" {
			notice.print(b.stdout, b.stderr)
		} else {
			terminal.Println(b.stdout)
		}
		if first && hint != "" {
			terminal.Println(b.stdout, b.format.Style.dim(hint))
		}
		answer, err := b.prompt.line(b.prompt.promptText(detailsQuestion(rest != ""), false, nil, -1, ": "))
		if err != nil {
			action, err := endOfInput(err)
			return action, browseNotice{}, err
		}
		var message browseNotice
		switch strings.ToLower(answer) {
		case "", "b", "back":
			return browseBack, browseNotice{}, nil
		case "q", "quit":
			return browseQuit, browseNotice{}, nil
		case "t", "transcript":
			action, failure, err := b.transcript(ctx, row)
			if err != nil || action != browseStay {
				return action, browseNotice{}, err
			}
			message = browseNotice{text: failure, error: true}
		case "m", "more":
			if rest == "" {
				message.text = "Enter t for the transcript, b (or just Enter) for the list, or q to quit."
				break
			}
			if _, _, page := resolvePagerCommand(b.env, b.noPager, b.stdout); !page {
				// Without a pager, the lines left out are printed below the
				// prompt. The details no longer fit the screen, so they are
				// not drawn again: that would cut the summary again.
				terminal.Print(b.stdout, rest)
				rest, redraw = "", false
				continue
			}
			// The whole summary, through the pager as the transcript is.
			action, err := b.page(ctx, summary)
			return action, browseNotice{}, err
		default:
			if rest != "" {
				message.text = "Enter t for the transcript, m for the whole summary, b (or just Enter) for the list, or q to quit."
			} else {
				message.text = "Enter t for the transcript, b (or just Enter) for the list, or q to quit."
			}
		}
		if redraw {
			return browseRedraw, message, nil
		}
		message.print(b.stdout, b.stderr)
	}
}

// transcript downloads and verifies the session's bundle and shows its
// transcript through the pager. A bundle that cannot be read is reported
// in failure, with browseStay: the details stay open.
func (b *sessionBrowser) transcript(ctx context.Context, row listRow) (action browseAction, failure string, err error) {
	stop := startActivity(b.stdout, "Loading transcript…")
	text, err := b.renderTranscript(ctx, row)
	stop()
	if err != nil {
		return browseStay, "agent-archive: show: " + describeBundleError(err, row.SessionID, "--transcript"), nil
	}
	action, err = b.page(ctx, text)
	return action, "", err
}

// page shows text through the pager on a cleared screen and returns to the
// details once the user is done with it. A signal while the pager runs
// stops the pager, then the browser restores the screen and exits.
func (b *sessionBrowser) page(ctx context.Context, text []byte) (browseAction, error) {
	b.screen.clear()
	endPaging := func() {}
	if b.keys != nil {
		// The pager reads the terminal in the modes it had.
		endPaging = b.keys.page()
	}
	restoreTerminal := saveTerminalState(b.prompt.source)
	pagerCtx, stopPager := context.WithCancel(ctx)
	b.screen.startPaging(stopPager)
	paged, waited, err := pageText(pagerCtx, b.stdout, b.stderr, b.env, b.noPager, true, text)
	sig := b.screen.endPaging()
	stopPager()
	endPaging()
	if sig != nil {
		// A signal stopped the pager; the pager has exited, so exit as the
		// signal would have. A pager killed before it could restore the
		// terminal's modes (one behind a pipe) leaves them raw, so they
		// are restored first.
		restoreTerminal()
		b.screen.exitForSignal(sig)
		return browseQuit, nil
	}
	if err != nil {
		return browseQuit, err
	}
	if b.keys != nil {
		if err := b.keys.resume(); err != nil {
			return browseQuit, err
		}
	}
	if paged && waited {
		b.screen.reenter()
		return browseRedraw, nil
	}
	// Printed without a pager, or by a pager that may have returned at
	// once: wait, so the text stays on screen until the user asks for the
	// details again.
	action, err := b.transcriptPrompt()
	if paged {
		b.screen.reenter()
	}
	return action, err
}

func (b *sessionBrowser) transcriptPrompt() (browseAction, error) {
	if b.keys != nil {
		return b.transcriptKeys()
	}
	for {
		terminal.Println(b.stdout)
		answer, err := b.prompt.line(b.prompt.promptText("[Enter/b] back to details  [q] quit", false, nil, -1, ": "))
		if err != nil {
			return endOfInput(err)
		}
		switch strings.ToLower(answer) {
		case "", "b", "back":
			return browseRedraw, nil
		case "q", "quit":
			return browseQuit, nil
		default:
			terminal.Println(b.stdout, "Enter b (or just Enter) for the details, or q to quit.")
		}
	}
}

func (b *sessionBrowser) renderTranscript(ctx context.Context, row listRow) ([]byte, error) {
	key, err := locateMetadataKey(ctx, b.store, row.HarnessKey, row.SessionID)
	if err != nil {
		return nil, err
	}
	view, bundle, err := loadVerifiedSession(ctx, b.store, key)
	if err != nil {
		return nil, err
	}
	t, err := buildTranscript(ctx, b.env, bundle)
	if err != nil {
		return nil, fmt.Errorf("normalized view unavailable: %w", err)
	}
	var buf bytes.Buffer
	renderTranscript(&buf, view, t, transcriptOptions{summaryOptions: b.summaryOptions(false)})
	return buf.Bytes(), nil
}

func (b *sessionBrowser) summaryOptions(hints bool) summaryOptions {
	return summaryOptions{Now: b.format.Now, Style: b.format.Style, Projects: b.format.Projects, Hints: hints}
}

// endOfInput turns the end of input (Ctrl-D, or a script's last line) into
// quitting; any other read error is returned.
func endOfInput(err error) (browseAction, error) {
	if errors.Is(err, io.EOF) {
		return browseQuit, nil
	}
	return browseQuit, err
}

// minPickerPageRows is the fewest rows a page of the picker shows, however
// short the terminal.
const minPickerPageRows = 3

// sessionPicker is the numbered session table with its prompt. When the
// table, its footer, and the prompt do not fit the terminal's height, it
// shows one page of rows at a time, with n and p to move between pages.
// Row numbers stay those of the whole table, so any listed number or short
// ID can be typed from any page.
type sessionPicker struct {
	older         *browserLoadAction
	loadedOlder   bool
	boundedNotice string
	env           terminalSizeDependencies
	// keys, when set, reads the list a key at a time: it scrolls instead
	// of turning pages (see pickKeys).
	keys *keyInput
	// clear, when set, blanks the screen before another page is drawn. A
	// message then replaces the blank line above the prompt of a redrawn
	// page, instead of being printed below the prompt.
	clear func()
	// start is the position, in the table's top-to-bottom order, of the
	// first row of the page shown, or, reading keys, of the first row the
	// list is scrolled to. It survives returning to the list.
	start int
	// cache is the last split into pages, reused while the terminal's size
	// and the table stay the same.
	cache *pickerPages
	// bottom is the scroll position of the last screen, reused while the
	// terminal's size and the table stay the same.
	bottom *pickerBottom
	// rendered counts the rows drawn to measure pages, which tests bound.
	rendered int
	// heading, when set, is the line above the table, naming what is shown.
	heading string
	// toggles is set when `a`, typed alone, asks for the other scope: the
	// list then returns with toggled set, and the caller shows the other.
	toggles bool
	toggled bool

	// verb is what Enter does, which leads the heading of a browser that
	// picks ("Hand off", "Show").
	verb string
	// filter is the words the table is narrowed to; blank shows every row.
	// filtering is set while the key browser's filter line is open: typed
	// characters then go to the filter, and cursor, the position of a row
	// from the top of the table, is the row Enter acts on.
	filter    string
	filtering bool
	cursor    int
	// moved is set once a key moved the highlight; until then it is on the
	// first row the filter's words match.
	moved bool
	// search, when set, lists every session of the table's scope for the
	// filter to search, which the table's rows alone would not (a limit cut
	// them, and they leave out subagents); searched holds what it returned.
	search     func() []listRow
	searched   []listRow
	searchRead bool
	// headingFor, when set, words the heading for the table shown, and for
	// a filter's words and how many sessions match them.
	headingFor func(words string, matches int) string
	// noteText is a line the footer adds while the filter holds noteWords.
	noteText  string
	noteWords string
	// shown is the heading of the table drawn last, which the table's pages
	// leave room for.
	shown string
	// memo is the table drawn last, kept until the filter changes.
	memo *pickerView
}

// pickScoped is pickRows over the scope's choices: it shows the one now
// chosen, under its heading, and shows the other when `a` is typed alone.
// It returns when a row is picked or the user quits.
func (l *sessionPicker) pickScoped(p *prompter, stdout io.Writer, choices *scopeChoices, action string) (listRow, bool, error) {
	for {
		c := choices.shown()
		l.heading, l.toggles, l.toggled = c.heading, choices.canToggle(), false
		l.headingFor = func(words string, matches int) string {
			return choices.headingWith(choices.current, c.scopeView, c.constants, headingOptions{Verb: l.verb, Words: words, Matches: matches, Filtering: l.filtering && l.keys != nil})
		}
		l.search, l.searched, l.searchRead = c.search, nil, false
		l.noteText, l.noteWords = c.searchNote, c.searchWords
		row, ok, err := l.pickRows(p, stdout, c.rows, c.total, c.truncated, c.format, action)
		if l.loadedOlder && err == nil {
			l.loadedOlder = false
			more, e := l.older.Load()
			if e != nil {
				return listRow{}, false, e
			}
			if !more {
				l.older = nil
			}
			l.memo, l.cache, l.bottom = nil, nil, nil
			l.searched, l.searchRead = nil, false
			if l.clear != nil {
				l.clear()
			}
			continue
		}
		if !l.toggled || err != nil {
			return row, ok, err
		}
		choices.toggle()
		// The other table starts at its top, on a cleared screen.
		l.start, l.cache, l.bottom = 0, nil, nil
		if l.clear != nil {
			l.clear()
		}
	}
}

// lineCommand is what an answer at the line-mode prompt asks for besides a
// row or words to filter by: to leave, to clear the filter, or (when the
// scope can change) the other scope.
type lineCommand int

const (
	lineOther lineCommand = iota
	lineQuit
	lineScope
	lineClear
)

// lineCommand reads a command word. An empty answer clears the filter, and
// quits when there is none.
func (l *sessionPicker) lineCommand(answer string) lineCommand {
	switch {
	case answer == "":
		if l.words() != "" {
			return lineClear
		}
		return lineQuit
	case strings.EqualFold(answer, "q") || strings.EqualFold(answer, "quit"):
		return lineQuit
	case l.toggles && strings.EqualFold(answer, "a"):
		return lineScope
	}
	return lineOther
}

// printHeading writes the heading, cut to one row of a terminal width
// columns wide (0 when its size is unknown).
func (l *sessionPicker) printHeading(stdout io.Writer, width int) {
	if l.shown != "" {
		terminal.Println(stdout, oneRow(l.shown, width))
	}
}

// headRows is how many terminal rows the heading takes: one, cut to fit.
func (l *sessionPicker) headRows() int {
	if l.shown == "" {
		return 0
	}
	return 1
}

// pickerPage is the rows [start, end) of the table, in its top-to-bottom
// order.
type pickerPage struct {
	start int
	end   int
}

// pickerPages is a table split into pages for one terminal size.
type pickerPages struct {
	width    int
	height   int
	rows     int
	footer   string
	question string
	pages    []pickerPage
}

// pickRows shows the numbered table of rows and prompts until the user
// selects a row (ok=true), quits or input ends (ok=false), or an error occurs.
// A table taller than the terminal is shown a page at a time, or scrolled
// when keys are read. format.Numbered must be set. An answer that is not a
// row's number (matchRow), an ID, or a command word is words to filter the
// table by; they add to the filter already there.
func (l *sessionPicker) pickRows(p *prompter, stdout io.Writer, rows []listRow, totalMatched int, truncated bool, format listFormatOptions, action string) (listRow, bool, error) {
	l.memo = nil
	if l.keys != nil {
		return l.pickKeys(p, stdout, rows, totalMatched, truncated, format, action)
	}
	question := p.promptText("Enter number (or unique short SESSION_ID) to "+action+", words to filter, or q to quit", true, nil, -1, ": ")
	notice := ""
	for {
		v := l.view(rows, totalMatched, truncated, format)
		l.shown = v.heading
		pages, width, sized := l.pages(stdout, v.groups, v.format, len(v.rows), v.footer, question)
		// A message is shown in place on a redrawn screen of known size.
		redraw := sized && l.clear != nil
		page := 0
		for i, pg := range pages {
			if l.start >= pg.start && l.start < pg.end {
				page = i
			}
		}
		paged := len(pages) > 1
		l.printHeading(stdout, width)
		if paged {
			l.start = pages[page].start
			if err := printSessionGroups(stdout, pageSessionGroups(v.groups, pages[page]), v.format); err != nil {
				return listRow{}, false, err
			}
		} else {
			l.start = 0
			if err := printSessionGroups(stdout, v.groups, v.format); err != nil {
				return listRow{}, false, err
			}
		}
		terminal.Print(stdout, v.footer)
		if paged {
			terminal.Println(stdout, pageLine(page, len(pages), len(v.rows)))
		}
		if len(rows) == 0 && v.words == "" && l.older == nil {
			return listRow{}, false, nil
		}
		for {
			if notice != "" {
				// Cut to one row, so the page still fits.
				terminal.Println(stdout, oneRow(notice, width))
				notice = ""
			} else {
				terminal.Println(stdout)
			}
			answer, err := p.line(question)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return listRow{}, false, nil
				}
				return listRow{}, false, err
			}
			answer = strings.TrimSpace(answer)
			if l.older != nil && (strings.EqualFold(answer, "o") || strings.EqualFold(answer, "older")) {
				l.loadedOlder = true
				return listRow{}, false, nil
			}
			command := l.lineCommand(answer)
			if command == lineQuit || command == lineScope {
				l.toggled = command == lineScope
				return listRow{}, false, nil
			}
			var message string
			var again bool
			switch {
			case command == lineClear:
				l.setFilter("")
				again = true
			case isPageWord(answer):
				message = l.turnPage(pages, page, answer)
				again = message == "" || redraw
			default:
				onPage := v.order
				if paged {
					onPage = v.order[pages[page].start:pages[page].end]
				}
				if row, matched := l.matchRow(answer, v.rows, onPage, rows); matched {
					return row, true, nil
				}
				// Words to narrow the table by, added to those already there.
				l.setFilter(l.words() + " " + answer)
				again = true
			}
			if !again {
				terminal.Println(stdout, message)
				continue
			}
			// Another page, or the table again narrowed, or this page again
			// with the message.
			notice = message
			break
		}
		if l.clear != nil {
			l.clear()
		}
	}
}

// isPageWord reports whether an answer asks for another page of the table.
func isPageWord(answer string) bool {
	switch strings.ToLower(answer) {
	case "n", "next", "p", "prev", "previous":
		return true
	}
	return false
}

// turnPage moves to the next or the previous page, as the answer asks. It
// returns a message when there is no such page, or when the table has only
// one.
func (l *sessionPicker) turnPage(pages []pickerPage, page int, answer string) string {
	next := strings.HasPrefix(strings.ToLower(answer), "n")
	switch {
	case len(pages) < 2:
		return "Everything is on this page."
	case next && page == len(pages)-1:
		return "This is the last page; p goes back."
	case !next && page == 0:
		return "This is the first page; n goes on."
	case next:
		l.start = pages[page+1].start
	default:
		l.start = pages[page-1].start
	}
	return ""
}

// matchRow resolves an answer to a row listed: by its number, one of the
// table's rows (the number it has unfiltered), or one past the table that
// the filter shows on the page drawn (onPage); by its ID, one of those the
// filter shows. A number past the table on another page of the filter, or
// on none, names a session the person has not seen, and is words. A word
// shorter than the ID prefixes the matcher takes (minIDPrefixWord) is words,
// not an ID: "db" or "add" would otherwise pick the one session whose ID
// starts with it.
func (l *sessionPicker) matchRow(answer string, shown, onPage, rows []listRow) (listRow, bool) {
	if _, err := strconv.Atoi(answer); err != nil && len(answer) < minIDPrefixWord {
		return listRow{}, false
	}
	if l.words() == "" {
		return matchBrowseRow(answer, rows)
	}
	if n, err := strconv.Atoi(answer); err == nil && len(answer) < minShortSessionID {
		if n >= 1 && n <= len(rows) {
			return rows[n-1], true
		}
		for _, r := range onPage {
			if r.Index == n && n > 0 {
				return r, true
			}
		}
		return listRow{}, false
	}
	return matchBrowseRow(answer, shown)
}

// pages splits the table's n rows into pages that fit the terminal above
// the footer, the page line, and the prompt. It returns one page holding
// every row when they all fit or the terminal's size is unknown (sized is
// then false). The size is read each time, so a resized terminal gets
// pages of the new size; the pages for a size are kept until it changes.
func (l *sessionPicker) pages(stdout io.Writer, groups []sessionTableGroup, format listFormatOptions, n int, footer, question string) (pages []pickerPage, width int, sized bool) {
	all := []pickerPage{{0, n}}
	width, height, ok := l.env.terminalSize(stdout)
	if !ok {
		return all, 0, false
	}
	if c := l.cache; c != nil && c.width == width && c.height == height && c.rows == n && c.footer == footer && c.question == question {
		return c.pages, width, true
	}
	pages = l.split(groups, format, n, width, height, footer, question)
	l.cache = &pickerPages{width: width, height: height, rows: n, footer: footer, question: question, pages: pages}
	return pages, width, true
}

// split measures pages from the top, each as full as fits in the lines
// left for the table.
func (l *sessionPicker) split(groups []sessionTableGroup, format listFormatOptions, n, width, height int, footer, question string) []pickerPage {
	all := []pickerPage{{0, n}}
	if n == 0 {
		return all
	}
	tableLines := func(pg pickerPage) int {
		return l.tableLines(groups, format, pg, width)
	}
	// Below the table: the footer, a blank line, and the prompt.
	chrome := l.headRows() + displayLines(footer, width) + 1 + displayLines(question, width)
	// Every row, and the column header, takes a line at least, so a table
	// with more rows than the terminal has lines is not drawn to find out.
	if n+1+chrome <= height && tableLines(all[0])+chrome <= height {
		return all
	}
	// The widest the page line gets.
	budget := height - chrome - lineRows(pageLine(1, n, n), width)
	var pages []pickerPage
	for start := 0; start < n; {
		// The most rows that fit: a page's height only grows with its
		// rows, and no more rows than budget lines can fit.
		lo, hi := 1, min(n-start, max(budget, minPickerPageRows))
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if tableLines(pickerPage{start, start + mid}) <= budget {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		rows := max(lo, min(minPickerPageRows, n-start))
		pages = append(pages, pickerPage{start, start + rows})
		start += rows
	}
	return pages
}

// tableLines is how many terminal rows the table's rows pg take, with
// their headings, on a terminal width columns wide.
func (l *sessionPicker) tableLines(groups []sessionTableGroup, format listFormatOptions, pg pickerPage, width int) int {
	l.rendered += pg.end - pg.start
	var buf bytes.Buffer
	_ = printSessionGroups(&buf, pageSessionGroups(groups, pg), format)
	return displayLines(buf.String(), width)
}

// pageLine says which page of how many is shown, and how to reach the
// others. It names no row numbers: in the grouped table a page's rows are
// not a numeric range.
func pageLine(page, pages, n int) string {
	noun := "sessions"
	if n == 1 {
		noun = "session"
	}
	line := fmt.Sprintf("Page %d of %d · %d %s ·", page+1, pages, n, noun)
	if page < pages-1 {
		line += " [n] next "
	}
	if page > 0 {
		line += " [p] previous"
	}
	return strings.TrimRight(line, " ")
}

// pageSessionGroups is the part of groups a page shows. A group whose
// earlier rows are on a previous page keeps its heading, marked continued.
func pageSessionGroups(groups []sessionTableGroup, pg pickerPage) []sessionTableGroup {
	var out []sessionTableGroup
	offset := 0
	for _, group := range groups {
		from, to := max(pg.start-offset, 0), min(pg.end-offset, len(group.rows))
		offset += len(group.rows)
		if from >= to {
			continue
		}
		part := group
		part.rows = group.rows[from:to]
		part.continued = from > 0
		out = append(out, part)
	}
	if out == nil {
		// An empty table still prints its column header.
		return groups
	}
	return out
}

// matchBrowseRow resolves a typed answer to one listed row. A unique exact
// short or full SESSION_ID wins first so an all-decimal short ID is not
// mistaken for a row number. A small integer (shorter than a short ID) then
// selects a 1-based index. Otherwise a unique SESSION_ID prefix matches.
func matchBrowseRow(answer string, rows []listRow) (listRow, bool) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return listRow{}, false
	}
	lower := strings.ToLower(answer)
	var exact listRow
	exactMatches := 0
	for _, r := range rows {
		if lower == strings.ToLower(r.SessionID) || lower == strings.ToLower(r.ShortID) {
			exact = r
			exactMatches++
		}
	}
	if exactMatches > 0 {
		return exact, exactMatches == 1
	}
	if n, err := strconv.Atoi(answer); err == nil && len(answer) < minShortSessionID {
		if n >= 1 && n <= len(rows) {
			return rows[n-1], true
		}
		return listRow{}, false
	}
	var match listRow
	matches := 0
	for _, r := range rows {
		id := strings.ToLower(r.SessionID)
		if strings.HasPrefix(id, lower) {
			match = r
			matches++
		}
	}
	if matches == 1 {
		return match, true
	}
	return listRow{}, false
}

// readSessionView reads one session's metadata sidecar and resolves its
// linked sessions' availability: what `show SESSION_ID` prints, without
// the source bundle.
func readSessionView(ctx context.Context, store storage.ObjectStore, harness, sessionID string) (sessionView, error) {
	key, err := locateMetadataKey(ctx, store, harness, sessionID)
	if err != nil {
		return sessionView{}, err
	}
	metadata, err := reader.ReadMetadata(ctx, store, key)
	if err != nil {
		return sessionView{}, err
	}
	return metadataWithLinks(ctx, store, metadata), nil
}

// loadSessionsForBrowse lists metadata with the same filters list uses, for
// interactive show and handoff. It reads every match, newest activity first,
// for the caller to scope and limit.
func loadSessionsForBrowse(env browseReadDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string) ([]archive.Metadata, error) {
	ctx, stopReads := readCommandContext(env)
	defer stopReads()
	sessions, err := reader.ListMetadataWithOptions(ctx, store, archiveSessionsPrefix, opts.filter, reader.ListOptions{Cache: listCache(env, opts.noCache), Skipped: warnSkippedSidecar(stderr, command)})
	if err != nil {
		return nil, err
	}
	sortByActivity(sessions)
	return filterListOrigin(sessions, opts.imported, opts.hookCaptured), nil
}

// archiveRows builds the choices of a browser over archived sessions, read
// without a limit: each scope's rows are its top-level sessions, newest first,
// cut to limit (0 for all). Subagents are dropped before the limit and
// counted in the footer; a parent carries a hint of how many it has.
func archiveRows(sessions []archive.Metadata, limit int, format listFormatOptions) scopeRowsFunc {
	format.Children = childCounts(sessions)
	return func(scope sessionScope) scopeView {
		v := topLevelView(sessions, scope, limit)
		return scopeView{rows: formatSessionRows(v.shown, format), total: v.total, truncated: v.truncated, hidden: v.hidden,
			search: archiveSearch(sessions, scope, format)}
	}
}

// archiveSearch lists the sessions of a scope for the filter to search: every
// top-level session, numbered as the table numbers them (past its limit too),
// then every subagent, which has no number.
func archiveSearch(sessions []archive.Metadata, scope sessionScope, format listFormatOptions) func() []listRow {
	return func() []listRow {
		scoped := scope.filter(sessions)
		rows := formatSessionRows(topLevelSessions(scoped), format)
		subagents := formatSessionRows(subagentSessions(scoped), format)
		for i := range subagents {
			subagents[i].Index = 0
		}
		return append(rows, subagents...)
	}
}

// findBrowseSessions loads what bare show offers, in the working directory's
// scope first. ok is false, with code 0, when none match (after saying so),
// and with code 1 after an error.
func findBrowseSessions(env sessionSelectionDependencies, store storage.ObjectStore, cfg config.Config, stdout, stderr io.Writer, harness, command string) (choices *scopeChoices, ok bool, code int) {
	scope, err := scopeFor(env, "", false)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %s: %v\n", command, err)
		return nil, false, 1
	}
	stopBrowse := startActivity(stdout, "Finding sessions…")
	sessions, err := loadSessionsForBrowse(env, store, listOptions{filter: reader.Filter{Harness: harness, Replays: reader.ReplaysHidden}}, stderr, command)
	stopBrowse()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %s: %v\n", command, err)
		return nil, false, 1
	}
	format := listFormatOptions{Now: env.now(), Projects: projectLabels(cfg), Style: styleFor(stdout), GroupByProject: true, Numbered: true}
	choices = newScopeChoices(scope, format, false, archiveRows(sessions, defaultListLimit, format))
	if len(choices.shown().rows) == 0 {
		terminal.Println(stdout, "No archived sessions match.")
		return nil, false, 0
	}
	return choices, true, 0
}

// archivedSessionDependencies is what `show --json`'s picker uses: the
// archive's listing and the browser.
type archivedSessionDependencies interface {
	sessionSelectionDependencies
	sessionBrowserDependencies
}

// selectArchivedSession is `show --json`'s one-shot picker: the browser, which
// returns the session chosen; handoff's also lists local sessions
// (selectHandoffSession). verb is what Enter does, as the heading says it. It
// returns selected=false when the archive is empty or the user quits.
func selectArchivedSession(env archivedSessionDependencies, store storage.ObjectStore, cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, harness, command, verb string) (row listRow, selected bool, code int) {
	choices, ok, code := findBrowseSessions(env, store, cfg, stdout, stderr, harness, command)
	if !ok {
		return listRow{}, false, code
	}
	return runBrowser(context.Background(), env, newPrompter(stdin, stdout), stdout, stderr, browserSpec{Mode: pickSession, Verb: verb, Choices: choices, Command: command})
}

// saveTerminalState records the terminal modes of in, when it is a
// terminal, and returns a function that restores them.
func saveTerminalState(in io.Reader) (restore func()) {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return func() {}
	}
	state, err := term.GetState(int(file.Fd()))
	if err != nil {
		return func() {}
	}
	return func() { _ = term.Restore(int(file.Fd()), state) }
}
