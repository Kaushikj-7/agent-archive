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
	pendingAction contentSearchAction
	// afterRead is an action typed while the passage loaded that applies to
	// it once read: i or t.
	afterRead contentSearchAction
	// style and now are the screen's color and clock, for the rows, the
	// heading and the detail strip.
	style textStyle
	now   time.Time
	// matches is which match of each session the detail strip shows, by the
	// session's selection key; n and p move it. A rerun starts them again.
	matches map[string]*searchMatchState
	// request is what a key asked of the browser that the list cannot do
	// itself: a new query, asked for in place.
	request searchRequest
	// toggleScope, set with rerun, switches between all projects and the
	// current project before the query runs again.
	toggleScope bool
	// coverage, when set, words the coverage line under the heading for the
	// page shown, in place of the local index's ("Indexed 105 of 211 ·
	// catching up"): a search that reads no index says what it searched
	// ("Searched 104 of 2,350 sessions"). It is made safe for one line.
	coverage func(search.Page) string
}

// searchRequest is something the results list hands to the browser around
// it.
type searchRequest string

const (
	searchRequestNone  searchRequest = ""
	searchRequestQuery searchRequest = "query"
)

// searchStripPassageLines is how many lines of the matching passage the
// detail strip under the results shows.
const searchStripPassageLines = 3

// searchStripLines is how many lines the search detail strip takes: a rule,
// who wrote the match, the passage, the session's details, and a rule. A
// shorter passage leaves blank lines after it, so the list does not move as
// the highlight does.
const searchStripLines = 4 + searchStripPassageLines

func runContentSearchBrowser(ctx context.Context, env searchCommandDependencies, cfg config.Config, namespace string, index *search.Store, page search.Page, opts contentSearchOptions, stdout, stderr io.Writer) (listRow, bool, *typedInput, int) {
	style := styleFor(stdout)
	format := listFormatOptions{Search: true, Numbered: true, Now: env.now(), Style: style, NarrowHint: "o next indexed results, or narrow the content query and rerun."}
	s := &contentSearchBrowser{env: env, cfg: cfg, namespace: namespace, index: index, opts: opts, page: page, hits: page.Hits, pages: 1, choices: rowChoices(nil, format), style: style, now: format.Now}
	s.choices.loadOlder = &browserLoadAction{LoadContext: s.load, Available: func() bool { return true }}
	s.installRows()
	input := newTypedInput(opts.stdin)
	p := input.prompter(stdout)
	row, picked, code := runBrowser(ctx, env, p, stdout, stderr, browserSpec{Mode: browseSearch, Search: s, Choices: s.choices, Command: "search", Verb: "Read matching passage"})
	return row, picked, input, code
}

func (s *contentSearchBrowser) installRows() {
	s.byKey = make(map[string]search.Hit, len(s.hits))
	if s.matches == nil {
		s.matches = map[string]*searchMatchState{}
	}
	ids := make([]string, len(s.hits))
	for i, h := range s.hits {
		ids[i] = h.SessionID
	}
	shorts := uniqueShortIDs(ids)
	rows := make([]listRow, 0, len(s.hits))
	for i, h := range s.hits {
		key := childKey(h.Harness, h.SessionID)
		s.byKey[key] = h
		snippet := s.style.dim(searchRole(h.Role, h.Harness)+" ›") + " " + highlightedLine(s.opts.query.Text, h.Snippet, s.style)
		title := archive.DisplayLine(h.Title)
		if strings.TrimSpace(title) == "" {
			title = shorts[i]
		}
		project := archive.DisplayLine(h.Project)
		if project == "" {
			project = "-"
		}
		// Indexed hits carry capture time, not current session activity. Do not
		// invent Active or let ordinary activity-day grouping change relevance.
		rows = append(rows, listRow{selectionKey: key, Index: i + 1, SessionID: h.SessionID, ShortID: shorts[i], HarnessKey: h.Harness, Harness: archive.DisplayLine(h.Harness), Title: title, ListTitle: listTitle(archive.DisplayLine(h.Title)), When: s.longWhen(h.CapturedAt), WhenShort: shortWhen(s.now, h.CapturedAt), Project: project, searchSnippet: snippet})
	}
	total, truncated := len(rows), s.page.Next != "" || s.opts.query.Cursor != ""
	if truncated {
		total = -1
	}
	s.choices.rowsFor = func(sessionScope) scopeView {
		return scopeView{rows: rows, total: total, truncated: truncated}
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

// longWhen is when a match was captured, for the detail strip: "Oct 16,
// 09:12", with the year when it is not this year's.
func (s *contentSearchBrowser) longWhen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	local := t.In(s.now.Location())
	if local.Year() == s.now.Year() {
		return local.Format("Jan 2, 15:04")
	}
	return local.Format("Jan 2 2006, 15:04")
}

// canToggleScope reports whether Tab has another scope to search: the
// search knows a project (the working directory's, or --project's) to narrow
// to or widen from.
func (s *contentSearchBrowser) canToggleScope() bool {
	return s.opts.query.Scope.ProjectName != ""
}

// scopeLabel names the scope the query searches.
func (s *contentSearchBrowser) scopeLabel() string {
	if !s.opts.query.Scope.Restricted {
		return allProjectsLabel
	}
	return archive.DisplayLine(s.opts.query.Scope.ProjectName)
}

// heading is the lines above the results: what is searched, for what, and
// how many sessions match, with the key that switches scope (Tab reading
// keys, a reading lines); then the index's coverage in one dim line; then a
// blank line.
func (s *contentSearchBrowser) heading(keys bool) string {
	count := plural(len(s.hits), "session")
	if s.page.Next != "" || s.opts.query.Cursor != "" {
		count = fmt.Sprintf("%d+ sessions", len(s.hits))
	}
	line := s.style.bold(s.scopeLabel()) + " · \"" + archive.DisplayLine(s.opts.query.Text) + "\" · " + count + ", best match first"
	if s.canToggleScope() {
		key := "a"
		if keys {
			key = "Tab"
		}
		other := strings.ToLower(allProjectsLabel)
		if !s.opts.query.Scope.Restricted {
			other = archive.DisplayLine(s.opts.query.Scope.ProjectName) + " only"
		}
		line += " · " + s.style.dim(key+" "+other)
	}
	return line + "\n" + s.style.dim(s.coverageLine()) + "\n"
}

// coverageLine is the index's coverage in one line ("Indexed 105 of 211 ·
// catching up", "Index complete"), then what else the person needs to know
// about these results. The whole coverage is in --json.
func (s *contentSearchBrowser) coverageLine() string {
	o := s.page.Observation
	var parts []string
	switch {
	case s.coverage != nil:
		parts = append(parts, archive.DisplayLine(s.coverage(s.page)))
	case o.Complete:
		parts = append(parts, "Index complete")
	case o.Pending > 0:
		parts = append(parts, fmt.Sprintf("Indexed %d of %d", o.Indexed, o.Known), "catching up")
	default:
		parts = append(parts, fmt.Sprintf("Indexed %d of %d", o.Indexed, o.Known), "coverage incomplete")
	}
	if o.Failed > 0 && s.coverage == nil {
		parts = append(parts, fmt.Sprintf("%d failed", o.Failed))
	}
	if o.Unsupported > 0 && s.coverage == nil {
		parts = append(parts, fmt.Sprintf("%d unsupported", o.Unsupported))
	}
	if s.pages == 3 && s.page.Next != "" {
		parts = append(parts, "result limit reached; narrow or rerun the query")
	}
	if s.stale {
		parts = append(parts, "index changed; r reruns this query")
	}
	if s.notice != "" {
		parts = append(parts, s.notice)
	}
	return strings.Join(parts, " · ")
}

// hasNextPage reports whether load would add the next indexed page rather
// than only say there is none.
func (s *contentSearchBrowser) hasNextPage() bool {
	return !s.stale && s.pages < 3 && s.page.Next != ""
}

func (s *contentSearchBrowser) load(ctx context.Context) (bool, error) {
	q := s.opts.query
	reset := s.rerun
	s.rerun = false
	toggle := s.toggleScope && reset
	s.toggleScope = false
	if !reset && !s.hasNextPage() {
		s.notice = "No next page in this retained ranking; r explicitly reruns the query."
		s.installRows()
		return true, nil
	}
	if reset {
		q.Cursor = ""
	} else {
		q.Cursor = s.page.Next
	}
	if toggle {
		q.Scope.Restricted = !q.Scope.Restricted
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
		s.opts.query = q
		s.opts.query.Cursor = ""
		s.opts.all = !q.Scope.Restricted
		s.generation++
		s.hits = nil
		s.pages = 0
		s.stale = false
		s.matches = nil
	}
	s.notice = ""
	s.hits = append(s.hits, page.Hits...)
	s.pages++
	s.page = page
	s.installRows()
	return true, nil
}

// searchMatchState is the match of one session that the detail strip and the
// passage show: which of its matches it is, counted from its best (1), the
// reference of a body match (nil for the title), and its indexed role and
// text.
type searchMatchState struct {
	ordinal int
	ref     *search.Reference
	role    string
	text    string
	nav     searchMatchNavigation
}

// matchState is the match the strip shows for hit, its best one until n or p
// moves it.
func (s *contentSearchBrowser) matchState(hit search.Hit) *searchMatchState {
	key := childKey(hit.Harness, hit.SessionID)
	if s.matches == nil {
		s.matches = map[string]*searchMatchState{}
	}
	if st, ok := s.matches[key]; ok {
		return st
	}
	st := &searchMatchState{ordinal: 1, ref: hit.Reference, role: hit.Role, text: hit.Passage, nav: searchMatchNavigation{position: -1}}
	s.matches[key] = st
	return st
}

// stepMatch moves st to the session's next (direction 1) or previous (-1)
// match, reading only the local index. A title match comes before the body's.
func (s *contentSearchBrowser) stepMatch(ctx context.Context, hit search.Hit, st *searchMatchState, direction int) error {
	if direction < 0 && st.ref != nil && hit.Reference == nil && st.ordinal == 2 {
		st.ordinal, st.ref, st.role, st.text = 1, nil, hit.Role, hit.Passage
		st.nav.position = -1
		return nil
	}
	fromTitle := st.ref == nil
	ref, err := s.moveMatch(ctx, hit, st.ref, direction, &st.nav)
	if err != nil {
		return err
	}
	st.ref = ref
	if p := st.nav.position; p >= 0 && p < len(st.nav.page.Passages) {
		st.role, st.text = st.nav.page.Passages[p].Role, st.nav.page.Passages[p].Text
	}
	if fromTitle {
		st.ordinal = 2
	} else {
		st.ordinal = max(st.ordinal+direction, 1)
	}
	return nil
}

// stepStrip moves the detail strip of row to its session's next or previous
// match, and returns what to say when there is none.
func (s *contentSearchBrowser) stepStrip(ctx context.Context, row listRow, direction int) string {
	hit, ok := s.byKey[row.selectionKey]
	if !ok {
		return ""
	}
	if err := s.stepMatch(ctx, hit, s.matchState(hit), direction); err != nil {
		return archive.DisplayLine(err.Error())
	}
	return ""
}

// strip is the detail strip under the results for the highlighted row,
// exactly searchStripLines lines: a rule; dim, who wrote the match and which
// of the session's matches it is; the passage's text, wrapped to width, with
// every matched term highlighted; then the agent's colored dot and dim the
// agent, project, capture time and short ID; and a rule.
func (s *contentSearchBrowser) strip(r listRow, width int) []string {
	ruleWidth := width
	if ruleWidth <= 0 {
		ruleWidth = unknownRuleWidth
	}
	rule := s.style.dim(strings.Repeat("─", ruleWidth))
	hit, ok := s.byKey[r.selectionKey]
	if !ok {
		return append([]string{rule, stripLine(r.Title, width)}, make([]string, searchStripLines-2)...)
	}
	st := s.matchState(hit)
	total := max(hit.MatchCount, st.ordinal)
	lines := []string{rule, s.style.dim(stripLine(fmt.Sprintf("%s › match %d of %d in this session", searchRole(st.role, hit.Harness), st.ordinal, total), width))}
	lines = append(lines, passageWindow(s.opts.query.Text, st.text, width, searchStripPassageLines, s.style)...)
	details := []string{archive.DisplayLine(appName(hit.Harness))}
	if r.Project != "" && r.Project != "-" {
		details = append(details, r.Project)
	}
	details = append(details, r.When, "id "+archive.DisplayLine(r.ShortID))
	if len(hit.Gaps) > 0 {
		details = append(details, "source coverage gaps")
	}
	dot, _ := agentLabel(hit.Harness)
	meta := s.style.agent(hit.Harness, dot) + " " + s.style.dim(stripLine(strings.Join(details, " · "), max(width-2, 0)))
	lines = append(lines, meta, rule)
	for len(lines) < searchStripLines {
		lines = append(lines, "")
	}
	return lines
}

// newSearchQuery asks for another query on the browser's screen, with line
// input back for the question, then runs it in the scope shown. quit is set
// when input ended while reading lines.
func (b *sessionBrowser) newSearchQuery(ctx context.Context) (quit bool, err error) {
	s := b.search
	b.screen.clear()
	if b.keys != nil {
		end := b.keys.page()
		defer func() {
			end()
			if e := b.keys.resume(); e != nil && err == nil {
				err = e
			}
		}()
	}
	opts := s.opts
	opts.project = ""
	if opts.query.Scope.Restricted {
		opts.project = s.scopeLabel()
	}
	text, err := promptSearchQuery(b.prompt, opts)
	if errors.Is(err, io.EOF) {
		// Ctrl-D at the question goes back to the results it replaced.
		return b.keys == nil, nil
	}
	if err != nil {
		return false, err
	}
	s.opts.query.Text = text
	s.rerun = true
	if _, err = s.load(ctx); err != nil {
		return false, err
	}
	b.list.keepSearchSelection("")
	b.list.olderSpent = ""
	return false, nil
}

func (l *sessionPicker) listQuestion(action string) string {
	if s := l.contentSearch; s != nil {
		scope := ""
		if s.canToggleScope() {
			scope = ", a scope"
		}
		return "Enter number (or unique short SESSION_ID) to " + action + ", / new search" + scope + ", o next results, r rerun, or q to quit"
	}
	return "Enter number (or unique short SESSION_ID) to " + action + ", words to filter, or q to quit"
}

type contentSearchAction string

const (
	searchActionNone     contentSearchAction = ""
	searchActionQuit     contentSearchAction = "q"
	searchActionNext     contentSearchAction = "n"
	searchActionPrevious contentSearchAction = "p"
	searchActionMore     contentSearchAction = "m"
	searchActionBack     contentSearchAction = "b"
	searchActionHandoff  contentSearchAction = "h"
	searchActionRerun    contentSearchAction = "r"
	// searchActionDetails shows or hides the passage's exact reference.
	searchActionDetails contentSearchAction = "i"
	// searchActionTranscript opens the session's transcript.
	searchActionTranscript contentSearchAction = "t"
	searchActionPageDown   contentSearchAction = "pgdn"
	searchActionPageUp     contentSearchAction = "pgup"
	searchActionDown       contentSearchAction = "down"
	searchActionUp         contentSearchAction = "up"
	searchActionHome       contentSearchAction = "home"
	searchActionEnd        contentSearchAction = "end"
)

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
		answer, err := b.searchReadAction()
		if err != nil {
			return search.Surrounding{}, err
		}
		switch answer {
		case searchActionQuit:
			b.list.operationQuit = true
			return search.Surrounding{}, context.Canceled
		case searchActionNext, searchActionPrevious, searchActionMore, searchActionBack, searchActionHandoff, searchActionRerun:
			s.pendingAction = answer
			return search.Surrounding{}, context.Canceled
		case searchActionDetails, searchActionTranscript:
			// Applied once the passage is read, which they do not replace.
			s.afterRead = answer
		case searchActionNone, searchActionPageDown, searchActionPageUp, searchActionDown, searchActionUp, searchActionHome, searchActionEnd:
		}
	}
}

// searchReadAction consumes only one currently available input action. The
// caller joins its read before applying an action that replaces the selection.
func (b *sessionBrowser) searchReadAction() (contentSearchAction, error) {
	if b.keys == nil {
		line, complete, err := b.list.pollBrowserLine(b.prompt)
		if err != nil || !complete {
			return searchActionNone, err
		}
		if line == "" {
			return searchActionBack, nil
		}
		return contentSearchAction(strings.ToLower(line)), nil
	}
	k, err := b.keys.nextWait(50 * time.Millisecond)
	if err != nil {
		return searchActionNone, err
	}
	switch k.kind {
	case keyEscape:
		return searchActionNone, context.Canceled
	case keyEndOfInput:
		return searchActionQuit, nil
	case keyRune:
		return contentSearchAction(strings.ToLower(string(k.r))), nil
	case keyEnter, keyBackspace:
		return searchActionBack, nil
	case keyUp, keyDown, keyLeft, keyRight, keyPageUp, keyPageDown, keyHome, keyEnd, keyResize, keyTab:
	}
	return searchActionNone, nil
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
			return nil, errors.New("no further matching body passage")
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
		return nil, errors.New("no matching body passage")
	}
	nav.position = next
	selected := nav.page.References[next]
	return &selected, nil
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

// keepSearchSelection rebinds rerun selection by full identity; a missing hit
// starts at the first result rather than reusing an ordinal from old ranking.
func (l *sessionPicker) keepSearchSelection(identity string) {
	l.keep = identity
	l.start, l.cursor = 0, 0
	l.memo, l.cache, l.bottom = nil, nil, nil
}
