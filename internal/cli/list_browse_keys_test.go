package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// fakeEvent is a stand-in a fakeKeys input sends instead of bytes.
type fakeEvent string

const (
	fakeResize  fakeEvent = "\x00resize"
	fakeSuspend fakeEvent = "\x00suspend"
	fakeFailure fakeEvent = "\x00fail"
	// fakeBlock waits until the test closes the fake's unblock channel,
	// then ends input.
	fakeBlock fakeEvent = "\x00block"
)

// fakeAfter starts a chunk that arrives a while after the one before it:
// later builds one.
const fakeAfter = "\x00after "

// later is a chunk of data that arrives d after the chunk before it. A
// read waiting less than d for the rest of a burst times out first.
func later(d time.Duration, data string) string {
	return fakeAfter + strconv.FormatInt(int64(d), 10) + "\x00" + data
}

// fakeKeys is a terminal read a key at a time that feeds raw bytes: each
// chunk is one burst, as the terminal would send it, unless later says it
// arrives soon enough to join the burst before it.
type fakeKeys struct {
	mu      sync.Mutex
	chunks  []string
	on      bool
	events  []string
	unblock chan struct{}
	// blocked is closed when a read starts waiting on fakeBlock.
	blocked chan struct{}
	// arrived is set while the rest of a chunk longer than one read waits.
	arrived bool
	// onResize, when set, is called as fakeResize is read.
	onResize func()
	// clock is the fake's time, moved on by each chunk that arrives later.
	clock time.Time
	// stopPending is a Ctrl-Z that came while no key was read.
	stopPending bool
}

func newFakeKeys(chunks ...string) *fakeKeys {
	return &fakeKeys{clock: time.Unix(1_700_000_000, 0), chunks: append([]string(nil), chunks...), unblock: make(chan struct{}), blocked: make(chan struct{})}
}

func (f *fakeKeys) keys() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on = true
	f.events = append(f.events, "keys")
	return nil
}

func (f *fakeKeys) lines() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on = false
	f.events = append(f.events, "lines")
}

// note records a test's own event among the terminal's.
func (f *fakeKeys) note(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeKeys) pendingStop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := f.stopPending
	f.stopPending = false
	return pending
}

func (f *fakeKeys) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeKeys) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "release")
}

func (f *fakeKeys) whilePaging() func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "paging")
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, "paged")
	}
}

func (f *fakeKeys) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "flush")
}

func (f *fakeKeys) stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "stop")
	return nil
}

func (f *fakeKeys) keyMode() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

func (f *fakeKeys) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeKeys) read(p []byte, wait time.Duration) (int, error) {
	f.mu.Lock()
	if len(f.chunks) == 0 {
		f.mu.Unlock()
		if wait >= 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	chunk := f.chunks[0]
	if rest, ok := strings.CutPrefix(chunk, fakeAfter); ok {
		delay, data, _ := strings.Cut(rest, "\x00")
		d, _ := strconv.ParseInt(delay, 10, 64)
		if wait >= 0 && time.Duration(d) > wait {
			// Waited out before it arrived.
			f.mu.Unlock()
			return 0, nil
		}
		chunk, f.chunks[0] = data, data
		f.clock = f.clock.Add(time.Duration(d))
	} else if wait >= 0 && !f.arrived && chunk != string(fakeSuspend) {
		// The burst is over, unless Ctrl-Z came during the wait.
		f.mu.Unlock()
		return 0, nil
	}
	if strings.HasPrefix(chunk, "\x00") {
		f.chunks = f.chunks[1:]
		f.mu.Unlock()
		return f.event(fakeEvent(chunk))
	}
	defer f.mu.Unlock()
	if !f.on {
		return 0, errors.New("read a key with key mode off")
	}
	n := copy(p, chunk)
	f.arrived = n < len(chunk)
	if f.arrived {
		f.chunks[0] = chunk[n:]
	} else {
		f.chunks = f.chunks[1:]
	}
	return n, nil
}

// event answers a read with what a stand-in stands for.
func (f *fakeKeys) event(event fakeEvent) (int, error) {
	switch event {
	case fakeResize:
		if f.onResize != nil {
			f.onResize()
		}
		return 0, errWindowResized
	case fakeSuspend:
		return 0, errSuspended
	case fakeBlock:
		close(f.blocked)
		<-f.unblock
		return 0, io.EOF
	case fakeFailure:
	}
	return 0, errors.New("terminal went away")
}

func TestDecodeKeys(t *testing.T) {
	t.Parallel()
	up, down := key{kind: keyUp}, key{kind: keyDown}
	for input, want := range map[string][]key{
		"\x1b[A\x1b[B\x1bOA\x1bOB":   {up, down, up, down},
		"\x1b[C\x1b[D":               {{kind: keyRight}, {kind: keyLeft}},
		"\x1b[5~\x1b[6~":             {{kind: keyPageUp}, {kind: keyPageDown}},
		"\x1b[H\x1b[F\x1b[1~\x1b[4~": {{kind: keyHome}, {kind: keyEnd}, {kind: keyHome}, {kind: keyEnd}},
		"\x1bOH\x1bOF\x1b[7~\x1b[8~": {{kind: keyHome}, {kind: keyEnd}, {kind: keyHome}, {kind: keyEnd}},
		"1 a":                        {{kind: keyRune, r: '1'}, {kind: keyRune, r: ' '}, {kind: keyRune, r: 'a'}},
		"\r\n\r\n":                   {{kind: keyEnter}, {kind: keyEnter}},
		"\n\n":                       {{kind: keyEnter}, {kind: keyEnter}},
		"\x7f\x08":                   {{kind: keyBackspace}, {kind: keyBackspace}},
		"\x04":                       {{kind: keyEndOfInput}},
		"é字":                         {{kind: keyRune, r: 'é'}, {kind: keyRune, r: '字'}},
		"\x1b":                       {{kind: keyEscape}},
		"\x1b\x1b[B":                 {{kind: keyEscape}, down},
		// Unknown sequences, Alt with a key, cut-off sequences, and control
		// characters are dropped.
		"\x1b[1;5A\x1b[200~\x1b[Z\x1b[<0;3;4M\x1bx\x1bOP\t\x01\x1b[": nil,
		"\x1b[99~2":  {{kind: keyRune, r: '2'}},
		"\x1b[\x01B": {{kind: keyRune, r: 'B'}},
	} {
		if got := decodeKeys([]byte(input)); !reflect.DeepEqual(got, want) {
			t.Errorf("decodeKeys(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestIncompleteKeyWaitsOnlyForACutOffSequence(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]bool{
		"\x1b":         true,
		"\x1b[":        true,
		"\x1b[6":       true,
		"\x1bO":        true,
		"\xe5\xad":     true,
		"\x1b[B":       false,
		"\x1b[B\x1b":   true,
		"\x1b[6~":      false,
		"12":           false,
		"\x1b\x1b":     true,
		"\x1bx":        false,
		"\x1b[A12\xe5": true,
	} {
		if got := incompleteKey([]byte(input)); got != want {
			t.Errorf("incompleteKey(%q) = %v, want %v", input, got, want)
		}
	}
}

// runKeyPicker runs picker reading keys from chunks, and returns the chosen
// row and what each screen showed.
func runKeyPicker(t *testing.T, picker *sessionPicker, sessions []archive.Metadata, format listFormatOptions, chunks ...string) (row listRow, ok bool, screens []string) {
	t.Helper()
	fake := newFakeKeys(chunks...)
	picker.keys = startKeys(fake)
	defer picker.keys.close()
	var out bytes.Buffer
	picker.clear = func() { out.WriteString(screenBreak) }
	format.Now = pickerNow
	row, ok, err := picker.pick(newPrompter(strings.NewReader(""), &out), &out, sessions, format, "show")
	if err != nil {
		t.Fatal(err)
	}
	return row, ok, strings.Split(out.String(), screenBreak)
}

// statusLine is the footer line of a screen of the list read by keys: the
// keys, and which rows are on screen.
func statusLine(screen string) string {
	for line := range strings.SplitSeq(screen, "\n") {
		if strings.HasPrefix(line, "↑↓ move") {
			return line
		}
	}
	return ""
}

// positionOf is which rows a screen of the list read by keys shows, as its
// footer line says: "1–13 of 50".
func positionOf(screen string) string {
	line := strings.TrimSpace(statusLine(screen))
	if at := strings.LastIndex(line, "   "); at >= 0 {
		return strings.TrimSpace(line[at:])
	}
	return line
}

// highlightedRow is the number of the row a screen of the list read by keys
// highlights, or 0 when none is.
func highlightedRow(screen string) int {
	for line := range strings.SplitSeq(screen, "\n") {
		if !strings.Contains(line, cursorMark+" ") {
			continue
		}
		// The mark leads a row laid out for the width, and its title in the
		// plain table of a terminal whose size is unknown.
		if fields := strings.Fields(strings.TrimPrefix(line, cursorMark)); len(fields) > 0 {
			n, _ := strconv.Atoi(fields[0])
			return n
		}
	}
	return 0
}

// checkKeyScreensFit fails when a screen of the list read by keys is
// taller than height rows on a terminal width columns wide.
func checkKeyScreensFit(t *testing.T, screens []string, width, height int) {
	t.Helper()
	for i, screen := range screens {
		if n := displayLines(screen, width); n > height {
			t.Errorf("screen %d takes %d rows of %d:\n%s", i, n, height, screen)
		}
	}
}

// keyListPrompt is the prompt under the list read by keys, where a number
// or ID is typed.
const keyListPrompt = "Number or ID to show: "

// The mouse wheel sends arrows, several at once: they move the highlight a
// row each, and a burst is drawn once. The list scrolls only to keep the
// highlight in view.
func TestKeyPickerMovesTheHighlightOnTheWheel(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	_, ok, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{},
		"\x1b[B\x1b[B\x1b[B", "\x1bOA", strings.Repeat("\x1b[B", 12), "\x1b[B", strings.Repeat("\x1b[A", 14), "q")
	if ok || len(screens) != 6 {
		t.Fatalf("ok=%v, %d screens:\n%s", ok, len(screens), strings.Join(screens, "\n----\n"))
	}
	checkKeyScreensFit(t, screens, 120, 19)
	// The detail strip, the footer line, and the prompt leave 13 rows, one
	// line a session.
	for i, want := range []struct {
		span     string
		position string
		row      int
	}{
		{"1-13", "1–13 of 50", 1},
		{"1-13", "1–13 of 50", 4},
		{"1-13", "1–13 of 50", 3},
		{"3-15", "3–15 of 50", 15},
		{"4-16", "4–16 of 50", 16},
		{"2-14", "2–14 of 50", 2},
	} {
		if rowSpan(screens[i]) != want.span || positionOf(screens[i]) != want.position || highlightedRow(screens[i]) != want.row {
			t.Fatalf("screen %d, want rows %s, %q, row %d highlighted:\n%s", i, want.span, want.position, want.row, screens[i])
		}
	}
	if !strings.HasSuffix(screens[1], "Session number 4\nCodex · app · last active 3 hours ago · id "+formatSessionRows(sessions, listFormatOptions{})[3].ShortID+"\n"+
		strings.Repeat("─", 120)+"\n"+statusLine(screens[1])+"\n"+keyListPrompt) {
		t.Fatalf("below the table:\n%q", screens[1])
	}
}

// PgUp and PgDn (and space, n, and p) scroll a screen and take the
// highlight with them; at the end of the list they go to its first or last
// row. Home and End go to the top and bottom.
func TestKeyPickerPagesAndJumps(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{},
		"\x1b[6~", " ", "n", "\x1b[5~", "p", "\x1b[F", "\x1b[B", "\x1b[H", "\x1b[A", "\x1b[B\x1b[B\x1b[5~", "n\x1b[6~", "\x1b[6~", "q")
	checkKeyScreensFit(t, screens, 120, 19)
	var spans, positions []string
	var rows []int
	for _, screen := range screens {
		spans = append(spans, rowSpan(screen))
		positions = append(positions, positionOf(screen))
		rows = append(rows, highlightedRow(screen))
	}
	wantSpans := []string{"1-13", "14-26", "27-39", "38-50", "25-37", "12-24", "38-50", "38-50", "1-13", "1-13", "1-13", "27-39", "38-50"}
	wantPositions := []string{"1–13 of 50", "14–26 of 50", "27–39 of 50", "38–50 of 50", "25–37 of 50", "12–24 of 50", "38–50 of 50", "38–50 of 50", "1–13 of 50", "1–13 of 50", "1–13 of 50", "27–39 of 50", "38–50 of 50"}
	wantRows := []int{1, 14, 27, 38, 25, 12, 50, 50, 1, 1, 1, 27, 38}
	if !reflect.DeepEqual(spans, wantSpans) || !reflect.DeepEqual(positions, wantPositions) || !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("rows %v, want %v\npositions %v, want %v\nhighlighted %v, want %v", spans, wantSpans, positions, wantPositions, rows, wantRows)
	}
}

// Whatever the list is scrolled to, a typed number or short ID and Enter
// open that row, and Enter with nothing typed opens the highlighted one;
// Backspace and Esc edit what is typed, and what is typed is echoed at the
// prompt.
func TestKeyPickerTypesANumberFromAnyScrollPosition(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	rows := formatSessionRows(sessions, listFormatOptions{Now: pickerNow})
	for _, c := range []struct {
		chunks []string
		want   int
	}{
		{[]string{"\x1b[6~", "3", "7", "\r"}, 37},
		{[]string{"\x1b[F", "38\x7f7\r"}, 37},
		{[]string{"46", "\x1b", "2\r"}, 2},
		{[]string{"\x1b[B" + rows[45].ShortID + "\n"}, 46},
		{[]string{"\x1b[1;5A\x1b[200~5\x1b[Z\r"}, 5},
		{[]string{"\r"}, 1},
		{[]string{"\x1b[B\x1b[B", "\r"}, 3},
		{[]string{"\x1b[6~", "\x1b[B\r"}, 16},
		{[]string{"\x1b[F", "\n"}, 50},
		{[]string{"4", "\x7f", "\r"}, 1},
	} {
		row, ok, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{}, c.chunks...)
		if !ok || row.Index != c.want {
			t.Errorf("%q picked %+v ok=%v:\n%s", c.chunks, row, ok, strings.Join(screens, "\n----\n"))
		}
	}
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{}, "\x1b[6~", "3", "7", "\x7f", "\x1b[200~", "\x1b", "q")
	// The paste sequence is ignored without a redraw.
	if len(screens) != 6 {
		t.Fatalf("%d screens:\n%s", len(screens), strings.Join(screens, "\n----\n"))
	}
	for i, want := range []string{"", "", "3", "37", "3", ""} {
		if !strings.HasSuffix(screens[i], keyListPrompt+want) {
			t.Errorf("screen %d, want %q typed:\n%q", i, want, screens[i])
		}
	}
}

// n, p, and q act only when nothing is typed; otherwise they are typed. A
// message takes the footer line's place until the next key.
func TestKeyPickerLettersActOnlyWithNothingTyped(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	row, ok, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{}, "1", "n", "p", "q", "\r", "n", "q")
	if ok {
		t.Fatalf("picked %+v", row)
	}
	if len(screens) != 7 || !strings.HasSuffix(screens[4], keyListPrompt+"1npq") || rowSpan(screens[4]) != "1-13" {
		t.Fatalf("%d screens:\n%s", len(screens), strings.Join(screens, "\n----\n"))
	}
	if !strings.HasSuffix(screens[5], strings.Repeat("─", 120)+"\nEnter a listed number or unique short SESSION_ID, or q to quit.\n"+keyListPrompt) || statusLine(screens[5]) != "" {
		t.Fatalf("invalid answer:\n%s", screens[5])
	}
	if rowSpan(screens[6]) != "14-26" || statusLine(screens[6]) == "" {
		t.Fatalf("n with nothing typed did not page:\n%s", screens[6])
	}
}

// q and Ctrl-D quit the list; so does the end of input.
func TestKeyPickerQuits(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"q", "Q", "\x04", ""} {
		sessions := pickerSessions(5, oneProject)
		if row, ok, _ := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{}, input); ok {
			t.Errorf("%q picked %+v", input, row)
		}
	}
}

// A list that fits does not scroll, and says every row is on screen: the
// keys move the highlight only.
func TestKeyPickerFitsWithoutScrolling(t *testing.T) {
	t.Parallel()
	for _, size := range []fixedTerminal{{120, 40}, {}} {
		_, _, screens := runKeyPicker(t, &sessionPicker{env: size}, pickerSessions(12, oneProject), listFormatOptions{}, "\x1b[B\x1b[6~ \x1b[F", "\x1b[5~", "q")
		if len(screens) != 3 || highlightedRow(screens[1]) != 12 || highlightedRow(screens[2]) != 1 {
			t.Fatalf("%v:\n%s", size, strings.Join(screens, "\n----\n"))
		}
		for _, screen := range screens {
			if rowSpan(screen) != "1-12" || positionOf(screen) != "1–12 of 12" {
				t.Fatalf("%v:\n%s", size, screen)
			}
		}
	}
}

// A day scrolled into keeps its heading, marked continued, at the top of
// the screen.
func TestKeyPickerKeepsTheDayHeading(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{GroupByDay: true}, strings.Repeat("\x1b[B", 13), "\x1b[F", "q")
	checkKeyScreensFit(t, screens, 120, 20)
	if !strings.HasPrefix(screens[0], "TODAY\n▸  1  ") || !strings.HasPrefix(screens[1], "TODAY (continued)\n   4  ") || highlightedRow(screens[1]) != 14 {
		t.Fatalf("screens:\n%s", strings.Join(screens, "\n----\n"))
	}
	if numbers := rowNumbers(screens[2]); numbers[len(numbers)-1] != 30 || positionOf(screens[2]) != "18–30 of 30" || !strings.HasPrefix(screens[2], "YESTERDAY (continued)\n") {
		t.Fatalf("bottom:\n%s", screens[2])
	}
}

// Every screen fits the terminal, however narrow or short, wherever the
// list is scrolled.
func TestKeyPickerScreensFitTheTerminal(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(40, func(i int) string { return []string{"alpha", "beta", "gamma"}[i%7%3] })
	for i := range sessions {
		sessions[i].SkillsUsed = []archive.SkillUse{{Name: "code-review"}}
	}
	for _, width := range []int{40, 80, 120} {
		for _, height := range []int{8, 12, 24} {
			for _, format := range []listFormatOptions{{}, {GroupByDay: true, Style: textStyle{color: true}}} {
				_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{width, height}}, sessions, format,
					"\x1b[B", "\x1b[B\x1b[B\x1b[B", "\x1b[6~", "123456789", "\r", "\x1b[F", "\x1b[5~", "\x1b[A", "q")
				if len(screens) != 9 {
					t.Fatalf("%dx%d: %d screens", width, height, len(screens))
				}
				// A tiny terminal still shows a few rows, as the pages do,
				// though they take more rows than it has.
				if height >= 24 || width >= 80 && height >= 12 {
					checkKeyScreensFit(t, screens, width, height)
				}
				for i, screen := range screens {
					if highlightedRow(screen) == 0 {
						t.Fatalf("%dx%d screen %d has no highlight:\n%s", width, height, i, screen)
					}
				}
			}
		}
	}
}

// The window's size is read on every redraw, and a resize redraws at once,
// with the same row highlighted and in view.
func TestKeyPickerRedrawsOnResize(t *testing.T) {
	t.Parallel()
	size := &resizingTerminal{sizes: []fixedTerminal{{120, 29}, {120, 11}}}
	_, _, screens := runKeyPicker(t, &sessionPicker{env: size}, pickerSessions(50, oneProject), listFormatOptions{}, string(fakeResize), "q")
	if len(screens) != 2 || rowSpan(screens[0]) != "1-23" || rowSpan(screens[1]) != "1-5" {
		t.Fatalf("screens:\n%s", strings.Join(screens, "\n----\n"))
	}

	adjustable := &adjustableTerminal{size: fixedTerminal{120, 29}}
	fake := newFakeKeys(strings.Repeat("\x1b[B", 19), string(fakeResize), string(fakeResize), "\r")
	resizes := []fixedTerminal{{120, 11}, {80, 40}}
	fake.onResize = func() {
		adjustable.resize(resizes[0])
		resizes = resizes[1:]
	}
	picker := &sessionPicker{env: adjustable, keys: startKeys(fake)}
	defer picker.keys.close()
	var out bytes.Buffer
	picker.clear = func() { out.WriteString(screenBreak) }
	row, ok, err := picker.pick(newPrompter(strings.NewReader(""), &out), &out, pickerSessions(50, oneProject), listFormatOptions{Now: pickerNow}, "show")
	if err != nil || !ok || row.Index != 20 {
		t.Fatalf("picked %+v ok=%v err=%v", row, ok, err)
	}
	screens = strings.Split(out.String(), screenBreak)
	if len(screens) != 4 || highlightedRow(screens[1]) != 20 || highlightedRow(screens[2]) != 20 || highlightedRow(screens[3]) != 20 || rowSpan(screens[2]) != "16-20" {
		t.Fatalf("screens:\n%s", strings.Join(screens, "\n----\n"))
	}
	checkKeyScreensFit(t, screens[2:3], 120, 11)
	checkKeyScreensFit(t, screens[3:], 80, 40)
}

// The detail strip spells out the highlighted row: its full title (not the
// list's cleaned one), the agent, the project, the PR, its subagents, when
// it was last active, and its short ID, each line cut to the width. Its
// rules are as wide as the terminal.
func TestKeyPickerDetailStrip(t *testing.T) {
	t.Parallel()
	rows, _ := layoutRows(t)
	style := textStyle{}
	for _, c := range []struct {
		row   listRow
		width int
		want  []string
	}{
		{rows[0], 120, []string{
			"Review historical project command",
			"Claude · agent-archive · PR #505 · 4 subagents · last active 2 minutes ago · id d7a77938",
		}},
		{rows[3], 120, []string{
			"# Files pasted by the user: list_table.go, list_format.go",
			"Cursor · a-project-with-an-unusually-long-directory-name · PR #1234 · last active 9 days ago · id 5b0c1e22",
		}},
		{rows[1], 60, []string{
			"Cloud session archive/retrieval options and the many…",
			"Claude · built-in-browser · PR #514 · last active 13 hours…",
		}},
		{rows[5], 40, []string{"0c1d2e3f", "Claude · agent-archive · last active 3…"}},
	} {
		strip := detailStrip(c.row, c.width, style)
		rule := strings.Repeat("─", c.width)
		if want := append(append([]string{rule}, c.want...), rule); !reflect.DeepEqual(strip, want) {
			t.Errorf("width %d:\n%s\nwant\n%s", c.width, strings.Join(strip, "\n"), strings.Join(want, "\n"))
		}
		for _, line := range strip {
			if visibleWidth(line) > c.width {
				t.Errorf("%q is wider than %d", line, c.width)
			}
		}
	}

	// A subagent says whose it is, and a note about the row is spelled out.
	sub := rows[2]
	sub.Parent, sub.Hint, sub.Project, sub.PR = "d7a77938", "not yet uploaded", "-", ""
	if got := detailStrip(sub, 200, style)[2]; got != "Codex · subagent of d7a77938 · not yet uploaded · last active 2 days ago · id 76941c69" {
		t.Errorf("subagent strip %q", got)
	}
	// Colored, the details and the rules are dim.
	colored := detailStrip(rows[0], 80, textStyle{color: true})
	if colored[0] != (textStyle{color: true}).dim(strings.Repeat("─", 80)) || !strings.HasPrefix(colored[2], "\x1b[2m") || visibleWidth(colored[2]) > 80 {
		t.Errorf("colored strip %q", colored)
	}
}

// The strip, the footer line and the prompt are budgeted with the table:
// no screen is taller than the terminal, and a terminal too short for the
// strip and a few rows gives the strip's lines to the rows.
func TestKeyPickerStripFitsTheHeight(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(40, oneProject)
	for height := 8; height <= 30; height++ {
		picker := &sessionPicker{env: fixedTerminal{80, height}, heading: "All projects · 40 sessions"}
		_, _, screens := runKeyPicker(t, picker, sessions, listFormatOptions{GroupByDay: true}, "\x1b[6~", "\x1b[B", "q")
		checkKeyScreensFit(t, screens, 80, height)
		// The heading, the footer line and the prompt take 3 lines, the
		// strip 4, and the table keeps at least 5 with the strip.
		strip := strings.Contains(screens[0], strings.Repeat("─", 80))
		if want := height >= 3+4+5; strip != want {
			t.Errorf("height %d: strip %v, want %v:\n%s", height, strip, want, screens[0])
		}
		for _, screen := range screens {
			if displayLines(screen, 80) != height && height > 9 {
				// The table fills what is left, so every screen is the
				// terminal's height (rows that end a day may leave a line).
				if displayLines(screen, 80) < height-2 {
					t.Errorf("height %d: screen takes %d rows:\n%s", height, displayLines(screen, 80), screen)
				}
			}
		}
	}
}

// The fewest rows a screen shows, each under its own day's heading, can
// take more lines than the strip leaves: the strip goes before the screen
// outgrows the terminal.
func TestKeyPickerStripGivesWayToDayHeadings(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(5, oneProject)
	for i, hours := range []int{0, 26, 6 * 24, 15 * 24, 60 * 24} {
		sessions[i].CapturedAt = pickerNow.Add(-time.Duration(hours) * time.Hour)
	}
	rule := strings.Repeat("─", 80)
	for height := 11; height <= 20; height++ {
		picker := &sessionPicker{env: fixedTerminal{80, height}, heading: "All projects · 5 sessions"}
		_, _, screens := runKeyPicker(t, picker, sessions, listFormatOptions{GroupByDay: true}, "\x1b[B", "\x1b[B", "q")
		checkKeyScreensFit(t, screens, 80, height)
		// Three rows under three headings take 8 lines; the heading, the
		// footer line, the prompt and the strip 7.
		if strip := strings.Contains(screens[0], rule); strip != (height >= 15) {
			t.Errorf("height %d: strip %v:\n%s", height, strip, screens[0])
		}
	}
}

// The footer line names the keys and, on the right, which rows are on
// screen. A narrow terminal drops the keys least needed first; the
// position stays.
func TestKeyPickerFooterLine(t *testing.T) {
	t.Parallel()
	screen := listScreen{n: 86, start: 0, end: 14, width: 80}
	l := &sessionPicker{}
	if got := l.footerLine(screen, "show"); got != "↑↓ move   Enter show   / search   number + Enter jump   q quit        1–14 of 86" {
		t.Errorf("80 columns: %q", got)
	}
	// Older rows to load are named as long as Enter is: the position
	// counts only the rows loaded.
	l.older = &browserLoadAction{Label: "Older sessions"}
	screen.width = 60
	if got := l.footerLine(screen, "hand off"); got != "↑↓ move   Enter hand off   o older   q quit       1–14 of 86" {
		t.Errorf("60 columns: %q", got)
	}
	screen.width = 40
	if got := l.footerLine(screen, "show"); got != "Enter show   o older          1–14 of 86" {
		t.Errorf("40 columns: %q", got)
	}
	screen.width = 8
	if got := l.footerLine(screen, "show"); got != "1–14 of…" {
		t.Errorf("8 columns: %q", got)
	}
	screen.width = 0
	if got := l.footerLine(screen, "show"); got != "↑↓ move   Enter show   / search   number + Enter jump   o older   q quit   1–14 of 86" {
		t.Errorf("unknown width: %q", got)
	}
	l.filtering, screen.width = true, 80
	if got := l.footerLine(screen, "show"); !strings.HasPrefix(got, "↑↓ move   Enter show   Esc clear   ") || !strings.HasSuffix(got, " 1–14 of 86") || visibleWidth(got) != 80 {
		t.Errorf("filtering: %q", got)
	}
	if got := l.footerLine(listScreen{width: 80}, "show"); got != "↑↓ move   Enter show   Esc clear" {
		t.Errorf("no rows: %q", got)
	}
}

// Reading keys, the heading names the subagents the table hides, and the
// footer's count and flag advice are left to the table read by lines.
func TestKeyPickerHeadingNamesHiddenSubagents(t *testing.T) {
	t.Parallel()
	picker := &sessionPicker{env: fixedTerminal{100, 30}, heading: "All projects · 5 sessions"}
	format := listFormatOptions{HiddenSubagents: 403, Note: "2 more sessions match elsewhere."}
	_, _, screens := runKeyPicker(t, picker, pickerSessions(5, oneProject), format, "q")
	if !strings.HasPrefix(screens[0], "All projects · 5 sessions · 403 subagents hidden\n") || strings.Contains(screens[0], "session(s)") ||
		!strings.Contains(screens[0], "\n2 more sessions match elsewhere.\n↑↓ move") {
		t.Fatalf("screen:\n%s", screens[0])
	}
	format.Note = ""
	_, _, screens = runSessionPicker(t, &sessionPicker{env: fixedTerminal{100, 30}, heading: "All projects · 5 sessions"}, pickerSessions(5, oneProject), format, "q\n")
	if !strings.HasPrefix(screens[0], "All projects · 5 sessions\n") || !strings.Contains(screens[0], "5 sessions (403 subagent sessions hidden; search to find one).") {
		t.Fatalf("line mode:\n%s", screens[0])
	}
}

// Ctrl-Z gives the terminal its modes back while the process is stopped,
// then reads keys again and redraws.
func TestKeysSuspendRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys(string(fakeSuspend), "q")
	keys := startKeys(fake)
	var out bytes.Buffer
	picker := &sessionPicker{env: fixedTerminal{120, 20}, keys: keys, clear: func() { out.WriteString(screenBreak) }}
	_, ok, err := picker.pick(newPrompter(strings.NewReader(""), &out), &out, pickerSessions(30, oneProject), listFormatOptions{Now: pickerNow}, "show")
	keys.close()
	if err != nil || ok || strings.Count(out.String(), screenBreak) != 1 {
		t.Fatalf("ok=%v err=%v:\n%s", ok, err, out.String())
	}
	if got, want := fake.history(), []string{"keys", "lines", "stop", "keys", "flush", "lines", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal modes %v, want %v", got, want)
	}
}

// browseKeys runs `list` (or args) reading keys from chunks, with a
// recording pager, on a terminal of the given size.
func browseKeys(t *testing.T, size fixedTerminal, fake *fakeKeys, args ...string) (out string, pages []pagerCall, id string) {
	t.Helper()
	env, _, id := publishedFixture(t)
	env.TerminalSize = size.terminalSize
	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&stdout)
	}
	env.openKeys = func(in io.Reader) (keyTerminal, bool) {
		if in != io.Reader(stdin) {
			t.Errorf("keys read from %v", in)
		}
		return fake, true
	}
	env.RunPager = func(_ context.Context, command string, _ []string, in io.Reader, _, _ io.Writer) error {
		if fake.keyMode() {
			t.Error("the pager ran with the terminal in key mode")
		}
		text, err := io.ReadAll(in)
		pages = append(pages, pagerCall{command: command, text: string(text)})
		return err
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	if code := Run(args, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, stderr.String(), stdout.String())
	}
	if fake.keyMode() {
		t.Fatalf("the browser left the terminal in key mode: %v", fake.history())
	}
	return stdout.String(), pages, id
}

// keyDetailsScreens is each drawing of the details in out.
func keyDetailsScreens(out string) []string {
	var screens []string
	for screen := range strings.SplitSeq(out, clearScreenSequence) {
		screen, _, _ = strings.Cut(screen, leaveAltScreenSequence)
		if strings.Contains(screen, "t transcript") {
			screens = append(screens, screen)
		}
	}
	return screens
}

const (
	keyDetailsPrompt    = "t transcript · b back · q quit: "
	keyCutDetailsPrompt = "t transcript · m more · ↑↓ scroll · b back · q quit: "
)

// A summary taller than the window scrolls on the wheel, arrows, PgUp,
// PgDn, space, Home and End, and says how many lines are above and below.
func TestKeyDetailsScroll(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys("1\r", "\x1b[B\x1b[B", "\x1b[F", "\x1b[B", "\x1b[5~", "\x1b[H", " ", "q")
	out, pages, _ := browseKeys(t, fixedTerminal{100, 9}, fake)
	screens := keyDetailsScreens(out)
	if len(screens) != 7 || len(pages) != 0 {
		t.Fatalf("%d details screens, %d pages:\n%s", len(screens), len(pages), out)
	}
	var indicators []string
	for _, screen := range screens {
		if n := displayLines(screen, 100); n != 9 || !strings.HasSuffix(screen, keyCutDetailsPrompt) {
			t.Fatalf("details take %d rows of 9:\n%s", n, screen)
		}
		above, _, _ := strings.Cut(screen, "\n\nt opens")
		indicators = append(indicators, above[strings.LastIndex(above, "\n")+1:])
	}
	// The summary's 10 lines scroll 5 at a time above the indicator, the
	// blank line, the hint, and the prompt: ↓↓, End (↓ stops there), PgUp,
	// Home, space.
	want := []string{
		"↓ 5 more lines", "↑ 2 lines above · ↓ 3 more lines", "↑ 5 lines above", "↑ 5 lines above",
		"↓ 5 more lines", "↓ 5 more lines", "↑ 5 lines above",
	}
	if !reflect.DeepEqual(indicators, want) {
		t.Fatalf("indicators %q, want %q", indicators, want)
	}
}

// t and m act at once, through the pager, with the terminal's own modes
// back while it runs; key mode is on again when the details are redrawn.
func TestKeyDetailsSingleKeys(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys("1\r", "\x1b[B", "t", "m", "x", "q")
	out, pages, id := browseKeys(t, fixedTerminal{100, 9}, fake)
	screens := keyDetailsScreens(out)
	if len(pages) != 2 || !strings.Contains(pages[0].text, "visible") || !strings.Contains(pages[1].text, "ID "+id) || strings.Contains(pages[1].text, "visible") {
		t.Fatalf("pages %+v", pages)
	}
	if len(screens) != 5 {
		t.Fatalf("%d details screens:\n%s", len(screens), strings.Join(screens, "\n----\n"))
	}
	// The details keep their scroll position after the pager.
	for _, screen := range screens[1:] {
		if !strings.Contains(screen, "\n↑ 1 line above · ") {
			t.Fatalf("details scrolled back:\n%s", screen)
		}
	}
	if !strings.Contains(screens[4], "\nPress t for the transcript, m for the whole summary, b (or Enter) for the list, or q to quit.\nt opens") {
		t.Fatalf("unknown key:\n%s", screens[4])
	}
	if n := strings.Count(out, enterAltScreenSequence); n != 3 {
		t.Fatalf("alternate screen entered %d times, want 3", n)
	}
	history := fake.history()
	// Input is flushed only as the browser ends, not before a pager, and
	// Ctrl-Z is answered while each pager runs.
	if want := []string{"keys", "lines", "paging", "paged", "keys", "lines", "paging", "paged", "keys", "flush", "lines", "release"}; !reflect.DeepEqual(history, want) {
		t.Fatalf("terminal modes %v, want %v", history, want)
	}
}

// b, Enter, Backspace and Esc go back to the list, which is drawn as it was
// left; q and Ctrl-D quit.
func TestKeyDetailsBackAndQuit(t *testing.T) {
	t.Parallel()
	for _, back := range []string{"b", "B", "\r", "\x7f"} {
		out, _, _ := browseKeys(t, fixedTerminal{100, 60}, newFakeKeys("1\r", back, "1\r", "\x04"))
		if n := strings.Count(out, keyListPrompt); n != 2 || len(keyDetailsScreens(out)) != 2 || !strings.Contains(out, keyDetailsPrompt) {
			t.Fatalf("%q: list shown %d times:\n%s", back, n, out)
		}
	}
	out, _, _ := browseKeys(t, fixedTerminal{100, 60}, newFakeKeys("1\r", "Q"))
	if strings.Count(out, keyListPrompt) != 1 || strings.Contains(out, "m more") || strings.Contains(out, "↑↓ scroll") {
		t.Fatalf("q:\n%s", out)
	}
}

// Without a pager, the transcript is printed and the browser waits for a
// key before drawing the details again; m is not offered.
func TestKeyDetailsWithoutPager(t *testing.T) {
	t.Parallel()
	out, pages, _ := browseKeys(t, fixedTerminal{100, 9}, newFakeKeys("1\r", "m", "t", "x", "b", "q"), "list", "--no-pager")
	if len(pages) != 0 || !strings.Contains(out, "visible") || !strings.Contains(out, "t transcript · ↑↓ scroll · b back · q quit: ") {
		t.Fatalf("pages %d:\n%s", len(pages), out)
	}
	if !strings.Contains(out, "\nPress t for the transcript, b (or Enter) for the list, or q to quit.\n") || len(keyDetailsScreens(out)) != 3 {
		t.Fatalf("details:\n%s", out)
	}
}

// Every path out of the browser gives the terminal its modes back: quitting,
// the end of input, an error, and a panic.
func TestKeyBrowserRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	for _, chunks := range [][]string{{"q"}, {"1\r", "q"}, {}, {"1\r"}} {
		fake := newFakeKeys(chunks...)
		browseKeys(t, fixedTerminal{100, 20}, fake)
		if history := fake.history(); !reflect.DeepEqual(history, []string{"keys", "flush", "lines", "release"}) {
			t.Errorf("%q: terminal modes %v", chunks, history)
		}
	}

	env, _, _ := publishedFixture(t)
	fake := newFakeKeys("1\r", string(fakeFailure))
	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return fake, true }
	if code := Run([]string{"list"}, stdin, &stdout, &stderr, env); code != 1 || fake.keyMode() || !strings.Contains(stderr.String(), "terminal went away") {
		t.Fatalf("code %d, key mode %v, stderr %q", code, fake.keyMode(), stderr.String())
	}

	// A panic while the details are drawn, with the terminal in key mode.
	fake = newFakeKeys("1\r", "q")
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return fake, true }
	sizes := 0
	env.TerminalSize = func(io.Writer) (int, int, bool) {
		if sizes++; sizes > 1 && fake.keyMode() {
			panic("drawing failed")
		}
		return 100, 20, true
	}
	stdout.Reset()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no panic")
			}
		}()
		Run([]string{"list"}, stdin, &stdout, &stderr, env)
	}()
	if fake.keyMode() || !strings.HasSuffix(stdout.String(), leaveAltScreenSequence) {
		t.Fatalf("after a panic: key mode %v, modes %v, output %q", fake.keyMode(), fake.history(), stdout.String())
	}
}

// A signal while the browser waits for a key restores the terminal's modes
// and the screen before the process exits, with the shell's status for the
// signal: SIGTERM, and SIGQUIT (131), which would otherwise dump goroutines
// and leave the terminal raw on the alternate screen. The bare show is the
// same browser as list.
func TestKeyBrowserSignalRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"list", "show"} {
		for _, tc := range []struct {
			sig  syscall.Signal
			code int
		}{{syscall.SIGTERM, 143}, {syscall.SIGHUP, 129}, {syscall.SIGQUIT, 131}} {
			t.Run(fmt.Sprintf("%s %v", command, tc.sig), func(t *testing.T) {
				t.Parallel()
				env, _, _ := publishedFixture(t)
				fake := newFakeKeys("1\r", string(fakeBlock))
				stdin := strings.NewReader("")
				var stdout, stderr syncBuffer
				env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
				env.openKeys = func(io.Reader) (keyTerminal, bool) { return fake, true }
				signals := make(chan os.Signal, 1)
				env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
				exited := make(chan int, 1)
				env.exitProcess = func(code int) {
					if fake.keyMode() || !strings.HasSuffix(stdout.String(), leaveAltScreenSequence) {
						t.Errorf("exit before restoring: modes %v", fake.history())
					}
					exited <- code
				}
				done := make(chan int, 1)
				go func() { done <- Run([]string{command}, stdin, &stdout, &stderr, env) }()
				<-fake.blocked
				signals <- tc.sig
				select {
				case code := <-exited:
					if code != tc.code {
						t.Fatalf("exit %d, want %d", code, tc.code)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("no exit after %v", tc.sig)
				}
				close(fake.unblock)
				<-done
				// Closed once by the handler; the browser's own close does nothing.
				if history := fake.history(); !reflect.DeepEqual(history, []string{"keys", "flush", "lines", "release"}) {
					t.Fatalf("terminal modes %v", history)
				}
				if out := stdout.String(); strings.Count(out, enterAltScreenSequence) != 1 || strings.Count(out, leaveAltScreenSequence) != 1 {
					t.Errorf("the alternate screen was not entered and left once: %q", out)
				}
			})
		}
	}
}

// Without a terminal to read keys from, the browser reads lines exactly as
// before; key mode needs both a terminal on stdin and the alternate screen.
func TestKeysOnlyOnATerminal(t *testing.T) {
	t.Parallel()
	if _, ok := terminalKeys(strings.NewReader("1\n")); ok {
		t.Fatal("key mode on a strings.Reader")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	if _, ok := terminalKeys(reader); ok {
		t.Fatal("key mode on a pipe")
	}
	opened := false
	env := testEnv(t, t.TempDir(), time.Now())
	env.openKeys = func(io.Reader) (keyTerminal, bool) { opened = true; return newFakeKeys(), true }
	if keys := browserKeys(env, newPrompter(strings.NewReader(""), io.Discard), &altScreen{}); keys != nil || opened {
		t.Fatal("key mode off the alternate screen")
	}
	// The line-mode browser tests run with the package's default, which
	// never reads keys, and pass unchanged.
	lineOut, _, _, _ := browse(t, "1\nq\n")
	keyOut, _, _ := browseKeys(t, fixedTerminal{}, newFakeKeys("1\r", "q"))
	if !strings.Contains(lineOut, detailsPrompt+": ") || strings.Contains(lineOut, "t transcript ·") || !strings.Contains(keyOut, keyDetailsPrompt) {
		t.Fatalf("line mode:\n%s\nkey mode:\n%s", lineOut, keyOut)
	}
}

// Regenerate with `go test ./internal/cli -run TestKeyScreensGolden -update`
// and review the diff.
func TestKeyScreensGolden(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, func(i int) string {
		if i < 18 {
			return "alpha"
		}
		return "beta"
	})
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{80, 20}}, sessions, listFormatOptions{GroupByDay: true}, "\x1b[F", "\x1b[A\x1b[A", "1", "2", "q")
	golden.Check(t, filepath.Join("testdata", "browse", "keys-list-scrolled.txt"), []byte(screens[len(screens)-2]))

	env := testEnv(t, t.TempDir(), summaryNow)
	var out bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(&out) }
	env.TerminalSize = fixedTerminal{80, 14}.terminalSize
	fake := newFakeKeys("\x1b[B\x1b[B\x1b[B", "q")
	b := &sessionBrowser{env: env, prompt: newPrompter(strings.NewReader(""), &out), stdout: &out, screen: &altScreen{}, keys: startKeys(fake)}
	summary := renderSummaryText(summaryFixture(), summaryOptions{Now: summaryNow, Location: time.UTC})
	lines := strings.Split(strings.TrimSuffix(summary, "\n"), "\n")
	screen := b.drawDetailsKeys(lines, 3, true, browseNotice{})
	b.keys.close()
	if n := displayLines(out.String(), 80); n != 14 || screen.top != 3 {
		t.Fatalf("details take %d rows of 14, top %d:\n%s", n, screen.top, out.String())
	}
	golden.Check(t, filepath.Join("testdata", "browse", "keys-details-scrolled.txt"), out.Bytes())
}
