package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func TestBrowseRejectsSessionIDSharedByHarnesses(t *testing.T) {
	t.Parallel()
	const id = "abcdef0123456789abcdef0123456789"
	rows := []listRow{
		{Index: 1, SessionID: id, ShortID: id, HarnessKey: "codex"},
		{Index: 2, SessionID: id, ShortID: id, HarnessKey: "claude"},
	}
	for _, answer := range []string{id, id[:minShortSessionID]} {
		if _, ok := matchBrowseRow(answer, rows); ok {
			t.Fatalf("ambiguous ID %q selected a harness", answer)
		}
	}
	if row, ok := matchBrowseRow("2", rows); !ok || row.HarnessKey != "claude" {
		t.Fatalf("numbered selection failed: row=%+v ok=%t", row, ok)
	}
}

func TestListDefaultTableIsHumanReadable(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "TITLE") || !strings.Contains(text, "WHEN") || !strings.Contains(text, "PROJECT") || !strings.Contains(text, "ID") {
		t.Fatalf("missing human headers:\n%s", text)
	}
	if strings.Contains(text, "ORIGIN") || strings.Contains(text, "PARSER") || strings.Contains(text, "CAPTURED") {
		t.Fatalf("ops columns should be verbose-only:\n%s", text)
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	if !strings.Contains(text, short) || !strings.Contains(text, "now") || !strings.Contains(text, "codex") {
		t.Fatalf("missing short id / relative time / harness:\n%s", text)
	}
	if strings.Contains(text, "visible") {
		t.Fatalf("list printed transcript content:\n%s", text)
	}
}

func TestShowAcceptsShortIDPrintedByList(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id[:minShortSessionID]}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("show short ID: code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) {
		t.Fatalf("show short ID returned wrong session: %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id[:minShortSessionID], "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("show short ID with harness: code=%d stderr=%s", code, errOut.String())
	}
}

func TestListVerboseKeepsOpsColumns(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{id, "ORIGIN", "PARSER", "CAPTURED", "2026-01-02T00:00:00Z", "hook"} {
		if !strings.Contains(text, want) {
			t.Fatalf("verbose missing %q:\n%s", want, text)
		}
	}
}

func TestListInteractiveShowAndQuit(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader("1\nq\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	if code := Run([]string{"list"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	text := out.String()
	if !strings.Contains(text, "#") || !strings.Contains(text, "Enter number") {
		t.Fatalf("expected numbered interactive prompt:\n%s", text)
	}
	// The summary viewed last is printed again after the alternate screen
	// is left, so its ID stays in scrollback.
	_, after, ok := strings.Cut(text, leaveAltScreenSequence)
	if !ok || !strings.Contains(after, "ID "+id) {
		t.Fatalf("summary not printed after leaving the browser:\n%q", text)
	}
	if strings.Contains(text, "visible") {
		t.Fatalf("browser printed transcript content without t:\n%s", text)
	}
}

// Bare show on a terminal is the same browser as list, and loops.
func TestShowWithoutIDInteractiveBrowses(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader("1\nb\n1\nq\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	if code := Run([]string{"show"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	if strings.Count(out.String(), "Enter number") != 2 || !strings.Contains(out.String(), "ID "+id) {
		t.Fatalf("show should browse twice:\n%s", out.String())
	}
}

// show --json on a terminal opens the browser to pick one session (Show ·),
// and prints the chosen sidecar once it has left the screen.
func TestShowJSONWithoutIDPicksOnce(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader("1\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	if code := Run([]string{"show", "--json"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	var meta archiveMetadataSessionID
	if err := json.Unmarshal([]byte(extractJSONObject(out.String())), &meta); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	jsonAt := strings.Index(out.String(), "{")
	left := strings.LastIndex(out.String(), leaveAltScreenSequence)
	if meta.SessionID != id || strings.Count(out.String(), "Enter number") != 1 || !strings.Contains(out.String(), "Show · 1 session") || left < 0 || left > jsonAt {
		t.Fatalf("got %q want %q:\n%s", meta.SessionID, id, out.String())
	}
}

func TestShowWithoutIDNonInteractiveStillRequiresID(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"show"}, nil, &out, &errOut, env); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

type archiveMetadataSessionID struct {
	SessionID string `json:"session_id"`
}

// extractJSONObject returns the first top-level JSON object in text.
func extractJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return text
	}
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return text[start:]
}

// fixedTerminal is a terminal of a fixed size; a zero size is unknown.
type fixedTerminal struct {
	width  int
	height int
}

func (f fixedTerminal) terminalSize(io.Writer) (int, int, bool) {
	return f.width, f.height, f.width > 0 && f.height > 0
}

// resizingTerminal answers each size read with the next size, keeping the
// last.
type resizingTerminal struct {
	sizes []fixedTerminal
	reads int
}

func (r *resizingTerminal) terminalSize(out io.Writer) (int, int, bool) {
	size := r.sizes[min(r.reads, len(r.sizes)-1)]
	r.reads++
	return size.terminalSize(out)
}

var pickerNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// pickerSessions is n sessions, newest first, in the projects project names.
func pickerSessions(n int, project func(i int) string) []archive.Metadata {
	sessions := make([]archive.Metadata, n)
	for i := range sessions {
		sessions[i] = archive.Metadata{
			SessionID:   fmt.Sprintf("%08x%024x", 0x1a2b0000+i*7919, i),
			Title:       fmt.Sprintf("Session number %d", i+1),
			Harness:     archive.Harness{Name: "codex"},
			ProjectName: project(i),
			ProjectID:   "id-" + project(i),
			CapturedAt:  pickerNow.Add(-time.Duration(i) * time.Hour),
		}
	}
	return sessions
}

func oneProject(int) string { return "app" }

// screenBreak stands in for clearing the screen, so a test can split the
// output into what each redraw showed.
const screenBreak = "\f"

// runPicker runs picker on input and returns the chosen row and what each
// screen showed.
func runSessionPicker(t *testing.T, picker *sessionPicker, sessions []archive.Metadata, format listFormatOptions, input string) (row listRow, ok bool, screens []string) {
	t.Helper()
	var out bytes.Buffer
	picker.clear = func() { out.WriteString(screenBreak) }
	format.Now = pickerNow
	row, ok, err := picker.pick(newPrompter(strings.NewReader(input), &out), &out, sessions, format, "show")
	if err != nil {
		t.Fatal(err)
	}
	return row, ok, strings.Split(out.String(), screenBreak)
}

// checkScreensFit fails when a screen, as far as its first prompt, is
// taller than height rows on a terminal width columns wide.
func checkScreensFit(t *testing.T, screens []string, width, height int) {
	t.Helper()
	for i, screen := range screens {
		// What follows the first answer is printed below it.
		if at := strings.Index(screen, " to quit: "); at >= 0 {
			screen = screen[:at+len(" to quit: ")]
		}
		if n := displayLines(screen, width); n > height {
			t.Errorf("screen %d takes %d rows of %d:\n%s", i, n, height, screen)
		}
	}
}

// rowNumbers is the row numbers a screen of the picker lists, top to
// bottom.
func rowNumbers(screen string) []int {
	var numbers []int
	for line := range strings.SplitSeq(screen, "\n") {
		if !strings.Contains(line, "Session number") {
			continue
		}
		// The number is right-aligned, after the filter's cursor mark when
		// there is one.
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), cursorMark))
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.Atoi(fields[0]); err == nil {
			numbers = append(numbers, n)
		}
	}
	return numbers
}

// rowSpan is the first and last row number a screen lists, as "first-last".
func rowSpan(screen string) string {
	numbers := rowNumbers(screen)
	if len(numbers) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d-%d", numbers[0], numbers[len(numbers)-1])
}

func TestPickerPagesATableTallerThanTheTerminal(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	row, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{}, "n\nn\np\n37\n")
	if !ok || row.Index != 37 || row.SessionID != sessions[36].SessionID {
		t.Fatalf("picked %+v ok=%v", row, ok)
	}
	if len(screens) != 4 {
		t.Fatalf("%d screens, want 4:\n%s", len(screens), strings.Join(screens, "\n----\n"))
	}
	checkScreensFit(t, screens, 120, 20)
	// The footer, page line, blank line, and prompt leave 16 rows: 16
	// sessions, one line each.
	if rowSpan(screens[0]) != "1-16" || !strings.Contains(screens[0], "50 session(s).\nPage 1 of 4 · 50 sessions · [n] next\n") || strings.Contains(screens[0], "[p]") {
		t.Fatalf("first page:\n%s", screens[0])
	}
	if rowSpan(screens[1]) != "17-32" || !strings.Contains(screens[1], "Page 2 of 4 · 50 sessions · [n] next  [p] previous\n") {
		t.Fatalf("second page:\n%s", screens[1])
	}
	if rowSpan(screens[2]) != "33-48" || screens[3] != screens[1] {
		t.Fatalf("n then p did not return to the second page:\n%s\n----\n%s", screens[2], screens[3])
	}
}

// In the grouped table a page's rows are not a numeric range when the rows
// given are not newest first, so the page line counts pages instead of
// naming row numbers.
func TestPickerPageLineNamesNoRowRangeWhenDaysInterleave(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	for i := range sessions {
		sessions[i].CapturedAt = pickerNow.Add(-time.Duration(i%2) * 72 * time.Hour)
	}
	_, _, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{GroupByDay: true}, "n\nq\n")
	checkScreensFit(t, screens, 120, 19)
	first := rowNumbers(screens[0])
	if len(first) < 2 || first[1] != 3 || !strings.HasPrefix(screens[0], "TODAY\n") {
		t.Fatalf("first page does not list today's rows 1, 3, …:\n%s", screens[0])
	}
	for i, screen := range screens {
		if strings.Contains(screen, "Sessions ") || !strings.Contains(screen, fmt.Sprintf("Page %d of 3 · 30 sessions ·", i+1)) {
			t.Fatalf("page %d:\n%s", i+1, screen)
		}
	}
}

func TestPickerAcceptsAnyRowFromAnyPage(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	rows := formatSessionRows(sessions, listFormatOptions{Now: pickerNow})
	for answer, want := range map[string]int{"46": 46, rows[45].ShortID: 46, "2": 2} {
		row, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{}, "n\n"+answer+"\n")
		if !ok || row.Index != want {
			t.Errorf("%q from the second page picked %+v ok=%v:\n%s", answer, row, ok, strings.Join(screens, "\n----\n"))
		}
	}
}

// On a screen that can be cleared, a message about an answer replaces the
// blank line above the prompt of the page drawn again, so the page still
// fits instead of scrolling its top away.
func TestPickerStopsAtTheFirstAndLastPage(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(12, oneProject)
	_, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 9}}, sessions, listFormatOptions{}, "p\nn\nn\nn\nq\n")
	if ok || len(screens) != 5 {
		t.Fatalf("ok=%v, %d screens:\n%s", ok, len(screens), strings.Join(screens, "\n----\n"))
	}
	checkScreensFit(t, screens, 120, 9)
	// 12 rows, 5 to a page: n works twice.
	for i, want := range []struct {
		span string
		line string
	}{
		{"1-5", "Page 1 of 3 · 12 sessions · [n] next\n\nEnter number"},
		{"1-5", "[n] next\nThis is the first page; n goes on.\nEnter number"},
		{"6-10", "Page 2 of 3 · 12 sessions · [n] next  [p] previous\n\nEnter number"},
		{"11-12", "Page 3 of 3 · 12 sessions · [p] previous\n\nEnter number"},
		{"11-12", "[p] previous\nThis is the last page; p goes back.\nEnter number"},
	} {
		if rowSpan(screens[i]) != want.span || !strings.Contains(screens[i], want.line) {
			t.Fatalf("screen %d, want rows %s and %q:\n%s", i, want.span, want.line, screens[i])
		}
	}
}

// On the normal screen (a picker that cannot clear), messages are printed
// below the prompt as before.
func TestPickerOnTheNormalScreenPrintsMessagesBelow(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	format := listFormatOptions{Now: pickerNow}
	_, _, err := (&sessionPicker{env: fixedTerminal{120, 9}}).pick(newPrompter(strings.NewReader("p\nn\nn\nn\nq\n"), &out), &out, pickerSessions(12, oneProject), format, "show")
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Count(text, "Page 1 of 3") != 1 || !strings.Contains(text, "to quit: This is the first page; n goes on.\n\nEnter number") || !strings.Contains(text, "to quit: This is the last page; p goes back.\n\nEnter number") {
		t.Fatalf("output:\n%s", text)
	}
}

// A page that starts partway through a day repeats the day's heading,
// marked continued. The 30 sessions an hour apart from noon are 13 of today
// and 17 of yesterday.
func TestPickerRepeatsTheDayHeadingOnAPageStartingMidDay(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	_, _, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 13}}, sessions, listFormatOptions{GroupByDay: true}, "n\nn\nn\nq\n")
	checkScreensFit(t, screens, 120, 13)
	if len(screens) != 4 {
		t.Fatalf("%d screens:\n%s", len(screens), strings.Join(screens, "\n----\n"))
	}
	if !strings.HasPrefix(screens[0], "TODAY\n 1  ") {
		t.Fatalf("first page:\n%s", screens[0])
	}
	if !strings.HasPrefix(screens[1], "TODAY (continued)\n") || !strings.Contains(screens[1], "\n\nYESTERDAY\n") {
		t.Fatalf("second page:\n%s", screens[1])
	}
	if !strings.HasPrefix(screens[2], "YESTERDAY (continued)\n") || !strings.HasPrefix(screens[3], "YESTERDAY (continued)\n") {
		t.Fatalf("later pages:\n%s\n----\n%s", screens[2], screens[3])
	}
	// Every row is on exactly one page, in order.
	var all []int
	for _, screen := range screens {
		all = append(all, rowNumbers(screen)...)
	}
	for i, n := range all {
		if n != i+1 || len(all) != 30 {
			t.Fatalf("rows across pages: %v", all)
		}
	}
}

// When the size is unknown or the whole table fits, the picker prints
// exactly what it printed before it knew the terminal's size.
func TestPickerPrintsTheWholeTableWhenItFits(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, func(i int) string { return []string{"alpha", "beta"}[i%2] })
	for _, size := range []fixedTerminal{{}, {120, 40}, {200, 1000}} {
		width, _, _ := size.terminalSize(nil)
		format := listFormatOptions{Now: pickerNow, GroupByDay: true, Numbered: true, Width: width}
		var want bytes.Buffer
		if err := printSessionTable(&want, formatSessionRows(sessions, format), format); err != nil {
			t.Fatal(err)
		}
		printListFooter(&want, len(sessions), len(sessions), false, listFormatOptions{})
		want.WriteString("\nEnter number (or unique short SESSION_ID) to show, words to filter, or q to quit: ")
		_, _, screens := runSessionPicker(t, &sessionPicker{env: size}, sessions, listFormatOptions{GroupByDay: true}, "q\n")
		if len(screens) != 1 || screens[0] != want.String() {
			t.Errorf("%v:\n%s\nwant:\n%s", size, screens[0], want.String())
		}
	}
}

func TestPickerShowsAFewRowsOnATinyTerminal(t *testing.T) {
	t.Parallel()
	_, _, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 3}}, pickerSessions(10, oneProject), listFormatOptions{}, "q\n")
	if rowSpan(screens[0]) != "1-3" || !strings.Contains(screens[0], "Page 1 of 4 · 10 sessions · [n] next\n") {
		t.Fatalf("tiny terminal:\n%s", screens[0])
	}
}

// Every row takes one line however narrow the terminal, color codes take
// no room, and the prompt's wrapped rows are counted.
func TestPickerCountsWrappedAndColoredLines(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(40, oneProject)
	for i := range sessions {
		sessions[i].SkillsUsed = []archive.SkillUse{{Name: "code-review"}}
	}
	color := listFormatOptions{Style: textStyle{color: true}}
	_, _, wide := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, color, "q\n")
	_, _, narrow := runSessionPicker(t, &sessionPicker{env: fixedTerminal{40, 20}}, sessions, color, "q\n")
	checkScreensFit(t, wide, 120, 20)
	checkScreensFit(t, narrow, 40, 20)
	if !strings.Contains(wide[0], "\x1b[38;5;39m") || rowSpan(wide[0]) != "1-16" {
		t.Fatalf("wide page:\n%s", wide[0])
	}
	// The prompt takes three rows at 40 columns, which leave the table 13
	// lines, one a row.
	if rowSpan(narrow[0]) != "1-13" {
		t.Fatalf("narrow page:\n%s", narrow[0])
	}
}

// The size is read before every redraw, so a resized terminal gets pages
// of its new size: the one holding the row the last page started with.
func TestPickerReadsTheSizeOnEveryRedraw(t *testing.T) {
	t.Parallel()
	size := &resizingTerminal{sizes: []fixedTerminal{{120, 29}, {120, 11}}}
	_, _, screens := runSessionPicker(t, &sessionPicker{env: size}, pickerSessions(50, oneProject), listFormatOptions{}, "n\nq\n")
	// Row 26 starts the second page of 25 rows; 7 rows fit after the resize.
	if rowSpan(screens[0]) != "1-25" || rowSpan(screens[1]) != "22-28" {
		t.Fatalf("pages:\n%s", strings.Join(screens, "\n----\n"))
	}
}

// Back at the list from a session's details, the browser shows the page
// the session was picked from.
func TestPickerKeepsItsPageBetweenVisits(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	picker := &sessionPicker{env: fixedTerminal{120, 19}}
	if row, ok, _ := runSessionPicker(t, picker, sessions, listFormatOptions{}, "n\n20\n"); !ok || row.Index != 20 {
		t.Fatalf("picked %+v", row)
	}
	_, _, screens := runSessionPicker(t, picker, sessions, listFormatOptions{}, "q\n")
	if rowSpan(screens[0]) != "16-30" {
		t.Fatalf("list reopened on another page:\n%s", screens[0])
	}
}

// Splitting a long list into pages draws only about a page of rows per
// page, and is done once per terminal size: moving between pages and
// coming back to the list reuse it.
func TestPickerSplitsALongListCheaplyAndOnce(t *testing.T) {
	t.Parallel()
	const n = 5000
	sessions := pickerSessions(n, oneProject)
	once := &sessionPicker{env: fixedTerminal{120, 20}}
	runSessionPicker(t, once, sessions, listFormatOptions{}, "q\n")
	// A page holds 15 rows; its binary search draws a few pages' worth.
	if once.rendered > 5*n {
		t.Fatalf("drew %d rows to split %d", once.rendered, n)
	}
	moving := &sessionPicker{env: fixedTerminal{120, 20}}
	runSessionPicker(t, moving, sessions, listFormatOptions{}, "n\nn\np\nq\n")
	runSessionPicker(t, moving, sessions, listFormatOptions{}, "q\n")
	if moving.rendered != once.rendered {
		t.Fatalf("drew %d rows over five draws, %d for one", moving.rendered, once.rendered)
	}
}

// Regenerate with `go test ./internal/cli -run TestPickerPageGolden -update`
// and review the diff.
func TestPickerPageGolden(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, func(i int) string {
		if i < 18 {
			return "alpha"
		}
		return "beta"
	})
	_, _, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{80, 20}}, sessions, listFormatOptions{GroupByDay: true}, "n\nq\n")
	golden.Check(t, filepath.Join("testdata", "browse", "list-page-2.txt"), []byte(screens[1]))
}

func TestDisplayLinesCountsWrappedRowsWithoutColorCodes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text  string
		width int
		want  int
	}{
		{"", 10, 0},
		{"abc\n", 10, 1},
		{"abc\n\ndef", 10, 3},
		{"\x1b[1mabcdefghij\x1b[0m\n", 10, 1},
		{"abcdefghijk\n", 10, 2},
		{"abcdefghijk\n", 0, 1},
		{strings.Repeat("字", 6) + "\n", 10, 2},
		{strings.Repeat("x", 30), 10, 3},
	} {
		if got := displayLines(c.text, c.width); got != c.want {
			t.Errorf("displayLines(%q, %d) = %d, want %d", c.text, c.width, got, c.want)
		}
	}
}
