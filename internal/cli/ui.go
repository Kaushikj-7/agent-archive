package cli

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
	"golang.org/x/text/width"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// The terminal UI's vocabulary. Color is a role, never a meaning on its own:
// every colored element also carries a symbol or words that say the same
// thing, a line has at most one colored element, and it is the symbol that
// is colored rather than the sentence after it.
//
//	ok    green  ✓  done, nothing to do
//	warn  yellow !  needs you
//	fail  red    ✗  blocked
//	cmd   cyan      something the user types or opens
//	dim             secondary detail
//	bold            structure: headings, questions
const (
	symbolOK   = "✓"
	symbolWarn = "!"
	symbolFail = "✗"
)

// textStyle renders the terminal UI for one output stream. The zero value
// is plain: no color, no redrawing, no wrapping. That is what redirected
// output and tests see.
type textStyle struct {
	// color allows ANSI color and emphasis.
	color bool
	// live allows redrawing a line in place, as the spinner does.
	live bool
	// width is the column count to wrap to; 0 means don't wrap.
	width int
}

// colorOutput is an output that says itself whether it is a color terminal.
// The screen goldens write to one, so they can record the colored screens.
type colorOutput interface{ colorTerminal() bool }

// styleFor returns the style for writing to out: plain unless out is a
// terminal, and without color when NO_COLOR is set or TERM is dumb.
func styleFor(out io.Writer) textStyle {
	out = underlyingWriter(out)
	if c, ok := out.(promptOutput); ok {
		capabilities := c.promptCapabilities()
		return textStyle{color: capabilities.Color, live: capabilities.Redraw, width: capabilities.Width}
	}
	if c, ok := out.(colorOutput); ok {
		return textStyle{color: c.colorTerminal()}
	}
	file, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return textStyle{}
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		width = 0
	}
	return terminalStyle(os.Getenv, width)
}

// terminalSize is the size of the terminal out writes to: ok is false when
// out (unwrapped from a lockedWriter) is not a terminal or its size cannot
// be read.
func (e Env) terminalSize(out io.Writer) (width, height int, ok bool) {
	if e.TerminalSize != nil {
		return e.TerminalSize(out)
	}
	file, isFile := underlyingWriter(out).(*os.File)
	if !isFile || !term.IsTerminal(int(file.Fd())) {
		return 0, 0, false
	}
	width, height, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// displayLines is how many terminal rows text takes when printed on a
// terminal width columns wide: each line takes at least one row, and a
// line wider than the terminal wraps onto more. Color codes take no room.
// A width of 0 or less means lines never wrap.
func displayLines(text string, width int) int {
	if text == "" {
		return 0
	}
	n := 0
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		n += lineRows(line, width)
	}
	return n
}

// lineRows is how many terminal rows one line takes, as displayLines.
func lineRows(line string, width int) int {
	rows, _ := lineMetrics(line, width)
	return rows
}

// lineMetrics keeps the actual ending column as well as the wrapped row count.
func lineMetrics(line string, width int) (int, int) {
	if width <= 0 {
		return 1, visibleWidth(line)
	}
	rows, column := 1, 0
	line = ansiEscape.ReplaceAllString(line, "")
	for line != "" {
		r, size := utf8.DecodeRuneInString(line)
		w := runeWidthBefore(r, line[size:])
		// A wide glyph wraps before the last cell, leaving that cell unused.
		// Total visible width alone therefore undercounts some wrapped lines.
		if w > 0 && column > 0 && column+w > width {
			rows++
			column = 0
		}
		column += w
		line = line[size:]
	}
	return rows, column
}

// terminalStyle is the style for a terminal width columns wide, given the
// process environment. A dumb terminal gets neither color nor redrawing.
func terminalStyle(getenv func(string) string, width int) textStyle {
	if getenv("TERM") == "dumb" {
		return textStyle{width: width}
	}
	return textStyle{color: getenv("NO_COLOR") == "", live: true, width: width}
}

func (s textStyle) paint(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// ok marks something done that needs nothing more.
func (s textStyle) ok(text string) string { return s.paint("32", text) }

// warn marks something that needs the user.
func (s textStyle) warn(text string) string { return s.paint("33", text) }

// fail marks something that blocks.
func (s textStyle) fail(text string) string { return s.paint("31", text) }

// cmd marks a command, slash command, or link the user types or opens.
// Paths are not commands: show them with displayPath, dim when secondary.
func (s textStyle) cmd(text string) string { return s.paint("36", text) }

// dim marks secondary detail.
func (s textStyle) dim(text string) string { return s.paint("2", text) }

// bold marks structure: headings and questions.
func (s textStyle) bold(text string) string { return s.paint("1", text) }

// okMark, warnMark and failMark are the colored symbols a status line
// starts with.
func (s textStyle) okMark() string { return s.ok(symbolOK) }

func (s textStyle) warnMark() string { return s.warn(symbolWarn) }

func (s textStyle) failMark() string { return s.fail(symbolFail) }

// hang writes prefix and text, wrapping text to the style's width with each
// following line indented to where text started, so a list item's text
// lines up under itself:
//
//	! The bucket looks public. Fix its access before
//	  archiving.
//
// Line breaks in text are kept and indented the same way. A word too long
// for a line, such as a link, gets a line of its own and is never split.
func (s textStyle) hang(prefix, text string) string {
	return hangingIndent(prefix, text, s.width)
}

func hangingIndent(prefix, text string, width int) string {
	indent := strings.Repeat(" ", visibleWidth(prefix))
	var b strings.Builder
	for i, paragraph := range strings.Split(text, "\n") {
		lead := prefix
		if i > 0 {
			b.WriteString("\n")
			lead = indent
		}
		if width <= 0 {
			b.WriteString(lead + paragraph)
			continue
		}
		b.WriteString(lead)
		column := visibleWidth(lead)
		// The spacing before each word is kept as written, including a
		// paragraph's own indentation, except where the line breaks.
		gapStart := 0
		for _, span := range wordSpan.FindAllStringIndex(paragraph, -1) {
			gap, word := paragraph[gapStart:span[0]], paragraph[span[0]:span[1]]
			gapStart = span[1]
			w := visibleWidth(word)
			if column > len(indent) && column+visibleWidth(gap)+w > width {
				b.WriteString("\n" + indent)
				column = len(indent)
				gap = ""
			}
			b.WriteString(gap + word)
			column += visibleWidth(gap) + w
		}
	}
	return b.String()
}

// wordSpan matches a run of text between spaces.
var wordSpan = regexp.MustCompile(`[^ ]+`)

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// ansiReset matches a color code that turns color off.
var ansiReset = regexp.MustCompile("^\x1b\\[0*m$")

// visibleWidth is the number of columns text takes on a terminal: wide
// characters such as CJK take two, combining marks none, a narrow character
// that a variation selector turns into an emoji (a heart, then U+FE0F) two,
// and color codes none.
func visibleWidth(text string) int {
	n := 0
	text = ansiEscape.ReplaceAllString(text, "")
	for text != "" {
		r, size := utf8.DecodeRuneInString(text)
		n += runeWidthBefore(r, text[size:])
		text = text[size:]
	}
	return n
}

func runeWidth(r rune) int {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) {
		return 0
	}
	if kind := width.LookupRune(r).Kind(); kind == width.EastAsianWide || kind == width.EastAsianFullwidth {
		return 2
	}
	return 1
}

// emojiSelector is the variation selector that asks for a character's emoji
// form, U+FE0F ("VS16").
const emojiSelector = '️'

// runeWidthBefore is the columns r takes when rest is the text after it: as
// runeWidth says, except that a narrow, non-ASCII character followed by
// emojiSelector is drawn as a two-column emoji (❤️, ✔️, 🖥️), which runeWidth
// alone would count as one. The selector itself takes none. ASCII is left
// alone: "1" and U+FE0F is not something a terminal draws wide.
//
// Terminals do not agree about these; the ones that draw them narrow show a
// line up to a column shorter than counted, where the reverse would wrap it.
func runeWidthBefore(r rune, rest string) int {
	w := runeWidth(r)
	if w == 1 && r > unicode.MaxASCII {
		if next, _ := utf8.DecodeRuneInString(rest); next == emojiSelector {
			return 2
		}
	}
	return w
}

// truncateVisible cuts text to at most limit columns. Color codes are kept
// whole, and a cut through colored text ends with a reset so the color
// does not run on.
func truncateVisible(text string, limit int) string {
	if visibleWidth(text) <= limit {
		return text
	}
	var b strings.Builder
	column := 0
	colored := false
	for text != "" {
		if loc := ansiEscape.FindStringIndex(text); loc != nil && loc[0] == 0 {
			code := text[:loc[1]]
			b.WriteString(code)
			colored = !ansiReset.MatchString(code)
			text = text[loc[1]:]
			continue
		}
		r, size := utf8.DecodeRuneInString(text)
		w := runeWidthBefore(r, text[size:])
		if column+w > limit {
			break
		}
		b.WriteString(text[:size])
		column += w
		text = text[size:]
	}
	if colored {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerInterval = 100 * time.Millisecond

// spinner animates a label on one line while something slow runs. Stop it
// before writing anything else to the same stream.
type spinner struct {
	done     chan struct{}
	finished chan struct{}
	once     sync.Once
}

// spin starts a spinner with label on out. Where the style cannot redraw a
// line (output is not a terminal, or TERM is dumb) it prints nothing at all,
// so the caller's result line is the only trace.
func (s textStyle) spin(out io.Writer, label string) *spinner {
	return s.spinEvery(out, label, spinnerInterval)
}

func (s textStyle) spinEvery(out io.Writer, label string, every time.Duration) *spinner {
	return s.spinLabelEvery(out, func() string { return label }, every)
}

// spinLabelEvery is spinEvery for a label that changes while it runs (a
// count of what is done): label is asked for on every frame, from the
// spinner's goroutine, and must be safe for that. A frame that is shorter
// than the last is padded so nothing of the last is left behind.
func (s textStyle) spinLabelEvery(out io.Writer, label func() string, every time.Duration) *spinner {
	sp := &spinner{}
	if !s.live {
		return sp
	}
	sp.done = make(chan struct{})
	sp.finished = make(chan struct{})
	go func() {
		defer close(sp.finished)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		lastWidth := 0
		for frame := 0; ; frame++ {
			text := label()
			// A label wider than the terminal would wrap, and "\r" could not
			// take the spinner's line back.
			if s.width > 2 {
				text = truncateVisible(text, s.width-3)
			}
			width := visibleWidth(text)
			terminal.Printf(out, "\r%s %s%s", s.dim(spinnerFrames[frame%len(spinnerFrames)]), text, strings.Repeat(" ", max(lastWidth-width, 0)))
			lastWidth = width
			select {
			case <-sp.done:
				terminal.Print(out, "\r\x1b[K")
				return
			case <-ticker.C:
			}
		}
	}()
	return sp
}

// stop ends the animation and clears its line, leaving the cursor at the
// start of it for the result. It returns once the spinner has stopped
// writing, and may be called more than once.
func (sp *spinner) stop() {
	if sp.done == nil {
		return
	}
	sp.once.Do(func() { close(sp.done) })
	<-sp.finished
}

// activityStyle is styleFor after unwrapping a lockedWriter, so a spinner
// still detects the real terminal when stdout is wrapped for interrupt
// messages.
func activityStyle(out io.Writer) textStyle {
	return styleFor(underlyingWriter(out))
}

// startActivity runs a spinner with label on a live terminal. Elsewhere it
// prints nothing: the caller's result line is the only trace, matching spin.
// Call stop before writing anything else to out.
func startActivity(out io.Writer, label string) (stop func()) {
	span := trace.Start(spinnerSpan)
	sp := activityStyle(out).spin(out, label)
	return func() {
		sp.stop()
		span.End()
	}
}

// pausingWriter calls stop before every write it passes on to w, so a
// spinner on the same terminal is gone before a message or a prompt is
// written there. stop must be safe to call more than once.
type pausingWriter struct {
	w    io.Writer
	stop func()
}

func (p *pausingWriter) Write(b []byte) (int, error) {
	p.stop()
	return p.w.Write(b)
}

// spinnerSpan names the trace span of an activity spinner: how long a
// command was loading before it drew anything, such as the handoff picker's
// list, which is otherwise hard to read off a trace whose root span also
// holds the time a person spent choosing.
const spinnerSpan = "spinner (until first draw)"

// startAnnouncedActivity is startActivity, but on a non-live stream it prints
// label once so scripts and CI still see what is running.
func startAnnouncedActivity(out io.Writer, label string) (stop func()) {
	span := trace.Start(spinnerSpan)
	style := activityStyle(out)
	if !style.live {
		terminal.Println(out, label)
		return span.End
	}
	sp := style.spin(out, label)
	return func() {
		sp.stop()
		span.End()
	}
}

// displayPath shows path with the home directory as ~.
func displayPath(path, home string) string {
	if !local.PathWithin(path, home) {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil {
		return path
	}
	if rel == "." {
		return "~"
	}
	return filepath.Join("~", rel)
}
