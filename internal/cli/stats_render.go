package cli

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Layout constants for the stats screens.
const (
	// statsFullWidth is the terminal width from which "where it went" is two
	// columns: the screen is designed for 80 columns.
	statsFullWidth = 80
	// statsBarWidth is the narrowest terminal that draws bars; below it the
	// lists are compact rows of a name and its number.
	statsBarWidth = 60
	// statsMaxWidth is the width the screens are laid out to on a wider
	// terminal, so lines stay readable.
	statsMaxWidth = 120
	// statsUnknownWidth is the width assumed for output that is not a
	// terminal (a pipe, a file): the full layout.
	statsUnknownWidth = 100
	// statsMinWidth is the narrowest terminal the screens adapt to.
	statsMinWidth = 40

	minLabelWidth = 8
	// minBarWidth is the narrowest a table's bar is shrunk to before it is
	// dropped.
	minBarWidth = 6
	// statsMaxBar is the widest a bar is drawn.
	statsMaxBar = 30

	statsNameLimit = 24
	// statsFilterLimit cuts a --harness or --model value in the heading.
	statsFilterLimit = 30
	// statsMaxListRows bounds a list view: a few hundred rows are already
	// more than a screen is read for; --json --all has every project, and
	// --json every model.
	statsMaxListRows = 500
	// statsMaxGroupRows bounds a --by day, week or month table.
	statsMaxGroupRows = 60
	// statsMaxUseRows bounds the skills and MCP lists of the detail view.
	statsMaxUseRows = 40
)

// statsGlyphs are the characters the screens draw with. Bars and charts use
// Unicode block characters; ASCII is the fallback for a locale that is not
// UTF-8 and for a dumb terminal.
type statsGlyphs struct {
	// block is a filled cell of a bar, and a full row of a chart.
	block string
	// spark are the partial cells of a chart: 1/8 to 8/8 of a row. The first
	// is also a day without spend: the baseline.
	spark [8]string
	// sparkUnknown marks a day whose spend is unknown.
	sparkUnknown string
	// moreBelow marks a screen with more below it, in the key bar.
	moreBelow string
	bullet    string
	up        string
	down      string
	sep       string
	ellipsis  string
}

var unicodeGlyphs = statsGlyphs{
	block:        "█",
	spark:        [8]string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"},
	sparkUnknown: "·",
	moreBelow:    "↓",
	bullet:       "●",
	up:           "▲",
	down:         "▼",
	sep:          "·",
	ellipsis:     "…",
}

var asciiGlyphs = statsGlyphs{
	block:        "#",
	spark:        [8]string{"_", ".", ":", "-", "=", "+", "*", "#"},
	sparkUnknown: "?",
	moreBelow:    "v",
	bullet:       "*",
	up:           "+",
	down:         "-",
	sep:          "-",
	ellipsis:     "...",
}

// statsPage is one of the screens: the default overview, and the views the
// interactive screen switches between (overview, detail, projects, models and
// agents). --view names one.
type statsPage string

// The stats pages.
const (
	pageOverview statsPage = "overview"
	pageDetail   statsPage = "detail"
	pageProjects statsPage = "projects"
	pageModels   statsPage = "models"
	pageAgents   statsPage = "agents"
)

// statsPages are the pages in the order they are named.
var statsPages = []statsPage{pageOverview, pageDetail, pageProjects, pageModels, pageAgents}

// parseStatsPage is the page a --view value names.
func parseStatsPage(name string) (statsPage, bool) {
	for _, page := range statsPages {
		if string(page) == name {
			return page, true
		}
	}
	return "", false
}

// statsView is how a stats page is drawn: the terminal's width and style, the
// characters to draw with, and the filters to name in the heading.
type statsView struct {
	style textStyle
	// width is the terminal's width in columns; 0 (output that is not a
	// terminal) means the full layout.
	width   int
	glyphs  statsGlyphs
	filters statsFilters
	// interactive is set for the screen that has a key bar of its own: the
	// footer then leaves out the flags that name the other views.
	interactive bool
}

// statsWidthOutput is an output that says its own width; the stats goldens
// write to one.
type statsWidthOutput interface{ terminalWidth() int }

// newStatsView is the view for writing to out: its style and width (a width
// of 0, for output that is not a terminal, means the full layout), and ASCII
// when the locale is not UTF-8.
func newStatsView(out io.Writer, env interface{ lookupEnv(string) (string, bool) }) statsView {
	style := styleFor(underlyingWriter(out))
	if w, ok := underlyingWriter(out).(statsWidthOutput); ok {
		style.width = w.terminalWidth()
	}
	glyphs := unicodeGlyphs
	if !localeIsUTF8(env) {
		glyphs = asciiGlyphs
	}
	return statsView{style: style, width: style.width, glyphs: glyphs}
}

// localeIsUTF8 reads the locale as the C library does: the first of
// LC_ALL, LC_CTYPE and LANG that is set names the character set. Nothing
// set, or a name with UTF-8 in it, is Unicode; "C" and "POSIX" and any other
// name are not. A dumb terminal is not either.
func localeIsUTF8(env interface{ lookupEnv(string) (string, bool) }) bool {
	if term, _ := env.lookupEnv("TERM"); term == "dumb" {
		return false
	}
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value, _ := env.lookupEnv(name); value != "" {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "utf-8") || strings.Contains(lower, "utf8")
		}
	}
	return true
}

// wrap breaks a message to the view's width (at most statsMaxWidth).
func (v statsView) wrap(text string) string {
	width := v.width
	if width <= 0 {
		width = statsUnknownWidth
	}
	return hangingIndent("", text, min(max(width, statsMinWidth), statsMaxWidth))
}

// statsPrinter builds a page line by line.
type statsPrinter struct {
	s     stats.Stats
	v     statsView
	g     statsGlyphs
	width int // the width the page is laid out to
}

func newStatsPrinter(s stats.Stats, v statsView) *statsPrinter {
	width := v.width
	if width <= 0 {
		width = statsUnknownWidth
	}
	return &statsPrinter{s: s, v: v, g: v.glyphs, width: min(max(width, statsMinWidth), statsMaxWidth)}
}

// renderStats writes a stats page for s to w. The lines are the page's:
// see renderPage.
func renderStats(w io.Writer, s stats.Stats, page statsPage, v statsView) error {
	for _, line := range renderPage(page, s, v) {
		terminal.Println(w, line)
	}
	return nil
}

// renderPage is a stats page as lines of text, styled when the view's style
// has color. It is pure: it reads nothing but its arguments, and prints
// nothing, so the interactive screen draws the same pages. No line is wider
// than the view's width (at least statsMinWidth), whatever the archive's
// names; the page is laid out for that width and cut to it as a last resort.
func renderPage(page statsPage, s stats.Stats, v statsView) []string {
	p := newStatsPrinter(s, v)
	var sections [][]string
	switch page {
	case pageDetail:
		sections = p.detailPage()
	case pageProjects:
		sections = p.projectsPage()
	case pageModels:
		sections = p.modelsPage()
	case pageAgents:
		sections = p.agentsPage()
	case pageOverview:
		sections = p.overviewPage()
	}
	var out []string
	for _, section := range sections {
		if len(section) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, section...)
	}
	for i, line := range out {
		out[i] = strings.TrimRight(truncateVisible(line, p.width), " ")
	}
	return out
}

func (p *statsPrinter) bold(text string) string { return p.role(roleEmphasis, text) }

func (p *statsPrinter) dim(text string) string { return p.role(roleLabel, text) }

// bars is whether the terminal is wide enough for bars.
func (p *statsPrinter) bars() bool { return p.width >= statsBarWidth }

// heading is a section's title, with an optional note aligned to the right
// edge of the given width.
func (p *statsPrinter) heading(title, note string, edge int) string {
	if note == "" {
		return p.bold(title)
	}
	gap := max(edge-visibleWidth(title)-visibleWidth(note), 2)
	return p.bold(title) + strings.Repeat(" ", gap) + note
}

// wrap breaks text to the screen's width.
func (p *statsPrinter) wrap(text string) []string {
	return strings.Split(hangingIndent("", text, p.width), "\n")
}

// hang prints text after a label, wrapping to the screen's width with the
// following lines indented under the text. The label is padded to labelW and
// styled by paint; text is plain.
func (p *statsPrinter) hang(label string, labelW int, text string, paint func(string) string) []string {
	prefix := padRight(label, labelW) + "  "
	lines := strings.Split(hangingIndent(prefix, text, p.width), "\n")
	lines[0] = paint(padRight(label, labelW)) + lines[0][len(padRight(label, labelW)):]
	return lines
}

// cut shortens text to at most limit columns, with an ellipsis when it cuts.
func (p *statsPrinter) cut(text string, limit int) string {
	if visibleWidth(text) <= limit {
		return text
	}
	dots := visibleWidth(p.g.ellipsis)
	if limit <= dots {
		return truncateVisible(text, limit)
	}
	return truncateVisible(text, limit-dots) + p.g.ellipsis
}

// clean is a name from the archive made safe to print.
func clean(text string) string { return archive.DisplayLine(text) }

// nameOf is a name from the archive made safe to print and cut to the length
// a name is shown at.
func nameOf(text string) string { return truncateVisible(archive.DisplayLine(text), statsNameLimit) }

// packItems lays items out in lines, joined by gap, breaking before an item
// that would pass width; following lines are indented. An item is never split.
func packItems(items []string, first, indent, gap, width int) []string {
	var lines []string
	var line strings.Builder
	column := first
	sep := strings.Repeat(" ", gap)
	for i, item := range items {
		w := visibleWidth(item)
		if i > 0 && column+gap+w > width {
			lines = append(lines, line.String())
			line.Reset()
			line.WriteString(strings.Repeat(" ", indent))
			column = indent
		} else if i > 0 {
			line.WriteString(sep)
			column += gap
		}
		line.WriteString(item)
		column += w
	}
	return append(lines, line.String())
}

// window names the window and the period before it as the screens do.
func (p *statsPrinter) windowNames() (this, prior string) {
	if p.s.Window.Days == 1 {
		return "today", "the day before"
	}
	return fmt.Sprintf("last %d days", p.s.Window.Days), fmt.Sprintf("prior %dd", p.s.Window.Days)
}

func (p *statsPrinter) header(page statsPage) []string {
	s := p.s
	this, _ := p.windowNames()
	parts := []string{"agent-archive stats"}
	if page != pageOverview {
		parts = append(parts, string(page))
	}
	parts = append(parts, this)
	if f := p.filterText(); f != "" {
		parts = append(parts, f)
	}
	agents := "1 agent"
	if s.Coverage.Agents != 1 {
		agents = fmt.Sprintf("%d agents", s.Coverage.Agents)
	}
	parts = append(parts, agents)
	sep := " " + p.g.sep + " "
	var lines []string
	line := ""
	for _, part := range parts {
		switch {
		case line == "":
			line = part
		case visibleWidth(line)+visibleWidth(sep)+visibleWidth(part) > p.width:
			lines = append(lines, line)
			line = part
		default:
			line += sep + part
		}
	}
	lines = append(lines, line)
	// The name is bold; what follows it is grey.
	title := "agent-archive stats"
	lines[0] = p.bold(title) + p.dim(strings.TrimPrefix(lines[0], title))
	for i := 1; i < len(lines); i++ {
		lines[i] = p.dim(lines[i])
	}
	return lines
}

// filterText names the filters the numbers were narrowed by.
func (p *statsPrinter) filterText() string {
	var parts []string
	f := p.v.filters
	if f.Harness != "" {
		parts = append(parts, "harness "+truncateVisible(clean(f.Harness), statsFilterLimit))
	}
	if f.Model != "" {
		parts = append(parts, "model "+truncateVisible(clean(f.Model), statsFilterLimit))
	}
	if f.Origin != "" {
		parts = append(parts, f.Origin+" only")
	}
	switch replaysFlag(f.Replays) {
	case replaysInclude:
		parts = append(parts, "replays included")
	case replaysOnly:
		parts = append(parts, "replays only")
	case replaysHide:
		// The default: nothing to name.
	}
	return strings.Join(parts, ", ")
}

// footer is the screen's last line: what the numbers are, and where to go
// next. It is one line when it fits; else the note has its own line(s) and
// the hints follow.
func (p *statsPrinter) footer(hints ...string) []string {
	note := "Estimated at list price, not a bill."
	if p.s.TokenObservations != nil {
		s := p.s.TokenObservations
		if s.Observed.Available() && s.Input != nil && s.Output != nil {
			note = fmt.Sprintf("Observed input %d, output %d across %d sessions; full usage, composition and cost unavailable. ", *s.Input, *s.Output, p.s.ObservationSessions) + note
		} else {
			note = "Token observations unavailable: exact aggregate limit. " + note
		}
	}
	if n := p.s.Coverage.ObservationOnlySessions; n > 0 {
		note += fmt.Sprintf(" Priced subset excludes %d observation-only sessions.", n)
	}
	if p.partialSpend() {
		if p.s.Coverage.ObservationOnlySessions == 0 || p.s.Overview.Cost.UnpricedTokens > 0 {
			note += " + leaves out models with no price."
		} else {
			note += " + marks partial priced coverage."
		}
	}
	if p.v.interactive {
		hints = nil
	}
	joined := strings.Join(hints, " "+p.g.sep+" ")
	if joined == "" {
		return p.dimAll(p.wrap(note))
	}
	if visibleWidth(note)+3+visibleWidth(joined) <= p.width {
		return []string{p.dim(note + "   " + joined)}
	}
	lines := p.wrap(note)
	if visibleWidth(joined) <= p.width {
		return p.dimAll(append(lines, joined))
	}
	return p.dimAll(append(lines, packItems(hints, 0, 0, 3, p.width)...))
}

func (p *statsPrinter) dimAll(lines []string) []string {
	for i := range lines {
		lines[i] = p.dim(lines[i])
	}
	return lines
}

// partialSpend is whether the headline spend leaves out tokens of models the
// price table does not list, which its "+" says.
func (p *statsPrinter) partialSpend() bool {
	o := p.s.Overview
	return o.Cost.Value != nil && o.Cost.Partial
}

// money is an amount in the price table's currency, without cents from ten up.
func (p *statsPrinter) money(usd float64) string {
	return statsfmt.Money(p.s.Prices.Currency, usd, false)
}

// estimate is money that is an estimate: a leading ~.
func (p *statsPrinter) estimate(usd float64) string { return "~" + p.money(usd) }

// spend is a table cell of a cost: n/a when the tokens are unknown, unpriced
// when no model could be priced, and the amount otherwise, with a trailing +
// when it leaves out tokens of unpriced models.
func (p *statsPrinter) spend(c stats.Cost, tokens *int64) string {
	switch {
	case tokens == nil:
		return "n/a"
	case c.USD == nil:
		return "unpriced"
	case c.Partial:
		return p.money(*c.USD) + "+"
	}
	return p.money(*c.USD)
}

// count is a number with its noun, thousands separated: "1,234 sessions".
func count(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return statsfmt.CommaInt(n) + " " + unit + "s"
}

// tokensText is a token count, or unknown.
func tokensText(n *int64) string {
	if n == nil {
		return "unknown"
	}
	return statsfmt.TokenCount(*n)
}

// tableCol is one right-aligned column of a table.
type tableCol struct {
	head  string
	cells []string
	// drop is whether the column may be left out of a table too narrow for it.
	drop bool
}

// tableBar is the bar drawn beside each row of a table: width cells at most,
// with shares (0 to 1) per row; a negative share leaves a row's bar blank.
// codes are the SGR code of each row's bar, "" for none.
type tableBar struct {
	width  int
	shares []float64
	codes  []string
}

// table draws rows under a title: each row's label, an optional bar, and the
// columns, right-aligned under their heads on the title's line. A row that
// would run past the terminal is fitted to it, giving up the least first: the
// bars shrink, then the columns that may be dropped go, last to first, then
// the bars go, and only then are labels cut. No line is wider than the
// terminal.
func (p *statsPrinter) table(title string, labels []string, bar *tableBar, cols []tableCol) []string {
	wantLeft := 0
	for _, l := range labels {
		wantLeft = max(wantLeft, visibleWidth(l))
	}
	wantBar := 0
	if bar != nil && p.bars() {
		wantBar = bar.width
	}
	cols, barW, leftW := p.fitColumns(title, wantLeft, wantBar, cols)
	widths := colWidths(cols)
	barSpace := 0
	if barW > 0 {
		barSpace = barW + 2
	}
	shown := make([]string, len(labels))
	for i, l := range labels {
		shown[i] = p.cut(l, leftW)
	}
	var head strings.Builder
	head.WriteString(p.bold(title) + strings.Repeat(" ", max(leftW+barSpace-visibleWidth(title), 0)))
	// Columns after the last one with a head have nothing to say over them:
	// the head line stops there, with no trailing blanks.
	lastHead := -1
	for i, c := range cols {
		if c.head != "" {
			lastHead = i
		}
	}
	for i, c := range cols[:lastHead+1] {
		head.WriteString("  " + p.dim(padLeft(c.head, widths[i])))
	}
	lines := []string{strings.TrimRight(head.String(), " ")}
	for r, label := range shown {
		var line strings.Builder
		line.WriteString(padRight(label, leftW))
		if barW > 0 {
			line.WriteString("  " + p.bar(bar.shares[r], barW, bar.codes[r]))
		}
		for i, c := range cols {
			line.WriteString("  " + padLeft(c.cells[r], widths[i]))
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	return lines
}

// fitColumns is the columns a table keeps, the width of its bars and of its
// labels, so that a row fits the terminal. Of the ways to fit, it takes the
// first that needs no label cut: with a bar shrunk to no less than
// minBarWidth and fewer of the droppable columns, then without the bar and
// the same, then cutting labels. Failing all of them it keeps only the
// columns that cannot be dropped, with the narrowest labels.
func (p *statsPrinter) fitColumns(title string, wantLeft, wantBar int, cols []tableCol) (kept []tableCol, barW, leftW int) {
	titleW := visibleWidth(title)
	// space is the columns a table takes with a bar and labels that wide (the
	// title may set the label column's width).
	space := func(cols []tableCol, barW, labelW int) int {
		barSpace := 0
		if barW > 0 {
			barSpace = barW + 2
		}
		colsW := 0
		for _, w := range colWidths(cols) {
			colsW += w + 2
		}
		return max(labelW, titleW-barSpace) + barSpace + colsW
	}
	// without is cols minus the last k droppable columns.
	without := func(k int) []tableCol {
		out := slices.Clone(cols)
		for i := len(out) - 1; i >= 0 && k > 0; i-- {
			if out[i].drop {
				out = slices.Delete(out, i, i+1)
				k--
			}
		}
		return out
	}
	droppable := 0
	for _, c := range cols {
		if c.drop {
			droppable++
		}
	}
	if wantBar >= minBarWidth {
		for k := 0; k <= droppable; k++ {
			kept := without(k)
			for b := wantBar; b >= minBarWidth; b-- {
				if space(kept, b, wantLeft) <= p.width {
					return kept, b, wantLeft
				}
			}
		}
	}
	for k := 0; k <= droppable; k++ {
		if kept := without(k); space(kept, 0, wantLeft) <= p.width {
			return kept, 0, wantLeft
		}
	}
	for k := 0; k <= droppable; k++ {
		kept := without(k)
		for l := wantLeft; l >= minLabelWidth; l-- {
			if space(kept, 0, l) <= p.width {
				return kept, 0, l
			}
		}
	}
	return without(droppable), 0, minLabelWidth
}

func colWidths(cols []tableCol) []int {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = visibleWidth(c.head)
		for _, cell := range c.cells {
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	return widths
}

// bar draws share (0 to 1) of width cells: filled up to it, blank after; no
// track is drawn behind it. A negative share is blank: nothing to draw. code
// is the SGR code the filled cells are painted in.
func (p *statsPrinter) bar(share float64, width int, code string) string {
	if share < 0 || math.IsNaN(share) {
		return strings.Repeat(" ", width)
	}
	n := barCells(share, width)
	return p.paint(code, strings.Repeat(p.g.block, n)) + strings.Repeat(" ", width-n)
}

// clampShare is share limited to 0 to 1, and 0 for a share that is not a number.
func clampShare(share float64) float64 {
	if math.IsNaN(share) {
		return 0
	}
	return math.Min(math.Max(share, 0), 1)
}

// barCells is how many of width cells share (0 to 1) fills: at least one
// for any share above zero. A share outside 0 to 1, or not a number, is
// clamped: converting a NaN to an int is up to the CPU, and a negative count
// of cells would panic.
func barCells(share float64, width int) int {
	share = clampShare(share)
	n := int(math.Round(share * float64(width)))
	if share > 0 && n == 0 {
		n = 1
	}
	return min(n, width)
}

// dayLabel is a "2006-01-02" date as "Sep 17", with the year when the window
// is long enough for it to be ambiguous.
func (p *statsPrinter) dayLabel(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return clean(date)
	}
	if p.s.Window.Days > 300 {
		return t.Format("Jan 2 2006")
	}
	return t.Format("Jan 2")
}

// apportion splits total cells among shares (each 0 to 1, summing to about
// 1), by the largest remainder so the cells add up exactly, and gives every
// nonzero share at least one cell.
func apportion(shares []float64, total int) []int {
	cells := make([]int, len(shares))
	type remainder struct {
		index int
		frac  float64
	}
	var rest []remainder
	used := 0
	for i, s := range shares {
		exact := s * float64(total)
		cells[i] = int(exact)
		if s > 0 && cells[i] == 0 {
			cells[i] = 1
		}
		used += cells[i]
		rest = append(rest, remainder{i, exact - math.Floor(exact)})
	}
	for used < total && len(rest) > 0 {
		best := 0
		for i, r := range rest {
			if r.frac > rest[best].frac {
				best = i
			}
		}
		if shares[rest[best].index] > 0 {
			cells[rest[best].index]++
			used++
		}
		rest[best].frac = -1
		allDone := true
		for _, r := range rest {
			if r.frac >= 0 {
				allDone = false
			}
		}
		if allDone {
			break
		}
	}
	// A minimum of one cell can overshoot the total; take it back from the
	// widest segment.
	for used > total {
		widest := 0
		for i, n := range cells {
			if n > cells[widest] {
				widest = i
			}
		}
		cells[widest]--
		used--
	}
	return cells
}

// segment is one part of a stacked bar.
type segment struct {
	share float64
	code  string
}

// stackedBar draws segments side by side in width cells, one blank cell
// between neighbors so the parts can be told apart without color. A segment
// with any share has at least one cell.
func (p *statsPrinter) stackedBar(segments []segment, width int) string {
	var shown []segment
	for _, s := range segments {
		// A share is 0 to 1; one that is not (or not a number) draws nothing
		// more than the whole bar.
		if s.share = clampShare(s.share); s.share > 0 {
			shown = append(shown, s)
		}
	}
	if len(shown) == 0 {
		return ""
	}
	shares := make([]float64, len(shown))
	for i, s := range shown {
		shares[i] = s.share
	}
	cells := apportion(shares, max(width-(len(shown)-1), len(shown)))
	var b strings.Builder
	for i, n := range cells {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(p.paint(shown[i].code, strings.Repeat(p.g.block, n)))
	}
	return b.String()
}

func padRight(text string, width int) string {
	if gap := width - visibleWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

func padLeft(text string, width int) string {
	if gap := width - visibleWidth(text); gap > 0 {
		return strings.Repeat(" ", gap) + text
	}
	return text
}

func (p *statsPrinter) observationAccountingContext(text string) string {
	if p.s.TokenObservations != nil {
		return strings.ReplaceAll(text, "no token data", "no qualified token accounting")
	}
	return text
}
