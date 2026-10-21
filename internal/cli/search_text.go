package cli

import (
	"strings"
	"unicode"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/search"
)

// This file lays out matched text for the search browser: the passage with
// every matched query term highlighted (search.MatchSpans, the index's own
// tokens), wrapped to the terminal. It does no I/O.

// match paints a matched query term: bold yellow, or plain without color.
func (s textStyle) match(text string) string { return s.paint("1;33", text) }

// searchRole names who wrote a matching passage, as the browser labels it:
// "you" for the person, the agent's short name ("claude") for its replies,
// and "title" for a match in the session's title.
func searchRole(role, harness string) string {
	switch passageRole(role) {
	case passageUser:
		return "you"
	case passageAssistant:
		_, name := agentLabel(harness)
		return strings.ToLower(name)
	case passageTitle:
		return "title"
	}
	return archive.DisplayLine(role)
}

// passageRole is the role the index records for a passage.
type passageRole string

const (
	passageUser      passageRole = "user"
	passageAssistant passageRole = "assistant"
	// passageTitle is a title match's: it has no message.
	passageTitle passageRole = ""
)

// passageCell is one character of a passage as a terminal shows it: made
// safe to print (archive.DisplayLine), its width, and whether it is part of a
// matched term. A space or a line break is a cell of its own.
type passageCell struct {
	text    string
	width   int
	hit     bool
	space   bool
	newline bool
}

// passageCells is text as cells, those in spans marked as matched. With
// keepLines unset, every run of whitespace is one space, so the text reads as
// one paragraph; set, line breaks are kept and a tab reaches the next tab
// stop.
func passageCells(text string, spans []search.Span, keepLines bool) []passageCell {
	var cells []passageCell
	next := 0
	for i, r := range text {
		for next < len(spans) && spans[next].End <= i {
			next++
		}
		hit := next < len(spans) && spans[next].Start <= i
		switch {
		case r == '\n' || r == '\r' || r == ' ' || r == ' ':
			if keepLines {
				if r == '\n' && i > 0 && text[i-1] == '\r' {
					continue
				}
				cells = append(cells, passageCell{newline: true})
				continue
			}
			fallthrough
		case r == '\t' && keepLines:
			// To the next tab stop, as a terminal sets it.
			for column := lineColumns(cells); ; column++ {
				cells = append(cells, passageCell{text: " ", width: 1, hit: hit, space: true})
				if (column+1)%passageTabStop == 0 {
					break
				}
			}
		case unicode.IsSpace(r):
			if !keepLines && (len(cells) == 0 || cells[len(cells)-1].space) {
				continue
			}
			cells = append(cells, passageCell{text: " ", width: 1, hit: hit, space: true})
		default:
			shown := archive.DisplayLine(string(r))
			if shown == "" {
				continue
			}
			cells = append(cells, passageCell{text: shown, width: visibleWidth(shown), hit: hit})
		}
	}
	for len(cells) > 0 && cells[len(cells)-1].space {
		cells = cells[:len(cells)-1]
	}
	return cells
}

// passageTabStop is how many columns apart a passage's tab stops are.
const passageTabStop = 8

// lineColumns is how many columns the cells take since their last line
// break.
func lineColumns(cells []passageCell) int {
	columns := 0
	for i := len(cells) - 1; i >= 0 && !cells[i].newline; i-- {
		columns += cells[i].width
	}
	return columns
}

// cellRange is the cells [start, end) of one wrapped line.
type cellRange struct {
	start int
	end   int
}

// wrapCells breaks cells into lines at most width columns wide: at spaces,
// which a break drops, at line breaks, and inside a word too long for a line
// of its own. A width of 0 or less breaks only at line breaks.
func wrapCells(cells []passageCell, width int) []cellRange {
	var lines []cellRange
	start, used := 0, 0
	lastSpace := -1
	for i := range cells {
		c := cells[i]
		switch {
		case c.newline:
			lines = append(lines, cellRange{start, i})
			start, used, lastSpace = i+1, 0, -1
			continue
		case width > 0 && used > 0 && used+c.width > width && !c.space:
			if lastSpace > start {
				lines = append(lines, cellRange{start, lastSpace})
				start = lastSpace + 1
			} else {
				lines = append(lines, cellRange{start, i})
				start = i
			}
			used, lastSpace = 0, -1
			for j := start; j < i; j++ {
				used += cells[j].width
			}
		}
		if c.space {
			lastSpace = i
		}
		used += c.width
	}
	if start < len(cells) || len(lines) == 0 {
		lines = append(lines, cellRange{start, len(cells)})
	}
	for i, l := range lines {
		for l.end > l.start && cells[l.end-1].space {
			l.end--
		}
		lines[i] = l
	}
	return lines
}

// renderCells is cells as text, each run of matched cells painted with
// style.match.
func renderCells(cells []passageCell, style textStyle) string {
	var b, run strings.Builder
	hit := false
	flush := func() {
		if hit {
			b.WriteString(style.match(run.String()))
		} else {
			b.WriteString(run.String())
		}
		run.Reset()
	}
	for _, c := range cells {
		if c.newline {
			continue
		}
		if c.hit != hit {
			flush()
			hit = c.hit
		}
		run.WriteString(c.text)
	}
	flush()
	return b.String()
}

// passageWindow is at most lines lines of text, as one paragraph wrapped to
// width, that show its first matched term: from the start when it is that
// close, else from the line before it, led by "…". A window that stops before
// the end of the text ends with "…"; when that would cover the match, the
// window starts at the match's line instead.
func passageWindow(query, text string, width, lines int, style textStyle) []string {
	if lines <= 0 {
		return nil
	}
	if width <= 0 {
		width = unknownRuleWidth * 2
	}
	cells := passageCells(text, search.MatchSpans(query, text), false)
	wrapped := wrapCells(cells, width)
	first := 0
	for i, l := range wrapped {
		if hasHit(cells[l.start:l.end]) {
			first = i
			break
		}
	}
	from := 0
	if first >= lines {
		from = first - 1
	}
	out, shown := windowFrom(cells, wrapped, from, width, lines, style)
	if !shown && from < first {
		out, _ = windowFrom(cells, wrapped, first, width, lines, style)
	}
	return out
}

// windowFrom is passageWindow's lines starting at the wrapped line from, led
// by "…" past the first, and whether they show a matched cell.
func windowFrom(cells []passageCell, wrapped []cellRange, from, width, lines int, style textStyle) (out []string, shown bool) {
	if from > 0 {
		lead := []passageCell{{text: "…", width: 1}, {text: " ", width: 1, space: true}}
		cells = append(lead, cells[wrapped[from].start:]...)
		wrapped = wrapCells(cells, width)
	}
	cut := len(wrapped) > lines
	wrapped = wrapped[:min(len(wrapped), lines)]
	for i, l := range wrapped {
		line := cells[l.start:l.end]
		if cut && i == len(wrapped)-1 {
			used := 0
			for _, c := range line {
				used += c.width
			}
			for len(line) > 0 && (used+2 > width || line[len(line)-1].space) {
				used -= line[len(line)-1].width
				line = line[:len(line)-1]
			}
			line = append(append([]passageCell{}, line...), passageCell{text: " ", width: 1, space: true}, passageCell{text: "…", width: 1})
		}
		shown = shown || hasHit(line)
		out = append(out, renderCells(line, style))
	}
	return out, shown
}

func hasHit(cells []passageCell) bool {
	for _, c := range cells {
		if c.hit {
			return true
		}
	}
	return false
}

// highlightedLine is text as one line (whitespace runs as one space) with
// every term of query highlighted; the caller cuts it to the terminal.
func highlightedLine(query, text string, style textStyle) string {
	return renderCells(passageCells(text, search.MatchSpans(query, text), false), style)
}
