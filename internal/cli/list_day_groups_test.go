package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// dayGroupsNow is a Wednesday afternoon: this week began on Monday the 12th.
var dayGroupsNow = time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)

// groupSummary is each group's heading, then the indices of its rows:
// "TODAY: 1 2 | OLDER (continued): 9".
func groupSummary(groups []sessionTableGroup) string {
	parts := make([]string, len(groups))
	for i, group := range groups {
		indices := make([]string, len(group.rows))
		for j, r := range group.rows {
			indices[j] = fmt.Sprint(r.Index)
		}
		parts[i] = groupHeading(group) + ": " + strings.Join(indices, " ")
	}
	return strings.Join(parts, " | ")
}

// dayRows are rows active at the given times, numbered from 1 in order.
func dayRows(times ...time.Time) []listRow {
	rows := make([]listRow, len(times))
	for i, at := range times {
		rows[i] = listRow{Index: i + 1, ShortID: fmt.Sprintf("%08x", i+1), Title: fmt.Sprintf("Session %d", i+1), Harness: "codex", Project: "app"}
		rows[i].setActive(dayGroupsNow, at)
	}
	return rows
}

// Rows go under the heading of their day, the days newest first, each
// day's rows in the order given; a zero time is Older.
func TestSessionTableGroupsByDay(t *testing.T) {
	t.Parallel()
	day := func(month time.Month, d, hour int) time.Time {
		return time.Date(2026, month, d, hour, 0, 0, 0, time.UTC)
	}
	rows := dayRows(
		day(10, 14, 14),                // 1 today
		day(10, 14, 0),                 // 2 today, at midnight
		day(10, 13, 23),                // 3 yesterday
		day(10, 12, 9),                 // 4 Monday: earlier this week
		day(10, 11, 22),                // 5 Sunday: last week
		day(10, 5, 0),                  // 6 last Monday: last week
		day(10, 4, 23),                 // 7 earlier this month
		day(10, 1, 0),                  // 8 earlier this month
		day(9, 30, 23),                 // 9 September: older
		time.Time{},                    // 10 never: older
		day(10, 14, 15),                // 11 today, though listed last
		dayGroupsNow.AddDate(-1, 0, 0), // 12 a year back: older
	)
	groups := sessionTableGroups(rows, listFormatOptions{Now: dayGroupsNow, GroupByDay: true})
	want := "TODAY: 1 2 11 | YESTERDAY: 3 | EARLIER THIS WEEK: 4 | LAST WEEK: 5 6 | EARLIER THIS MONTH: 7 8 | OLDER: 9 10 12"
	if got := groupSummary(groups); got != want {
		t.Fatalf("groups:\n%s\nwant\n%s", got, want)
	}

	// Not grouped: one group without a heading, rows as given.
	for _, format := range []listFormatOptions{{Now: dayGroupsNow}, {Now: dayGroupsNow, GroupByDay: true, Verbose: true}} {
		if groups := sessionTableGroups(rows, format); len(groups) != 1 || groups[0].label != "" || len(groups[0].rows) != len(rows) {
			t.Errorf("%+v: %s", format, groupSummary(groups))
		}
	}
}

// One day still gets its heading, the landmark the reader learns, and an
// empty table has none.
func TestSessionTableGroupsOneDay(t *testing.T) {
	t.Parallel()
	rows := dayRows(dayGroupsNow.Add(-time.Minute), dayGroupsNow.Add(-2*time.Hour))
	format := listFormatOptions{Now: dayGroupsNow, GroupByDay: true, Width: 80}
	var out bytes.Buffer
	if err := printSessionTable(&out, rows, format); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "TODAY" || !strings.Contains(lines[1], "Session 1") {
		t.Fatalf("one day:\n%s", out.String())
	}
	if groups := sessionTableGroups(nil, format); len(groups) != 1 || groups[0].label != "" {
		t.Fatalf("empty table: %s", groupSummary(groups))
	}
	// Never-active rows alone are Older.
	if got := groupSummary(sessionTableGroups(dayRows(time.Time{}, time.Time{}), format)); got != "OLDER: 1 2" {
		t.Fatalf("zero times: %s", got)
	}
}

// Each heading is one line, bold with color, with a blank line before every
// heading after the first, and sessionLines counts exactly the lines drawn.
func TestDayHeadingsTakeOneLineEach(t *testing.T) {
	t.Parallel()
	rows := dayRows(dayGroupsNow, dayGroupsNow.Add(-24*time.Hour), time.Time{})
	for _, color := range []bool{false, true} {
		for _, width := range []int{8, 20, 80} {
			format := listFormatOptions{Now: dayGroupsNow, GroupByDay: true, Width: width, Style: textStyle{color: color}}
			groups := sessionTableGroups(rows, format)
			var out bytes.Buffer
			if err := printSessionGroups(&out, pageSessionGroups(groups, pickerPage{1, 3}), format); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if n := displayLines(text, width); n != sessionLines(pageSessionGroups(groups, pickerPage{1, 3})) || n != 5 {
				t.Errorf("color=%v width=%d: %d lines:\n%s", color, width, n, text)
			}
			lines := strings.Split(text, "\n")
			if lines[2] != "" {
				t.Errorf("no blank line between days:\n%s", text)
			}
			if color && !strings.HasPrefix(lines[0], "\x1b[1m") {
				t.Errorf("heading not bold: %q", lines[0])
			}
			if !color && width >= 20 && (lines[0] != "YESTERDAY" || lines[3] != "OLDER") {
				t.Errorf("headings: %q", lines)
			}
		}
	}
}

// The plain table a script reads is one flat table, newest first, with no
// day headings.
func TestPipedListHasNoDayHeadings(t *testing.T) {
	t.Parallel()
	rows := dayRows(dayGroupsNow, dayGroupsNow.Add(-24*time.Hour), time.Time{})
	c := &scopeChoice{scopeView: scopeView{rows: rows, total: len(rows)}, format: listFormatOptions{Now: dayGroupsNow, GroupByDay: true}}
	var out bytes.Buffer
	if err := printListTable(&out, c, 0); err != nil {
		t.Fatal(err)
	}
	for _, heading := range []string{"TODAY", "YESTERDAY", "OLDER"} {
		if strings.Contains(out.String(), heading) {
			t.Fatalf("piped table has %s:\n%s", heading, out.String())
		}
	}
	out.Reset()
	if err := printListTable(&out, c, 80); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "TODAY\n") || !strings.Contains(out.String(), "\n\nOLDER\n") {
		t.Fatalf("terminal table:\n%s", out.String())
	}
}

// The filter's matches are grouped by day too, in the order they matched
// within a day, a subagent staying under its parent in the parent's day.
func TestFilterResultsAreGroupedByDay(t *testing.T) {
	t.Parallel()
	rows := dayRows(
		dayGroupsNow.Add(-time.Hour),    // 1 fix: today
		dayGroupsNow.Add(-2*time.Hour),  // 2 other: today
		dayGroupsNow.Add(-24*time.Hour), // 3 fix: yesterday
		time.Time{},                     // 4 fix: older
		dayGroupsNow.Add(-3*time.Hour),  // 5 fix: today, listed after the rest
		dayGroupsNow.Add(-time.Minute),  // 6 fix: today, a subagent of 4
	)
	for i, title := range []string{"fix one", "other", "fix three", "fix four", "fix five", "fix six"} {
		rows[i].fields = sessionFields{Title: title, Harness: "codex", SessionID: rows[i].ShortID}
		rows[i].SessionID, rows[i].HarnessKey = rows[i].ShortID, "codex"
	}
	rows[5].parentID, rows[5].Index = rows[3].SessionID, 0
	l := &sessionPicker{}
	l.setFilter("fix")
	v := l.view(rows, len(rows), false, listFormatOptions{Now: dayGroupsNow, GroupByDay: true, Numbered: true})
	if got, want := groupSummary(v.groups), "TODAY: 1 5 | YESTERDAY: 3 | OLDER: 4 0"; got != want {
		t.Fatalf("filtered groups %s, want %s", got, want)
	}
	if last := v.order[len(v.order)-1]; last.Depth != 1 || last.SessionID != rows[5].SessionID {
		t.Fatalf("subagent not under its parent: %+v", v.order)
	}
}

// A handoff row is filed under the day of its latest activity, which can be
// newer than its metadata says: a live local session's metadata may date
// from its capture.
func TestHandoffRowsGroupByLatestActivity(t *testing.T) {
	t.Parallel()
	captured := dayGroupsNow.AddDate(0, -2, 0)
	rows := []handoffPickerRow{
		{metadata: archive.Metadata{SessionID: "live-one", Harness: archive.Harness{Name: "codex"}, CapturedAt: captured}, active: dayGroupsNow.Add(-time.Hour)},
		{metadata: archive.Metadata{SessionID: "old-two", Harness: archive.Harness{Name: "codex"}, CapturedAt: captured}, active: captured},
	}
	format := listFormatOptions{Now: dayGroupsNow, GroupByDay: true, Numbered: true}
	if got, want := groupSummary(sessionTableGroups(formatHandoffRows(rows, format), format)), "TODAY: 1 | OLDER: 2"; got != want {
		t.Fatalf("handoff groups %s, want %s", got, want)
	}
}
