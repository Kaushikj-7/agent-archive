package cli

import (
	"context"
	"encoding/json"
	"regexp"
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
	for _, hidden := range []string{"Reference:", "Source revision", "raw bytes", "[selected"} {
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
	shown := lastScreenWith(t, screens, "Reference: message")
	joined := strings.ReplaceAll(shown, "\n", "")
	for _, want := range []string{"Session: codex / outside-session", "Source revision: active source", "Reference: message 3 · raw bytes 0–39 · source " + ref.Source.Reference.SHA256, "Passage bytes 0–39"} {
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
	lines, cut := s.passageLines(hit, st, out, 80, passageView{})
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
	lines, cut = s.passageLines(hit, st, out, 80, passageView{expanded: true})
	if cut || strings.Count(strings.Join(lines, "\n"), "one more line of the reply") != 10 {
		t.Fatalf("m did not show the neighbor whole: %q", lines)
	}

	// A title match shows the title, and how to reach the body's matches.
	title := &searchMatchState{ordinal: 1, text: "Sublinear reads"}
	lines, _ = s.passageLines(hit, title, search.Surrounding{}, 80, passageView{})
	plain = ansiEscape.ReplaceAllString(strings.Join(lines, "\n"), "")
	if !strings.Contains(plain, "  title › Sublinear reads") || !strings.Contains(plain, "n shows the session's first matching message") {
		t.Fatalf("title match:\n%s", plain)
	}

	// An unreadable exact source shows no stand-in text.
	lines, _ = s.passageLines(hit, st, search.Surrounding{}, 80, passageView{})
	if len(lines) != 2 {
		t.Fatalf("unread passage shows content: %q", lines)
	}
}

func TestHighlightedLinesKeepLineBreaksAndSafeText(t *testing.T) {
	style := textStyle{color: true}
	lines := highlightedLines("visible", "first visible\n\n  indented \x1b[2J visible\u202e", 80, style)
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
	lines, _ := s.passageLines(hit, st, out, 24, passageView{})
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
	lines, _ := s.passageLines(hit, st, out, 80, passageView{details: true})
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
