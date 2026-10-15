package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func prepareSearchBrowser(t *testing.T) (Env, storage.ObjectStore, string) {
	t.Helper()
	env, objects, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("refresh %d %s %s", code, &out, &errOut)
	}
	return env, objects, id
}

func TestSearchBrowserKeysAndLinesKeepExactBodyReference(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		t.Run(map[bool]string{false: "lines", true: "keys"}[keyed], func(t *testing.T) {
			env, _, id := prepareSearchBrowser(t)
			var document, diagnostics bytes.Buffer
			if code := Run([]string{"search", "visible", "--all-projects", "--json"}, nil, &document, &diagnostics, env); code != 0 {
				t.Fatalf("JSON query: %d %s", code, &diagnostics)
			}
			var jsonPage contentSearchDocument
			if err := json.Unmarshal(document.Bytes(), &jsonPage); err != nil || len(jsonPage.Results) != 1 || jsonPage.Results[0].Reference == nil {
				t.Fatalf("JSON identity: %s %v", &document, err)
			}
			jsonReference, err := decodeSearchReference(*jsonPage.Results[0].Reference)
			if err != nil {
				t.Fatal(err)
			}
			var fake *fakeKeys
			if keyed {
				fake = newFakeKeys("1\r", later(100*time.Millisecond, "n"), "p", "b", "q")
			}
			var out, errOut string
			var code int
			if keyed {
				out, errOut, code = runOnTerminal(t, env, fake, "", "search", "visible", "--all-projects")
			} else {
				out, errOut, code = runSearchAfterContext(t, env, "n\np\nb\nq\n")
			}
			if code != 0 {
				t.Fatalf("%d %s %s", code, errOut, out)
			}
			for _, want := range []string{id, "1. " + shortSessionID(id), jsonPage.Results[0].Harness + " / " + jsonPage.Results[0].Session, "Reference:", "[selected · assistant]", "visible", "Hand off current session", "no further matching body passage"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
			if !strings.Contains(strings.ReplaceAll(out, "\n", ""), jsonReference.Source.Reference.SHA256) {
				t.Fatalf("terminal wrapped source identity differs from JSON: %s", out)
			}
			if strings.Contains(out, "words to filter") || strings.Contains(out, "/ filter ·") {
				t.Fatalf("metadata filter exposed: %s", out)
			}
			if !strings.Contains(out, leaveAltScreenSequence) {
				t.Fatal("screen not restored")
			}
		})
	}
}

func TestSearchBrowserQueryAndPaintNeverOpenArchive(t *testing.T) {
	env, _, _ := prepareSearchBrowser(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("query or paint opened archive")
		return nil, errors.New("offline")
	}
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("/", "\x1b", "\x1b[B", "\x1b[6~", "q"), "", "search", `"visible"`, "--all-projects")
	if code != 0 || !strings.Contains(out, "visible") {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
}

func TestSearchBrowserPipedAndJSONNeverReadInput(t *testing.T) {
	env, _, _ := prepareSearchBrowser(t)
	env.IsTerminal = func(any) bool { return false }
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "visible", "--all-projects"}, failingSearchInput{t}, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s", code, &errOut)
	}
	env.IsTerminal = func(any) bool { return true }
	out.Reset()
	if code := Run([]string{"search", "visible", "--all-projects", "--json"}, failingSearchInput{t}, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s", code, &errOut)
	}
	if strings.Contains(out.String(), enterAltScreenSequence) {
		t.Fatal("JSON entered browser")
	}
}

type failingSearchInput struct{ t *testing.T }

func (r failingSearchInput) Read([]byte) (int, error) {
	r.t.Error("noninteractive search read input")
	return 0, io.EOF
}

func TestSearchBrowserResultRetentionAndExplicitRerun(t *testing.T) {
	env, _, _ := prepareSearchBrowser(t)
	home, _ := env.readHome()
	cfg, _, _ := config.Load(home)
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.OpenRead(context.Background(), dir, namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	q := search.Query{Text: "visible", Limit: 1}
	page, err := index.Search(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	s := &contentSearchBrowser{env: env, index: index, opts: contentSearchOptions{query: q}, page: page, hits: page.Hits, pages: 1, choices: rowChoices(nil, listFormatOptions{Numbered: true})}
	s.installRows()
	s.stale = true
	s.rerun = true
	if _, err = s.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.stale || s.pages != 1 || len(s.hits) != 1 || len(s.byKey) != 1 {
		t.Fatalf("rerun retained old state: %+v", s)
	}
	s.pages = 3
	if _, err = s.load(t.Context()); err != nil || len(s.hits) != 1 {
		t.Fatalf("retention cap failed: %v", err)
	}
}

// cancellationSearchStore completes only after cancellation. The browser must
// join it before returning or replacing the selected reference.
type cancellationSearchStore struct {
	storage.ObjectStore
	started chan struct{}
	stopped chan struct{}
}

func (s *cancellationSearchStore) GetLimited(ctx context.Context, _ string, _ int64) ([]byte, error) {
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return nil, ctx.Err()
}

func TestSearchSelectedReadCancelsAndJoinsBeforeReturning(t *testing.T) {
	for _, action := range []string{"q", "n", "b", "h"} {
		t.Run(action, func(t *testing.T) {
			env, objects, _ := prepareSearchBrowser(t)
			home, _ := env.readHome()
			cfg, _, _ := config.Load(home)
			namespace, dir := contentSearchLocation(home, cfg)
			index, err := search.OpenRead(t.Context(), dir, namespace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = index.Close() }()
			page, err := index.Search(t.Context(), search.Query{Text: "visible"})
			if err != nil || len(page.Hits) != 1 || page.Hits[0].Reference == nil {
				t.Fatalf("page: %+v %v", page, err)
			}
			blocked := &cancellationSearchStore{ObjectStore: objects, started: make(chan struct{}), stopped: make(chan struct{})}
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return blocked, nil }
			fake := newFakeKeys(later(20*time.Millisecond, action))
			keys := startKeys(fake)
			defer keys.close()
			b := &sessionBrowser{search: &contentSearchBrowser{env: env, cfg: cfg, namespace: namespace}, keys: keys, prompt: newPrompter(strings.NewReader(""), io.Discard), stdout: io.Discard, list: &sessionPicker{keys: keys}}
			_, err = b.readSearchSelection(t.Context(), *page.Hits[0].Reference, page.Hits[0].Reference.Start)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("read: %v", err)
			}
			select {
			case <-blocked.started:
			default:
				t.Fatal("read never started")
			}
			select {
			case <-blocked.stopped:
			default:
				t.Fatal("read was not joined")
			}
			if action == "q" {
				if !b.list.operationQuit {
					t.Fatal("quit lost")
				}
			} else if b.search.pendingAction != contentSearchAction(action) {
				t.Fatalf("replacement action lost: %q", b.search.pendingAction)
			}
		})
	}
}

func TestSearchHandoffPreservesTerminalAndBufferedDestinationAnswer(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		t.Run(map[bool]string{false: "lines", true: "keys"}[keyed], func(t *testing.T) {
			env, _, id := prepareSearchBrowser(t)
			fake := (*fakeKeys)(nil)
			input := "1\nh\np\n"
			if keyed {
				fake = newFakeKeys("1\r", "h")
				input = "p\n"
			}
			out, errOut, code := runOnTerminal(t, env, fake, input, "search", "visible", "--all-projects")
			if code != 0 {
				t.Fatalf("%d %s %s", code, errOut, out)
			}
			restored := strings.LastIndex(out, leaveAltScreenSequence)
			prompt := strings.Index(out, "Continue in")
			if restored < 0 || prompt < restored {
				t.Fatalf("handoff prompt before terminal restore: %s", out)
			}
			if !strings.Contains(out, id) || !strings.Contains(out, "source: archive") || !strings.Contains(out, "visible") {
				t.Fatalf("selected archive session not handed off: %s", out)
			}
		})
	}
}

func TestSearchRowsKeepRankFullIdentityAndEscapeText(t *testing.T) {
	s := &contentSearchBrowser{hits: []search.Hit{
		{Harness: "codex", SessionID: "same-session", Title: "body-only first", Snippet: "needle\x1b[2J\u202e", MatchCount: 3},
		{Harness: "claude", SessionID: "same-session", Title: "second", Snippet: "needle"},
	}, page: search.Page{Next: "more"}, pages: 1, choices: rowChoices(nil, listFormatOptions{Numbered: true, NarrowHint: "o next results"})}
	s.installRows()
	c := s.choices.shown()
	if c.total != -1 || !c.truncated {
		t.Fatal("unknown total was presented as exact")
	}
	if len(s.byKey) != 2 || c.rows[0].HarnessKey != "codex" || c.rows[1].HarnessKey != "claude" {
		t.Fatalf("identity/order lost: %+v", c.rows)
	}
	if strings.ContainsAny(c.rows[0].searchSnippet, "\x1b\u202e") {
		t.Fatalf("unsafe snippet: %q", c.rows[0].searchSnippet)
	}
	if c.rows[0].pickerSelection != nil || c.rows[1].pickerSelection != nil {
		t.Fatal("search manufactured picker authority")
	}
	if _, ok := matchBrowseRow("same-session", c.rows); ok {
		t.Fatal("ambiguous cross-harness ID selected")
	}
	if row, ok := matchBrowseRow("2", c.rows); !ok || row.selectionKey != childKey("claude", "same-session") {
		t.Fatal("numeric selection lost qualified identity")
	}
}

func TestSearchDetailsWrapLongUnicodeLinesForScrolling(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.TerminalSize = fixedTerminal{36, 20}.terminalSize
	b := &sessionBrowser{env: env, stdout: io.Discard, prompt: newPrompter(strings.NewReader(""), io.Discard), search: &contentSearchBrowser{}}
	lines := []string{strings.Repeat("界", 2000) + "TAILMARK"}
	screen := b.drawSearchDetails(lines, 0)
	if !screen.cut || screen.bottom == 0 || len(screen.lines) < 100 {
		t.Fatalf("long line cannot scroll: %+v", screen)
	}
	for _, line := range screen.lines {
		if visibleWidth(line) > 36 {
			t.Fatalf("oversized physical row: %d", visibleWidth(line))
		}
	}
	screen.scroll(scrollBottom)
	if !strings.Contains(strings.Join(screen.lines[screen.top:screen.top+screen.shown], ""), "TAILMARK") {
		t.Fatal("tail unreachable")
	}
}

func TestSearchSnippetRowsFitNarrowSharedBrowserMeasurements(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.TerminalSize = fixedTerminal{36, 20}.terminalSize
	state := &contentSearchBrowser{}
	rows := make([]listRow, 10)
	for i := range rows {
		rows[i] = listRow{Index: i + 1, Title: strings.Repeat("title", 100), Harness: "codex", ShortID: "12345678", When: "2026-01-01", searchSnippet: strings.Repeat("界", 256), searchMatches: 8}
	}
	format := listFormatOptions{Search: true, Numbered: true}
	groups := sessionTableGroups(rows, format)
	l := &sessionPicker{env: env, contentSearch: state}
	screen := l.screen(io.Discard, groups, format, len(rows), "coverage\nquery\nnext page\n", "Enter number: ")
	if screen.end >= len(rows) || screen.end < 1 {
		t.Fatalf("not paged: %+v", screen)
	}
	if used := l.tableLines(groups, format, pickerPage{screen.start, screen.end}, 36); used > screen.budget {
		t.Fatalf("%d rendered lines exceed budget%d", used, screen.budget)
	}
	var rendered bytes.Buffer
	if err := printSessionGroups(&rendered, pageSessionGroups(groups, pickerPage{screen.start, screen.end}), (&pickerView{format: format}).formatFor(36)); err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(rendered.String(), "\n") {
		if visibleWidth(line) > 36 {
			t.Fatalf("oversized row: %q", line)
		}
	}
	if !strings.Contains(rendered.String(), "7 other indexed title/body matches") {
		t.Fatalf("known count missing: %s", &rendered)
	}
}

func TestSearchAndMetadataRowsKeepDistinctWidthMeasurements(t *testing.T) {
	rows, format := layoutRows(t)
	metadata := &pickerView{rows: rows, format: format}
	wide := metadata.formatFor(100)
	if wide.layout == nil || metadata.formatFor(100).layout != wide.layout {
		t.Fatal("metadata width layout was not reused across pages")
	}
	narrow := metadata.formatFor(36)
	if narrow.layout == wide.layout {
		t.Fatal("metadata resize kept the old width layout")
	}
	for i := range rows {
		rows[i].searchSnippet = strings.Repeat("matched context ", 32)
		rows[i].searchMatches = 3
	}
	searchView := &pickerView{rows: rows, format: listFormatOptions{Search: true, Numbered: true}}
	picker := &sessionPicker{contentSearch: &contentSearchBrowser{}}
	for _, width := range []int{36, 100, 36} {
		searchFormat := searchView.formatFor(width)
		if searchFormat.Width != width || searchFormat.layout != nil {
			t.Fatalf("search inherited one-line metadata layout at width%d", width)
		}
		groups := sessionTableGroups(rows, searchFormat)
		var actual bytes.Buffer
		if err := printSessionGroups(&actual, groups, searchFormat); err != nil {
			t.Fatal(err)
		}
		measured := picker.tableLines(groups, searchFormat, pickerPage{0, len(rows)}, width)
		if measured != displayLines(actual.String(), width) || measured <= len(rows) {
			t.Fatalf("search multiline measure%d differs from actual or became one line per hit", measured)
		}
	}
	var ordinary bytes.Buffer
	if err := printSessionGroups(&ordinary, sessionTableGroups(rows, narrow), narrow); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ordinary.String(), "matched context") || displayLines(ordinary.String(), 36) != len(rows) {
		t.Fatalf("search snippet changed ordinary one-line rows: %s", &ordinary)
	}
}

func TestSearchResultsKeepRelevanceAcrossOrdinaryActivityDays(t *testing.T) {
	rows := dayRows(time.Time{}, dayGroupsNow, dayGroupsNow.Add(-24*time.Hour))
	for i := range rows {
		rows[i].SessionID = rows[i].ShortID
		rows[i].HarnessKey = "codex"
		rows[i].selectionKey = childKey("codex", rows[i].SessionID)
		rows[i].searchSnippet = "matching indexed text"
	}
	format := listFormatOptions{Now: dayGroupsNow, GroupByDay: true, Numbered: true, Search: true}
	picker := &sessionPicker{contentSearch: &contentSearchBrowser{}}
	view := picker.view(rows, len(rows), false, format)
	if len(view.groups) != 1 || view.groups[0].label != "" || len(view.order) != len(rows) {
		t.Fatalf("search was day-grouped: %s", groupSummary(view.groups))
	}
	for i, row := range view.order {
		if row.selectionKey != rows[i].selectionKey || row.Index != i+1 {
			t.Fatalf("search relevance moved at%d: %+v", i, view.order)
		}
	}
	var rendered bytes.Buffer
	if err := printSessionGroups(&rendered, view.groups, view.formatFor(36)); err != nil {
		t.Fatal(err)
	}
	for _, heading := range []string{"TODAY", "YESTERDAY", "OLDER"} {
		if strings.Contains(rendered.String(), heading) {
			t.Fatalf("day heading in ranked search: %s", &rendered)
		}
	}
	format.Search = false
	if got := groupSummary(sessionTableGroups(rows, format)); got != "TODAY: 2 | YESTERDAY: 3 | OLDER: 1" {
		t.Fatalf("ordinary activity groups changed: %s", got)
	}
}

func TestSearchBufferedActionsCancelAndJoinSelectedRead(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		for _, action := range []string{"q", "b", "h", "n"} {
			t.Run(fmt.Sprintf("keys%v/%s", keyed, action), func(t *testing.T) {
				env, objects, _ := prepareSearchBrowser(t)
				home, _ := env.readHome()
				cfg, _, _ := config.Load(home)
				namespace, dir := contentSearchLocation(home, cfg)
				index, err := search.OpenRead(t.Context(), dir, namespace)
				must(t, err)
				page, err := index.Search(t.Context(), search.Query{Text: "visible"})
				must(t, err)
				must(t, index.Close())
				blocked := &cancellationSearchStore{ObjectStore: objects, started: make(chan struct{}), stopped: make(chan struct{})}
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return blocked, nil }
				prompt := newPrompter(strings.NewReader(action+"\n"), io.Discard)
				var keys *keyInput
				if keyed {
					keys = startKeys(newFakeKeys())
					defer keys.close()
					keys.pending = []key{{kind: keyRune, r: rune(action[0])}}
				} else {
					_, err = prompt.in.Peek(1)
					must(t, err)
				}
				b := &sessionBrowser{search: &contentSearchBrowser{env: env, cfg: cfg, namespace: namespace}, keys: keys, prompt: prompt, stdout: io.Discard, list: &sessionPicker{keys: keys}}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				_, err = b.readSearchSelection(ctx, *page.Hits[0].Reference, page.Hits[0].Reference.Start)
				if !errors.Is(err, context.Canceled) || action == "q" && !b.list.operationQuit || action != "q" && b.search.pendingAction != contentSearchAction(action) {
					t.Fatalf("buffered action was not applied before read deadline: err=%v quit=%v pending=%q", err, b.list.operationQuit, b.search.pendingAction)
				}
				select {
				case <-blocked.stopped:
				default:
					t.Fatal("selected read was not joined")
				}
			})
		}
	}
}

func TestSearchInteractiveRefreshFailureKeepsCachedResultsAndFailure(t *testing.T) {
	for _, action := range []string{"quit", "handoff", "handoff unavailable"} {
		t.Run(action, func(t *testing.T) {
			env, _, _ := prepareSearchBrowser(t)
			home, _ := env.readHome()
			release, err := local.NamedLock(home, "search-worker.lock")
			must(t, err)
			defer release()
			keys := newFakeKeys("q")
			if action != "quit" {
				keys = newFakeKeys("1\r", "h")
			}
			if action == "handoff unavailable" {
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
					return nil, errors.New("selected archive unavailable")
				}
			}
			out, diagnostics, code := runOnTerminal(t, env, keys, "p\n", "search", "visible", "--all-projects", "--refresh")
			if code != 1 || !strings.Contains(out, "visible") || !strings.Contains(diagnostics, "another search refresh") {
				t.Fatalf("refresh failure lost beside cached result: code%d diagnostics=%s output=%s", code, diagnostics, out)
			}
			if action == "handoff" && !strings.Contains(out, "source: archive") {
				t.Fatalf("cached handoff did not complete: %s %s", out, diagnostics)
			}
			if action == "handoff unavailable" && (!strings.Contains(diagnostics, "agent-archive: handoff:") || !strings.Contains(diagnostics, "selected archive unavailable")) {
				t.Fatalf("handoff failure was erased by refresh failure: %s", diagnostics)
			}
		})
	}
}

func TestSearchBrowserCoverageShowsPartialObservationAndGaps(t *testing.T) {
	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &contentSearchBrowser{page: search.Page{Observation: search.Observation{LastObserved: observed, Indexed: 4, Known: 7, Unsupported: 3}}, pages: 1, choices: rowChoices(nil, listFormatOptions{})}
	s.installRows()
	note := s.choices.shown().note
	for _, want := range []string{"incomplete", "2026-01-02T03:04:05Z", "3 unsupported"} {
		if !strings.Contains(note, want) || !strings.Contains((&sessionPicker{contentSearch: s}).displayFooter(note, 36), want) {
			t.Errorf("coverage omits %q in narrow view: %s", want, note)
		}
	}
	s.page.Observation.LastObserved = time.Time{}
	s.installRows()
	if note = s.choices.shown().note; !strings.Contains(note, "freshness unknown") {
		t.Fatalf("unobserved archive not explained: %s", note)
	}
}

// A real pipe lets the line-mode browser poll without blocking. Actions arrive
// only after selected context is painted, distinguishing retrieval from cancel.
func runSearchAfterContext(t *testing.T, env Env, actions string) (string, string, int) {
	t.Helper()
	input, send, err := os.Pipe()
	must(t, err)
	defer func() { _ = input.Close() }()
	shown := make(chan struct{})
	out := &searchContextOutput{shown: shown}
	var diagnostics bytes.Buffer
	env.TerminalSize = fixedTerminal{100, 40}.terminalSize
	env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(out) }
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return nil, false }
	done := make(chan error, 1)
	go func() {
		defer func() { _ = send.Close() }()
		if _, e := io.WriteString(send, "1\n"); e != nil {
			done <- e
			return
		}
		select {
		case <-shown:
			_, e := io.WriteString(send, actions)
			done <- e
		case <-time.After(5 * time.Second):
			done <- errors.New("selected context was not painted")
		}
	}()
	code := Run([]string{"search", "visible", "--all-projects"}, input, out, &diagnostics, env)
	must(t, <-done)
	return out.String(), diagnostics.String(), code
}

type searchContextOutput struct {
	bytes.Buffer
	shown chan struct{}
}

func (o *searchContextOutput) Write(p []byte) (int, error) {
	n, err := o.Buffer.Write(p)
	if o.shown != nil && strings.Contains(o.String(), "[selected · assistant]") {
		close(o.shown)
		o.shown = nil
	}
	return n, err
}

// Search uses the same persistent highlight and detail strip as ordinary lists,
// but its query is fixed and its multiline rows and footer are measured as drawn.
func TestSearchHighlightedNavigationAndCompactFooter(t *testing.T) {
	rows := []listRow{
		{Index: 1, selectionKey: childKey("codex", "same"), HarnessKey: "codex", Harness: "Codex", SessionID: "same", ShortID: "same", Title: "first ranked hit", searchSnippet: "first snippet"},
		{Index: 2, selectionKey: childKey("claude", "same"), HarnessKey: "claude", Harness: "Claude", SessionID: "same", ShortID: "same", Title: "second ranked hit", searchSnippet: "second snippet"},
	}
	keys := startKeys(newFakeKeys("/", "\x1b[B", "\r"))
	defer keys.close()
	l := &sessionPicker{env: fixedTerminal{160, 40}, keys: keys, contentSearch: &contentSearchBrowser{}, older: &browserLoadAction{Label: "next indexed results"}}
	var out bytes.Buffer
	l.clear = func() { out.WriteString(screenBreak) }
	row, ok, err := l.pickRowsContext(t.Context(), newPrompter(strings.NewReader(""), &out), &out, rows, len(rows), false, listFormatOptions{Search: true, Numbered: true, Note: "Coverage incomplete.\nLast observed: unknown.\n"}, "read")
	if err != nil || !ok || rowIdentity(row) != rowIdentity(rows[1]) || l.filtering {
		t.Fatalf("highlight selection lost full identity: row%+v ok%v err%v", row, ok, err)
	}
	screens := strings.Split(out.String(), screenBreak)
	checkKeyScreensFit(t, screens, 160, 40)
	for _, forbidden := range []string{"/ search", "/ filter", "o older"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("search advertised %q: %s", forbidden, &out)
		}
	}
	for _, want := range []string{cursorMark + " 1.", "first ranked hit", "r rerun", "o next results", "Coverage incomplete."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if !strings.Contains(screens[len(screens)-1], cursorMark+" 2.") || !strings.Contains(screens[len(screens)-1], strings.Repeat("─", 160)) {
		t.Fatalf("highlight or shared detail strip missing: %s", screens[len(screens)-1])
	}
}

func TestSearchRerunHighlightRebindsFullIdentityOrFirstHit(t *testing.T) {
	rows := []listRow{
		{selectionKey: childKey("codex", "same"), Index: 1, Title: "codex"},
		{selectionKey: childKey("claude", "same"), Index: 2, Title: "claude"},
	}
	for _, tc := range []struct {
		key  string
		want int
	}{{rowIdentity(rows[1]), 1}, {childKey("cursor", "same"), 0}} {
		l := &sessionPicker{env: fixedTerminal{80, 30}, contentSearch: &contentSearchBrowser{}, cursor: 1, start: 1}
		l.keepSearchSelection(tc.key)
		v := l.view(rows, len(rows), false, listFormatOptions{Search: true})
		screen := l.filterScreen(io.Discard, v, "Read: ")
		if l.cursor != tc.want || screen.cursor != tc.want || l.keep != "" {
			t.Fatalf("rerun key%q selected ordinal%d, want%d", tc.key, l.cursor, tc.want)
		}
	}
}

func TestSearchHighlightedScreenMeasuresStripFooterAndResize(t *testing.T) {
	rows := []listRow{{Index: 1, Title: "first", searchSnippet: strings.Repeat("界", 50), searchMatches: 3}, {Index: 2, Title: "second", searchSnippet: "second match"}}
	format := listFormatOptions{Search: true, Numbered: true, Cursor: true}
	footer := "Coverage incomplete; matching history may be missing.\nLast observed: unknown.\n"
	for _, size := range []fixedTerminal{{120, 40}, {36, 20}, {120, 40}} {
		l := &sessionPicker{env: size, contentSearch: &contentSearchBrowser{}}
		groups := highlightGroups(sessionTableGroups(rows, format), 0)
		screen := l.screen(io.Discard, groups, format, len(rows), footer, "Read: ")
		var frame bytes.Buffer
		f := format
		f.Width = screen.width
		if err := printSessionGroups(&frame, pageSessionGroups(groups, pickerPage{screen.start, screen.end}), f); err != nil {
			t.Fatal(err)
		}
		if screen.strip {
			for _, line := range detailStrip(rows[0], screen.width, format.Style) {
				frame.WriteString(line + "\n")
			}
		}
		frame.WriteString(l.displayFooter(footer, screen.width))
		frame.WriteString(l.footerLine(screen, "read") + "\nRead: ")
		if used := displayLines(frame.String(), screen.width); used > size.height {
			t.Fatalf("frame uses%d rows, height%d: %s", used, size.height, &frame)
		}
		if screen.end-screen.start < minKeyScreenRows {
			t.Fatal("no minimum key row")
		}
	}
}
