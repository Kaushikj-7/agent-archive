package cli

import (
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/stats"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// manyUsageSessions is one session in each of n projects, where each session
// used n skills and called n MCP servers, and project i used skill j for
// every j up to it, so the counts differ and the order is by use.
func manyUsageSessions(n int) []syntheticSession {
	sessions := make([]syntheticSession, 0, n)
	for i := range n {
		var skills []string
		mcp := map[string]int{}
		for j := range n - i {
			skills = append(skills, fmt.Sprintf("skill-%02d", j))
			mcp[fmt.Sprintf("server-%02d", j)] = 10 + j
		}
		sessions = append(sessions, syntheticSession{
			id: fmt.Sprintf("u%03d", i), harness: "claude", project: fmt.Sprintf("project-%02d", i),
			captured: statsNow.Add(-time.Hour), models: []string{"claude-opus-5"}, turns: 1,
			perModel: []modelTokenSpec{{"claude-opus-5", 1000 + i, 100, 0, 0}},
			skills:   skills, mcp: mcp,
		})
	}
	return sessions
}

// dropLists removes the lists --all lengthens from a document, so what is left
// can be compared: --all must change nothing else.
func dropLists(t *testing.T, raw string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "projects")
	delete(doc, "skills")
	delete(doc, "display_skills")
	if mcp, ok := doc["mcp"].(map[string]any); ok {
		delete(mcp, "servers")
	}
	return doc
}

// stats --json keeps the top five projects, skills and MCP servers; --all
// lists every one, with the same totals, in the same order (the top five are
// its first five), leaves everything else in the document as it was, and is
// the same bytes each time.
func TestStatsJSONAllListsEveryRow(t *testing.T) {
	t.Parallel()
	const n = 12
	env, mem := statsEnv(t)
	for _, s := range manyUsageSessions(n) {
		s.publish(t, mem)
	}
	plainRaw := mustRunStats(t, env, 0, "--no-cache", "--json")
	allRaw := mustRunStats(t, env, 0, "--no-cache", "--json", "--all")
	if again := mustRunStats(t, env, 0, "--no-cache", "--json", "--all"); again != allRaw {
		t.Error("--json --all is not the same document twice")
	}
	var plain, all statsDocument
	if err := json.Unmarshal([]byte(plainRaw), &plain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(allRaw), &all); err != nil {
		t.Fatal(err)
	}
	if plain.Version != 1 || all.Version != 1 {
		t.Errorf("schema versions %d and %d, want 1", plain.Version, all.Version)
	}
	if len(plain.Projects) != 5 || len(plain.DisplaySkills) != 5 || len(plain.Skills) != 5 || len(plain.MCP.Servers) != 5 {
		t.Fatalf("plain --json has %d projects, %d display skills, %d skills, %d servers; want the top 5 of each",
			len(plain.Projects), len(plain.DisplaySkills), len(plain.Skills), len(plain.MCP.Servers))
	}
	if len(all.Projects) != n || len(all.DisplaySkills) != n || len(all.Skills) != n || len(all.MCP.Servers) != n {
		t.Fatalf("--all has %d projects, %d display skills, %d skills, %d servers; want all %d",
			len(all.Projects), len(all.DisplaySkills), len(all.Skills), len(all.MCP.Servers), n)
	}
	// The totals agree with the lists, either way.
	for name, got := range map[string][2]int{
		"total_projects":       {plain.TotalProjects, all.TotalProjects},
		"total_skills":         {plain.TotalSkills, all.TotalSkills},
		"total_display_skills": {plain.TotalDisplaySkills, all.TotalDisplaySkills},
		"mcp.total_servers":    {plain.MCP.TotalServers, all.MCP.TotalServers},
	} {
		if got[0] != n || got[1] != n {
			t.Errorf("%s is %d without --all and %d with it, want %d", name, got[0], got[1], n)
		}
	}
	// The top five are the first five, in the same order.
	if !reflect.DeepEqual(plain.Projects, all.Projects[:5]) || !reflect.DeepEqual(plain.DisplaySkills, all.DisplaySkills[:5]) ||
		!reflect.DeepEqual(plain.Skills, all.Skills[:5]) || !reflect.DeepEqual(plain.MCP.Servers, all.MCP.Servers[:5]) {
		t.Error("the top five of --json are not the first five rows of --json --all")
	}
	// Additive only: nothing but those lists changes.
	if !reflect.DeepEqual(dropLists(t, plainRaw), dropLists(t, allRaw)) {
		t.Error("--all changed something besides the project, skill and MCP server lists")
	}
	// --all never invents a group table.
	if all.Groups != nil {
		t.Errorf("--all has a group table: %+v", all.Groups)
	}
}

// --all is for --json only: anywhere else it is a usage error, checked before
// the archive is read (exit 2, one line, nothing on stdout), the web page
// included: it keeps its top lists.
func TestStatsAllNeedsJSON(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, args := range [][]string{
		{"--all"},
		{"--all", "--view", "projects"},
		{"--all", "--by", "project"},
		{"--all", "--detail"},
		{"--all", "--no-pager"},
		{"--all", "--html"},
		{"--all", "--html", "--output", "unused.html"},
	} {
		out, errOut, code := runStats(t, env, 0, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "--all applies only to --json") || !strings.Contains(errOut, "run agent-archive stats --help") || strings.Count(errOut, "\n") != 1 {
			t.Errorf("%v: code=%d stdout=%q stderr=%q, want exit 2, one line, and \"--all applies only to --json\"", args, code, out, errOut)
		}
	}
	// With --json it is fine, and --html with --json is refused as before.
	if out, errOut, code := runStats(t, env, 0, "--json", "--all"); code != 0 || errOut != "" || !strings.Contains(out, `"schema_version": 1`) {
		t.Errorf("--json --all: code=%d stderr=%q", code, errOut)
	}
	if _, errOut, code := runStats(t, env, 0, "--json", "--html", "--all"); code != 2 || !strings.Contains(errOut, "--html and --json cannot be combined") {
		t.Errorf("--json --html --all: code=%d stderr=%q", code, errOut)
	}
}

// usageHint is a hint that ends a skills or MCP row: "+ 40 more (all in
// --json --all)".
var usageHint = regexp.MustCompile(`\+ (\d+) more \(all in ([^)]+)\)`)

// usageHints are the hints in a page, in order, with whitespace folded so a
// hint that wraps over lines is found.
func usageHints(page string) [][]string {
	return usageHint.FindAllStringSubmatch(strings.Join(strings.Fields(page), " "), -1)
}

// Every hint under the skills and MCP servers says how many it leaves out and
// the command that lists them all, and the command does: in the overview (a
// few of each), the detail screen (the first 40), and on the interactive
// screen, where the command is the one to run after quitting.
func TestStatsUsageHintsListEveryRow(t *testing.T) {
	t.Parallel()
	const n = statsMaxUseRows + 5
	env, mem := statsEnv(t)
	var sessions []archive.Metadata
	for _, s := range manyUsageSessions(n) {
		s.publish(t, mem)
		sessions = append(sessions, s.build())
	}
	// Plain --json keeps the top five: the hint must not name it.
	var plain statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--no-cache", "--json")), &plain); err != nil {
		t.Fatal(err)
	}
	if len(plain.DisplaySkills) != 5 || len(plain.MCP.Servers) != 5 {
		t.Fatalf("plain --json has %d skills and %d servers, want the top 5", len(plain.DisplaySkills), len(plain.MCP.Servers))
	}
	check := func(t *testing.T, where string, hints [][]string, wantLeft int, wantCommand string, args ...string) {
		t.Helper()
		if len(hints) != 2 {
			t.Fatalf("%s has %d hints under skills and MCP, want 2: %q", where, len(hints), hints)
		}
		for _, hint := range hints {
			if hint[1] != strconv.Itoa(wantLeft) || hint[2] != wantCommand {
				t.Errorf("%s: hint %q, want %d more and %q", where, hint[0], wantLeft, wantCommand)
			}
		}
		var doc statsDocument
		run := append([]string{"--no-cache"}, args...)
		run = append(run, strings.Fields(wantCommand)...)
		if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, run...)), &doc); err != nil {
			t.Fatalf("stats %v: %v", run, err)
		}
		if len(doc.DisplaySkills) != n || len(doc.Skills) != n || len(doc.MCP.Servers) != n || doc.TotalDisplaySkills != n || doc.MCP.TotalServers != n {
			t.Errorf("stats %v lists %d skills, %d display skills and %d servers (totals %d, %d), want all %d",
				run, len(doc.Skills), len(doc.DisplaySkills), len(doc.MCP.Servers), doc.TotalDisplaySkills, doc.MCP.TotalServers, n)
		}
	}
	t.Run("overview", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache")
		check(t, "the overview", usageHints(page), n-overviewSkills, "--json --all")
	})
	t.Run("detail", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache", "--detail")
		check(t, "the detail screen", usageHints(page), n-statsMaxUseRows, "--json --all")
	})
	t.Run("a window and filters", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache", "--detail", "--days", "7", "--harness", "claude")
		check(t, "the detail screen for 7 days", usageHints(page), n-statsMaxUseRows, "--json --all", "--days", "7", "--harness", "claude")
	})
	inputs := statsInputs{hasSessions: len(sessions) > 0, prepared: stats.Prepare(sessions, stats.PrepareOptions{Location: statsNow.Location()}), now: statsNow, location: statsNow.Location()}
	for _, days := range []int{7, 30, 90} {
		t.Run(fmt.Sprintf("interactive %dd", days), func(t *testing.T) {
			t.Parallel()
			run := runScreen(t, screenOptions{width: 100, height: 120, days: days, inputs: &inputs}, "q")
			page := strings.Join(run.frames[len(run.frames)-1], "\n")
			// The interactive line names no command to type as it is: it says
			// to quit, then run it with the window on show.
			flat := strings.Join(strings.Fields(ansiEscape.ReplaceAllString(page, "")), " ")
			want := fmt.Sprintf("+ %d more (quit, then run agent-archive stats --days %d --json --all)", n-overviewSkills, days)
			if strings.Count(flat, want) != 2 {
				t.Fatalf("the interactive overview lacks %q twice:\n%s", want, flat)
			}
			var doc statsDocument
			args := []string{"--no-cache", "--days", strconv.Itoa(days), "--json", "--all"}
			if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, args...)), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.DisplaySkills) != n || len(doc.MCP.Servers) != n {
				t.Errorf("stats %v lists %d skills and %d servers, want all %d", args, len(doc.DisplaySkills), len(doc.MCP.Servers), n)
			}
		})
	}
}
