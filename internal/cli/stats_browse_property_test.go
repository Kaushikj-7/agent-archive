package cli

import (
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/stats"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// clockTime is a time of day.
type clockTime struct {
	hour   int
	minute int
	second int
}

// moment is the local time a run happens at.
type moment struct {
	year   int
	month  int
	day    int
	hour   int
	minute int
}

// statsShape is a way an archive can be laid out over time, to check that a
// window means the same wherever it is counted from.
type statsShape struct {
	name  string
	build func(now time.Time, loc *time.Location) []syntheticSession
}

// spread is n sessions of harness, one per step days back from now, at hour.
func spread(harness, model string, now time.Time, loc *time.Location, n, step, hour int, tokens bool) []syntheticSession {
	year, month, day := now.In(loc).Date()
	var out []syntheticSession
	for i := range n {
		captured := time.Date(year, month, day-i*step, hour, 30, 0, 0, loc)
		var perModel []modelTokenSpec
		if tokens {
			perModel = []modelTokenSpec{{model, 1000 * (i + 1), 500 * (i + 1), 20_000 * (i + 1), 100 * i}}
		}
		out = append(out, syntheticSession{
			id: fmt.Sprintf("%s-%s-%d-%d-%d", harness, model, n, step, i), harness: harness, project: fmt.Sprintf("project-%d", i%4),
			captured: captured, models: []string{model}, turns: 3 + i%5, messages: 20, toolResults: 10, errors: i % 3,
			skills: []string{"review-pr"}, mcp: map[string]int{"github": 1 + i%3}, perModel: perModel,
		})
	}
	return out
}

// statsShapes are the archives the windows are checked against.
func statsShapes() []statsShape {
	return []statsShape{
		{"sparse", func(now time.Time, loc *time.Location) []syntheticSession {
			return spread("claude", "claude-opus-5", now, loc, 6, 37, 9, true)
		}},
		{"dense", func(now time.Time, loc *time.Location) []syntheticSession {
			return append(spread("claude", "claude-opus-5", now, loc, 260, 1, 9, true), spread("codex", "gpt-5", now, loc, 260, 1, 23, true)...)
		}},
		{"no prior period", func(now time.Time, loc *time.Location) []syntheticSession {
			return spread("claude", "claude-sonnet-5", now, loc, 5, 1, 0, true)
		}},
		{"unpriced", func(now time.Time, loc *time.Location) []syntheticSession {
			return append(spread("claude", "mystery-model-9", now, loc, 120, 2, 15, true), spread("claude", "claude-opus-5", now, loc, 40, 5, 3, true)...)
		}},
		{"cursor only", func(now time.Time, loc *time.Location) []syntheticSession {
			return spread("cursor", "cursor-auto", now, loc, 150, 1, 12, false)
		}},
		{"mixed with subagents", func(now time.Time, loc *time.Location) []syntheticSession {
			all := spread("claude", "claude-opus-5", now, loc, 100, 3, 11, true)
			subs := spread("claude", "claude-sonnet-5", now, loc, 100, 3, 12, true)
			for i := range subs {
				subs[i].id += "-sub"
				subs[i].parent = all[i].id
			}
			return append(append(all, subs...), spread("cursor", "cursor-auto", now, loc, 60, 4, 8, false)...)
		}},
		{"old descendants of recent roots", func(now time.Time, loc *time.Location) []syntheticSession {
			roots := spread("claude", "claude-opus-5", now, loc, 5, 3, 11, true)
			all := slices.Clone(roots)
			for i, root := range roots {
				child := root
				child.id, child.parent, child.captured = fmt.Sprintf("old-child-%d", i), root.id, now.AddDate(0, 0, -800)
				grandchild := child
				grandchild.id, grandchild.parent, grandchild.captured = fmt.Sprintf("old-grandchild-%d", i), child.id, now.AddDate(0, 0, -900)
				all = append(all, child, grandchild)
			}
			return all
		}},
		{"day boundaries", func(now time.Time, loc *time.Location) []syntheticSession {
			// Sessions on the first and last second of days around every
			// window's edge, and around the month rank's.
			year, month, day := now.In(loc).Date()
			var out []syntheticSession
			for _, back := range []int{0, 1, 6, 7, 8, 13, 14, 15, 29, 30, 31, 59, 60, 61, 89, 90, 91, 119, 120, 121, 179, 180, 181, 200, 300} {
				for j, edge := range []clockTime{{0, 0, 0}, {23, 59, 59}, {12, 0, 0}} {
					captured := time.Date(year, month, day-back, edge.hour, edge.minute, edge.second, 0, loc)
					out = append(out, syntheticSession{
						id: fmt.Sprintf("edge-%d-%d", back, j), harness: "claude", project: "edge", captured: captured,
						models: []string{"claude-opus-5"}, turns: 2, messages: 4, toolResults: 4,
						perModel: []modelTokenSpec{{"claude-opus-5", 100, 100, 1000, 10}},
					})
				}
			}
			for _, month := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12} {
				out = append(out, syntheticSession{
					id: fmt.Sprintf("first-of-month-%d", month), harness: "claude", project: "edge",
					captured: time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc), models: []string{"claude-opus-5"}, turns: 1,
					perModel: []modelTokenSpec{{"claude-opus-5", 10, 10, 10, 10}},
				})
			}
			return out
		}},
	}
}

// fetched is what a run that reads for the longest of windows would read of
// sessions: date matches plus descendants of those matches. This oracle walks
// backwards through parents independently of the reader's forward queue.
func fetched(sessions []archive.Metadata, now time.Time, loc *time.Location, windows []int) []archive.Metadata {
	from := statsFetchFrom(now, loc, slices.Max(windows))
	byID := make(map[string]archive.Metadata, len(sessions))
	for _, m := range sessions {
		byID[m.SessionID] = m
	}
	var out []archive.Metadata
	for _, m := range sessions {
		seen := make(map[string]bool)
		for current := m; !seen[current.SessionID]; {
			if !current.CapturedAt.Before(from) {
				out = append(out, m)
				break
			}
			seen[current.SessionID] = true
			parent, found := byID[current.ParentSessionID]
			if !found {
				break
			}
			current = parent
		}
	}
	return out
}

// Every window the interactive screen cycles to counts the same as a fresh
// static run with that --days, whatever the archive looks like, the time zone,
// or the moment the command runs. The interactive run reads for its longest
// window; the static one for its own. The numbers are compared whole, every
// row of every list, as --json prints them: each screen is a function of them.
func TestStatsInteractiveWindowsEqualStaticRunsForEveryShape(t *testing.T) {
	t.Parallel()
	zones := []string{"UTC", "America/New_York", "Australia/Lord_Howe"}
	moments := []moment{
		{2026, 9, 29, 12, 0},
		{2026, 3, 9, 0, 30}, // the day after New York's clocks went forward
		{2026, 10, 5, 0, 0}, // just after Lord Howe's and Sydney's clocks went forward
		{2026, 1, 1, 0, 0},
	}
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range moments {
			if testing.Short() {
				// Keep New Year in each zone plus its DST transition; UTC has
				// the ordinary September run. All shapes and window cycles stay.
				season := map[string]int{"UTC": 9, "America/New_York": 3, "Australia/Lord_Howe": 10}[zone]
				if at.month != 1 && at.month != season {
					continue
				}
			}
			now := time.Date(at.year, time.Month(at.month), at.day, at.hour, at.minute, 0, 0, loc)
			for _, shape := range statsShapes() {
				var metas []archive.Metadata
				for _, s := range shape.build(now, loc) {
					metas = append(metas, s.build())
				}
				// 7 and 14 cycle 7, (14,) 30, 90; 200 adds a window longer than the
				// six months of the month rank.
				for _, start := range []int{7, 14, 200} {
					windows, _ := statsWindowCycle(start)
					shared := fetched(metas, now, loc, windows)
					prepared := stats.Prepare(shared, stats.PrepareOptions{Location: loc})
					for _, days := range windows {
						alone := fetched(metas, now, loc, []int{days})
						got, err := json.Marshal(statsInputs{hasSessions: len(shared) > 0, prepared: prepared, now: now, location: loc}.compute(days, true))
						if err != nil {
							t.Fatal(err)
						}
						want, err := json.Marshal(statsInputs{hasSessions: len(alone) > 0, prepared: stats.Prepare(alone, stats.PrepareOptions{Location: loc}), now: now, location: loc}.compute(days, true))
						if err != nil {
							t.Fatal(err)
						}
						if string(got) != string(want) {
							t.Fatalf("%s %s at %v: window %d read for the cycle from %d differs from a static --days %d:\n%s\n---- static:\n%s", zone, shape.name, now, days, start, days, got, want)
						}
						if shape.name == "old descendants of recent roots" {
							complete, err := json.Marshal(statsInputs{hasSessions: len(metas) > 0, prepared: stats.Prepare(metas, stats.PrepareOptions{Location: loc}), now: now, location: loc}.compute(days, true))
							if err != nil || string(got) != string(complete) {
								t.Fatalf("%s window %d: old descendants differ from complete engine accounting: %v", zone, days, err)
							}
						}
					}
				}
			}
		}
	}
}

// Early in a month the month rank's five months reach back less far than the
// previous period of a 90-day window (which reaches 179 days). The screen
// reads for its longest window, so w to 90d is still the static 90-day screen:
// sessions from before April are in its previous period on 1 September.
func TestStatsInteractiveReadsForTheLongestWindowEarlyInAMonth(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	env.Now = func() time.Time { return now }
	for i, captured := range []time.Time{
		time.Date(2026, time.March, 10, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.March, 20, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.June, 10, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.August, 5, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.August, 20, 9, 0, 0, 0, time.UTC),
	} {
		syntheticSession{
			id: fmt.Sprintf("early-%d", i), harness: "claude", project: "agent-archive", captured: captured,
			models: []string{"claude-opus-5"}, turns: 4, messages: 20, toolResults: 10,
			perModel: []modelTokenSpec{{"claude-opus-5", 10_000, 50_000, 1_000_000, 100_000}},
		}.publish(t, mem)
	}
	static := beforeFooter(strings.Split(mustRunStats(t, env, 100, "--prices", goldenPrices, "--days", "90"), "\n"))
	run := runStatsOnTerminalWith(t, env, mem, fixedTerminal{100, 100}, []string{"w", "q"}, nil)
	if run.code != 0 || len(run.frames) != 2 {
		t.Fatalf("code %d, %d frames, stderr %q", run.code, len(run.frames), run.stderr)
	}
	got := beforeFooter(run.frames[1][:len(run.frames[1])-1])
	if strings.Join(got, "\n") != strings.Join(static, "\n") {
		t.Errorf("w to 90 days is not the static --days 90 screen:\n%s\n---- static:\n%s", strings.Join(got, "\n"), strings.Join(static, "\n"))
	}
	if !strings.Contains(strings.Join(static, "\n"), "vs prior 90d") {
		t.Errorf("the static screen has no previous period; the test proves nothing:\n%s", strings.Join(static, "\n"))
	}
}

// sgrSequence is a color or style sequence, the only escape the screen writes
// inside a frame's rows.
var sgrSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// hostileInputs are sessions with names meant to break a screen: wide and
// combining characters, an escape sequence, a very long name, right to left
// text and a control character.
func hostileInputs() statsInputs {
	names := []string{
		"日本語プロジェクト日本語プロジェクト日本語プロジェクト", "\x1b[31mred\x1b[0m", strings.Repeat("long-", 60), "éééé",
		"مشروع", "😀😀😀😀😀😀😀", "tab\there", "nul\x00byte",
	}
	var sessions []archive.Metadata
	for i, name := range names {
		for j := range 3 {
			var perModel []modelTokenSpec
			if j != 2 {
				perModel = []modelTokenSpec{{"claude-opus-5", 1000, 1000, 100_000, 100}}
			}
			sessions = append(sessions, syntheticSession{
				id: fmt.Sprintf("hostile-%d-%d", i, j), harness: []string{"claude", "codex", "cursor"}[j], project: name,
				captured: statsNow.AddDate(0, 0, -j*3-i), models: []string{"claude-opus-5"}, turns: 3, messages: 10, toolResults: 5,
				skills: []string{name}, mcp: map[string]int{name: 2}, perModel: perModel,
			}.build())
		}
	}
	return statsInputs{hasSessions: len(sessions) > 0, prepared: stats.Prepare(sessions, stats.PrepareOptions{Location: statsNow.Location()}), now: statsNow, location: statsNow.Location()}
}

// Whatever the terminal's size and however hostile the names, every frame is
// exactly the terminal's rows, none wider than its columns, on every view,
// the help and the save prompt.
func TestStatsScreenNeverExceedsTheTerminalWhateverTheNames(t *testing.T) {
	t.Parallel()
	inputs := hostileInputs()
	filters := statsFilters{Harness: "claude", Model: "日本語日本語日本語日本語日本語日本語\x1b[31m" + strings.Repeat("m", 300)}
	widths := []int{1, 20, 39, 40, 59, 60, 79, 80, 100, 120, 250}
	heights := []int{1, 2, 5, 24, 60}
	if testing.Short() {
		// Retain degenerate, footer-only and normal-height frames, all
		// views, color, hostile names and layout threshold neighbors.
		widths = []int{1, 39, 40, 41, 59, 60, 61, 79, 80, 81, 119, 120, 121, 250}
		heights = []int{1, 2, 24}
	}
	for _, width := range widths {
		for _, height := range heights {
			// Color adds escape sequences to every row: one height is enough.
			colors := []bool{false}
			if height == 24 {
				colors = append(colors, true)
			}
			for _, color := range colors {
				run := runScreen(t, screenOptions{width: width, height: height, color: color, inputs: &inputs, filters: filters},
					"d", "p", "m", "a", "o", "\x1b[6~", "?", "\x1b[6~", "x", "h", "日本語日本語日本語", "\x1b", "w", "\x1b[F", "\x1b[H", "q")
				if len(run.frames) < 12 {
					t.Fatalf("%dx%d: %d frames", width, height, len(run.frames))
				}
				for i, rows := range run.frames {
					if len(rows) != height {
						t.Fatalf("%dx%d frame %d: %d rows", width, height, i, len(rows))
					}
					for _, row := range rows {
						if w := visibleWidth(row); w > width {
							t.Fatalf("%dx%d frame %d: a row is %d columns: %q", width, height, i, w, row)
						}
						if strings.ContainsAny(sgrSequence.ReplaceAllString(row, ""), "\x1b\x07\x00") {
							t.Fatalf("%dx%d frame %d: an escape sequence from a name reached the screen: %q", width, height, i, row)
						}
					}
				}
			}
		}
	}
}
