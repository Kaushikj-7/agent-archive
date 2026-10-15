package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// layoutSessions are sessions of every shape a row has to fit: long and
// short titles, a wide-character title, a raw prompt listTitle cleans, a
// long project name, PRs, three agents, and times from minutes to a past
// year. The content is invented.
func layoutSessions() []archive.Metadata {
	type session struct {
		id      string
		harness string
		project string
		title   string
		pr      int
		ago     time.Duration
	}
	list := []session{
		{"d7a77938", "claude", "agent-archive", "Review historical project command", 505, 2 * time.Minute},
		{"36a7d5ee", "claude", "built-in-browser", "Cloud session archive/retrieval options and the many trade-offs between them that we discussed", 514, 13 * time.Hour},
		{"76941c69", "codex", "agent-archive", "Better loading indicator when running a long search over a very large archive", 0, 50 * time.Hour},
		{"5b0c1e22", "cursor", "a-project-with-an-unusually-long-directory-name", "# Files pasted by the user: list_table.go, list_format.go", 1234, 9 * 24 * time.Hour},
		{"9f3a64d0", "claude", "agent-archive", "修复上传队列中的重试预算并更新相关文档和测试", 0, 400 * 24 * time.Hour},
		{"0c1d2e3f", "claude", "agent-archive", "", 0, 3 * time.Hour},
	}
	sessions := make([]archive.Metadata, len(list))
	for i, s := range list {
		sessions[i] = archive.Metadata{
			SessionID: s.id + strings.Repeat("0", 24), Title: s.title, Harness: archive.Harness{Name: s.harness},
			ProjectName: s.project, ProjectID: "project-" + s.project, CapturedAt: pickerNow.Add(-s.ago),
		}
		if s.pr > 0 {
			sessions[i].GitActivity = []archive.GitEvent{{Kind: archive.GitEventPRCreated, PRNumber: s.pr}}
		}
	}
	return sessions
}

// layoutRows are layoutSessions as a numbered picker shows them, with a
// live session and one with subagents.
func layoutRows(t *testing.T) ([]listRow, listFormatOptions) {
	t.Helper()
	format := listFormatOptions{Now: pickerNow, Numbered: true, Children: map[string]int{childKey("claude", "d7a77938"+strings.Repeat("0", 24)): 4}}
	rows := formatSessionRows(layoutSessions(), format)
	rows[0].Live = true
	laid, _ := format.withColumns(rows)
	return rows, laid
}

// On a terminal of any width every row is exactly one line no wider than
// the terminal, and the columns that do not fit go in order: the project
// (once keeping it would leave the title under projectTitleWidth), then the
// PR, then the agent's name. The title column stays between 24 and 56
// columns while there is room.
func TestSessionRowsFitTheTerminalWidth(t *testing.T) {
	t.Parallel()
	rows, laid := layoutRows(t)
	cases := []struct {
		width     int
		project   bool
		pr        bool
		agentName bool
		title     int
	}{
		{width: 220, project: true, pr: true, agentName: true, title: maxTitleWidth},
		{width: 140, project: true, pr: true, agentName: true, title: maxTitleWidth},
		{width: 120, project: true, pr: true, agentName: true, title: maxTitleWidth},
		{width: 100, project: true, pr: true, agentName: true, title: 42},
		{width: 94, project: true, pr: true, agentName: true, title: projectTitleWidth},
		{width: 93, project: false, pr: true, agentName: true, title: maxTitleWidth},
		{width: 80, project: false, pr: true, agentName: true, title: 48},
		{width: 60, project: false, pr: true, agentName: true, title: 28},
		{width: 50, project: false, pr: false, agentName: true, title: 25},
		{width: 40, project: false, pr: false, agentName: false, title: 22},
		{width: 20, project: false, pr: false, agentName: false, title: 2},
	}
	for _, c := range cases {
		layout := newSessionLayout(rows, listFormatOptions{Width: c.width, Numbered: true, ShowPR: laid.ShowPR})
		if layout.project != c.project || layout.pr != c.pr || layout.agentName != c.agentName || layout.title != c.title {
			t.Errorf("width %d: project=%v pr=%v agent name=%v title=%d, want %v %v %v %d",
				c.width, layout.project, layout.pr, layout.agentName, layout.title, c.project, c.pr, c.agentName, c.title)
		}
		for _, variant := range []struct {
			name       string
			cursor     bool
			color      bool
			groupByDay bool
		}{{name: "plain"}, {name: "cursor", cursor: true}, {name: "color", color: true}, {name: "grouped", groupByDay: true, color: true}} {
			format := laid
			format.Width, format.Cursor, format.GroupByDay = c.width, variant.cursor, variant.groupByDay
			format.Style = textStyle{color: variant.color}
			shown := rows
			if variant.cursor {
				shown = append([]listRow(nil), rows...)
				shown[1].Highlight = true
				child := shown[0]
				child.Depth, child.Index = 1, 0
				shown = append(shown, child)
			}
			groups := sessionTableGroups(shown, format)
			var out bytes.Buffer
			if err := printSessionGroups(&out, groups, format); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			if len(lines) != sessionLines(groups) || displayLines(text, c.width) != len(lines) {
				t.Errorf("width %d %s: %d lines, counted %d, %d on screen:\n%s", c.width, variant.name, len(lines), sessionLines(groups), displayLines(text, c.width), text)
			}
			for _, line := range lines {
				if visibleWidth(line) > c.width {
					t.Errorf("width %d %s: %d columns: %q", c.width, variant.name, visibleWidth(line), line)
				}
			}
			if variant.name == "plain" {
				t.Logf("width %d:\n%s", c.width, text)
			}
		}
	}
}

// With the usual project names, an 80-column terminal keeps the project
// column and shrinks the title toward projectTitleWidth instead; wider
// terminals give the title its most.
func TestSessionRowsKeepTheProjectWhenThereIsRoom(t *testing.T) {
	t.Parallel()
	rows, laid := layoutRows(t)
	rows = rows[:3] // agent-archive and built-in-browser, PRs, recent times
	for _, c := range []struct {
		width int
		title int
	}{{80, 37}, {100, maxTitleWidth}, {120, maxTitleWidth}} {
		layout := newSessionLayout(rows, listFormatOptions{Width: c.width, Numbered: true, ShowPR: laid.ShowPR})
		if !layout.project || !layout.pr || !layout.agentName || layout.title != c.title {
			t.Errorf("width %d: project=%v pr=%v agent name=%v title=%d, want all columns and title %d",
				c.width, layout.project, layout.pr, layout.agentName, layout.title, c.title)
		}
		for _, r := range rows {
			if line := layout.renderRow(r, textStyle{}); visibleWidth(line) > c.width || !strings.Contains(line, r.Project) {
				t.Errorf("width %d: %q", c.width, line)
			}
		}
	}
}

// A width-aware row keeps the cursor mark and a subagent's indent, cuts a
// long title at a word, has no ID, and reads "live" for WHEN while the
// session is active, in green with color and as the word without.
func TestSessionRowMarks(t *testing.T) {
	t.Parallel()
	rows, laid := layoutRows(t)
	laid.Width, laid.Cursor = 100, true
	rows[0].Highlight = true
	child := rows[2]
	child.Depth, child.Index = 1, 0
	shown := []listRow{rows[0], child, rows[1]}
	var out bytes.Buffer
	if err := printSessionGroups(&out, sessionTableGroups(shown, laid), laid); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines:\n%s", out.String())
	}
	if !strings.HasPrefix(lines[0], cursorMark+" 1  "+agentDot+" Claude  Review historical project command +4 ") || !strings.HasSuffix(lines[0], "  "+liveWhen) {
		t.Errorf("highlighted live row with subagents: %q", lines[0])
	}
	if strings.Count(lines[0], agentDot) != 1 {
		t.Errorf("a second dot on the live row: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "     "+agentDot+" Codex     "+subagentMark+" Better loading indicator when running a long…") {
		t.Errorf("subagent under its parent: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "  2  "+agentDot+" Claude  Cloud session archive/retrieval options and the many…") {
		t.Errorf("cut title: %q", lines[2])
	}
	for _, line := range lines {
		if strings.Contains(line, "d7a77938") || strings.Contains(line, "36a7d5ee") {
			t.Errorf("an interactive row shows an ID: %q", line)
		}
	}
	laid.Style = textStyle{color: true}
	out.Reset()
	if err := printSessionGroups(&out, sessionTableGroups(shown, laid), laid); err != nil {
		t.Fatal(err)
	}
	colored := strings.Split(out.String(), "\n")
	if !strings.HasSuffix(colored[0], laid.Style.ok(liveWhen)) || strings.Contains(colored[2], liveWhen) {
		t.Errorf("live in color: %q", colored[0])
	}
}

// The subagent count follows the title only where it fits without cutting
// it. A row's hint ("not uploaded") always shows: the title is cut to make
// room for it, and only a terminal too narrow for the hint itself cuts it.
func TestSessionRowHints(t *testing.T) {
	t.Parallel()
	row := listRow{Index: 1, HarnessKey: "claude", Title: "Short title", WhenShort: "2m", Children: 4, Hint: notUploadedHint, ShortHint: notUploadedShortHint}
	format := listFormatOptions{Width: 80, Numbered: true, HideProject: true}
	wide := newSessionLayout([]listRow{row}, format).renderRow(row, textStyle{})
	if !strings.Contains(wide, "Short title +4 · not uploaded") {
		t.Errorf("room for both notes: %q", wide)
	}
	row.Title = strings.Repeat("word ", 20) + "end"
	for _, width := range []int{80, 40, 30} {
		format.Width = width
		cut := newSessionLayout([]listRow{row}, format).renderRow(row, textStyle{})
		if !strings.Contains(cut, "word… · not uploaded") || strings.Contains(cut, "+4") || visibleWidth(cut) > width {
			t.Errorf("width %d: a long title is cut for the hint, and the count goes: %q", width, cut)
		}
		if strings.Contains(cut, "subagent") || strings.Contains(cut, notUploadedHint) {
			t.Errorf("width %d: a row spells out a note: %q", width, cut)
		}
	}
	format.Width = 18
	narrow := newSessionLayout([]listRow{row}, format).renderRow(row, textStyle{color: true})
	if !strings.Contains(narrow, "· not") || visibleWidth(narrow) > 18 {
		t.Errorf("too narrow for any title: %q", narrow)
	}
	row.Hint, row.ShortHint = "preview unavailable", ""
	format.Width = 60
	if native := newSessionLayout([]listRow{row}, format).renderRow(row, textStyle{}); !strings.Contains(native, "… · preview unavailable") {
		t.Errorf("a hint with no short form: %q", native)
	}
}

// cutAtWord ends a cut title with "…" after its last whole word, unless that
// would waste more than half the room.
func TestCutAtWord(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text  string
		limit int
		want  string
	}{
		{"Fix the retry budget", 40, "Fix the retry budget"},
		{"Fix the retry budget", 20, "Fix the retry budget"},
		{"Fix the retry budget", 15, "Fix the retry…"},
		{"Fix the retry budget", 12, "Fix the…"},
		{"Supercalifragilistic word", 12, "Supercalifr…"},
		{"Fix: the retry budget", 6, "Fix…"},
		{"修复上传队列", 7, "修复上…"},
		{"anything", 1, "…"},
		{"anything", 0, ""},
	} {
		if got := cutAtWord(c.text, c.limit); got != c.want || visibleWidth(got) > c.limit {
			t.Errorf("cutAtWord(%q, %d) = %q, want %q", c.text, c.limit, got, c.want)
		}
	}
}

// Piped, the table is one plain line a row in the columns WHEN AGENT
// PROJECT PR ID TITLE, newest first with no project headings, and the title
// last and uncut.
func TestPipedListColumns(t *testing.T) {
	t.Parallel()
	sessions := layoutSessions()
	format := listFormatOptions{Now: pickerNow, GroupByDay: true}
	choices := newScopeChoices(sessionScope{}, format, true, archiveRows(sessions, defaultListLimit, format))
	var out bytes.Buffer
	if err := printListTable(&out, choices.shown(), 0); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if got := strings.Join(strings.Fields(lines[0]), " "); got != "WHEN AGENT PROJECT PR ID TITLE" {
		t.Fatalf("header %q:\n%s", got, out.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("color in the plain table:\n%s", out.String())
	}
	want := []string{
		"2m claude agent-archive #505 d7a77938 Review historical project command",
		"Mon claude built-in-browser #514 36a7d5ee Cloud session archive/retrieval options and the many trade-offs between them that we discussed",
		"Sun codex agent-archive - 76941c69 Better loading indicator when running a long search over a very large archive",
		"Sep 20 cursor a-project-with-an-unusually-long-directory-name #1234 5b0c1e22 # Files pasted by the user: list_table.go, list_format.go",
		"2025-08-25 claude agent-archive - 9f3a64d0 修复上传队列中的重试预算并更新相关文档和测试",
		"3h claude agent-archive - 0c1d2e3f 0c1d2e3f",
	}
	for i, line := range want {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); got != strings.Join(strings.Fields(line), " ") {
			t.Errorf("row %d:\n got %q\nwant %q", i+1, got, strings.Join(strings.Fields(line), " "))
		}
	}
	// Columns sit where the header names them: WHEN keeps its space
	// ("Sep 20"; scripts read --json), and TITLE is the rest of the line,
	// the session's own words rather than the readable list title.
	whenAt, titleAt := strings.Index(lines[0], "WHEN"), strings.Index(lines[0], "TITLE")
	row := lines[4]
	if got := strings.TrimSpace(row[whenAt:strings.Index(lines[0], "AGENT")]); got != "Sep 20" {
		t.Errorf("WHEN cell %q, want %q", got, "Sep 20")
	}
	if got := row[titleAt:]; got != "# Files pasted by the user: list_table.go, list_format.go" {
		t.Errorf("TITLE cell %q", got)
	}
}

// Piped, TITLE spells out the subagent note and the handoff hint after the
// full title; the width-aware cell shows the readable title and short forms.
func TestPipedTitleKeepsTheFullTitleAndNotes(t *testing.T) {
	t.Parallel()
	rows, laid := layoutRows(t)
	rows[3].Hint, rows[3].ShortHint = notUploadedHint, notUploadedShortHint
	var out bytes.Buffer
	if err := printSessionTable(&out, rows, laid); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Review historical project command · 4 subagents",
		"# Files pasted by the user: list_table.go, list_format.go · not yet uploaded",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plain table lacks %q:\n%s", want, out.String())
		}
	}
	laid.Width = 120
	got := newSessionLayout(rows, laid).renderRow(rows[3], textStyle{})
	if !strings.Contains(got, "Pasted files: list_table.go") || !strings.Contains(got, "· not uploaded") || strings.Contains(got, "# Files") {
		t.Errorf("width-aware row: %q", got)
	}
}

// The readable list title is made only from a session's own title: a row
// with none shows its short ID, and a replay keeps its mark. The verbose
// table keeps the full title.
func TestListTitleOnlyForDisplayTitles(t *testing.T) {
	t.Parallel()
	id := "abcdef01" + strings.Repeat("0", 24)
	sessions := []archive.Metadata{
		{SessionID: id, Title: "# files pasted by the user: a.go, b.go", Harness: archive.Harness{Name: "claude"}, CapturedAt: pickerNow},
		{SessionID: "12345678" + strings.Repeat("0", 24), Harness: archive.Harness{Name: "claude"}, CapturedAt: pickerNow},
		{SessionID: "99999999" + strings.Repeat("0", 24), Title: "> help me fix the build. then the docs", Harness: archive.Harness{Name: "claude"}, CapturedAt: pickerNow, Replay: &archive.Replay{}},
	}
	rows := formatSessionRows(sessions, listFormatOptions{Now: pickerNow})
	if rows[0].ListTitle != "Pasted files: a.go" || rows[0].Title != "# files pasted by the user: a.go, b.go" {
		t.Errorf("pasted files: list %q, full %q", rows[0].ListTitle, rows[0].Title)
	}
	if rows[1].ListTitle != "" || cellTitle(rows[1]) != "12345678" {
		t.Errorf("no title: list %q, cell %q", rows[1].ListTitle, cellTitle(rows[1]))
	}
	if rows[2].ListTitle != "[replay] Help me fix the build" {
		t.Errorf("replay: %q", rows[2].ListTitle)
	}
	native := listRow{Index: 1, HarnessKey: "codex", Title: "019a2b3c-native-id", WhenShort: "2m"}
	if got := newSessionLayout([]listRow{native}, listFormatOptions{Width: 100, HideProject: true}).renderRow(native, textStyle{}); !strings.Contains(got, "019a2b3c-native-id") {
		t.Errorf("native ID title: %q", got)
	}
	var out bytes.Buffer
	if err := printSessionTable(&out, rows, listFormatOptions{Verbose: true, Width: 100}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "# files pasted by the user: a.go, b.go") {
		t.Errorf("verbose table:\n%s", out.String())
	}
}

// Laid out for a known width, a table with no column header fits on one
// page when its rows take exactly the lines left above the footer and
// prompt; one more row splits it.
func TestSessionRowsFillTheScreenExactly(t *testing.T) {
	t.Parallel()
	rows, laid := layoutRows(t)
	laid.Width = 100
	groups := sessionTableGroups(rows, laid)
	question := "Enter number: "
	// The footer's line, the blank line and the prompt's line.
	chrome := 3
	l := &sessionPicker{}
	if pages := l.split(groups, laid, len(rows), laid.Width, len(rows)+chrome, "6 session(s).\n", question); len(pages) != 1 {
		t.Errorf("rows that fill the screen: %d pages, want 1: %v", len(pages), pages)
	}
	if pages := l.split(groups, laid, len(rows), laid.Width, len(rows)+chrome-1, "6 session(s).\n", question); len(pages) < 2 {
		t.Errorf("a row too many: %d pages, want 2 or more", len(pages))
	}
}
