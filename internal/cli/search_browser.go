package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// contentSearchBrowser owns only bounded local search results and references.
// The fixed FTS query never enters the metadata picker's filter grammar.
type contentSearchBrowser struct {
	env           searchCommandDependencies
	cfg           config.Config
	namespace     string
	index         *search.Store
	opts          contentSearchOptions
	page          search.Page
	hits          []search.Hit
	byKey         map[string]search.Hit
	choices       *scopeChoices
	pages         int
	stale         bool
	rerun         bool
	notice        string
	handoff       *listRow
	generation    uint64
	pendingAction string
}

func runContentSearchBrowser(ctx context.Context, env searchCommandDependencies, cfg config.Config, namespace string, index *search.Store, page search.Page, opts contentSearchOptions, stdout, stderr io.Writer) (listRow, bool, *typedInput, int) {
	s := &contentSearchBrowser{env: env, cfg: cfg, namespace: namespace, index: index, opts: opts, page: page, hits: page.Hits, pages: 1}
	format := listFormatOptions{Search: true, Numbered: true, HideProject: true, Now: env.now(), NarrowHint: "o next indexed results, or narrow the content query and rerun."}
	s.choices = rowChoices(nil, format)
	s.choices.loadOlder = &browserLoadAction{LoadContext: s.load, Available: func() bool { return true }}
	s.installRows()
	input := newTypedInput(opts.stdin)
	p := input.prompter(stdout)
	row, picked, code := runBrowser(ctx, env, p, stdout, stderr, browserSpec{Mode: browseSearch, Search: s, Choices: s.choices, Command: "search", Verb: "Read matching passage", Notice: "Content query: " + archive.DisplayLine(opts.query.Text) + "; / filtering disabled. r rerun · o next results. Scope never widens."})
	return row, picked, input, code
}

func (s *contentSearchBrowser) installRows() {
	s.byKey = make(map[string]search.Hit, len(s.hits))
	ids := make([]string, len(s.hits))
	for i, h := range s.hits {
		ids[i] = h.SessionID
	}
	shorts := uniqueShortIDs(ids)
	rows := make([]listRow, 0, len(s.hits))
	for i, h := range s.hits {
		key := childKey(h.Harness, h.SessionID)
		s.byKey[key] = h
		snippet := archive.DisplayLine(h.Snippet)
		if len(h.Gaps) > 0 {
			snippet += " · source coverage gaps"
		}
		title := archive.DisplayLine(h.Title)
		if strings.TrimSpace(title) == "" {
			title = shorts[i]
		}
		rows = append(rows, listRow{selectionKey: key, Index: i + 1, SessionID: h.SessionID, ShortID: shorts[i], HarnessKey: h.Harness, Harness: archive.DisplayLine(h.Harness), Title: title, ListTitle: listTitle(h.Title), When: h.CapturedAt.Format("2006-01-02"), WhenShort: h.CapturedAt.Format("2006-01-02"), Project: "-", searchSnippet: snippet, searchMatches: h.MatchCount})
	}
	scope := archive.DisplayLine(s.opts.query.Scope.ProjectName)
	if s.opts.all {
		scope = "all indexed projects"
	}
	if scope == "" {
		scope = "original project scope"
	}
	coverage := "Scope: " + scope + ". " + fmt.Sprintf("%d indexed; %d pending; %d failed. Remote changes after observation are unknown.", s.page.Observation.Indexed, s.page.Observation.Pending, s.page.Observation.Failed)
	notice := coverage
	if s.notice != "" {
		notice += " " + s.notice
	}
	if s.pages == 3 && s.page.Next != "" {
		notice += " Result retention cap reached (three pages); narrow or rerun the query."
	}
	if s.stale {
		notice += " Index changed; r explicitly reruns this query."
	}
	total, truncated := len(rows), s.page.Next != "" || s.opts.query.Cursor != ""
	if truncated {
		total = -1
	}
	s.choices.rowsFor = func(sessionScope) scopeView {
		return scopeView{rows: rows, total: total, truncated: truncated, note: notice}
	}
	if loader := s.choices.loadOlder; loader != nil {
		loader.Label = "next indexed page; r reruns query"
		if s.page.Next == "" {
			loader.Label = "no further indexed page; r reruns query"
		}
		if s.pages == 3 {
			loader.Label = "retained-page limit reached; r reruns query"
		}
		if s.stale {
			loader.Label = "index changed; r reruns query"
		}
	}
	s.choices.built = [2]*scopeChoice{}
}

func (s *contentSearchBrowser) load(ctx context.Context) (bool, error) {
	q := s.opts.query
	reset := s.rerun
	s.rerun = false
	if !reset && (s.stale || s.pages >= 3 || s.page.Next == "") {
		s.notice = "No next page in this retained ranking; r explicitly reruns the query."
		s.installRows()
		return true, nil
	}
	if reset {
		q.Cursor = ""
	} else {
		q.Cursor = s.page.Next
	}
	page, err := s.index.Search(ctx, q)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, search.ErrCursorChanged) {
		s.stale = true
		s.installRows()
		return true, nil
	}
	if err != nil {
		s.notice = archive.DisplayLine(err.Error())
		s.installRows()
		return true, nil
	}
	if reset {
		s.opts.query.Cursor = ""
		s.generation++
		s.hits = nil
		s.pages = 0
		s.stale = false
		s.notice = ""
	}
	s.notice = ""
	s.hits = append(s.hits, page.Hits...)
	s.pages++
	s.page = page
	s.installRows()
	return true, nil
}

func (l *sessionPicker) listQuestion(action string) string {
	if l.contentSearch != nil {
		return "Enter number (or unique short SESSION_ID) to " + action + ", o next results, r rerun, or q to quit"
	}
	return "Enter number (or unique short SESSION_ID) to " + action + ", words to filter, or q to quit"
}

type searchContextResult struct {
	context    search.Surrounding
	err        error
	generation uint64
	panicValue any
}

// readSearchSelection services cancellation while the finite selected-source
// read runs. The deferred join precedes every return and store/browser cleanup.
func (b *sessionBrowser) readSearchSelection(ctx context.Context, ref search.Reference, offset int) (search.Surrounding, error) {
	s := b.search
	s.generation++
	generation := s.generation
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	done := make(chan searchContextResult, 1)
	go func() {
		result := searchContextResult{generation: generation}
		defer func() { result.panicValue = recover(); done <- result }()
		if ref.Source.Destination != s.namespace {
			result.err = searchFailure("reference_context_mismatch")
			return
		}
		objects, err := s.env.openStore(s.cfg)
		if err != nil {
			result.err = err
			return
		}
		result.context, result.err = search.ReadContext(ctx, objects, parsersFor(s.env), ref, offset, 4096, 1, 1, reader.Limits{})
	}()
	joined := false
	join := func() searchContextResult {
		r := <-done
		joined = true
		if r.panicValue != nil {
			panic(r.panicValue)
		}
		return r
	}
	defer func() {
		cancel()
		if !joined {
			join()
		}
	}()
	terminal.Println(b.stdout, "Loading selected passage… Esc cancels")
	typedAhead := b.keys != nil && b.keys.buffered() || b.keys == nil && b.prompt.in.Buffered() > 0
	for {
		select {
		case r := <-done:
			joined = true
			if r.panicValue != nil {
				panic(r.panicValue)
			}
			if r.generation != s.generation || ctx.Err() != nil {
				return search.Surrounding{}, context.Canceled
			}
			return r.context, r.err
		case <-ctx.Done():
			return search.Surrounding{}, ctx.Err()
		default:
		}
		if typedAhead {
			r := join()
			if r.generation != s.generation || ctx.Err() != nil {
				return search.Surrounding{}, context.Canceled
			}
			return r.context, r.err
		}
		answer, err := b.searchReadAction()
		if err != nil {
			return search.Surrounding{}, err
		}
		switch answer {
		case "q":
			b.list.operationQuit = true
			return search.Surrounding{}, context.Canceled
		case "n", "p", "m", "b", "h", "r":
			s.pendingAction = answer
			return search.Surrounding{}, context.Canceled
		}
	}
}

func searchContextLines(h search.Hit, ref *search.Reference, out search.Surrounding, notice string) []string {
	text := archive.DisplayLine(h.Harness) + " / " + archive.DisplayLine(h.SessionID) + "\n" + archive.DisplayLine(h.Title) + "\n"
	for _, gap := range h.Gaps[:min(len(h.Gaps), 3)] {
		text += "Coverage gap: " + oneRow(archive.DisplayLine(gap.RevisionID+": "+gap.Reason), 160) + "\n"
	}
	if len(h.Gaps) > 3 {
		text += "Additional source coverage gaps are reported in JSON.\n"
	}
	if ref == nil {
		text += "Title match: no body passage reference. n loads matching body passages if available.\n"
	} else {
		revision := "active source"
		if ref.Source.RevisionID != "" {
			revision = archive.DisplayLine(ref.Source.RevisionID)
		}
		text += "Source revision: " + revision + "\n"
		text += fmt.Sprintf("Reference: message %d · raw bytes %d–%d · source %s\n", ref.Record, ref.Start, ref.End, archive.DisplayLine(ref.Source.Reference.SHA256))
		for _, v := range out.Before {
			text += "[before · " + archive.DisplayLine(v.Role) + "]\n" + searchDisplayText(v.Context.Text) + "\n"
		}
		text += "[selected · " + archive.DisplayLine(out.Role) + "]\n" + searchDisplayText(out.Selected.Text) + "\n"
		for _, v := range out.After {
			text += "[after · " + archive.DisplayLine(v.Role) + "]\n" + searchDisplayText(v.Context.Text) + "\n"
		}
		text += fmt.Sprintf("Passage bytes %d–%d", out.Selected.Start, out.Selected.End)
		if out.Selected.Next != 0 {
			text += fmt.Sprintf("; m more context at raw offset %d", out.Selected.Next)
		}
		text += "\n"
	}
	if notice != "" {
		text += archive.DisplayLine(notice) + "\n"
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func searchDisplayText(text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = archive.DisplayLine(lines[i])
	}
	return strings.Join(lines, "\n")
}

const searchDetailsQuestion = "↑↓/PgUp/PgDn (or pgup/pgdn) scroll · n next match · p previous match · m more context · h Hand off current session · r rerun · b back · q quit"

func (b *sessionBrowser) drawSearchDetails(lines []string, top int) detailsScreen {
	if width, _, ok := b.env.terminalSize(b.stdout); ok && width > 0 {
		var wrapped []string
		for _, line := range lines {
			wrapped = append(wrapped, splitColumns(line, width)...)
		}
		lines = wrapped
	}
	screen, _ := b.measureDetails(lines, top, false, "", browseNotice{})
	for _, line := range lines[screen.top : screen.top+screen.shown] {
		terminal.Println(b.stdout, line)
	}
	if screen.cut {
		terminal.Println(b.stdout, scrollIndicator(screen.top, len(lines)-screen.top-screen.shown))
	}
	terminal.Println(b.stdout, searchDetailsQuestion)
	return screen
}

// searchReadAction consumes only one currently available input action. The
// caller joins its read before applying an action that replaces the selection.
func (b *sessionBrowser) searchReadAction() (string, error) {
	if b.keys == nil {
		line, complete, err := b.list.pollBrowserLine(b.prompt)
		if err != nil || !complete {
			return "", err
		}
		if line == "" {
			return "b", nil
		}
		return strings.ToLower(line), nil
	}
	k, err := b.keys.nextWait(50 * time.Millisecond)
	if err != nil {
		return "", err
	}
	switch k.kind {
	case keyEscape:
		return "", context.Canceled
	case keyEndOfInput:
		return "q", nil
	case keyRune:
		return strings.ToLower(string(k.r)), nil
	case keyEnter, keyBackspace:
		return "b", nil
	case keyUp, keyDown, keyLeft, keyRight, keyPageUp, keyPageDown, keyHome, keyEnd, keyResize:
	}
	return "", nil
}

type searchMatchNavigation struct {
	page     search.MatchPage
	position int
	loaded   bool
}

func (s *contentSearchBrowser) matchPage(ctx context.Context, hit search.Hit, cursor string) (search.MatchPage, error) {
	page, err := s.index.Matches(ctx, s.opts.query, hit.Harness, hit.SessionID, s.page.Generation, cursor)
	if errors.Is(err, search.ErrCursorChanged) {
		s.stale = true
		s.installRows()
	}
	return page, err
}

func (s *contentSearchBrowser) moveMatch(ctx context.Context, hit search.Hit, ref *search.Reference, direction int, nav *searchMatchNavigation) (*search.Reference, error) {
	if s.stale {
		return nil, errors.New("index changed; r reruns this fixed query")
	}
	if !nav.loaded {
		page, err := s.matchPage(ctx, hit, "")
		if err != nil {
			return nil, err
		}
		nav.page, nav.loaded = page, true
		if ref != nil {
			for i := range page.References {
				if encodeSearchReference(page.References[i]) == encodeSearchReference(*ref) {
					nav.position = i
					break
				}
			}
		}
	}
	next := nav.position + direction
	if ref == nil && direction == 1 {
		next = 0
	}
	cursor := ""
	if next >= len(nav.page.References) {
		cursor = nav.page.Next
	}
	if next < 0 {
		cursor = nav.page.Previous
	}
	if next < 0 || next >= len(nav.page.References) {
		if cursor == "" {
			return nil, errors.New("no further matching body passage.")
		}
		page, err := s.matchPage(ctx, hit, cursor)
		if err != nil {
			return nil, err
		}
		nav.page = page
		next = 0
		if direction < 0 {
			next = len(page.References) - 1
		}
	}
	if next < 0 || next >= len(nav.page.References) {
		return nil, errors.New("no matching body passage.")
	}
	nav.position = next
	selected := nav.page.References[next]
	return &selected, nil
}

// printContentSearchRows is the finite search presentation of shared browser
// groups. It bounds each snippet to two physical rows, without widening other
// columns or changing the layout of existing metadata pickers.
func printContentSearchRows(out io.Writer, groups []sessionTableGroup, width int) error {
	if width <= 0 {
		width = 80
	}
	var frame strings.Builder
	frame.WriteString(oneRow("Ranked content matches", width) + "\n")
	for _, group := range groups {
		for _, row := range group.rows {
			frame.WriteString(oneRow(fmt.Sprintf("%d. %s", row.Index, row.Title), width) + "\n")
			frame.WriteString(oneRow(row.Harness+" · "+row.ShortID+" · "+row.When, width) + "\n")
			snippets := splitColumns(row.searchSnippet, max(width-2, 1))
			for i, line := range snippets[:min(len(snippets), 2)] {
				if i == 1 && len(snippets) > 2 {
					line = oneRow(line+"…", max(width-2, 1))
				}
				frame.WriteString("  " + line + "\n")
			}
			if row.searchMatches > 1 {
				frame.WriteString(oneRow(fmt.Sprintf("%d other indexed title/body matches", row.searchMatches-1), width) + "\n")
			}
		}
	}
	return writeOutput(out, []byte(frame.String()))
}

func (l *sessionPicker) minimumRows() int {
	if l.contentSearch != nil {
		return 1
	}
	return minPickerPageRows
}

func (l *sessionPicker) displayFooter(footer string, width int) string {
	if l.contentSearch == nil || width <= 0 {
		return footer
	}
	lines := strings.Split(footer, "\n")
	for i := range lines {
		lines[i] = oneRow(lines[i], width)
	}
	return strings.Join(lines, "\n")
}
