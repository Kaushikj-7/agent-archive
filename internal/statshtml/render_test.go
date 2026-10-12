package statshtml

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

func render(t *testing.T, s stats.Stats, opts Options) []byte {
	t.Helper()
	if opts.GeneratedAt.IsZero() {
		opts.GeneratedAt = fixtureNow
	}
	out, err := Render(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, out)
	return out
}

func goldenPath(name string) string { return filepath.Join("testdata", name+".html.golden") }

// The page is pinned for a multi-agent archive with several models,
// subagents, Cursor sessions without tokens and an unpriced model: by default
// (project names replaced) with a weekly breakdown, and with real names and a
// breakdown by project. A change to the page shows up as a golden diff to
// review.
func TestPageGoldens(t *testing.T) {
	t.Parallel()
	sessions := fixtureSessions()
	golden.Check(t, goldenPath("shareable"), render(t, computeFixture(t, sessions, 30, stats.GroupWeek), Options{}))
	golden.Check(t, goldenPath("real-names"), render(t, computeFixture(t, sessions, 30, stats.GroupProject),
		Options{IncludeNames: true, Filters: Filters{Harness: "claude", Origin: "hook"}}))
	golden.Check(t, goldenPath("empty"), render(t, computeFixture(t, nil, 30, stats.GroupNone), Options{}))
	golden.Check(t, goldenPath("realistic"), render(t, modelStats(t, realisticSessions(), realisticPrices, stats.GroupNone), Options{}))
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	sessions := fixtureSessions()
	first := render(t, computeFixture(t, sessions, 30, stats.GroupDay), Options{})
	for range 3 {
		if again := render(t, computeFixture(t, sessions, 30, stats.GroupDay), Options{}); !bytes.Equal(first, again) {
			t.Fatal("two renders of the same stats differ")
		}
	}
}

// The default page carries no project name, and the page as a whole carries
// no session ID, path or prompt-shaped text: aggregates only.
func TestShareablePageHasNoProjectNamesOrIDs(t *testing.T) {
	t.Parallel()
	sessions := fixtureSessions()
	out := string(render(t, computeFixture(t, sessions, 30, stats.GroupProject), Options{}))
	for _, secret := range []string{"agent-archive", "proj-api", "dotfiles"} {
		// The page names the tool itself; the project must not be named.
		text := strings.ReplaceAll(out, "agent-archive stats", "")
		text = strings.ReplaceAll(text, "agent-archive --", "")
		if strings.Contains(text, secret) {
			t.Errorf("the shareable page names the project %q", secret)
		}
	}
	for _, id := range []string{"claude-big", "claude-00", "codex-00", "cursor-00", "sessions/", "source.jsonl"} {
		if strings.Contains(out, id) {
			t.Errorf("the page contains %q, which looks like a session ID or path", id)
		}
	}
	if !strings.Contains(out, "project A") || !strings.Contains(out, "project B") {
		t.Error("the page has no stand-in labels")
	}
	if !strings.Contains(out, "--include-names") {
		t.Error("the page does not say how to show project names")
	}
}

// One project has one label throughout the file: its row in "by project", the
// heads-up about the costliest session, the highlight and the breakdown by
// project agree, and the labels follow the order the page names the projects
// in, dearest first.
func TestProjectLabelsAreConsistentAcrossSections(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, fixtureSessions(), 30, stats.GroupProject)
	out := string(render(t, s, Options{}))
	dearest := ""
	best := -1.0
	for _, p := range s.Projects {
		if p.Cost.USD != nil && *p.Cost.USD > best {
			dearest, best = p.Name, *p.Cost.USD
		}
	}
	// The first project the page names is the dearest, and is project A.
	names := newNamer(false, "project")
	if got := names.project(dearest); got != "project A" {
		t.Fatalf("the dearest project is %q, want project A", got)
	}
	_, table, found := strings.Cut(out, `id="h-projects"`)
	first, second := strings.Index(table, "project A"), strings.Index(table, "project B")
	if !found || first < 0 || second < first {
		t.Error("the dearest project is not the first row of the by-project table")
	}
	costliest := s.Highlights.CostliestSession.Project
	label := names.project(costliest)
	if !strings.Contains(out, " · "+label+" · long context") && !strings.Contains(out, "· "+label+" (long context") {
		t.Errorf("the costliest session's project %q is not labelled %q in the heads-up and the highlights", costliest, label)
	}
	// Every project of the breakdown has a label, and labels are not reused.
	seen := map[string]string{}
	for _, row := range s.Groups.Rows {
		l := names.project(row.Key)
		if other, dup := seen[l]; dup && other != row.Key {
			t.Errorf("label %q names both %q and %q", l, other, row.Key)
		}
		seen[l] = row.Key
	}
	if strings.Count(out, "project A") < 4 {
		t.Errorf("project A appears %d times, want it in by project, heads-up, highlights and the breakdown", strings.Count(out, "project A"))
	}
}

func TestLettersCountLikeSpreadsheetColumns(t *testing.T) {
	t.Parallel()
	for i, want := range map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"} {
		if got := letters(i); got != want {
			t.Errorf("letters(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestIncludeNamesShowsThem(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupProject), Options{IncludeNames: true}))
	for _, name := range []string{"agent-archive", "proj-api", "dotfiles"} {
		if !strings.Contains(out, name) {
			t.Errorf("the page with real names does not show %q", name)
		}
	}
	if strings.Contains(out, "project A") || strings.Contains(out, "replaced by letters") {
		t.Error("the page with real names still talks about stand-ins")
	}
}

// Every name that reaches the page from an archive is escaped or cleaned: a
// project, model, skill or MCP server name built to break out of its element
// or its attribute, to run script, to hide text with a right-to-left override,
// or to end a line, comes out as inert text.
func TestHostileNamesAreInertText(t *testing.T) {
	t.Parallel()
	for i, payload := range hostilePayloads {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			s := computeFixture(t, hostileSessions(payload), 30, stats.GroupProject)
			for _, reveal := range []bool{true, false} {
				out := render(t, s, Options{
					IncludeNames: reveal,
					Filters:      Filters{Harness: payload, Model: payload, Origin: payload},
				})
				assertInert(t, string(out), payload)
			}
		})
	}
}

// hostileSessions are sessions whose project, model, skill and MCP names are
// the payload.
func hostileSessions(payload string) []archive.Metadata {
	spec := sessionSpec{
		id: "hostile", harness: "claude", project: payload, captured: day(time.September, 20, 10),
		models: []string{payload}, turns: 5, messages: 20, toolResults: 10, errors: 1,
		tokens: []tokenSpec{{payload, 1_000, 2_000, 30_000, 4_000}},
		skills: []string{payload}, mcp: map[string]int{payload: 3},
	}
	other := spec
	other.id, other.project = "hostile-2", payload+" two"
	return []archive.Metadata{spec.build(), other.build()}
}

// assertInert fails when what a payload would do, if it reached the page as
// markup, is in it: the payload's dangerous fragments raw, or a character a
// name may not carry.
func assertInert(t *testing.T, page, payload string) {
	t.Helper()
	for _, raw := range []string{
		"</script><script>", "<img src=x", "<svg onload", "onerror=alert(1)>", "</style><style>", "</title></head>",
		"\xe2\x80\xa8", "\xe2\x80\xa9", "\xe2\x80\xae", "\xe2\x80\xac", "\x1b", "\x00", "\x07", "\x7f", "\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd",
	} {
		if strings.Contains(page, raw) {
			t.Errorf("payload %q: the page contains %q", payload, raw)
		}
	}
	if strings.Contains(payload, "<") && !strings.Contains(page, "&lt;") {
		t.Errorf("payload %q: the page has no escaped < at all, so the name was dropped or not escaped", payload)
	}
}

// A name the page shows is shortened, so a name of any length cannot stretch
// the page.
func TestLongNamesAreShortened(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 5000)
	out := string(render(t, computeFixture(t, hostileSessions(long), 30, stats.GroupNone), Options{IncludeNames: true}))
	if strings.Contains(out, strings.Repeat("x", nameLimit+1)) {
		t.Error("a 5,000 character name is not shortened")
	}
	if !strings.Contains(out, strings.Repeat("x", nameLimit-1)+"…") {
		t.Error("a shortened name does not end in an ellipsis")
	}
}

func TestPageIsSelfContainedInBothThemes(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupWeek), Options{}))
	for _, want := range []string{
		`name="viewport"`, `prefers-color-scheme: dark`, `@media print`, `forced-colors: active`, `max-width: 559px`,
		`Content-Security-Policy`, `default-src &#39;none&#39;`, `lang="en"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(out, "script-src") || strings.Contains(out, "connect-src") {
		t.Error("the policy opens something it should not")
	}
}

// The chart names its peak, every bar answers a hover, and a day with sessions
// but no token counts is marked, not drawn as zero.
func TestDailyChartLabelsThePeakAndAnswersHover(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	s := computeFixture(t, fixtureSessions(), 30, stats.GroupNone)
	if s.PeakSpend == nil || s.PeakSpend.Date != "2026-09-17" {
		t.Fatalf("the fixture's dearest day is %+v, want Sep 17", s.PeakSpend)
	}
	if !strings.Contains(out, "Peak $66 · Sep 17") {
		t.Error("the chart does not label its peak spend")
	}
	if got := strings.Count(out, `<g class="slot"><title>`); got != 30 {
		t.Errorf("%d hover titles, want one per day (30)", got)
	}
	for _, want := range []string{
		"<title>Sep 17 · $66 · 2 sessions</title>",
		"<title>Sep 3 · no sessions</title>",
		"<title>Sep 15 · 1 session · cost unknown</title>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the chart lacks the hover text %q", want)
		}
	}
	if !strings.Contains(out, `class="bar unknown"`) || !strings.Contains(out, `class="bar day peak"`) {
		t.Error("the chart does not mark an unknown day and the peak")
	}
	if !strings.Contains(out, `role="img" aria-label="Spend by day, Aug 31 to Sep 29. Peak $66 on Sep 17."`) {
		t.Error("the chart has no text alternative naming the peak")
	}
	if strings.Contains(out, "Tokens by day") {
		t.Error("the page still charts tokens by day")
	}
	// The peak bar is the tallest of the plot, and the table view carries the
	// spend of every day: sessions, spend, tokens.
	if !strings.Contains(out, `<tr><th scope="row">Sep 17</th><td class="num">2</td><td class="num">$66</td><td class="num">29M</td></tr>`) {
		t.Error("the chart's table does not give Sep 17's sessions, spend and tokens")
	}
	if !strings.Contains(out, `<tr><th scope="row">Sep 15</th><td class="num">1</td><td class="num">n/a</td><td class="num">unknown</td></tr>`) {
		t.Error("the chart's table does not show an unpriced day as n/a, not $0")
	}
	if !strings.Contains(out, `<tr><th scope="row">Sep 3</th><td class="num">0</td><td class="num">0</td><td class="num">0</td></tr>`) {
		t.Error("the chart's table does not show a day without sessions as 0")
	}
	if !strings.Contains(out, "<summary>Show as a table</summary>") {
		t.Error("the chart has no table view")
	}
}

// A long window draws at most maxBars bars, each the busiest day of its run,
// and the page stays small however large the archive.
func TestLargeArchiveStaysWithinItsBudget(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	start := fixtureNow.AddDate(-9, 0, 0)
	for i := range 6000 {
		sessions = append(sessions, sessionSpec{
			id: fmt.Sprintf("s%05d", i), harness: []string{"claude", "codex", "cursor"}[i%3],
			project:  fmt.Sprintf("project-%d-%s", i%700, strings.Repeat("n", i%50)),
			captured: start.Add(time.Duration(i) * 13 * time.Hour), models: []string{"claude-opus-5", fmt.Sprintf("model-%d", i%40)},
			turns: 5, messages: 30, toolResults: 20, errors: 1, skills: []string{fmt.Sprintf("skill-%d", i%60)},
			mcp: map[string]int{fmt.Sprintf("server-%d", i%30): 4},
			tokens: []tokenSpec{
				{fmt.Sprintf("model-%d", i%40), 10_000 + i, 5_000, 200_000, 20_000},
				{"claude-opus-5", 1_000 + i, 500, 20_000, 2_000},
			},
		}.build())
	}
	for _, by := range []stats.Grouping{stats.GroupNone, stats.GroupDay, stats.GroupProject} {
		s := computeFixture(t, sessions, stats.MaxDays, by)
		out := render(t, s, Options{IncludeNames: true})
		if len(out) > 300_000 {
			t.Errorf("by %q: the page is %d bytes, over the 300,000 budget", by, len(out))
		}
		if bars := strings.Count(string(out), `<g class="slot">`); bars > maxBars {
			t.Errorf("by %q: %d bars, over %d", by, bars, maxBars)
		}
		per := (len(s.ChartDays()) + maxBars - 1) / maxBars
		if !strings.Contains(string(out), fmt.Sprintf("Each bar is %d days and shows its busiest day.", per)) {
			t.Errorf("by %q: the chart does not say what a bar is", by)
		}
	}
}

// Cursor records no tokens: its tokens and cost read unknown and n/a, never 0,
// and a window with no token data says so instead of drawing an empty chart.
func TestUnknownTokensAreNeverZero(t *testing.T) {
	t.Parallel()
	cursor := sessionSpec{id: "c1", harness: "cursor", project: "p", captured: day(time.September, 20, 10), models: []string{"cursor-auto"}, turns: 3}
	s := computeFixture(t, []archive.Metadata{cursor.build()}, 30, stats.GroupNone)
	out := string(render(t, s, Options{}))
	for _, want := range []string{"unknown", "n/a", "No session in this window reports token counts"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Cursor-only page lacks %q", want)
		}
	}
	for _, bad := range []string{"Spend by day, ", "What used your tokens", ">0 tokens", "$0"} {
		if strings.Contains(out, bad) {
			t.Errorf("the Cursor-only page contains %q", bad)
		}
	}
	mixed := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	if !strings.Contains(mixed, `<td class="num">3 (14%)</td><td class="num">unknown</td><td class="num">n/a</td><td class="num">unknown</td>`) {
		t.Error("the agents table does not show Cursor as unknown tokens and n/a cost")
	}
}

func TestEmptyWindowGetsAHelpfulPage(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, nil, 30, stats.GroupNone)
	out := string(render(t, s, Options{}))
	for _, want := range []string{"Nothing to show yet", "Try a longer window", "This page holds counts and names only"} {
		if !strings.Contains(out, want) {
			t.Errorf("the empty page lacks %q", want)
		}
	}
	custom := string(render(t, s, Options{EmptyMessage: "No archived sessions match <these> filters."}))
	if !strings.Contains(custom, "No archived sessions match &lt;these&gt; filters.") {
		t.Error("the caller's empty message is not shown, escaped")
	}
	if strings.Contains(out, "Daily spend") || strings.Contains(out, `class="stat"`) || strings.Contains(out, "Details") {
		t.Error("the empty page draws sections without data")
	}
}

// The composition donut's segments add up to the whole ring, a segment with
// no tokens is not drawn, and every segment is in the legend with its share.
func TestDonutSegmentsAddUp(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, fixtureSessions(), 30, stats.GroupNone)
	out := string(render(t, s, Options{}))
	circ := regexp.MustCompile(`stroke-dasharray="([0-9.]+) ([0-9.]+)"`)
	matches := circ.FindAllStringSubmatch(out, -1)
	if len(matches) != 4 {
		t.Fatalf("%d ring segments, want 4", len(matches))
	}
	for _, m := range matches {
		var a, b float64
		if _, err := fmt.Sscanf(m[1]+" "+m[2], "%g %g", &a, &b); err != nil {
			t.Fatal(err)
		}
		if total := a + b; total < 427.2 || total > 427.3 {
			t.Errorf("segment %s + %s = %g, want the circumference 427.26", m[1], m[2], total)
		}
	}
	for _, label := range []string{"Cache read", "Cache write", "Input", "Output"} {
		if !strings.Contains(out, ">"+label+"</th>") && !strings.Contains(out, "</svg>"+label+"</th>") {
			t.Errorf("the legend lacks %q", label)
		}
	}
	c := s.Composition
	// A composition of one type is a whole ring, without a gap.
	only := *c
	only.CacheRead = stats.Segment{Tokens: c.Total, Share: 1}
	only.CacheWrite, only.FreshInput, only.Output = stats.Segment{}, stats.Segment{}, stats.Segment{}
	s.Composition = &only
	single := string(render(t, s, Options{}))
	if got := strings.Count(single, "stroke-dasharray="); got != 1 {
		t.Errorf("%d ring segments for a single-type composition, want 1", got)
	}
	if !strings.Contains(single, `stroke-dasharray="427.26 0"`) {
		t.Error("a single-type composition is not a whole ring")
	}
}

// The renderer is a pure function of the stats: it reads no clock, file,
// environment or network, so it can never fetch or write anything itself.
// Its archive dependency transitively reaches pure agent identity metadata.
func TestRendererImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/statshtml")
	importgraph.Forbid(t, "internal/statshtml", direct,
		"os", "os/exec", "io/ioutil", "io/fs", "path/filepath", "net", "net/http", "math/rand", "math/rand/v2",
		"golang.org/x/term", "text/template", "unsafe",
		"github.com/wangjohn/agent-archive/internal/agentmeta", "github.com/wangjohn/agent-archive/internal/jsonwire", "github.com/wangjohn/agent-archive/internal/codexmeta")
	importgraph.Forbid(t, "internal/statshtml (transitively)", all, "net/http", "os/exec")
	for _, path := range all {
		if strings.HasPrefix(path, "github.com/wangjohn/agent-archive/internal/") &&
			path != "github.com/wangjohn/agent-archive/internal/archive" &&
			path != "github.com/wangjohn/agent-archive/internal/stats" &&
			path != "github.com/wangjohn/agent-archive/internal/agentmeta" &&
			path != "github.com/wangjohn/agent-archive/internal/codexmeta" &&
			path != "github.com/wangjohn/agent-archive/internal/statsfmt" {
			t.Errorf("internal/statshtml reaches %s; only internal/stats, internal/statsfmt, internal/archive and its pure agentmeta/codexmeta dependencies are allowed", path)
		}
	}
}

// A value typed as trusted markup skips the template's escaping and would let
// a name through as markup, so nothing in the renderer is: text is strings.
// The stylesheet is the template's own text, and holds no template action.
func TestNothingIsMarkedTrusted(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"model.go", "render.go", "charts.go", "format.go"} {
		src := readSource(t, file)
		for _, unsafe := range []string{"template.HTML", "template.JS", "template.URL", "template.HTMLAttr", "template.JSStr", "template.Srcset", "template.CSS"} {
			if strings.Contains(src, unsafe) {
				t.Errorf("%s uses %s: a value marked trusted skips escaping", file, unsafe)
			}
		}
	}
	if strings.Contains(pageTemplateSource, "safeHTML") || strings.Contains(pageTemplateSource, "{{define \"script") {
		t.Error("the template opts out of escaping")
	}
	if strings.Contains(styleSheet, "{{") {
		t.Error("the stylesheet holds a template action")
	}
}
