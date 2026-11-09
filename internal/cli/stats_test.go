package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// statsTerminal is an output that says it is a terminal of a given width,
// which is how the stats tests see the screen at 80 and 120 columns.
type statsTerminal struct {
	bytes.Buffer
	width int
	color bool
}

func (s *statsTerminal) colorTerminal() bool { return s.color }

func (s *statsTerminal) terminalWidth() int { return s.width }

// runStats runs `stats` with args against env, on a terminal of the width
// (0 for output that is not a terminal), and returns what it printed.
func runStats(t *testing.T, env Env, width int, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	out := &statsTerminal{width: width}
	var errOut bytes.Buffer
	code = Run(append([]string{"stats"}, args...), nil, out, &errOut, env)
	return out.String(), errOut.String(), code
}

// mustRunStats is runStats for a run that must succeed without a word on
// stderr.
func mustRunStats(t *testing.T, env Env, width int, args ...string) string {
	t.Helper()
	out, errOut, code := runStats(t, env, width, args...)
	if code != 0 || errOut != "" {
		t.Fatalf("stats %v: code=%d stderr=%q\n%s", args, code, errOut, out)
	}
	return out
}

// goldenPrices pins the prices the screen goldens are costed with, so a change
// to the built-in table does not rewrite them.
const goldenPrices = "testdata/stats/prices.json"

func statsGolden(name string) string { return filepath.Join("testdata", "stats", name+".golden") }

// The screens are pinned end to end at the widths they have layouts for: a
// terminal of 60 columns (stacked lists), the 80 the overview is designed for
// (two columns) and one of 120, and output that is not a terminal, all from
// one multi-agent archive with several models, subagents, Cursor sessions
// without tokens and an unpriced model. The page tests pin every view at
// every width, with and without color, from hand-built numbers.
func TestStatsScreenGoldens(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, tc := range []struct {
		name  string
		width int
		args  []string
	}{
		{"width-60", 60, nil},
		{"width-80", 80, nil},
		{"width-120", 120, nil},
		{"not-a-terminal", 0, nil},
		{"detail-80", 80, []string{"--detail"}},
		{"by-week-80", 80, []string{"--by", "week"}},
		{"projects-80", 80, []string{"--view", "projects"}},
		{"models-80", 80, []string{"--view", "models"}},
		{"agents-80", 80, []string{"--view", "agents"}},
		{"by-project-80", 80, []string{"--by", "project", "--days", "14"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := mustRunStats(t, env, tc.width, append([]string{"--prices", goldenPrices}, tc.args...)...)
			golden.Check(t, statsGolden(tc.name), []byte(out))
			limit := tc.width
			if tc.width == 0 {
				limit = statsUnknownWidth
			}
			for line := range strings.SplitSeq(out, "\n") {
				if w := visibleWidth(line); w > limit {
					t.Errorf("line is %d columns, over %d: %q", w, limit, line)
				}
			}
		})
	}
}

// The overview is two columns from 80 terminal columns, stacked from 60 with
// bars, and plain rows below that.
func TestStatsLayoutSwitchesAtEightyColumns(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, tc := range []struct {
		width      int
		bars       bool
		twoColumns bool
	}{
		{40, false, false}, {50, false, false}, {59, false, false}, {60, true, false}, {79, true, false},
		{80, true, true}, {100, true, true}, {120, true, true}, {0, true, true},
	} {
		out := mustRunStats(t, env, tc.width)
		sideBySide, bars := false, false
		for line := range strings.SplitSeq(out, "\n") {
			sideBySide = sideBySide || (strings.Contains(line, "By project") && strings.Contains(line, "By model"))
			// The project rows are the ones led by the project's name.
			bars = bars || (strings.HasPrefix(line, "proj-api") && strings.Contains(line, "█"))
		}
		if bars != tc.bars {
			t.Errorf("width %d: bars=%v, want %v\n%s", tc.width, bars, tc.bars, out)
		}
		if sideBySide != tc.twoColumns {
			t.Errorf("width %d: two columns=%v, want %v\n%s", tc.width, sideBySide, tc.twoColumns, out)
		}
	}
}

// A terminal too narrow for the layout is still readable: no line runs past
// its width, down to the narrowest the screen adapts to.
func TestStatsNarrowTerminalNeverOverflows(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, width := range []int{statsMinWidth, 60, 79, 80, 81, 90, 99, 100, 120, 200} {
		out := mustRunStats(t, env, width, "--by", "day")
		for line := range strings.SplitSeq(out, "\n") {
			if w := visibleWidth(line); w > width {
				t.Errorf("width %d: line is %d columns: %q", width, w, line)
			}
		}
	}
}

var ansiSequence = regexp.MustCompile("\x1b\\[[0-9;]*m")

// Color is for a terminal that has it: none otherwise, and with it the text
// is the same, so nothing depends on color to be understood.
func TestStatsColorOnlyOnAColorTerminalAndNeverChangesTheText(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	plain := mustRunStats(t, env, 120)
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("output that is not colored has escape codes:\n%q", plain)
	}
	out := &statsTerminal{width: 120, color: true}
	var errOut bytes.Buffer
	if code := Run([]string{"stats"}, nil, out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "\x1b[1m") || !strings.Contains(out.String(), "\x1b[2m") {
		t.Fatalf("a color terminal got no bold or dim:\n%q", out.String())
	}
	if got := ansiSequence.ReplaceAllString(out.String(), ""); got != plain {
		t.Fatalf("color changed the text:\n%s\nvs\n%s", got, plain)
	}
}

// A locale that is not UTF-8 gets ASCII bars, and a UTF-8 one (or none)
// Unicode blocks.
func TestStatsFallsBackToASCIIOutsideUTF8Locales(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, tc := range []struct {
		name  string
		vars  map[string]string
		ascii bool
	}{
		{"none set", nil, false},
		{"UTF-8", map[string]string{"LANG": "en_US.UTF-8"}, false},
		{"utf8 lower", map[string]string{"LC_ALL": "C.utf8"}, false},
		{"C", map[string]string{"LANG": "C"}, true},
		{"POSIX in LC_ALL beats UTF-8 LANG", map[string]string{"LC_ALL": "POSIX", "LANG": "en_US.UTF-8"}, true},
		{"dumb terminal", map[string]string{"TERM": "dumb"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := env
			e.LookupEnv = func(key string) (string, bool) { v, ok := tc.vars[key]; return v, ok }
			out := mustRunStats(t, e, 120, "--prices", goldenPrices)
			if nonASCII := regexp.MustCompile("[^\x00-\x7f]").FindString(out); (nonASCII != "") != !tc.ascii {
				t.Fatalf("ascii=%v but output has %q:\n%s", tc.ascii, nonASCII, out)
			}
			if tc.ascii {
				golden.Check(t, statsGolden("ascii"), []byte(out))
			}
		})
	}
}

func decodeStats(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	return doc
}

// --json is a versioned document: the engine's Stats at its top level, and
// nothing else. Its keys are pinned so a new field cannot leak unreviewed.
func TestStatsJSONIsAVersionedDocument(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	out := mustRunStats(t, env, 0, "--json", "--by", "project", "--harness", "claude")
	doc := decodeStats(t, out)
	var keys []string
	for k := range doc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{
		"agents", "composition", "coverage", "daily", "display_skills", "filters", "generated_at", "groups", "heads_up",
		"highlights", "mcp", "models", "overview", "peak", "peak_spend", "prices", "projects", "schema_version",
		"skills", "subagents", "total_display_skills", "total_projects", "total_skills", "window",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("top-level keys = %v\nwant %v", keys, want)
	}
	var version int
	if err := json.Unmarshal(doc["schema_version"], &version); err != nil || version != statsSchemaVersion {
		t.Fatalf("schema_version = %s", doc["schema_version"])
	}
	if compact := regexp.MustCompile(`\s+`).ReplaceAllString(string(doc["filters"]), ""); compact != `{"harness":"claude"}` {
		t.Fatalf("filters = %s", doc["filters"])
	}
	var generated time.Time
	if err := json.Unmarshal(doc["generated_at"], &generated); err != nil || !generated.Equal(statsNow) {
		t.Fatalf("generated_at = %s", doc["generated_at"])
	}
	var decoded statsDocument
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coverage.Sessions != 13 || decoded.Coverage.Agents != 1 || decoded.Groups == nil || decoded.Groups.By != stats.GroupProject {
		t.Fatalf("coverage=%+v groups=%+v", decoded.Coverage, decoded.Groups)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("JSON has a terminal escape")
	}
}

// Unknown is null in the JSON, never 0: Cursor's tokens.
func TestStatsJSONKeepsUnknownTokensNull(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	var cursor *stats.Agent
	for i := range doc.Agents {
		if doc.Agents[i].Harness == "cursor" {
			cursor = &doc.Agents[i]
		}
	}
	if cursor == nil || cursor.Tokens != nil || cursor.Cost.USD != nil || cursor.TokenShare != nil || cursor.UnknownTokenSessions != 3 {
		t.Fatalf("cursor agent = %+v", cursor)
	}
	if doc.Coverage.UnknownTokensByAgent["cursor"] != 3 {
		t.Fatalf("coverage = %+v", doc.Coverage)
	}
}

// An empty archive is still a document, with zero sessions.
func TestStatsJSONOfAnEmptyArchiveIsADocument(t *testing.T) {
	t.Parallel()
	env, _ := statsEnv(t)
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != statsSchemaVersion || doc.Coverage.Sessions != 0 || doc.Window.Days != 30 || doc.Daily == nil {
		t.Fatalf("doc = %+v", doc)
	}
}

// Flags are checked before anything is read: exit 2, one line on stderr,
// nothing on stdout (no JSON either).
func TestStatsRejectsBadFlags(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	dir := t.TempDir()
	notPrices := filepath.Join(dir, "not-prices.json")
	if err := os.WriteFile(notPrices, []byte(`{"version": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--days", "0"}, "--days must be from 1 to 3660"},
		{[]string{"--days", "3661"}, "--days must be from 1 to 3660"},
		{[]string{"--days", "abc"}, "invalid value"},
		{[]string{"--days", "7", "--since", "3d"}, "choose one of --days and --since"},
		{[]string{"--days", "30", "--since", "3d"}, "choose one of --days and --since"},
		{[]string{"--since", "last tuesday"}, "--since:"},
		{[]string{"--since", "2026-10-15"}, "--since is in the future"},
		{[]string{"--since", "2000-01-01"}, "--since reaches back"},
		{[]string{"--by", "hour"}, `--by must be day, week, month, or project, not "hour"`},
		{[]string{"--harness", "vim"}, "--harness must be claude, codex, cursor, muse, or agy"},
		{[]string{"--imported", "--hook-captured"}, "choose one of --imported and --hook-captured"},
		{[]string{"--prices", filepath.Join(dir, "missing.json")}, "--prices:"},
		{[]string{"--prices", notPrices}, "--prices:"},
		{[]string{"extra"}, `unexpected argument "extra"`},
		{[]string{"--pdf"}, "unknown flag --pdf"},
	} {
		out, errOut, code := runStats(t, env, 0, append([]string{"--json"}, tc.args...)...)
		if code != 2 || out != "" || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "run agent-archive stats --help") {
			t.Errorf("%v: code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", tc.args, code, out, errOut, tc.want)
		}
		if strings.Count(errOut, "\n") != 1 {
			t.Errorf("%v: stderr is not one line: %q", tc.args, errOut)
		}
	}
}

// --since names a local day and the window runs from it through today, so
// the JSON window says exactly what the flag asked for.
func TestStatsSinceAndDaysAgreeWithTheEngineWindow(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, tc := range []struct {
		args       []string
		days       int
		firstDay   string
		lastDay    string
		sessionsIn int
	}{
		{nil, 30, "2026-08-31", "2026-09-29", 22},
		{[]string{"--days", "7"}, 7, "2026-09-23", "2026-09-29", 2},
		{[]string{"--since", "2026-09-01"}, 29, "2026-09-01", "2026-09-29", 22},
		{[]string{"--since", "2026-09-29"}, 1, "2026-09-29", "2026-09-29", 0},
		{[]string{"--since", "7d"}, 8, "2026-09-22", "2026-09-29", 3},
		{[]string{"--since", "12h"}, 1, "2026-09-29", "2026-09-29", 0},
		{[]string{"--since", "2026-09-01T09:30:00Z"}, 29, "2026-09-01", "2026-09-29", 22},
	} {
		var doc statsDocument
		if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, append([]string{"--json"}, tc.args...)...)), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Window.Days != tc.days || doc.Window.FirstDay != tc.firstDay || doc.Window.LastDay != tc.lastDay || doc.Coverage.Sessions != tc.sessionsIn {
			t.Errorf("%v: window=%+v sessions=%d, want %d days %s to %s and %d sessions",
				tc.args, doc.Window, doc.Coverage.Sessions, tc.days, tc.firstDay, tc.lastDay, tc.sessionsIn)
		}
	}
}

// A window is calendar days even across a daylight-saving change, in the
// machine's zone.
func TestStatsWindowCountsCalendarDaysAcrossDaylightSaving(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone database")
	}
	// The clocks went forward on 2026-03-08: March 7 to 9 is two days, in
	// 47 hours.
	from := time.Date(2026, 3, 7, 23, 0, 0, 0, ny)
	to := time.Date(2026, 3, 9, 22, 0, 0, 0, ny)
	if got := calendarDaysBetween(from, to); got != 2 {
		t.Fatalf("calendarDaysBetween = %d, want 2", got)
	}
	if got := calendarDaysBetween(to, from); got != -2 {
		t.Fatalf("reversed = %d, want -2", got)
	}
	// An instant in another zone counts on the later time's calendar.
	if got := calendarDaysBetween(time.Date(2026, 3, 8, 3, 0, 0, 0, time.UTC), to); got != 2 {
		t.Fatalf("UTC instant = %d, want 2 (it is March 7 in New York)", got)
	}
}

// The window is read with what surrounds it: the previous period for the
// overview's changes and five earlier months for the month rank, however
// short the window.
func TestStatsFetchIncludesThePreviousPeriodAndTheMonthRankMonths(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, loc)
	for _, tc := range []struct {
		days int
		want time.Time
	}{
		{1, time.Date(2026, 4, 1, 0, 0, 0, 0, loc)},    // the month rank reaches back further
		{30, time.Date(2026, 4, 1, 0, 0, 0, 0, loc)},   // 60 days is less than five months
		{200, time.Date(2025, 8, 26, 0, 0, 0, 0, loc)}, // the previous period reaches back further
	} {
		if got := statsFetchFrom(now, loc, tc.days); !got.Equal(tc.want) {
			t.Errorf("days=%d: from %v, want %v", tc.days, got, tc.want)
		}
	}
}

// The overview's changes and the month rank need sessions outside the
// window: a one-week window still shows both.
func TestStatsShortWindowStillHasChangesAndMonthRank(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json", "--days", "7")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Overview.Sessions.Previous == nil || *doc.Overview.Sessions.Previous == 0 {
		t.Fatalf("no previous period: %+v", doc.Overview.Sessions)
	}
	if doc.Highlights.MonthRank == nil || doc.Highlights.MonthRank.Of != stats.MonthsCompared {
		t.Fatalf("month rank = %+v", doc.Highlights.MonthRank)
	}
}

// The first run reads every sidecar; a second reuses the local cache, and
// --no-cache reads them all again. Nothing but metadata is ever read.
func TestStatsReusesTheMetadataCache(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	sidecars := 0
	keys, err := mem.List(context.Background(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.HasSuffix(k.Key, "/metadata.json") {
			sidecars++
		}
	}
	for _, step := range []struct {
		args  []string
		reads int
	}{
		{nil, sidecars},
		{nil, 0},
		{[]string{"--no-cache"}, sidecars},
		{[]string{"--json"}, 0},
	} {
		mustRunStats(t, env, 0, step.args...)
		_, gets := recorder.take()
		if len(gets) != step.reads {
			t.Fatalf("stats %v read %d objects, want %d", step.args, len(gets), step.reads)
		}
		for _, key := range gets {
			if !strings.HasSuffix(key, "/metadata.json") {
				t.Fatalf("stats %v read %q: only metadata sidecars may be read", step.args, key)
			}
		}
	}
}

// Filters narrow the sessions before they are counted, the previous period
// and month rank included.
func TestStatsFiltersNarrowTheSessions(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	sessions := func(args ...string) int {
		var doc statsDocument
		if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, append([]string{"--json"}, args...)...)), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Coverage.Sessions
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, 22},
		{[]string{"--harness", "codex"}, 6},
		{[]string{"--harness", "claude-code"}, 13},
		{[]string{"--harness", "cursor"}, 3},
		{[]string{"--model", "gpt-5"}, 5},
		{[]string{"--model", "nothing-uses-this"}, 0},
		{[]string{"--hook-captured"}, 22},
		{[]string{"--imported"}, 0},
	} {
		if got := sessions(tc.args...); got != tc.want {
			t.Errorf("%v: %d sessions, want %d", tc.args, got, tc.want)
		}
	}
	// The screen says which filters it applied.
	out := mustRunStats(t, env, 120, "--harness", "codex", "--model", "gpt-5")
	if !strings.Contains(strings.SplitN(out, "\n", 2)[0], "harness codex, model gpt-5") {
		t.Fatalf("heading does not name the filters:\n%s", out)
	}
}

// --imported keeps only sessions backfill imported.
func TestStatsImportedAndHookCapturedSplitTheArchive(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	syntheticSession{
		id: "hooked", harness: "claude", project: "p", captured: statsDay(time.September, 28, 9), models: []string{"claude-opus-5"},
		turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 1000, 0, 0}},
	}.publish(t, mem)
	syntheticSession{
		id: "imported", harness: "claude", project: "p", captured: statsDay(time.September, 28, 10), models: []string{"claude-opus-5"},
		origin: archive.SessionOriginImport, turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 1000, 0, 0}},
	}.publish(t, mem)
	for args, want := range map[string]int{"--imported": 1, "--hook-captured": 1, "--days": 2} {
		a := []string{"--json", args}
		if args == "--days" {
			a = []string{"--json", "--days", "5"}
		}
		var doc statsDocument
		if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, a...)), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Coverage.Sessions != want {
			t.Errorf("%v: %d sessions, want %d", a, doc.Coverage.Sessions, want)
		}
		if args == "--imported" && doc.Filters.Origin != "imported" {
			t.Errorf("filters = %+v", doc.Filters)
		}
	}
}

// Before setup there is nothing to read: stderr and exit 1, as list.
func TestStatsReportsNotSetUp(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), statsNow)
	out, errOut, code := runStats(t, env, 0)
	if code != 1 || out != "" || !strings.Contains(errOut, "Not set up") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// An empty archive and a window without sessions are answered, with what to
// do next, and exit 0.
func TestStatsSaysWhyThereIsNothingToShow(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	out := mustRunStats(t, env, 0)
	if !strings.Contains(out, "No archived sessions in the last 30 days (2026-08-31 to 2026-09-29)") ||
		!strings.Contains(out, "agent-archive status") || !strings.Contains(out, "--days 90") {
		t.Fatalf("empty archive:\n%s", out)
	}
	golden.Check(t, statsGolden("empty-archive"), []byte(out))

	// Only old sessions.
	syntheticSession{
		id: "old", harness: "claude", project: "p", captured: statsDay(time.September, 1, 9), models: []string{"claude-opus-5"},
		turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 1000, 0, 0}},
	}.publish(t, mem)
	out = mustRunStats(t, env, 0, "--days", "7")
	if !strings.Contains(out, "No archived sessions were captured in the last 7 days") || strings.Contains(out, "WHERE IT WENT") {
		t.Fatalf("empty window:\n%s", out)
	}
	// A session in the previous period only.
	out = mustRunStats(t, env, 0, "--since", "2026-09-29")
	if !strings.Contains(out, "No archived sessions were captured today (2026-09-29).") {
		t.Fatalf("empty day:\n%s", out)
	}
	out = mustRunStats(t, env, 0, "--days", "7", "--harness", "codex")
	if !strings.Contains(out, "No archived sessions match these filters in the last 7 days") {
		t.Fatalf("empty with filters:\n%s", out)
	}
	// The screen says "opus"; the filter needs the id, and the answer says so.
	out = mustRunStats(t, env, 0, "--days", "60", "--model", "opus")
	if !strings.Contains(out, "No archived sessions match these filters") || !strings.Contains(flatten(out), "--model takes a full model id") {
		t.Fatalf("empty with a family name:\n%s", out)
	}
}

// A one-day window says "the day before" and names its date once.
func TestStatsOneDayWindowReadsNaturally(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	syntheticSession{
		id: "today", harness: "claude", project: "p", captured: statsDay(time.September, 29, 9), models: []string{"claude-opus-5"},
		turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 1000, 0, 0}},
	}.publish(t, mem)
	// The day before has a session, so the detail screen compares with it.
	syntheticSession{
		id: "yesterday", harness: "claude", project: "p", captured: statsDay(time.September, 28, 9), models: []string{"claude-opus-5"},
		turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 1000, 0, 0}},
	}.publish(t, mem)
	out := mustRunStats(t, env, 100, "--days", "1", "--prices", goldenPrices)
	for _, bad := range []string{"1 days", "Sep 29 Sep 29"} {
		if strings.Contains(out, bad) {
			t.Errorf("one-day window says %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "today") {
		t.Errorf("one-day window:\n%s", out)
	}
	// A chart of one bar says nothing the headline does not.
	if strings.Contains(out, "DAILY SPEND") {
		t.Errorf("a one-day window draws a one-bar chart:\n%s", out)
	}
	if two := mustRunStats(t, env, 100, "--days", "2", "--prices", goldenPrices); !strings.Contains(two, "DAILY SPEND") {
		t.Errorf("a two-day window has no chart:\n%s", two)
	}
	if detail := mustRunStats(t, env, 100, "--days", "1", "--prices", goldenPrices, "--view", "detail"); !strings.Contains(detail, "day before") {
		t.Errorf("one-day detail does not name the day before:\n%s", detail)
	}
}

// A sidecar that does not validate is warned about on stderr and left out;
// the rest is still counted.
func TestStatsWarnsAboutASkippedSidecar(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	key, err := archive.MetadataObjectKey("claude", "damaged")
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runStats(t, env, 0)
	if code != 0 || !strings.Contains(errOut, "agent-archive: stats: warning: skipped a session whose metadata could not be read") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "22 sessions") {
		t.Fatalf("the other sessions were not counted:\n%s", out)
	}
}

// A text listing on a terminal is paged; --no-pager and --json are not.
func TestStatsPagesOnATerminal(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	var pagerInput string
	env.IsTerminal = func(any) bool { return true }
	env.RunPager = func(_ context.Context, command string, _ []string, stdin io.Reader, _, _ io.Writer) error {
		data, err := io.ReadAll(stdin)
		pagerInput = string(data)
		return err
	}
	out := mustRunStats(t, env, 120)
	if out != "" || !strings.Contains(pagerInput, "WHERE IT WENT") {
		t.Fatalf("stdout=%q pager input=%q", out, pagerInput)
	}
	pagerInput = ""
	for _, args := range [][]string{{"--no-pager"}, {"--json"}} {
		out := mustRunStats(t, env, 120, args...)
		if pagerInput != "" || out == "" {
			t.Fatalf("%v was paged: stdout=%q pager=%q", args, out, pagerInput)
		}
	}
	// Inside an agent (or with the switch on) nothing is paged, even on a
	// terminal.
	env.LookupEnv = func(key string) (string, bool) { return "1", key == envNonInteractive }
	if out := mustRunStats(t, env, 120); pagerInput != "" || !strings.Contains(out, "WHERE IT WENT") {
		t.Fatalf("paged with %s=1: stdout=%q pager=%q", envNonInteractive, out, pagerInput)
	}
}

// --prices puts the person's own prices on top of the built-in table, and
// the output says so.
func TestStatsPricesFileOverridesAndIsNamed(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	syntheticSession{
		id: "one", harness: "claude", project: "p", captured: statsDay(time.September, 28, 9), models: []string{"claude-opus-5"},
		turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1_000_000, 1_000_000, 0, 0}},
	}.publish(t, mem)
	file := filepath.Join(t.TempDir(), "prices.json")
	custom := `{"version": "mine-1", "as_of": "2026-09-30", "models": [
	  {"id": "claude-opus-5", "family": "opus", "input_per_mtok": 1, "output_per_mtok": 2, "cache_read_per_mtok": 0, "cache_write_per_mtok": 0}]}`
	if err := os.WriteFile(file, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	cost := func(args ...string) statsDocument {
		var doc statsDocument
		out := mustRunStats(t, env, 0, append([]string{"--json"}, args...)...)
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	builtin := cost()
	mine := cost("--prices", file)
	if builtin.Overview.Cost.Value == nil || mine.Overview.Cost.Value == nil || *mine.Overview.Cost.Value != 3 || *builtin.Overview.Cost.Value == 3 {
		t.Fatalf("built-in cost %v, own cost %v (want 3)", builtin.Overview.Cost.Value, mine.Overview.Cost.Value)
	}
	if mine.Prices.Version != "mine-1" || !mine.Prices.Overridden || builtin.Prices.Overridden {
		t.Fatalf("prices = %+v / %+v", mine.Prices, builtin.Prices)
	}
	text := flatten(mustRunStats(t, env, 0, "--prices", file, "--detail"))
	if !strings.Contains(text, "Prices mine-1, as of 2026-09-30, with your --prices file applied") {
		t.Fatalf("the screen does not say whose prices these are:\n%s", text)
	}
	if strings.Contains(text, file) || strings.Contains(text, filepath.Base(file)) {
		t.Fatalf("the prices file's path is printed:\n%s", text)
	}
	if got := flatten(mustRunStats(t, env, 0, "--detail")); !strings.Contains(got, "Prices "+stats.DefaultPriceTable().Version+", as of "+stats.DefaultPriceTable().AsOf) {
		t.Fatalf("built-in prices are not named:\n%s", got)
	}
}

// Text from the archive (a project or model name) can carry terminal
// control sequences; none reaches the screen or the JSON raw.
func TestStatsNeverPrintsControlSequencesFromTheArchive(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	evil := "proj\x1b[31mRED\x1b]0;title\x07"
	syntheticSession{
		id: "evil", harness: "claude", project: evil, captured: statsDay(time.September, 28, 9),
		models: []string{"claude-opus-5\x1b[2J"}, turns: 1, skills: []string{"skill\x1b[1m"}, mcp: map[string]int{"srv\x1b[5m": 1},
		perModel: []modelTokenSpec{{"claude-opus-5\x1b[2J", 1000, 1000, 0, 0}},
	}.publish(t, mem)
	for _, args := range [][]string{nil, {"--json"}, {"--by", "project"}} {
		out := mustRunStats(t, env, 0, args...)
		if strings.Contains(out, "\x1b") || strings.Contains(out, "\x07") {
			t.Fatalf("%v: raw control sequence in the output:\n%q", args, out)
		}
	}
}

// Sessions that only one agent recorded leave out what they cannot say:
// Cursor alone has no tokens (no token sections, "unknown"), and Codex has
// no MCP row.
func TestStatsOmitsSectionsThatHaveNoData(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	syntheticSession{id: "cursor-only", harness: "cursor", project: "p", captured: statsDay(time.September, 28, 9), models: []string{"cursor-auto"}, turns: 3}.publish(t, mem)
	out := mustRunStats(t, env, 0, "--prices", goldenPrices)
	flat := flatten(out)
	detail := mustRunStats(t, env, 0, "--prices", goldenPrices, "--detail")
	for _, screen := range []string{out, detail} {
		for _, absent := range []string{"DAILY SPEND", "WHAT USED YOUR TOKENS", "By model", "Subagents", "Skills", "MCP", "Tool errors", "runs"} {
			if strings.Contains(screen, absent) {
				t.Errorf("Cursor-only output has %q:\n%s", absent, screen)
			}
		}
	}
	for _, present := range []string{"spend unknown", "tokens unknown", "1 session", "Cursor 1"} {
		if !strings.Contains(flat, present) {
			t.Errorf("Cursor-only output lacks %q:\n%s", present, out)
		}
	}
	if !strings.Contains(detail, "n/a") || !strings.Contains(detail, "unknown") {
		t.Errorf("Cursor-only detail does not say unknown:\n%s", detail)
	}
	golden.Check(t, statsGolden("cursor-only"), []byte(out))

	env2, mem2 := statsEnv(t)
	syntheticSession{
		id: "codex-only", harness: "codex", project: "p", captured: statsDay(time.September, 28, 9), models: []string{"gpt-5"}, turns: 3,
		toolResults: 10, perModel: []modelTokenSpec{{"gpt-5", 2000, 500, 1000, 0}},
	}.publish(t, mem2)
	out = mustRunStats(t, env2, 0, "--detail")
	for _, absent := range []string{"MCP", "Subagents", "Skills", "Tool errors"} {
		if strings.Contains(out, absent) {
			t.Errorf("Codex-only output has %q:\n%s", absent, out)
		}
	}
	// Codex's input includes its cached input: 2000 - 1000 fresh.
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env2, 0, "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	if c := doc.Composition; c == nil || c.Total != 2500 || c.FreshInput.Tokens != 1000 || c.CacheRead.Tokens != 1000 {
		t.Fatalf("composition = %+v", doc.Composition)
	}
}

// The screen never shows paths: nothing from the sessions but project names,
// models and counts reaches it, and the fixture's paths (source keys) do not.
func TestStatsPrintsNoPathsOrSourceKeys(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, args := range [][]string{nil, {"--json", "--by", "project"}} {
		out := mustRunStats(t, env, 0, args...)
		for _, leak := range []string{"source.jsonl", "sessions/", "/Users/", "metadata.json"} {
			if strings.Contains(out, leak) {
				t.Errorf("%v: output contains %q", args, leak)
			}
		}
	}
}

// The command is in the top-level help and has help of its own.
func TestStatsHasHelp(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := Run([]string{"stats", "--help"}, nil, &out, &errOut, testEnv(t, t.TempDir(), statsNow)); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-archive stats") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	out.Reset()
	Run([]string{"--help"}, nil, &out, &errOut, testEnv(t, t.TempDir(), statsNow))
	if !strings.Contains(out.String(), "agent-archive stats") {
		t.Fatalf("top-level help has no stats:\n%s", out.String())
	}
}

// A few thousand sidecars are read in one pass and only the window's are
// counted. (No cache: this measures the read and the computation, not the
// disk.)
func TestStatsReadsThousandsOfSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("reads several thousand sidecars")
	}
	t.Parallel()
	env, mem := statsEnv(t)
	for i := range 3000 {
		syntheticSession{
			id: fmt.Sprintf("bulk-%05d", i), harness: []string{"claude", "codex", "cursor"}[i%3], project: fmt.Sprintf("project-%d", i%40),
			captured: statsNow.Add(-time.Duration(i) * 2 * time.Hour), models: []string{"claude-opus-5"}, turns: 5, messages: 40, toolResults: 20,
			perModel: []modelTokenSpec{{"claude-opus-5", 10_000, 5_000, 200_000, 20_000}},
		}.publish(t, mem)
	}
	start := time.Now()
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json", "--no-cache")), &doc); err != nil {
		t.Fatal(err)
	}
	// Captured 2 hours apart, back from Sep 29 12:00: 355 fall on or after Aug 31.
	if doc.Coverage.Sessions != 355 || doc.Coverage.Agents != 3 || doc.TotalProjects != 40 {
		t.Fatalf("coverage = %+v, projects = %d", doc.Coverage, doc.TotalProjects)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("reading 3000 sidecars took %v", elapsed)
	}
}

// A session whose metadata predates parser 0.14.0 has no per-model split: it
// is priced at its main model, marked ~, and the footer says why.
func TestStatsMarksSessionsPricedAtTheirMainModel(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	syntheticSession{
		id: "old-parser", harness: "claude", project: "p", captured: statsDay(time.September, 28, 9), parser: "0.13.0",
		models: []string{"claude-opus-5"}, turns: 2, toolResults: 10, perModel: []modelTokenSpec{{"claude-opus-5", 100_000, 50_000, 0, 0}},
	}.publish(t, mem)
	out := mustRunStats(t, env, 0, "--detail")
	if !strings.Contains(out, "~$") || !strings.Contains(flatten(out), "1 session priced at the session's main model, with no per-model split") {
		t.Fatalf("no ~ mark or explanation:\n%s", out)
	}
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Coverage.SessionsBeforeParser014 != 1 || doc.Coverage.SessionsPricedAtMainModel != 1 || !doc.Overview.Cost.Approximate {
		t.Fatalf("coverage=%+v cost=%+v", doc.Coverage, doc.Overview.Cost)
	}
	if doc.Highlights.ToolErrors != nil {
		t.Fatalf("tool errors are unknown before parser 0.14.0, got %+v", doc.Highlights.ToolErrors)
	}
}

// flatten joins a screen's wrapped lines, so a test can look for a sentence
// wherever the terminal's width broke it.
func flatten(screen string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(screen, " ")
}
