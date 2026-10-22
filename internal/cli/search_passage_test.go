package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// passageScreens opens the second result (outside's best match, the third of
// the four messages) and returns every screen drawn after the keys given.
func passageScreens(t *testing.T, keys ...string) []string {
	t.Helper()
	env := searchProjectsFixture(t)
	return searchScreens(t, env, newFakeKeys(append([]string{"\x1b[B", "\r"}, keys...)...), "", "read performance")
}

func TestSearchPassageReadsLikeShow(t *testing.T) {
	screens := passageScreens(t, "b", "q")
	passage := lastScreenWith(t, screens, searchMatchFooter)
	lines := strings.Split(passage, "\n")
	if lines[0] != "Picker slowness" || !strings.HasPrefix(lines[1], "Codex · outside · Jan 1, 22:00 ") || !strings.HasSuffix(lines[1], " match 1 of 3") || visibleWidth(lines[1]) != 100 {
		t.Fatalf("heading:\n%s", passage)
	}
	want := "\n  codex › I'll trace list, show and handoff against the bucket first, then write the proposal.\n\n" +
		"  you ›   Also note the handoff READ-performance.\n\n" +
		"  codex › Done: read performance notes are in dev/bench.\n"
	if !strings.Contains(passage, want) {
		t.Fatalf("messages around the match:\n%s", passage)
	}
	for _, hidden := range []string{"Reference:", "Source revision", "text bytes", "[selected"} {
		if strings.Contains(passage, hidden) {
			t.Errorf("%q shown before i:\n%s", hidden, passage)
		}
	}
	if last := screens[len(screens)-1]; !strings.HasPrefix(last, "All projects · ") {
		t.Fatalf("b did not go back to the results:\n%s", last)
	}
}

func TestSearchPassageDetailsShowTheExactReference(t *testing.T) {
	env := searchProjectsFixture(t)
	var document, diagnostics strings.Builder
	if code := Run([]string{"search", "read performance", "--json"}, nil, &document, &diagnostics, env); code != 0 {
		t.Fatalf("%d %s", code, &diagnostics)
	}
	var page contentSearchDocument
	must(t, json.Unmarshal([]byte(document.String()), &page))
	if len(page.Results) != 2 || page.Results[1].Reference == nil {
		t.Fatalf("JSON results: %s", &document)
	}
	ref, err := decodeSearchReference(*page.Results[1].Reference)
	must(t, err)
	screens := searchScreens(t, env, newFakeKeys("\x1b[B", "\r", "i", "i", "q"), "", "read performance")
	shown := lastScreenWith(t, screens, "Reference: record")
	joined := strings.ReplaceAll(shown, "\n", "")
	for _, want := range []string{"Session: codex / outside-session", "Source revision: active source", "Reference: record 3 · text bytes 0–39 · source " + ref.Source.Reference.SHA256, "Passage bytes 0–39"} {
		if !strings.Contains(joined, want) {
			t.Errorf("details miss %q:\n%s", want, shown)
		}
	}
	if last := screens[len(screens)-1]; strings.Contains(last, "Reference:") {
		t.Fatalf("second i did not hide the details:\n%s", last)
	}
}

func TestSearchPassageMovesBetweenMatchesWithTheStrip(t *testing.T) {
	screens := passageScreens(t, "n", "p", "n", "b", "q")
	want := []string{
		"match 1 of 3",
		"match 2 of 3\n\n  you ›   Also note the handoff READ-performance.\n\n  codex › Done: read performance notes are in dev/bench.",
		"match 1 of 3",
		"match 2 of 3",
		// Back in the results, the strip shows the match the passage did.
		"codex › match 2 of 3 in this session\nDone: read performance notes are in dev/bench.",
	}
	at := 0
	for _, screen := range screens {
		if at < len(want) && strings.Contains(screen, want[at]) {
			at++
		}
	}
	if at != len(want) {
		t.Fatalf("did not show %q in order:\n%s", want[at], strings.Join(screens, "\n----\n"))
	}
}

func TestSearchPassageTOpensTheTranscriptInShowsPager(t *testing.T) {
	env := searchProjectsFixture(t)
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("\x1b[B", "\r", "t", "q"), "", "search", "read performance")
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	// The pager (a copy here) gets show --transcript's rendering of the
	// session, every message, not only the passage's three.
	for _, want := range []string{"── You ─", "┃ The list and show commands feel slow on the big bucket.", "Codex ›\nDone: read performance notes are in dev/bench."} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript misses %q:\n%s", want, out)
		}
	}
}

func TestSearchPassageByLines(t *testing.T) {
	env := searchProjectsFixture(t)
	out, errOut, code := runSearchAfterContext(t, env, "2\n", "Also note the handoff", "i\nb\nq\n", "read performance")
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	for _, want := range []string{"  you ›   Also note the handoff READ-performance.", "Session: codex / outside-session", searchMatchFooter} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestSearchPassageCutsLongNeighborsAndHighlightsTheMatch(t *testing.T) {
	style := textStyle{color: true}
	s := &contentSearchBrowser{opts: contentSearchOptions{query: search.Query{Text: "read performance"}}, style: style, now: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	hit := search.Hit{SessionID: "0a13656b", Harness: "claude", Title: "Sublinear reads", Project: "agent-archive", MatchCount: 3, CapturedAt: time.Date(2026, 10, 16, 9, 12, 0, 0, time.UTC)}
	ref := &search.Reference{Record: 4, Start: 0, End: 60}
	st := &searchMatchState{ordinal: 1, ref: ref, role: "user"}
	long := strings.TrimSpace(strings.Repeat("one more line of the reply\n", 10))
	out := search.Surrounding{Role: "user",
		Selected: search.Context{Text: "Can you improve the read performance?", End: 37},
		Before:   []search.ContextSlice{{Role: "assistant", Context: search.Context{Text: "Merged #633."}}},
		After:    []search.ContextSlice{{Role: "assistant", Context: search.Context{Text: long}}},
	}
	lines, cut, _ := s.passageLines(hit, st, out, 80, passageView{})
	plain := ansiEscape.ReplaceAllString(strings.Join(lines, "\n"), "")
	meta := "Claude Code · agent-archive · Oct 16, 09:12"
	want := "Sublinear reads\n" + meta + strings.Repeat(" ", 80-visibleWidth(meta)-len("match 1 of 3")) + "match 1 of 3\n\n" +
		"  claude › Merged #633.\n\n" +
		"  you ›    Can you improve the read performance?\n\n" +
		"  claude › one more line of the reply\n           one more line of the reply\n           one more line of the reply\n           … 7 more lines (m)"
	if !cut || plain != want {
		t.Fatalf("cut %v, passage:\n%s\nwant:\n%s", cut, plain, want)
	}
	joined := strings.Join(lines, "\n")
	for _, styled := range []string{style.bold("Sublinear reads"), style.agent("claude", "Claude Code"), style.match("read") + " " + style.match("performance"), style.dim("Merged #633."), style.bold("you ›   ")} {
		if !strings.Contains(joined, styled) {
			t.Errorf("missing %q in %q", styled, joined)
		}
	}
	lines, cut, _ = s.passageLines(hit, st, out, 80, passageView{expanded: true})
	if cut || strings.Count(strings.Join(lines, "\n"), "one more line of the reply") != 10 {
		t.Fatalf("m did not show the neighbor whole: %q", lines)
	}

	// A title match shows the title, and how to reach the body's matches.
	title := &searchMatchState{ordinal: 1, text: "Sublinear reads"}
	lines, _, _ = s.passageLines(hit, title, search.Surrounding{}, 80, passageView{})
	plain = ansiEscape.ReplaceAllString(strings.Join(lines, "\n"), "")
	if !strings.Contains(plain, "  title › Sublinear reads") || !strings.Contains(plain, "n shows the session's first matching message") {
		t.Fatalf("title match:\n%s", plain)
	}

	// An unreadable exact source shows no stand-in text.
	lines, _, _ = s.passageLines(hit, st, search.Surrounding{}, 80, passageView{})
	if len(lines) != 2 {
		t.Fatalf("unread passage shows content: %q", lines)
	}
}

func TestHighlightedLinesKeepLineBreaksAndSafeText(t *testing.T) {
	style := textStyle{color: true}
	lines, _ := highlightedLines("visible", "first visible\n\n  indented \x1b[2J visible\u202e", 80, style)
	if len(lines) != 3 || lines[1] != "" || !strings.HasPrefix(lines[2], "  indented [2J ") {
		t.Fatalf("lines: %q", lines)
	}
	if strings.Count(strings.Join(lines, "\n"), style.match("visible")) != 2 || strings.Contains(strings.Join(lines, ""), "\u202e") {
		t.Fatalf("highlight or sanitizing lost: %q", lines)
	}
}

// firstReadWaits is a store whose first bounded read waits until it is
// cancelled; later reads go through.
type firstReadWaits struct {
	storage.ObjectStore
	reads *atomic.Int32
}

func (s firstReadWaits) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if s.reads.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if limited, ok := s.ObjectStore.(storage.LimitedGetter); ok {
		return limited.GetLimited(ctx, key, limit)
	}
	return s.Get(ctx, key)
}

func TestSearchPassageKeyTypedDuringALeftReadDoesNotFollow(t *testing.T) {
	env := searchProjectsFixture(t)
	open, reads := env.OpenStore, &atomic.Int32{}
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		objects, err := open(cfg)
		return firstReadWaits{ObjectStore: objects, reads: reads}, err
	}
	// t while the first passage loads, then b leaves it before it is read;
	// the next passage opened must not open the transcript.
	screens := searchScreens(t, env, newFakeKeys("\x1b[B", "\r", later(time.Millisecond, "t"), later(time.Millisecond, "b"), "\r", "b", "q"), "", "read performance")
	if reads.Load() < 2 {
		t.Fatalf("the second passage was not read")
	}
	for _, screen := range screens {
		if strings.Contains(screen, "── You ─") {
			t.Fatalf("t typed into a passage that was left opened the transcript of the next:\n%s", screen)
		}
	}
	lastScreenWith(t, screens, "  codex › Done: read performance notes are in dev/bench.")
}

func TestSearchPassageWrapsColoredLinesOnANarrowTerminal(t *testing.T) {
	style := textStyle{color: true}
	s := &contentSearchBrowser{opts: contentSearchOptions{query: search.Query{Text: "read performance"}}, style: style, now: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	hit := search.Hit{SessionID: "0a13656b", Harness: "claude", Title: "Sublinear reads", MatchCount: 1}
	st := &searchMatchState{ordinal: 1, ref: &search.Reference{End: 60}, role: "user"}
	out := search.Surrounding{Role: "user", Selected: search.Context{Text: "Can you improve the read performance of every read performance path?", End: 60}}
	lines, _, _ := s.passageLines(hit, st, out, 24, passageView{})
	escape := regexp.MustCompile("\x1b\\[[0-9;]*m")
	wrapped := 0
	for _, line := range lines {
		rows := splitColumns(line, 24)
		for i, row := range rows {
			// Color codes take no columns, and a row is cut only at the
			// terminal's edge, never inside a code.
			if visibleWidth(row) > 24 || i < len(rows)-1 && visibleWidth(row) != 24 || strings.Contains(escape.ReplaceAllString(row, ""), "\x1b") {
				t.Fatalf("row %q of %q", row, rows)
			}
		}
		wrapped += len(rows) - 1
	}
	if wrapped == 0 {
		t.Fatalf("no line was wider than 24: %q", lines)
	}
}

func TestSearchPassageEscapesEveryFieldAndHighlightsOnlyTheMatch(t *testing.T) {
	style := textStyle{color: true}
	s := &contentSearchBrowser{opts: contentSearchOptions{query: search.Query{Text: "read"}}, style: style, now: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	hit := search.Hit{SessionID: "0a13656b\x1b[2J", Harness: "claude", Title: "Sublinear \x1b]0;x\x07reads\u202e", Project: "agent\x1b[2J-archive", MatchCount: 2,
		Gaps: []search.Gap{{RevisionID: "rev\x1b[2J", Reason: "gone\u009b31m"}}}
	ref := &search.Reference{Record: 2, End: 20, Source: search.Source{RevisionID: "rev\x1b[2J"}}
	st := &searchMatchState{ordinal: 1, ref: ref, role: "user"}
	out := search.Surrounding{Role: "user\x1b[2J",
		Selected: search.Context{Text: "please read\x1b[2J this", End: 20},
		Before:   []search.ContextSlice{{Role: "assistant", Context: search.Context{Text: "I read it\u202e already"}}},
	}
	lines, _, _ := s.passageLines(hit, st, out, 80, passageView{details: true})
	text := strings.Join(lines, "\n")
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(text, "")
	if strings.ContainsAny(plain, "\x1b\x07\u202e\u009b") {
		t.Fatalf("unsafe passage: %q", plain)
	}
	if strings.Count(text, style.match("read")) != 1 {
		t.Fatalf("a neighbor's words are highlighted, or the match's are not: %q", text)
	}
}

func TestSearchPassageMShowsCutNeighborsWhole(t *testing.T) {
	s := &contentSearchBrowser{}
	p := &openPassage{b: &sessionBrowser{search: s}, state: &searchMatchState{ordinal: 1}, cut: true}
	if _, done, err := p.apply(t.Context(), searchActionMore, &detailsScreen{}); done || err != nil || !p.view.expanded {
		t.Fatalf("m did not expand: done %v err %v view %+v", done, err, p.view)
	}
	p.cut = false
	if _, _, err := p.apply(t.Context(), searchActionMore, &detailsScreen{}); err != nil || p.notice == "" {
		t.Fatalf("m with nothing more said nothing: %v %q", err, p.notice)
	}
}

// readWaitsForKeys is a store whose first bounded read waits until the
// terminal has only left keys to read, or the read is cancelled.
type readWaitsForKeys struct {
	storage.ObjectStore
	reads *atomic.Int32
	keys  *fakeKeys
	left  int
}

func (s readWaitsForKeys) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if s.reads.Add(1) == 1 {
		for {
			s.keys.mu.Lock()
			waiting := len(s.keys.chunks) > s.left
			s.keys.mu.Unlock()
			if !waiting {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
	if limited, ok := s.ObjectStore.(storage.LimitedGetter); ok {
		return limited.GetLimited(ctx, key, limit)
	}
	return s.Get(ctx, key)
}

func TestSearchPassageMTypedWhileLoadingAppliesOnceRead(t *testing.T) {
	env := searchProjectsFixture(t)
	// m while the passage loads; b and q come only after it is read.
	keys := newFakeKeys("\x1b[B", "\r", later(time.Millisecond, "m"), later(time.Hour, "b"), later(time.Hour, "q"))
	open, reads := env.OpenStore, &atomic.Int32{}
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		objects, err := open(cfg)
		return readWaitsForKeys{ObjectStore: objects, reads: reads, keys: keys, left: 2}, err
	}
	screens := searchScreens(t, env, keys, "", "read performance")
	shown := lastScreenWith(t, screens, "Nothing more of these messages to show.")
	if !strings.Contains(shown, "  codex › Done: read performance notes are in dev/bench.") {
		t.Fatalf("m typed while the passage loaded replaced its read:\n%s", shown)
	}
}

func TestSearchPassageEscWhileLoadingSaysMLoadsIt(t *testing.T) {
	env := searchProjectsFixture(t)
	open, reads := env.OpenStore, &atomic.Int32{}
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		objects, err := open(cfg)
		return firstReadWaits{ObjectStore: objects, reads: reads}, err
	}
	screens := searchScreens(t, env, newFakeKeys("\x1b[B", "\r", later(time.Millisecond, "\x1b"), later(time.Hour, "m"), later(time.Hour, "b"), later(time.Hour, "q")), "", "read performance")
	cancelled := lastScreenWith(t, screens, "Not loaded · m loads it · b goes back")
	if strings.Contains(cancelled, "codex › Done") {
		t.Fatalf("a cancelled read showed the passage:\n%s", cancelled)
	}
	for _, screen := range screens {
		if strings.Contains(screen, "context canceled") {
			t.Fatalf("cancel said:\n%s", screen)
		}
	}
	lastScreenWith(t, screens, "  codex › Done: read performance notes are in dev/bench.")
}

func TestSearchPassageOpensScrolledToItsFirstMatch(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.TerminalSize = fixedTerminal{60, 24}.terminalSize
	s := &contentSearchBrowser{opts: contentSearchOptions{query: search.Query{Text: "needle"}}}
	b := &sessionBrowser{env: env, stdout: io.Discard, prompt: newPrompter(strings.NewReader(""), io.Discard), search: s}
	text := strings.Repeat("hay\n", 40) + "the needle is here\n" + strings.Repeat("hay\n", 10)
	st := &searchMatchState{ordinal: 1, ref: &search.Reference{End: len(text)}, role: "user"}
	lines, _, match := s.passageLines(search.Hit{SessionID: "0a13656b", Harness: "claude", Title: "Hay", MatchCount: 1}, st, search.Surrounding{Role: "user", Selected: search.Context{Text: text, End: len(text)}}, 60, passageView{})
	if match < 0 || !strings.Contains(lines[match], "needle") {
		t.Fatalf("match line %d of %q", match, lines)
	}
	screen := b.drawSearchDetails(lines, 0, match, "")
	if !strings.Contains(strings.Join(screen.lines[screen.top:screen.top+screen.shown], "\n"), "the needle is here") {
		t.Fatalf("first match off screen from line %d", screen.top)
	}
	if screen := b.drawSearchDetails(lines, 0, -1, ""); screen.top != 0 {
		t.Fatalf("no seek moved to line %d", screen.top)
	}
}

func TestHighlightedLinesExpandTabsToTabStops(t *testing.T) {
	lines, _ := highlightedLines("", "a\tb\n\tc\nabcdefgh\ti", 80, textStyle{})
	if want := []string{"a       b", "        c", "abcdefgh        i"}; !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

// searchLongMessage is a 20 KB reply, three overlapping indexed passages,
// that zebrafish matches at its start and that ends in TAILMARK.
var searchLongMessage = "zebrafish notes. " + strings.Repeat("filler words go here ", 1000) + "TAILMARK"

// longMessageFixture is searchProjectsFixture with a prompt and
// searchLongMessage as its reply.
func longMessageFixture(t *testing.T) Env {
	t.Helper()
	return searchProjectsFixtureOf(t, `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Write it all down."}]}}
{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"`+searchLongMessage+`"}}`)
}

func TestSearchPassageMReadsALongMessageToItsEnd(t *testing.T) {
	env := longMessageFixture(t)
	keys := []string{"\x1b[B", "\r"}
	for range 7 {
		keys = append(keys, "m")
	}
	screens := searchScreens(t, env, newFakeKeys(append(keys, "\x1b[F", "b", "q")...), "", "zebrafish")
	tail := lastScreenWith(t, screens, "TAILMARK")
	if !strings.Contains(tail, searchMatchFooter) || strings.Contains(tail, "message continues") {
		t.Fatalf("the message's end:\n%s", tail)
	}
	for _, screen := range screens {
		if strings.Contains(screen, "Nothing more of these messages to show.") && !strings.Contains(screen, "TAILMARK") {
			t.Fatalf("m stopped before the message's end:\n%s", screen)
		}
	}
}

func TestSearchReadFollowsNextOffsetToTheMessagesEnd(t *testing.T) {
	env := longMessageFixture(t)
	code, page, diag := searchJSONRun(t, env, "zebrafish")
	if code != 0 || len(page.Results) == 0 || page.Results[0].Reference == nil {
		t.Fatalf("%d %s %+v", code, diag, page)
	}
	ref := *page.Results[0].Reference
	var read strings.Builder
	args := []string{"--read", ref, "--max-bytes", "4096"}
	for reads := 0; ; reads++ {
		code, doc, diag := searchJSONRun(t, env, args...)
		if code != 0 || doc.Context == nil {
			t.Fatalf("read %v: %d %s", args, code, diag)
		}
		read.WriteString(doc.Context.Selected.Text)
		next := doc.Context.Selected.Next
		if next == nil || reads > 10 {
			break
		}
		args = []string{"--read", ref, "--offset", strconv.Itoa(*next), "--max-bytes", "4096"}
	}
	if read.String() != searchLongMessage {
		t.Fatalf("read %d of %d bytes", read.Len(), len(searchLongMessage))
	}
}

func TestSearchReadOffsetPastTheMessageIsAUsageError(t *testing.T) {
	env := longMessageFixture(t)
	code, page, diag := searchJSONRun(t, env, "zebrafish")
	if code != 0 || len(page.Results) == 0 || page.Results[0].Reference == nil {
		t.Fatalf("%d %s %+v", code, diag, page)
	}
	code, doc, diag := searchJSONRun(t, env, "--read", *page.Results[0].Reference, "--offset", strconv.Itoa(1<<20))
	if code != 2 || doc.Error == nil || doc.Error.Code != "invalid_offset" {
		t.Fatalf("offset past the message: %d %s %+v", code, diag, doc.Error)
	}
}

func TestSearchPassageMSaysANeighborGoesOnInTheTranscript(t *testing.T) {
	env := searchProjectsFixtureOf(t, `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Write the zebrafish notes down."}]}}
{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"`+strings.Repeat("filler words go here ", 1000)+`TAILMARK"}}`)
	// m shows the cut reply whole, as far as one read goes; m again has
	// nothing more of the match to read.
	screens := searchScreens(t, env, newFakeKeys("\x1b[B", "\r", "m", "m", "b", "q"), "", "zebrafish")
	lastScreenWith(t, screens, "The message after goes on · t shows the transcript")
	for _, screen := range screens {
		if strings.Contains(screen, "Nothing more of these messages to show.") {
			t.Fatalf("m said nothing more while the reply goes on:\n%s", screen)
		}
	}
}

// readFails is a store whose reads fail with err once the terminal has only
// left keys to read.
type readFails struct {
	storage.ObjectStore
	keys *fakeKeys
	left int
	err  error
}

func (s readFails) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	for {
		s.keys.mu.Lock()
		waiting := len(s.keys.chunks) > s.left
		s.keys.mu.Unlock()
		if !waiting {
			return nil, s.err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSearchPassageMTypedWhileAReadFailsKeepsTheFailure(t *testing.T) {
	env := searchProjectsFixture(t)
	keys := newFakeKeys("\x1b[B", "\r", later(time.Millisecond, "m"), later(time.Hour, "b"), later(time.Hour, "q"))
	open := env.OpenStore
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		objects, err := open(cfg)
		return readFails{ObjectStore: objects, keys: keys, left: 2, err: errors.New("synthetic outage")}, err
	}
	screens := searchScreens(t, env, keys, "", "read performance")
	lastScreenWith(t, screens, "synthetic outage")
	for _, screen := range screens {
		if strings.Contains(screen, "Nothing more of these messages to show.") {
			t.Fatalf("m typed while the read failed replaced its failure:\n%s", screen)
		}
	}
}

// waitingKeys is a terminal no key is typed on that waits as a real one
// does.
type waitingKeys struct{ *fakeKeys }

func (k waitingKeys) read(_ []byte, wait time.Duration) (int, error) {
	if wait < 0 {
		return 0, io.EOF
	}
	time.Sleep(wait)
	return 0, nil
}

// deadlineStore fails every read with its context's error.
type deadlineStore struct{ storage.ObjectStore }

func (s deadlineStore) GetLimited(ctx context.Context, _ string, _ int64) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s deadlineStore) Get(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSearchSelectedReadReportsItsDeadline(t *testing.T) {
	env, objects, _ := prepareSearchBrowser(t)
	home, _ := env.readHome()
	cfg, _, _ := config.Load(home)
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.OpenRead(t.Context(), dir, namespace)
	must(t, err)
	defer func() { _ = index.Close() }()
	page, err := index.Search(t.Context(), search.Query{Text: "visible"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].Reference == nil {
		t.Fatalf("page: %+v %v", page, err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return deadlineStore{objects}, nil }
	keys := startKeys(waitingKeys{newFakeKeys()})
	defer keys.close()
	b := &sessionBrowser{search: &contentSearchBrowser{env: env, cfg: cfg, namespace: namespace}, keys: keys, prompt: newPrompter(strings.NewReader(""), io.Discard), stdout: io.Discard, list: &sessionPicker{keys: keys}}
	// The deadline passes while a key is waited for, and the read ends with
	// it, so its result and the deadline are ready together; either way the
	// deadline is what is reported.
	for range 20 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		_, err := b.readSearchSelection(ctx, *page.Hits[0].Reference, page.Hits[0].Reference.Start, false)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read out of time: %v", err)
		}
	}
}
