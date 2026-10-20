package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// This file is the passage a search result opens: the matching message read
// from its exact source revision, between the messages before and after it,
// laid out as show lays out a session.

// searchMatchFooter is the footer under a matching passage: its keys.
const searchMatchFooter = "n/p next/previous match   ↑↓ scroll   t transcript   h hand off   i details   b back   q quit"

// searchNeighborLines is how many lines of the message before and after the
// match the passage shows until m shows them whole.
const searchNeighborLines = 3

// passageView is how the passage is shown: its neighbors whole (m), and the
// exact reference it was read from (i).
type passageView struct {
	expanded bool
	details  bool
}

// passageMessage is one message of the passage: who wrote it, its text, and
// whether the bounded read cut it short at its start or end.
type passageMessage struct {
	role     string
	text     string
	selected bool
	cutStart bool
	cutEnd   bool
}

// passageLines lays out the passage of hit's match st for a terminal width
// columns wide: the session's title, bold; the agent in its color, dim the
// project and capture time, and right-aligned which match it is; then the
// message before (dim), the matching message with its matched terms
// highlighted, and the message after (dim), each led by who wrote it with
// its lines hung under its first. A neighbor longer than searchNeighborLines
// is cut, saying how many more lines m shows. With view.details, the exact
// reference follows.
func (s *contentSearchBrowser) passageLines(hit search.Hit, st *searchMatchState, out search.Surrounding, width int, view passageView) (lines []string, cut bool) {
	if width <= 0 {
		width = unknownRuleWidth * 2
	}
	title := archive.DisplayLine(hit.Title)
	if strings.TrimSpace(title) == "" {
		title = shortSessionID(hit.SessionID)
	}
	lines = []string{s.style.bold(stripLine(title, width))}
	left := s.style.agent(hit.Harness, archive.DisplayLine(appName(hit.Harness)))
	meta := []string{}
	if project := archive.DisplayLine(hit.Project); project != "" {
		meta = append(meta, project)
	}
	meta = append(meta, s.longWhen(hit.CapturedAt))
	left += s.style.dim(" · " + strings.Join(meta, " · "))
	right := s.style.dim(fmt.Sprintf("match %d of %d", st.ordinal, max(hit.MatchCount, st.ordinal)))
	if gap := width - visibleWidth(left) - visibleWidth(right); gap >= 2 {
		lines = append(lines, left+strings.Repeat(" ", gap)+right)
	} else {
		lines = append(lines, oneRow(left, width), right)
	}

	var messages []passageMessage
	switch {
	case st.ref == nil:
		messages = append(messages, passageMessage{role: "", text: st.text, selected: true})
	case out.Selected.Text == "" && out.Selected.End == 0:
		// The exact revision could not be read; nothing else stands in.
	default:
		for _, v := range out.Before {
			messages = append(messages, passageMessage{role: v.Role, text: v.Context.Text, cutStart: v.Context.Start > 0, cutEnd: v.Context.Next != 0})
		}
		messages = append(messages, passageMessage{role: out.Role, text: out.Selected.Text, selected: true, cutStart: out.Selected.Start > 0, cutEnd: out.Selected.Next != 0})
		for _, v := range out.After {
			messages = append(messages, passageMessage{role: v.Role, text: v.Context.Text, cutStart: v.Context.Start > 0, cutEnd: v.Context.Next != 0})
		}
	}
	labelWidth := 0
	for _, m := range messages {
		labelWidth = max(labelWidth, visibleWidth(searchRole(m.role, hit.Harness)+" ›"))
	}
	for _, m := range messages {
		shown, cutHere := s.messageLines(hit, m, labelWidth, width, view.expanded)
		lines = append(append(lines, ""), shown...)
		cut = cut || cutHere
	}
	if st.ref == nil && hit.MatchCount > 1 {
		lines = append(lines, "", s.style.dim(stripLine("A title match. n shows the session's first matching message.", width)))
	}
	if view.details {
		lines = append(lines, "")
		lines = append(lines, s.passageDetails(hit, st.ref, out, width)...)
	}
	return lines, cut
}

// messageLines is one message of the passage: two spaces, who wrote it
// padded to labelWidth, then its text wrapped to what is left of width, each
// further line set in under the first. The match is plain with its terms
// highlighted, a neighbor dim and, unless expanded, cut to
// searchNeighborLines; cut reports that it was.
func (s *contentSearchBrowser) messageLines(hit search.Hit, m passageMessage, labelWidth, width int, expanded bool) (lines []string, cut bool) {
	label := searchRole(m.role, hit.Harness) + " ›"
	indent := 2 + labelWidth + 1
	textWidth := max(width-indent, 20)
	var text []string
	if m.selected {
		text = highlightedLines(s.opts.query.Text, m.text, textWidth, s.style)
	} else {
		text = highlightedLines("", m.text, textWidth, textStyle{})
		for i := range text {
			text[i] = s.style.dim(text[i])
		}
	}
	if m.cutStart {
		text = append([]string{s.style.dim("…")}, text...)
	}
	if !m.selected && !expanded && len(text) > searchNeighborLines {
		cut = true
		more := len(text) - searchNeighborLines
		text = append(text[:searchNeighborLines], s.style.dim(fmt.Sprintf("… %s (m)", strings.Replace(plural(more, "line"), " ", " more ", 1))))
	} else if m.cutEnd {
		text = append(text, s.style.dim("… message continues"+map[bool]string{true: " (m)", false: ""}[m.selected]))
	}
	shownLabel := padRight(label, labelWidth)
	if m.selected {
		shownLabel = s.style.bold(shownLabel)
	} else {
		shownLabel = s.style.dim(shownLabel)
	}
	for i, line := range text {
		lead := strings.Repeat(" ", indent)
		if i == 0 {
			lead = "  " + shownLabel + " "
		}
		lines = append(lines, strings.TrimRight(lead+line, " "))
	}
	if len(lines) == 0 {
		lines = append(lines, "  "+shownLabel)
	}
	return lines, cut
}

// passageDetails is what i shows under the passage, dim: the session's full
// identity, the source revision and exact reference the passage was read
// from, its bytes, and any source coverage gaps.
func (s *contentSearchBrowser) passageDetails(hit search.Hit, ref *search.Reference, out search.Surrounding, width int) []string {
	facts := []string{"Session: " + archive.DisplayLine(hit.Harness) + " / " + archive.DisplayLine(hit.SessionID)}
	if ref == nil {
		facts = append(facts, "Title match: no message reference.")
	} else {
		revision := "active source"
		if ref.Source.RevisionID != "" {
			revision = archive.DisplayLine(ref.Source.RevisionID)
		}
		facts = append(facts, "Source revision: "+revision,
			fmt.Sprintf("Reference: message %d · raw bytes %d–%d · source %s", ref.Record, ref.Start, ref.End, archive.DisplayLine(ref.Source.Reference.SHA256)))
		passage := fmt.Sprintf("Passage bytes %d–%d", out.Selected.Start, out.Selected.End)
		if out.Selected.Next != 0 {
			passage += fmt.Sprintf("; m reads on from raw offset %d", out.Selected.Next)
		}
		facts = append(facts, passage)
	}
	for _, gap := range hit.Gaps[:min(len(hit.Gaps), 3)] {
		facts = append(facts, "Coverage gap: "+archive.DisplayLine(gap.RevisionID+": "+gap.Reason))
	}
	if len(hit.Gaps) > 3 {
		facts = append(facts, "Additional source coverage gaps are reported in JSON.")
	}
	var lines []string
	for _, fact := range facts {
		for _, row := range splitColumns(fact, width) {
			lines = append(lines, s.style.dim(row))
		}
	}
	return lines
}

// openPassage is the passage a result opened: the match shown, what was read
// of it, and how it is shown.
type openPassage struct {
	b      *sessionBrowser
	row    listRow
	hit    search.Hit
	state  *searchMatchState
	out    search.Surrounding
	view   passageView
	cut    bool
	top    int
	notice string
}

// read reads the shown match's exact source from offset; a title match has
// none to read.
func (p *openPassage) read(ctx context.Context, offset int) {
	p.out = search.Surrounding{}
	if p.state.ref == nil {
		return
	}
	var err error
	p.out, err = p.b.readSearchSelection(ctx, *p.state.ref, offset)
	// A key that replaced the read says what happens instead.
	if err != nil && (p.b.search.pendingAction == searchActionNone || !errors.Is(err, context.Canceled)) {
		p.notice = err.Error()
	}
}

// searchDetails is the passage a result opens, from the match the strip
// showed: n and p move to the session's other matches (and the strip's with
// them), m shows cut neighbors whole or reads on in a long message, i shows
// the exact reference, and t the session's transcript in show's pager. It
// retains one match page and one bounded context; scrolling never performs
// IO, and only the exact reference's verified source is shown.
func (b *sessionBrowser) searchDetails(ctx context.Context, row listRow) (browseAction, error) {
	s := b.search
	hit, ok := s.byKey[row.selectionKey]
	if !ok {
		return browseBack, nil
	}
	p := &openPassage{b: b, row: row, hit: hit, state: s.matchState(hit)}
	if p.state.ref != nil {
		p.read(ctx, p.state.ref.Start)
	}
	for {
		if b.list.operationQuit {
			return browseQuit, nil
		}
		b.screen.clear()
		width, _, _ := b.env.terminalSize(b.stdout)
		var lines []string
		lines, p.cut = s.passageLines(hit, p.state, p.out, width, p.view)
		screen := b.drawSearchDetails(lines, p.top, p.notice)
		p.notice = ""
		answer := s.pendingAction
		s.pendingAction = searchActionNone
		if answer == searchActionNone {
			answer, s.afterRead = s.afterRead, searchActionNone
		}
		// Pending actions apply only after readSearchSelection joined its read.
		if answer == searchActionNone {
			var err error
			answer, err = b.searchDetailAction(&screen)
			if err != nil {
				return endOfInput(err)
			}
			p.top = screen.top
			if answer == searchActionNone {
				continue
			}
		}
		if action, done, err := p.apply(ctx, answer, &screen); done || err != nil {
			return action, err
		}
	}
}

// apply does what answer asks of the passage drawn as screen. done is set
// when the browser leaves the passage, with action saying where to.
func (p *openPassage) apply(ctx context.Context, answer contentSearchAction, screen *detailsScreen) (action browseAction, done bool, err error) {
	b, s := p.b, p.b.search
	switch answer {
	case searchActionPageDown, searchActionPageUp, searchActionDown, searchActionUp, searchActionHome, searchActionEnd:
		move := map[contentSearchAction]scrollMove{searchActionPageDown: scrollPageDown, searchActionPageUp: scrollPageUp, searchActionDown: scrollLineDown, searchActionUp: scrollLineUp, searchActionHome: scrollTop, searchActionEnd: scrollBottom}[answer]
		screen.scroll(move)
		p.top = screen.top
	case searchActionBack, searchActionNone:
		return browseBack, true, nil
	case searchActionQuit:
		return browseQuit, true, nil
	case searchActionHandoff:
		s.handoff = &p.row
		return browseQuit, true, nil
	case searchActionRerun:
		s.rerun = true
		_, err := s.load(ctx)
		b.list.keepSearchSelection(rowIdentity(p.row))
		return browseBack, true, err
	case searchActionDetails:
		p.view.details = !p.view.details
	case searchActionTranscript:
		action, failure, err := b.searchTranscript(ctx, p.row)
		if err != nil || action == browseQuit {
			return action, true, err
		}
		p.notice = failure
	case searchActionMore:
		switch {
		case p.cut:
			p.view.expanded = true
		case p.state.ref != nil && p.out.Selected.Next != 0:
			p.read(ctx, p.out.Selected.Next)
			p.top = 0
		default:
			p.notice = "Nothing more of these messages to show."
		}
	case searchActionNext, searchActionPrevious:
		direction := 1
		if answer == searchActionPrevious {
			direction = -1
		}
		if err := s.stepMatch(ctx, p.hit, p.state, direction); err != nil {
			p.notice = err.Error()
			break
		}
		p.out = search.Surrounding{}
		if p.state.ref != nil {
			p.read(ctx, p.state.ref.Start)
		}
		p.view.expanded, p.top = false, 0
	default:
		p.notice = "Press n or p for another match, t for the transcript, b for the results, or q to quit."
	}
	return browseStay, false, nil
}

// searchTranscript shows the session's transcript in the pager show
// --transcript uses: the session as archived now, verified, which may have
// grown since the match was indexed. The archive is opened only when t is
// pressed. A transcript that cannot be read is reported in failure.
func (b *sessionBrowser) searchTranscript(ctx context.Context, row listRow) (browseAction, string, error) {
	if b.store == nil {
		store, err := b.search.env.openStore(b.search.cfg)
		if err != nil {
			return browseStay, "agent-archive: search: " + archive.DisplayLine(err.Error()), nil
		}
		b.store = store
	}
	return b.transcript(ctx, row)
}

// drawSearchDetails draws the passage from line top, as much as fits above
// the scroll indicator, the notice (or a blank line), and the footer.
func (b *sessionBrowser) drawSearchDetails(lines []string, top int, notice string) detailsScreen {
	if width, _, ok := b.env.terminalSize(b.stdout); ok && width > 0 {
		var wrapped []string
		for _, line := range lines {
			wrapped = append(wrapped, splitColumns(line, width)...)
		}
		lines = wrapped
	}
	screen, shown := b.measureDetails(lines, top, false, "", browseNotice{text: archive.DisplayLine(notice)})
	var frame strings.Builder
	for _, line := range lines[screen.top : screen.top+screen.shown] {
		frame.WriteString(line + "\n")
	}
	if screen.cut {
		frame.WriteString(b.format.Style.dim(scrollIndicator(screen.top, len(lines)-screen.top-screen.shown)) + "\n")
	}
	frame.WriteString(shown.text + "\n")
	frame.WriteString(oneRow(searchMatchFooter, screen.width) + "\n")
	terminal.Print(b.stdout, frame.String())
	return screen
}

// highlightedLines is text wrapped to width with every term of query
// highlighted, line breaks kept.
func highlightedLines(query, text string, width int, style textStyle) []string {
	cells := passageCells(text, search.MatchSpans(query, text), true)
	var out []string
	for _, l := range wrapCells(cells, width) {
		out = append(out, renderCells(cells[l.start:l.end], style))
	}
	return out
}
