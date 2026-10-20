package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/search"
)

// searchTranscript is a synthetic Codex session of four messages, three of
// which match "read performance".
const searchTranscript = `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"The list and show commands feel slow on the big bucket. Can you check the read performance of our archive commands?"}]}}
{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"I'll trace list, show and handoff against the bucket first, then write the proposal."}}
{"type":"response_item","id":"m3","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Also note the handoff READ-performance."}]}}
{"type":"response_item","id":"m4","payload":{"type":"message","role":"assistant","content":"Done: read performance notes are in dev/bench."}}`

func writeSearchTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "codex.jsonl")
	must(t, os.WriteFile(path, []byte(searchTranscript), 0o600))
	return path
}

// searchProjectsFixture indexes the search transcript as two sessions: one in
// project inside (repository inside-repo), titled so its title matches too,
// and an older one in project outside. Commands run from a directory of
// inside, so search offers Tab to narrow to it.
func searchProjectsFixture(t *testing.T) Env {
	t.Helper()
	env, objects, originalID := publishedFixtureWith(t, writeSearchTranscript)
	_, seed, err := reader.RefreshAndLoad(t.Context(), objects, "sessions/codex/"+originalID+"/metadata.json", reader.Limits{})
	must(t, err)
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.Open(t.Context(), dir, namespace)
	must(t, err)
	sessions := []archive.Metadata{
		{SessionID: "inside-session", ProjectID: "inside", ProjectName: "inside", RepoKey: "inside-repo", Name: "Read performance plan"},
		{SessionID: "outside-session", ProjectID: "outside", ProjectName: "outside", RepoKey: "outside-repo", Name: "Picker slowness"},
	}
	for i, m := range sessions {
		m.CapturedAt = env.now().Add(-time.Duration(i+1) * time.Hour)
		bundle := seed
		bundle.ArchiveSessionID = m.SessionID
		bundle.ProjectID = m.ProjectID
		bundle.Capture.CapturedAt = m.CapturedAt
		packed, e := archive.BuildCompressedSource(bundle)
		must(t, e)
		key, e := archive.SourceObjectKey(bundle, packed.SHA256)
		must(t, e)
		must(t, objects.Put(t.Context(), key, packed.Bytes))
		parser, ok := parsersFor(env).LookupParser("codex")
		if !ok {
			t.Fatal("missing parser")
		}
		analysis, parseErr := agentapi.Analyze(t.Context(), parser, bundle)
		verified, e := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, "synthetic", m.CapturedAt, m.CapturedAt, archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{Version: parser.Version()})
		must(t, e)
		verified.Name, verified.ProjectName, verified.RepoKey = m.Name, m.ProjectName, m.RepoKey
		must(t, index.Observe(t.Context(), verified, parsersFor(env), env.now(), 0))
		progress, e := index.Run(t.Context(), objects, parsersFor(env), env.now(), search.Budget{})
		if e != nil || progress.Committed != 1 {
			t.Fatalf("%+v %v", progress, e)
		}
	}
	must(t, index.Close())
	work := filepath.Join(t.TempDir(), "inside")
	must(t, os.Mkdir(work, 0o700))
	env.WorkingDir = func() (string, error) { return work, nil }
	env.repoKey = func(string) string { return "inside-repo" }
	return env
}

// searchScreens runs search on a terminal read by keys, 100 by 40, and
// returns each screen it drew.
func searchScreens(t *testing.T, env Env, keys *fakeKeys, input string, args ...string) []string {
	t.Helper()
	out, errOut, code := runOnTerminal(t, env, keys, input, append([]string{"search"}, args...)...)
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	screens := strings.Split(out, clearScreenSequence)
	checkKeyScreensFit(t, screens, 100, 40)
	return screens
}

// lastScreenWith is the last screen that contains want.
func lastScreenWith(t *testing.T, screens []string, want string) string {
	t.Helper()
	for i := len(screens) - 1; i >= 0; i-- {
		if strings.Contains(screens[i], want) {
			return screens[i]
		}
	}
	t.Fatalf("no screen has %q:\n%s", want, strings.Join(screens, "\n----\n"))
	return ""
}

func TestSearchResultsReadLikeTheSessionList(t *testing.T) {
	env := searchProjectsFixture(t)
	screens := searchScreens(t, env, newFakeKeys("q"), "", "read performance")
	first := lastScreenWith(t, screens, "best match first")
	lines := strings.Split(first, "\n")
	if lines[0] != `All projects · "read performance" · 2 sessions, best match first · Tab inside only` || lines[1] != "Indexed 2 of 2 · coverage incomplete" || lines[2] != "" {
		t.Fatalf("heading:\n%s", first)
	}
	rows := regexp.MustCompile(`(?m)^▸ 1  ●  Read performance plan +inside +\S+\n  2  ●  Picker slowness +outside +\S+$`)
	if !rows.MatchString(first) {
		t.Fatalf("rows are not list rows ranked by relevance:\n%s", first)
	}
	for _, want := range []string{
		strings.Repeat("─", 100),
		"title › match 1 of 4 in this session\nRead performance plan\n● Codex · inside · Jan 1, 23:00 · id inside-s\n",
		"↑↓ move   Enter read   n/p match   / new search   Tab scope   h hand off   q quit",
		"1–2 of 2\n",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("missing %q:\n%s", want, first)
		}
	}
	for _, gone := range []string{"Content query", "Scope:", "locally known", "Ranked content matches"} {
		if strings.Contains(first, gone) {
			t.Errorf("old layout %q:\n%s", gone, first)
		}
	}
}

func TestSearchResultsStepThroughTheHighlightedSessionsMatches(t *testing.T) {
	env := searchProjectsFixture(t)
	screens := searchScreens(t, env, newFakeKeys("\x1b[B", "n", "n", "n", "p", "\x1b[A", "n", "p", "q"), "", "read performance")
	want := []string{
		"you › match 1 of 3 in this session\nAlso note the handoff READ-performance.\n",
		"codex › match 2 of 3 in this session\nDone: read performance notes are in dev/bench.\n",
		"you › match 3 of 3 in this session\nThe list and show commands feel slow on the big bucket. Can you check the read performance of our\narchive commands?\n",
		"no further matching body passage",
		"codex › match 2 of 3 in this session",
		// The title match is the first of its session's; n goes on into the
		// body and p comes back.
		"title › match 1 of 4 in this session",
		"› match 2 of 4 in this session",
		"title › match 1 of 4 in this session",
	}
	at := 0
	for _, screen := range screens {
		if at < len(want) && strings.Contains(screen, want[at]) {
			at++
		}
	}
	if at != len(want) {
		t.Fatalf("strip did not show %q in order:\n%s", want[at], strings.Join(screens, "\n----\n"))
	}
}

func TestSearchTabSwitchesScopeAndRerunsTheQuery(t *testing.T) {
	env := searchProjectsFixture(t)
	screens := searchScreens(t, env, newFakeKeys("\t", "\t", "q"), "", "read performance")
	inside := lastScreenWith(t, screens, `inside · "read performance" · 1 session, best match first · Tab all projects`)
	if strings.Contains(inside, "Picker slowness") || !strings.Contains(inside, "Read performance plan") {
		t.Fatalf("Tab did not narrow to the repository:\n%s", inside)
	}
	last := screens[len(screens)-1]
	if !strings.HasPrefix(last, `All projects · "read performance" · 2 sessions`) || !strings.Contains(last, "Picker slowness") {
		t.Fatalf("second Tab did not widen again:\n%s", last)
	}

	// Read by lines, a switches scope.
	out, errOut, code := runOnTerminal(t, env, nil, "a\nq\n", "search", "read performance")
	if code != 0 || !strings.Contains(out, `inside · "read performance" · 1 session, best match first · a all projects`) {
		t.Fatalf("a did not narrow: %d %s %s", code, errOut, out)
	}
}

func TestSearchOutsideAProjectOffersNoScopeKey(t *testing.T) {
	env := searchProjectsFixture(t)
	env.repoKey = func(string) string { return "" }
	screens := searchScreens(t, env, newFakeKeys("\t", "q"), "", "read performance")
	for _, screen := range screens {
		if strings.Contains(screen, "Tab") || strings.Contains(screen, "Searching…") {
			t.Fatalf("scope offered with no project to narrow to:\n%s", screen)
		}
	}
}

func TestSearchSlashAsksForANewQueryInPlace(t *testing.T) {
	env := searchProjectsFixture(t)
	screens := searchScreens(t, env, newFakeKeys("\t", "/", "q"), "handoff\n", "read performance")
	asked := lastScreenWith(t, screens, "Search for:")
	if !strings.Contains(asked, "Searching project inside.") {
		t.Fatalf("new query not asked in the scope shown:\n%s", asked)
	}
	last := screens[len(screens)-1]
	if !strings.HasPrefix(last, `inside · "handoff" · 1 session, best match first`) || !strings.Contains(last, "you › match 1 of 2 in this session\nAlso note the handoff READ-performance.") {
		t.Fatalf("new query did not run in place:\n%s", last)
	}

	// Ctrl-D at the question keeps the results it would have replaced.
	screens = searchScreens(t, env, newFakeKeys("/", "q"), "", "read performance")
	if last := screens[len(screens)-1]; !strings.HasPrefix(last, `All projects · "read performance"`) {
		t.Fatalf("ended question lost the results:\n%s", last)
	}
}

func TestSearchResultsByLinesShowEachPassageUnderItsRow(t *testing.T) {
	env := searchProjectsFixture(t)
	out, errOut, code := runOnTerminal(t, env, nil, "q\n", "search", "read performance")
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	for _, want := range []string{
		`All projects · "read performance" · 2 sessions, best match first · a inside only`,
		"   title › Read performance plan\n",
		"   you › Also note the handoff READ-performance.\n",
		", / new search, a scope, o next results, r rerun, or q to quit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestSearchHandOffFromTheResultsList(t *testing.T) {
	env, _, id := prepareSearchBrowser(t)
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("h"), "p\n", "search", "visible")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "source: archive") {
		t.Fatalf("h did not hand off the highlighted session: %d %s %s", code, errOut, out)
	}
}

func TestSearchStripHighlightsEveryMatchedTerm(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	hit := search.Hit{SessionID: "0a13656b25d0", Harness: "claude", Title: "Sublinear reads", Role: "user", Project: "agent-archive", MatchCount: 3, CapturedAt: time.Date(2026, 10, 16, 9, 12, 0, 0, time.UTC),
		Passage: "Can you check whether there are better ways to improve the READ-performance for our agent archive commands?"}
	for _, color := range []bool{false, true} {
		style := textStyle{color: color}
		s := &contentSearchBrowser{opts: contentSearchOptions{query: search.Query{Text: "read perf*"}}, hits: []search.Hit{hit}, pages: 1, style: style, now: now, choices: rowChoices(nil, listFormatOptions{})}
		s.installRows()
		strip := s.strip(s.choices.shown().rows[0], 60)
		if len(strip) != searchStripLines {
			t.Fatalf("strip of %d lines", len(strip))
		}
		text := strings.Join(strip, "\n")
		plain := ansiEscape.ReplaceAllString(text, "")
		for _, want := range []string{
			"you › match 1 of 3 in this session\nCan you check whether there are better ways to improve the\nREAD-performance for our agent archive commands?\n● Claude Code · agent-archive · Oct 16, 09:12 · id 0a13656b",
		} {
			if !strings.Contains(plain, want) {
				t.Errorf("color %v: missing %q:\n%s", color, want, plain)
			}
		}
		if !color {
			if text != plain {
				t.Errorf("styled without color: %q", text)
			}
			continue
		}
		for _, want := range []string{style.match("READ") + "-" + style.match("performance"), style.dim("you › match 1 of 3 in this session"), style.agent("claude", agentDot)} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q in %q", want, text)
			}
		}
		if strings.Contains(text, style.match("improve")) {
			t.Errorf("unmatched word highlighted: %q", text)
		}
	}
}

func TestSearchPassageWindowShowsTheFirstMatch(t *testing.T) {
	text := strings.Repeat("filler words go here ", 20) + "the needle is here " + strings.Repeat("and more words after ", 20)
	lines := passageWindow("needle", text, 30, 3, textStyle{})
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "… ") || !strings.HasSuffix(lines[2], " …") {
		t.Fatalf("window not cut around the match: %q", lines)
	}
	if !strings.Contains(strings.Join(lines, " "), "needle") {
		t.Fatalf("match outside the window: %q", lines)
	}
	for _, line := range lines {
		if visibleWidth(line) > 30 {
			t.Fatalf("line wider than 30: %q", line)
		}
	}
	short := passageWindow("needle", "a needle", 30, 3, textStyle{})
	if len(short) != 1 || short[0] != "a needle" {
		t.Fatalf("short passage changed: %q", short)
	}
	if long := passageWindow("x", strings.Repeat("界", 40), 10, 3, textStyle{}); len(long) != 3 || visibleWidth(long[0]) > 10 {
		t.Fatalf("long word not split to the width: %q", long)
	}
}
