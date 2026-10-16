package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// allInHint matches the line that says how many rows a list leaves out and
// where they all are.
var allInHint = regexp.MustCompile(`(?m)^(?:\+ \d+ more|\d+ earlier rows not shown) \(all in ([^)]+)\)$`)

// hintCommand returns the command a page's hint points at: "+ 5 more (all in
// --json --by project)" names "--json --by project". A page with no such line
// fails the test.
func hintCommand(t *testing.T, page string) []string {
	t.Helper()
	m := allInHint.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page has no hint to a command that lists the rest:\n%s", page)
	}
	return strings.Fields(m[1])
}

// runHint runs the command a hint names, and decodes its JSON.
func runHint(t *testing.T, env Env, hint []string, extra ...string) statsDocument {
	t.Helper()
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, append(append([]string{"--no-cache"}, extra...), hint...)...)), &doc); err != nil {
		t.Fatalf("stats %v: %v", hint, err)
	}
	return doc
}

// projectAndModelSessions is n sessions an hour before statsNow, each in a
// project and a model of its own.
func projectAndModelSessions(n int) []syntheticSession {
	sessions := make([]syntheticSession, 0, n)
	for i := range n {
		sessions = append(sessions, syntheticSession{
			id: fmt.Sprintf("h%03d", i), harness: "claude", project: fmt.Sprintf("project-%03d", i),
			captured: statsNow.Add(-time.Hour), models: []string{fmt.Sprintf("model-%03d", i)}, turns: 1,
			perModel: []modelTokenSpec{{fmt.Sprintf("model-%03d", i), 1000 + i, 100, 0, 0}},
		})
	}
	return sessions
}

// Every "all in ..." hint under a list a screen cuts is true: the command it
// names really lists every row, and plain --json, which a hint must not name,
// does not (it keeps the top five projects). The archive has more projects
// and models than a list screen keeps (500).
func TestStatsAllInHintsListEveryRow(t *testing.T) {
	t.Parallel()
	const projects = statsMaxListRows + 5
	env, mem := statsEnv(t)
	for _, s := range projectAndModelSessions(projects) {
		s.publish(t, mem)
	}
	// Plain --json keeps the top five projects and has no groups; that is why
	// no hint may name it for the projects.
	plain := runHint(t, env, []string{"--json"})
	if len(plain.Projects) != 5 || plain.TotalProjects != projects || plain.Groups != nil {
		t.Fatalf("plain --json has %d projects of %d, groups %v; want the top 5 of %d and no groups", len(plain.Projects), plain.TotalProjects, plain.Groups, projects)
	}

	t.Run("projects screen", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache", "--view", "projects")
		hint := hintCommand(t, page)
		if !strings.Contains(page, "+ 5 more (all in --json --all)") {
			t.Errorf("the projects screen says something else about the 5 it leaves out:\n%s", page[len(page)-400:])
		}
		doc := runHint(t, env, hint)
		if len(doc.Projects) != projects || doc.TotalProjects != projects {
			t.Fatalf("%v lists %d of %d projects, want all %d", hint, len(doc.Projects), doc.TotalProjects, projects)
		}
		// The screen shows the first rows of that same list, in that order.
		for i, row := range doc.Projects[:statsMaxListRows] {
			if !strings.Contains(page, row.Name) {
				t.Fatalf("row %d of %v, %q, is not on the projects screen", i, hint, row.Name)
			}
		}
		for _, row := range doc.Projects[statsMaxListRows:] {
			if strings.Contains(page, row.Name) {
				t.Errorf("%q is one of the rows the screen says it left out", row.Name)
			}
		}
	})

	t.Run("models screen", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache", "--view", "models")
		hint := hintCommand(t, page)
		if !strings.Contains(page, "+ 5 more (all in --json)") {
			t.Errorf("the models screen says something else about the 5 it leaves out:\n%s", page[len(page)-500:])
		}
		if doc := runHint(t, env, hint); len(doc.Models) != projects {
			t.Errorf("%v lists %d models, want all %d", hint, len(doc.Models), projects)
		}
	})
}

// The day, week and month tables of the detail screen keep the newest 60
// rows, and the hint under them is true: the command it names has every row.
func TestStatsAllInHintsListEveryGroupRow(t *testing.T) {
	t.Parallel()
	// More rows than the table keeps, one session for each.
	const rows = statsMaxGroupRows + 5
	for _, tc := range []struct {
		by stats.Grouping
		// captured is when session i happened, the newest first; days is a
		// window that reaches the oldest.
		captured func(i int) time.Time
		days     int
	}{
		{stats.GroupDay, func(i int) time.Time { return statsNow.AddDate(0, 0, -i-1) }, rows + 7},
		{stats.GroupWeek, func(i int) time.Time { return statsNow.AddDate(0, 0, -7*i-1) }, 7*rows + 7},
		{stats.GroupMonth, func(i int) time.Time { return time.Date(2026, time.Month(9-i), 15, 11, 0, 0, 0, time.UTC) }, 31*rows + 62},
	} {
		t.Run("detail by "+string(tc.by), func(t *testing.T) {
			t.Parallel()
			env, mem := statsEnv(t)
			for i := range rows {
				syntheticSession{
					id: fmt.Sprintf("g%03d", i), harness: "claude", project: "one", captured: tc.captured(i),
					models: []string{"claude-opus-5"}, turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 100, 0, 0}},
				}.publish(t, mem)
			}
			window := strconv.Itoa(tc.days)
			page := mustRunStats(t, env, 100, "--no-cache", "--by", string(tc.by), "--view", "detail", "--days", window)
			hint := hintCommand(t, page)
			if want := "(all in --json --by " + string(tc.by) + ")"; !strings.Contains(page, want) {
				t.Errorf("the detail screen lacks %q:\n%s", want, page)
			}
			doc := runHint(t, env, hint, "--days", window)
			if doc.Groups == nil || doc.Groups.By != tc.by || len(doc.Groups.Rows) != rows {
				t.Fatalf("%v does not list all %d rows: %+v", hint, rows, doc.Groups)
			}
			// The screen keeps the newest rows and says how many earlier ones
			// it left out, and those are in the JSON.
			if want := "5 earlier rows not shown"; !strings.Contains(page, want) {
				t.Errorf("the detail screen lacks %q:\n%s", want, page)
			}
			if oldest := doc.Groups.Rows[0].Key; strings.Contains(page, oldest) {
				t.Errorf("the oldest row, %s, is on the screen that says it left out 5", oldest)
			}
			if newest := doc.Groups.Rows[rows-1].Key; !strings.Contains(page, newest) {
				t.Errorf("the newest row, %s, is not on the screen", newest)
			}
		})
	}
}

// On the interactive screen a command is not one to type, and its window is
// whatever w chose: the line says to quit first, and names the window and
// that the filters apply. The command it names, with that window, lists every
// row.
func TestStatsAllInHintsOnTheInteractiveScreen(t *testing.T) {
	t.Parallel()
	const projects = statsMaxListRows + 5
	env, mem := statsEnv(t)
	var sessions []archive.Metadata
	for _, s := range projectAndModelSessions(projects) {
		s.publish(t, mem)
		sessions = append(sessions, s.build())
	}
	inputs := statsInputs{hasSessions: len(sessions) > 0, prepared: stats.Prepare(sessions, stats.PrepareOptions{Location: statsNow.Location()}), now: statsNow, location: statsNow.Location()}
	for _, tc := range []struct {
		view    string
		command string
		rows    func(statsDocument) int
	}{
		{"p", "--json --all", func(d statsDocument) int { return len(d.Projects) }},
		{"m", "--json", func(d statsDocument) int { return len(d.Models) }},
	} {
		for _, days := range []int{7, 30, 90} {
			t.Run(fmt.Sprintf("%s %dd", tc.view, days), func(t *testing.T) {
				t.Parallel()
				run := runScreen(t, screenOptions{width: 100, height: 40, days: days, inputs: &inputs}, tc.view, "\x1b[F", "q")
				text := strings.Join(run.frames[len(run.frames)-1], " ")
				flat := strings.Join(strings.Fields(ansiEscape.ReplaceAllString(text, "")), " ")
				want := fmt.Sprintf("+ 5 more (quit, then run agent-archive stats --days %d %s)", days, tc.command)
				if !strings.Contains(flat, want) {
					t.Fatalf("the interactive screen lacks %q:\n%s", want, strings.Join(run.frames[len(run.frames)-1], "\n"))
				}
				var doc statsDocument
				args := append([]string{"--no-cache", "--days", strconv.Itoa(days)}, strings.Fields(tc.command)...)
				if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, args...)), &doc); err != nil {
					t.Fatal(err)
				}
				if got := tc.rows(doc); got != projects {
					t.Errorf("stats %v lists %d rows, want all %d", args, got, projects)
				}
			})
		}
	}
	// Filters the screen was started with are still to be given.
	run := runScreen(t, screenOptions{width: 100, height: 40, inputs: &inputs, filters: statsFilters{Harness: "claude"}}, "p", "\x1b[F", "q")
	flat := strings.Join(strings.Fields(ansiEscape.ReplaceAllString(strings.Join(run.frames[len(run.frames)-1], " "), "")), " ")
	if want := "+ 5 more (quit, then run agent-archive stats --days 30 --json --all, with the same filters)"; !strings.Contains(flat, want) {
		t.Errorf("the interactive screen with a filter lacks %q:\n%s", want, flat)
	}
}

// The hint lines never run past the terminal, at any width the screens adapt
// to, with or without color and in ASCII, and they wrap rather than lose the
// command they name.
func TestStatsAllInHintsFitTheTerminal(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	s.Projects = nil
	s.Models = nil
	extraProjects, extraGroups := 12345, 100000
	if testing.Short() {
		// Still cut both lists, with a four-digit omission count to wrap.
		extraProjects, extraGroups = 1234, 1234
	}
	for i := range statsMaxListRows + extraProjects {
		name := fmt.Sprintf("p%05d", i)
		s.Projects = append(s.Projects, stats.Project{Name: name, Sessions: 1, Tokens: i64(1000), Cost: usd(float64(1000 - i%900))})
		s.Models = append(s.Models, stats.ModelRow{Label: "m" + name, Sessions: 1, Tokens: 1000, Priced: true, Cost: usd(1)})
	}
	s.TotalProjects = len(s.Projects)
	rows := make([]stats.Group, 0, statsMaxGroupRows+extraGroups)
	for i := range statsMaxGroupRows + extraGroups {
		rows = append(rows, stats.Group{Key: fmt.Sprintf("g%d", i), Sessions: 1})
	}
	type mode struct {
		color       bool
		ascii       bool
		interactive bool
	}
	modes := []mode{{false, false, false}, {true, false, false}, {false, true, false}, {false, false, true}, {true, true, true}}
	// Every width up to where the layouts change, then a spread to 250.
	var widths []int
	for w := statsMinWidth; w <= 100; w++ {
		widths = append(widths, w)
	}
	widths = append(widths, 111, 120, 137, 160, 200, 250)
	if testing.Short() {
		widths = statsMatrixWidths()
	}
	for _, tc := range []struct {
		name string
		page statsPage
		by   stats.Grouping
		want string
	}{
		{"projects", pageProjects, stats.GroupNone, "--json --all"},
		{"models", pageModels, stats.GroupNone, "--json"},
		{"detail by project", pageDetail, stats.GroupProject, "--json --by project"},
		{"detail by day", pageDetail, stats.GroupDay, "--json --by day"},
		{"detail by week", pageDetail, stats.GroupWeek, "--json --by week"},
		{"detail by month", pageDetail, stats.GroupMonth, "--json --by month"},
	} {
		s := s
		if tc.by != stats.GroupNone {
			s.Groups = &stats.Groups{By: tc.by, Rows: rows}
		}
		for _, width := range widths {
			for _, mode := range modes {
				// The interactive screen has no --by tables.
				if mode.interactive && tc.by != stats.GroupNone {
					continue
				}
				glyphs := unicodeGlyphs
				if mode.ascii {
					glyphs = asciiGlyphs
				}
				view := statsView{style: textStyle{color: mode.color}, width: width, glyphs: glyphs, interactive: mode.interactive}
				out := stripANSI(strings.Join(renderPage(tc.page, s, view), "\n"))
				checkTerminalSafe(t, fmt.Sprintf("%s %d", tc.name, width), out, width)
				// Wrapping may break the command over lines, never lose it.
				want := "(all in " + tc.want + ")"
				if mode.interactive {
					want = fmt.Sprintf("(quit, then run agent-archive stats --days %d %s)", s.Window.Days, tc.want)
				}
				if flat := strings.Join(strings.Fields(out), " "); !strings.Contains(flat, want) {
					t.Fatalf("%s at %d columns (%+v) lost the command %q:\n%s", tc.name, width, mode, want, out)
				}
				// A command is not cut in two: its flags are on one line
				// ("--by" at the end of one and "project)" at the start of
				// the next was the bug), and the whole command is too where
				// a line is wide enough for it.
				lines := strings.Split(out, "\n")
				onOneLine := func(text string) bool {
					return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, text) })
				}
				if !onOneLine(tc.want + ")") {
					t.Fatalf("%s at %d columns (%+v) split the command %q over lines:\n%s", tc.name, width, mode, tc.want, out)
				}
				if command := fmt.Sprintf("agent-archive stats --days %d %s)", s.Window.Days, tc.want); mode.interactive && visibleWidth(command) <= width && !onOneLine(command) {
					t.Fatalf("%s at %d columns (%+v) split the command %q, which fits a line:\n%s", tc.name, width, mode, command, out)
				}
			}
		}
	}
}
