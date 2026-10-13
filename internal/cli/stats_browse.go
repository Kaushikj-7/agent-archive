package cli

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Terminal control sequences the stats screen writes on top of altScreen's:
// the cursor is hidden while a view is on show and shown while a file name is
// typed, and always shown again as the screen is left.
const (
	hideCursorSequence = "\x1b[?25l"
	showCursorSequence = "\x1b[?25h"
)

// statsWindowChoices are the windows w cycles through, in days.
var statsWindowChoices = []int{7, 30, 90}

// statsWindowCycle is the windows w cycles through, and where start (the
// window --days or --since asked for) is among them. A start that is not one
// of the usual windows is a custom entry between them, in order: --days 14
// cycles 7, 14, 30, 90.
func statsWindowCycle(start int) (windows []int, index int) {
	windows = slices.Clone(statsWindowChoices)
	if !slices.Contains(windows, start) {
		windows = append(windows, start)
		slices.Sort(windows)
	}
	return windows, slices.Index(windows, start)
}

// statsInputs is what the interactive screen recomputes the numbers from
// when the window changes: the sessions read once, and how to count them.
type statsInputs struct {
	mcpServerNames map[string]string
	prepared       *stats.Prepared
	hasSessions    bool
	now            time.Time
	location       *time.Location
	filters        statsFilters
}

// compute is the numbers for a window of days ending today. A screen lists
// every project and skill (allRows); the web page keeps the engine's default
// lists, as --html does.
func (in statsInputs) compute(days int, allRows bool) stats.Stats {
	return in.prepared.Compute(stats.Options{
		Now: in.now, Days: days, AllRows: allRows, MCPServerNames: in.mcpServerNames,
	})
}

// statsBrowserStart is what runStatsBrowser opens with.
type statsBrowserStart struct {
	inputs statsInputs
	// windows are the windows w cycles through, and window the one on show
	// first; first is that window's numbers, already computed.
	windows []int
	window  int
	first   stats.Stats
	view    statsView
}

// statsMode is what the stats screen is showing and reading keys for.
type statsMode int

const (
	// statsViewing is one of the views.
	statsViewing statsMode = iota
	// statsHelp is the key list, over the view.
	statsHelp
	// statsSaving is the file name prompt of h.
	statsSaving
)

// statsBrowser is the interactive stats screen: the views of the static
// command on the terminal's alternate screen, cut to the window and scrolled,
// with a bar of the keys on the last row. It switches views and windows
// without reading the archive again, from the sessions it was started with.
type statsBrowser struct {
	env     statsBrowserDependencies
	stdout  io.Writer
	screen  *altScreen
	keys    *keyInput
	inputs  statsInputs
	view    statsView
	windows []int
	window  int
	page    statsPage
	// shown are the numbers of each window already computed, by its days.
	shown map[int]stats.Stats
	mode  statsMode
	top   int
	// pageTop is the view's scroll position while the help is open.
	pageTop int
	// message is a line that replaces the key bar until the next key: what
	// was saved, or why not.
	message string
	// typed is the file name typed so far at the save prompt.
	typed string
	// saved are the files h wrote, printed once the screen is left so their
	// paths stay in the scrollback, whether it ends by a quit or by a signal
	// (which runs on another goroutine, so the list has its own lock).
	savedMu sync.Mutex
	saved   []string
	// wrote makes the paths print once, whichever way out gets there first.
	wrote sync.Once
	// drawn is the lines of the last content laid out, and what they were
	// laid out for.
	drawn    []string
	drawnFor statsContent
}

// statsContent names what a set of laid out lines shows, so a burst of
// scrolling does not lay a view out again for every key.
type statsContent struct {
	mode  statsMode
	page  statsPage
	days  int
	width int
}

// runStatsBrowser shows the interactive screen until the person quits. ran is
// false, with nothing drawn, when keys cannot be read from stdin, so the
// caller prints the static screen instead. code is the command's exit code.
//
// The terminal is given back on every way out: quitting, end of input, an
// error, a panic (the deferred calls), and a signal (altScreen's handler,
// which restores it before the process exits). Ctrl-C is a signal, as it is
// in the session browser: ISIG stays on in key mode.
func runStatsBrowser(env statsBrowserDependencies, stdin io.Reader, stdout, stderr io.Writer, start statsBrowserStart) (code int, ran bool) {
	term, ok := env.openKeyTerminal(stdin)
	if !ok {
		return 0, false
	}
	screen := enterAltScreen(stdout, env)
	if !screen.clears() {
		return 0, false
	}
	keys := attachKeys(term, screen)
	if keys == nil {
		screen.leave()
		return 0, false
	}
	// The cursor is hidden while a view is on show: shown again before the
	// process stops for Ctrl-Z, and as the screen is left.
	keys.hide = func() {
		terminal.Print(stdout, showCursorSequence)
		screen.hide()
	}
	restored := false
	screen.restoreOnLeave(func() {
		keys.close()
		// leave calls this each time it is called: the cursor is shown once.
		if !restored {
			restored = true
			terminal.Print(stdout, showCursorSequence)
		}
	})
	// Deferred as well, so not even a panic leaves the alternate screen, or
	// the terminal without echo.
	defer screen.leave()
	defer keys.close()
	view := start.view
	view.interactive = true
	b := &statsBrowser{
		env: env, stdout: stdout, screen: screen, keys: keys, inputs: start.inputs, view: view,
		windows: start.windows, window: start.window, page: pageOverview,
		shown: map[int]stats.Stats{start.windows[start.window]: start.first},
	}
	screen.printAfterSignal(b.printSaved)
	err := b.run()
	screen.leave()
	b.printSaved()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1, true
	}
	return 0, true
}

// addSaved records a file h wrote.
func (b *statsBrowser) addSaved(path string) {
	b.savedMu.Lock()
	defer b.savedMu.Unlock()
	b.saved = append(b.saved, path)
}

// printSaved says where the pages h saved went, once, after the screen is
// left: on the normal screen, so the paths stay in the scrollback.
func (b *statsBrowser) printSaved() {
	b.wrote.Do(func() {
		b.savedMu.Lock()
		paths := slices.Clone(b.saved)
		b.savedMu.Unlock()
		for _, path := range paths {
			terminal.Printf(b.stdout, "Wrote %s\n", path)
		}
	})
}

// run draws the screen and applies keys until one quits. A burst of keys
// (a wheel's, say) is applied before the one redraw.
func (b *statsBrowser) run() error {
	for {
		b.draw()
		for {
			k, err := b.keys.next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if b.handle(k) {
				return nil
			}
			if !b.keys.buffered() {
				break
			}
		}
	}
}

// current is the numbers of the window on show, computed once.
func (b *statsBrowser) current() stats.Stats {
	days := b.windows[b.window]
	if s, ok := b.shown[days]; ok {
		return s
	}
	s := b.inputs.compute(days, true)
	b.shown[days] = s
	return s
}

// handle applies one key and reports whether it quits.
func (b *statsBrowser) handle(k key) (quit bool) {
	if k.kind == keyResize {
		// Drawn again at the new size, with the message still on it.
		return false
	}
	b.message = ""
	switch b.mode {
	case statsHelp:
		b.helpKey(k)
	case statsSaving:
		b.savingKey(k)
	case statsViewing:
		return b.viewKey(k)
	}
	return false
}

// viewKey applies a key at a view: scrolling, another view or window, help, h,
// or q and Ctrl-D to quit. Esc does not quit: on a slow link an arrow key can
// arrive in two reads, and the first is a lone Esc (the session browser's Esc
// does not go back for the same reason).
func (b *statsBrowser) viewKey(k key) (quit bool) {
	if move, ok := scrollKeys[k.kind]; ok {
		b.scroll(move)
		return false
	}
	switch k.kind {
	case keyEndOfInput:
		return true
	case keyRune:
		return b.viewRune(unicode.ToLower(k.r))
	case keyEscape, keyEnter, keyBackspace, keyLeft, keyRight, keyUp, keyDown, keyPageUp, keyPageDown, keyHome, keyEnd, keyResize:
		// Scrolled above, or nothing to do.
	}
	return false
}

// statsViewKeys are the keys that switch views.
var statsViewKeys = map[rune]statsPage{
	'o': pageOverview,
	'd': pageDetail,
	'p': pageProjects,
	'm': pageModels,
	'a': pageAgents,
}

// viewRune applies a typed character at a view.
func (b *statsBrowser) viewRune(r rune) (quit bool) {
	if page, ok := statsViewKeys[r]; ok {
		b.page, b.top = page, 0
		return false
	}
	switch r {
	case 'q':
		return true
	case 'w':
		// The other window's numbers are counted from the sessions already
		// read; the scroll position is kept, and clamped to the new page.
		b.window = (b.window + 1) % len(b.windows)
	case 'h':
		b.mode, b.typed = statsSaving, ""
	case '?':
		b.mode, b.pageTop, b.top = statsHelp, b.top, 0
	case ' ':
		b.scroll(scrollPageDown)
	case 'j':
		b.scroll(scrollLineDown)
	case 'k':
		b.scroll(scrollLineUp)
	}
	return false
}

// helpKey applies a key while the help is open: scrolling scrolls it, any
// other key closes it.
func (b *statsBrowser) helpKey(k key) {
	if move, ok := scrollKeys[k.kind]; ok {
		b.scroll(move)
		return
	}
	if k.kind == keyRune && k.r == ' ' {
		b.scroll(scrollPageDown)
		return
	}
	b.mode, b.top = statsViewing, b.pageTop
}

// maxTypedName bounds the file name typed at the save prompt.
const maxTypedName = 255

// savingKey applies a key at the save prompt: typing and Backspace edit the
// name, Enter saves, and Esc cancels.
func (b *statsBrowser) savingKey(k key) {
	switch k.kind {
	case keyEnter:
		b.mode = statsViewing
		b.message = b.saveHTML(b.typed)
	case keyEscape, keyEndOfInput:
		b.mode, b.message = statsViewing, "Not saved."
	case keyBackspace:
		_, size := utf8.DecodeLastRuneInString(b.typed)
		b.typed = b.typed[:len(b.typed)-size]
	case keyRune:
		if unicode.IsPrint(k.r) && utf8.RuneCountInString(b.typed) < maxTypedName {
			b.typed += string(k.r)
		}
	case keyUp, keyDown, keyLeft, keyRight, keyPageUp, keyPageDown, keyHome, keyEnd, keyResize:
		// Nothing to do at a prompt for a name.
	}
}

// scroll moves the top line of what is on show, within its lines.
func (b *statsBrowser) scroll(move scrollMove) {
	l := b.layout()
	maxTop := max(len(b.content(l.width))-l.body, 0)
	switch move {
	case scrollLineUp:
		b.top--
	case scrollLineDown:
		b.top++
	case scrollPageUp:
		b.top -= max(l.body-1, 1)
	case scrollPageDown:
		b.top += max(l.body-1, 1)
	case scrollTop:
		b.top = 0
	case scrollBottom:
		b.top = maxTop
	}
	b.top = min(max(b.top, 0), maxTop)
}

// statsLayout is the terminal's size and the rows left for what is on show
// once the last row is the key bar.
type statsLayout struct {
	width  int
	height int
	body   int
}

// layout reads the terminal's size, which may have changed since the last
// draw. An unknown size is the classic 80 by 24.
func (b *statsBrowser) layout() statsLayout {
	width, height, ok := b.env.terminalSize(b.stdout)
	if !ok {
		width, height = 80, 24
	}
	return statsLayout{width: width, height: height, body: max(height-1, 0)}
}

// content is the lines of what is on show laid out for width columns, none
// wider than that.
func (b *statsBrowser) content(width int) []string {
	want := statsContent{mode: b.mode, page: b.page, days: b.windows[b.window], width: width}
	if b.drawn != nil && b.drawnFor == want {
		return b.drawn
	}
	var lines []string
	if b.mode == statsHelp {
		lines = b.helpLines(width)
	} else {
		lines = b.pageLines(width)
	}
	for i, line := range lines {
		lines[i] = truncateVisible(line, width)
	}
	b.drawn, b.drawnFor = lines, want
	return lines
}

// pageLines is the view on show, as the static command prints it. A window
// with nothing in it says so, and how to look further back.
func (b *statsBrowser) pageLines(width int) []string {
	s := b.current()
	view := b.view
	view.width = width
	if s.Coverage.Sessions == 0 {
		message := statsEmptyMessageWith(s, b.inputs.filters, b.inputs.hasSessions, " Press w for another window.")
		return strings.Split(view.wrap(message), "\n")
	}
	return renderPage(b.page, s, view)
}

// draw shows the screen: the lines of what is on show from the top line on,
// as many as fit above the last row, and the last row (the message, the save
// prompt, or the key bar).
func (b *statsBrowser) draw() {
	l := b.layout()
	lines := b.content(l.width)
	maxTop := max(len(lines)-l.body, 0)
	b.top = min(max(b.top, 0), maxTop)
	var frame strings.Builder
	frame.WriteString(clearScreenSequence + hideCursorSequence)
	shown := lines[b.top:min(b.top+l.body, len(lines))]
	for _, line := range shown {
		frame.WriteString(line + "\n")
	}
	for range l.body - len(shown) {
		frame.WriteString("\n")
	}
	frame.WriteString(b.lastRow(l, len(lines), maxTop))
	if b.mode == statsSaving {
		frame.WriteString(showCursorSequence)
	}
	b.screen.draw(frame.String())
}

// lastRow is the screen's last row, one row wide: the file name prompt, a
// message, or the key bar with where the screen is in a view too tall for it.
func (b *statsBrowser) lastRow(l statsLayout, total, maxTop int) string {
	switch {
	case b.mode == statsSaving:
		return b.savePrompt(l.width)
	case b.message != "":
		return oneRow(b.message, l.width)
	}
	days, next := b.windows[b.window], b.windows[(b.window+1)%len(b.windows)]
	if b.mode == statsHelp {
		row := " Any key closes the help; Up, Down, PgUp and PgDn scroll it"
		if visibleWidth(row) > l.width {
			row = " Any key closes the help"
		}
		return b.view.style.dim(oneRow(row, l.width))
	}
	if maxTop == 0 || l.width < statsMinWidth {
		return statsKeyBar(b.view.style, b.page, days, next, l.width)
	}
	// A view too tall for the screen says where it is, and at the top that
	// there is more below; the bar shortens to leave room for that.
	position := "Top " + b.view.glyphs.moreBelow
	switch {
	case b.top >= maxTop:
		position = "End"
	case b.top > 0:
		position = strconv.Itoa((b.top+l.body)*100/total) + "%"
	}
	bar := statsKeyBar(b.view.style, b.page, days, next, l.width-visibleWidth(position)-1)
	room := l.width - visibleWidth(bar) - visibleWidth(position)
	return bar + strings.Repeat(" ", room) + b.view.style.dim(position)
}
