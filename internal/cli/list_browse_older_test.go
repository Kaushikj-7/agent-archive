package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/search"
)

// olderBatch is how many rows an olderTable shows at first and loads at a
// time.
const olderBatch = 20

// olderTable is a list of n sessions shown olderBatch rows at a time, with
// older rows loaded olderBatch more at a time; loads counts the loads.
type olderTable struct {
	choices *scopeChoices
	loads   int
}

func newOlderTable(n int) *olderTable {
	format := listFormatOptions{Now: pickerNow, Numbered: true}
	t := &olderTable{choices: pagedArchiveChoices(sessionScope{Label: "All projects"}, format, pickerSessions(n, oneProject), olderBatch, nil, "")}
	if t.choices.loadOlder == nil {
		return t
	}
	load := t.choices.loadOlder.Load
	t.choices.loadOlder.Load = func() (bool, error) {
		t.loads++
		return load()
	}
	return t
}

// olderScreens runs the list over table reading keys from chunks, on a
// terminal 120 columns by 19 lines (12 rows a screen, under the heading
// and above the strip), and
// returns each screen's position, highlighted row, and message line.
func olderScreens(t *testing.T, picker *sessionPicker, choices *scopeChoices, chunks ...string) (positions []string, rows []int, screens []string) {
	t.Helper()
	if picker.env == nil {
		picker.env = fixedTerminal{120, 19}
	}
	picker.older = choices.loadOlder
	screens = keyScreens(t, picker, choices, "show", chunks...)
	for _, screen := range screens {
		positions = append(positions, positionOf(screen))
		rows = append(rows, highlightedRow(screen))
	}
	return positions, rows, screens
}

func checkOlderScreens(t *testing.T, positions []string, rows []int, wantPositions []string, wantRows []int, screens []string) {
	t.Helper()
	if !reflect.DeepEqual(positions, wantPositions) || !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("positions %q, want %q\nhighlighted %v, want %v\n%s", positions, wantPositions, rows, wantRows, strings.Join(screens, "\n----\n"))
	}
}

// ↓ on the last row loaded loads older rows and goes on to the first of
// them; End goes to the last row loaded first. The position ends in "+"
// while more can load.
func TestKeyPickerDownPastTheEndLoadsOlder(t *testing.T) {
	t.Parallel()
	table := newOlderTable(30)
	positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices, "\x1b[F", "\x1b[B", "\x1b[B", "q")
	checkOlderScreens(t, positions, rows,
		[]string{"1–12 of 20+", "9–20 of 20+", "10–21 of 30", "11–22 of 30"},
		[]int{1, 20, 21, 22}, screens)
	if table.loads != 1 {
		t.Fatalf("%d loads", table.loads)
	}
	// Loaded to the end, ↓ on the last row stays.
	table = newOlderTable(30)
	positions, rows, screens = olderScreens(t, &sessionPicker{}, table.choices, "\x1b[F", "\x1b[B", "\x1b[F", "\x1b[B", "q")
	checkOlderScreens(t, positions, rows,
		[]string{"1–12 of 20+", "9–20 of 20+", "10–21 of 30", "19–30 of 30", "19–30 of 30"},
		[]int{1, 20, 21, 30, 30}, screens)
	if table.loads != 1 {
		t.Fatalf("%d loads", table.loads)
	}
}

// PgDn, space, and n with the last row on screen load older rows and go a
// screen further, as if the rows had been there.
func TestKeyPickerPageDownPastTheEndLoadsOlder(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"\x1b[6~", " ", "n"} {
		table := newOlderTable(100)
		positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices, "\x1b[6~", k, k, "q")
		// The first PgDn shows the last screen, with the last row on it;
		// the next loads, then turns the page from where the list was.
		checkOlderScreens(t, positions, rows,
			[]string{"1–12 of 20+", "9–20 of 20+", "21–32 of 40+", "29–40 of 40+"},
			[]int{1, 9, 21, 29}, screens)
		if table.loads != 1 {
			t.Fatalf("%q: %d loads", k, table.loads)
		}
	}
}

// End on the last row loads one batch, not every older row, and goes to
// its last row; End again loads the next.
func TestKeyPickerEndLoadsOneBatch(t *testing.T) {
	t.Parallel()
	table := newOlderTable(100)
	positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices, "\x1b[F", "\x1b[F", "\x1b[F", "q")
	checkOlderScreens(t, positions, rows,
		[]string{"1–12 of 20+", "9–20 of 20+", "29–40 of 40+", "49–60 of 60+"},
		[]int{1, 20, 40, 60}, screens)
	if table.loads != 2 {
		t.Fatalf("%d loads", table.loads)
	}
}

// The wheel sends ↓ several at a time. A burst held past the end loads
// once, goes on into the rows loaded with the rest of the burst, and the
// next burst past the new end loads the next batch.
func TestKeyPickerWheelBurstLoadsOncePerEnd(t *testing.T) {
	t.Parallel()
	table := newOlderTable(100)
	positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices,
		strings.Repeat("\x1b[B", 25), strings.Repeat("\x1b[B", 30), "q")
	checkOlderScreens(t, positions, rows,
		// Each load draws the first row loaded, then the rest of the burst.
		[]string{"1–12 of 20+", "10–21 of 40+", "15–26 of 40+", "30–41 of 60+", "45–56 of 60+"},
		[]int{1, 21, 26, 41, 56}, screens)
	if table.loads != 2 {
		t.Fatalf("%d loads", table.loads)
	}
}

// Keys pressed while older rows load do not load them again.
func TestKeyPickerKeysDuringALoadDoNotLoadAgain(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	loads := 0
	table := newOlderTable(100)
	load := table.choices.loadOlder.Load
	table.choices.loadOlder.Load = nil
	table.choices.loadOlder.LoadContext = func(context.Context) (bool, error) {
		loads++
		<-release
		return load()
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(release)
	}()
	positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices,
		"\x1b[F", "\x1b[B", later(50*time.Millisecond, "\x1b[B"), later(50*time.Millisecond, "\x1b[B\x1b[B"), later(400*time.Millisecond, "q"))
	checkOlderScreens(t, positions, rows,
		[]string{"1–12 of 20+", "9–20 of 20+", "10–21 of 40+"},
		[]int{1, 20, 21}, screens)
	if loads != 1 {
		t.Fatalf("%d loads", loads)
	}
}

// Nothing loads while the filter line is open, with something typed, or
// when there are no older rows; o still loads them.
func TestKeyPickerLoadsOlderOnlyFromTheList(t *testing.T) {
	t.Parallel()
	table := newOlderTable(30)
	_, rows, screens := olderScreens(t, &sessionPicker{}, table.choices, "/", strings.Repeat("\x1b[B", 25), "\x1b[F", "\x1b[6~", "\x1b", "q")
	if table.loads != 0 || rows[len(rows)-1] != 20 {
		t.Fatalf("filtering: %d loads, row %d:\n%s", table.loads, rows[len(rows)-1], strings.Join(screens, "\n----\n"))
	}
	table = newOlderTable(30)
	_, _, screens = olderScreens(t, &sessionPicker{}, table.choices, "\x1b[F", "2", "\x1b[B", "\x1b[F", "q")
	if table.loads != 0 {
		t.Fatalf("typed: %d loads:\n%s", table.loads, strings.Join(screens, "\n----\n"))
	}
	table = newOlderTable(20)
	positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices, "\x1b[F", "\x1b[B", "\x1b[6~", "\x1b[F", "q")
	checkOlderScreens(t, positions, rows,
		[]string{"1–12 of 20", "9–20 of 20", "9–20 of 20", "9–20 of 20", "9–20 of 20"},
		[]int{1, 20, 20, 20, 20}, screens)
	table = newOlderTable(30)
	positions, rows, screens = olderScreens(t, &sessionPicker{}, table.choices, "o", "q")
	checkOlderScreens(t, positions, rows, []string{"1–12 of 20+", "1–12 of 30"}, []int{1, 1}, screens)
	if table.loads != 1 || strings.Contains(statusLine(screens[0]), "o older") {
		t.Fatalf("o: %d loads:\n%s", table.loads, screens[0])
	}
}

// A load that adds no rows leaves the highlight where it was and says so.
func TestKeyPickerSaysWhenNoOlderSessionsLoad(t *testing.T) {
	t.Parallel()
	loads := 0
	choices := rowChoices(formatSessionRows(pickerSessions(5, oneProject), listFormatOptions{Numbered: true}), listFormatOptions{Now: pickerNow, Numbered: true})
	choices.loadOlder = &browserLoadAction{Label: "Older sessions", Load: func() (bool, error) {
		loads++
		return loads < 2, nil
	}}
	positions, rows, screens := olderScreens(t, &sessionPicker{}, choices, "\x1b[F", "\x1b[B", "\x1b[B", "\x1b[B", "q")
	checkOlderScreens(t, positions, rows,
		// The notice takes the footer line's place.
		[]string{"1–5 of 5+", "1–5 of 5+", "", "", "1–5 of 5"},
		[]int{1, 5, 5, 5, 5}, screens)
	for i, want := range []bool{false, false, true, true, false} {
		if got := strings.Contains(screens[i], "\nNo older sessions.\n"); got != want {
			t.Fatalf("screen %d: notice %v, want %v:\n%s", i, got, want, screens[i])
		}
	}
	if loads != 2 {
		t.Fatalf("%d loads", loads)
	}
}

// Esc stops a load that is taking a while: the highlight stays on the last
// row, and older rows can still load.
func TestKeyPickerEscStopsALoadPastTheEnd(t *testing.T) {
	t.Parallel()
	table := newOlderTable(30)
	table.choices.loadOlder.Load = nil
	table.choices.loadOlder.LoadContext = func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	positions, rows, screens := olderScreens(t, &sessionPicker{}, table.choices, "\x1b[F", "\x1b[B", later(50*time.Millisecond, "\x1b"), later(time.Second, "q"))
	checkOlderScreens(t, positions, rows, []string{"1–12 of 20+", "9–20 of 20+", "9–20 of 20+"}, []int{1, 20, 20}, screens)
	if strings.Contains(screens[2], "No older sessions.") {
		t.Fatalf("a stopped load said none loaded:\n%s", screens[2])
	}
}

// Content search loads its next results the same way, while there is a
// next page.
func TestKeyPickerContentSearchLoadsNextResultsPastTheEnd(t *testing.T) {
	t.Parallel()
	table := newOlderTable(30)
	cs := &contentSearchBrowser{page: search.Page{Next: "more"}, pages: 1}
	load := table.choices.loadOlder.Load
	table.choices.loadOlder.Load = func() (bool, error) {
		more, err := load()
		if !more {
			cs.page.Next = ""
		}
		return true, err
	}
	positions, rows, screens := olderScreens(t, &sessionPicker{contentSearch: cs}, table.choices, "\x1b[F", "\x1b[B", "\x1b[F", "\x1b[B", "q")
	checkOlderScreens(t, positions, rows,
		[]string{"1–12 of 20+", "9–20 of 20+", "10–21 of 30", "19–30 of 30", "19–30 of 30"},
		[]int{1, 20, 21, 30, 30}, screens)
	if table.loads != 1 || !strings.Contains(statusLine(screens[0]), "r rerun") || strings.Contains(screens[0], "o next results") {
		t.Fatalf("%d loads:\n%s", table.loads, screens[0])
	}
	// A load that brings no next results says so.
	cs = &contentSearchBrowser{page: search.Page{Next: "more"}, pages: 1}
	choices := rowChoices(formatSessionRows(pickerSessions(5, oneProject), listFormatOptions{Numbered: true}), listFormatOptions{Now: pickerNow, Numbered: true})
	choices.loadOlder = &browserLoadAction{Label: "next indexed page", Available: func() bool { return true }, Load: func() (bool, error) {
		cs.page.Next = ""
		return true, nil
	}}
	_, _, screens = olderScreens(t, &sessionPicker{contentSearch: cs}, choices, "\x1b[F", "\x1b[B", "\x1b[A", "q")
	if len(screens) != 4 || !strings.Contains(screens[2], "\nNo more results.\n") || positionOf(screens[3]) != "1–5 of 5" {
		t.Fatalf("no next results:\n%s", strings.Join(screens, "\n----\n"))
	}
}

// The footer line ends in "+" while older rows can load; it names PgUp and
// PgDn, which a narrow terminal drops after the number jump and before /
// search. Enter stays longest, and the line fits from 40 to 200 columns.
func TestKeyPickerFooterLineWithOlderRows(t *testing.T) {
	t.Parallel()
	l := &sessionPicker{older: &browserLoadAction{Label: "Older sessions"}}
	screen := listScreen{n: 50, start: 0, end: 14}
	for _, c := range []struct {
		width int
		want  string
	}{
		{0, "↑↓ move   PgUp/PgDn page   Enter show   / search   number + Enter jump   q quit   1–14 of 50+"},
		{100, "↑↓ move   PgUp/PgDn page   Enter show   / search   number + Enter jump   q quit          1–14 of 50+"},
		{80, "↑↓ move   PgUp/PgDn page   Enter show   / search   q quit            1–14 of 50+"},
		{60, "↑↓ move   Enter show   / search   q quit         1–14 of 50+"},
		{48, "↑↓ move   Enter show   q quit        1–14 of 50+"},
		{40, "↑↓ move   Enter show         1–14 of 50+"},
	} {
		screen.width = c.width
		if got := l.footerLine(screen, "show"); got != c.want {
			t.Errorf("%d columns: %q, want %q", c.width, got, c.want)
		}
	}
	for width := 40; width <= 200; width++ {
		screen.width = width
		for _, action := range []string{"show", "hand off", "read matching passage"} {
			got := l.footerLine(screen, action)
			if visibleWidth(got) > width || !strings.HasSuffix(got, "1–14 of 50+") || !strings.Contains(got, "Enter "+action) && width >= 60 {
				t.Errorf("%d columns, %s: %q", width, action, got)
			}
		}
	}
	// Without older rows to load, no "+".
	l.older, screen.width = nil, 120
	if got := l.footerLine(screen, "show"); !strings.HasSuffix(got, " 1–14 of 50") {
		t.Errorf("no older rows: %q", got)
	}
}
