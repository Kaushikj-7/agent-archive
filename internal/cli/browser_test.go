package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// runOnTerminal runs a command on a terminal 100 columns wide: reading keys
// from fake when it is set, else lines from input. A command that goes on to
// ask questions reads the answers from input in both.
func runOnTerminal(t *testing.T, env Env, fake *fakeKeys, input string, args ...string) (out, errOut string, code int) {
	t.Helper()
	stdin := strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	env.TerminalSize = fixedTerminal{100, 40}.terminalSize
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.RunPager = copyPager(new(string))
	if fake != nil {
		env.openKeys = func(io.Reader) (keyTerminal, bool) { return fake, true }
	}
	code = Run(args, stdin, &stdout, &stderr, env)
	return stdout.String(), stderr.String(), code
}

// flakyArchive is the picker fixture with two more archived sessions of its
// project that "flaky" finds, besides the fixture's own, in this order:
// the fixture's session, then a retention test, then a lint rule; and two
// sessions on this machine that are not uploaded, which handoff finds first.
func flakyArchive(t *testing.T) (pickerFixture, string) {
	t.Helper()
	f := newPickerFixture(t)
	label := filepath.Base(f.project)
	f.addSession(t, "codex", "native-flaky-1", "Flaky local rewrite", f.env.now().Add(-30*time.Minute))
	f.addSession(t, "codex", "native-flaky-2", "Flaky local cleanup", f.env.now().Add(-40*time.Minute))
	age := func(hours int) func(*archive.Metadata) {
		return func(m *archive.Metadata) { m.CapturedAt = m.CapturedAt.Add(-time.Duration(hours) * time.Hour) }
	}
	f.addArchivedWith(t, "flak0001", "Flaky retention test", label, age(1))
	f.addArchivedWith(t, "flak0002", "Flaky lint rule", label, age(2))
	return f, label
}

// Every caller of design section 5 opens the key browser on a terminal that
// can be read a key at a time and the line-mode fallback on one that cannot,
// with the heading its row gives and the Enter action its row gives: browse
// shows the session's details, pick returns it.
func TestEachChooserOpensTheKeyBrowserAndTheLineFallback(t *testing.T) {
	t.Parallel()
	const flaky = `"flaky" matches 3`
	callers := []struct {
		name string
		args []string
		// heading is what the browser's heading says; others are verbs that
		// must not lead it.
		heading string
		verbs   []string
		keys    []string
		lines   string
		answers string
		// acts is what Enter's action prints: the details of a session, the
		// session as JSON, or the question after a handoff's pick.
		acts string
	}{
		{name: "list", args: []string{"list"}, heading: "{label} · 4 sessions", verbs: []string{"Show ·", "Hand off ·"},
			keys: []string{"1\r", "q"}, lines: "1\nq\n", acts: "browse"},
		{name: "show", args: []string{"show"}, heading: "{label} · 4 sessions", verbs: []string{"Show ·", "Hand off ·"},
			keys: []string{"1\r", "q"}, lines: "1\nq\n", acts: "browse"},
		{name: "show --json", args: []string{"show", "--json"}, heading: "Show · {label} · 4 sessions", verbs: []string{"Hand off ·"},
			keys: []string{"1\r"}, lines: "1\n", acts: `"session_id"`},
		{name: "show words", args: []string{"show", "flaky"}, heading: flaky, verbs: []string{"Show ·", "Hand off ·"},
			keys: []string{"\r", "q"}, lines: "1\nq\n", acts: "browse"},
		{name: "handoff", args: []string{"handoff"}, heading: "Hand off · {label} · 7 sessions", verbs: []string{"Show ·"},
			keys: []string{"1\r"}, lines: "1\n", answers: "p\n", acts: "? Continue in"},
		{name: "handoff words", args: []string{"handoff", "flaky"}, heading: "Hand off · " + flaky, verbs: []string{"Show ·"},
			keys: []string{"\r"}, lines: "1\n", answers: "p\n", acts: "? Continue in"},
	}
	for _, c := range callers {
		for _, keyed := range []bool{true, false} {
			f, label := flakyArchive(t)
			heading := strings.ReplaceAll(c.heading, "{label}", label)
			var fake *fakeKeys
			input := c.lines + c.answers
			mode := "lines"
			if keyed {
				fake, input, mode = newFakeKeys(c.keys...), c.answers, "keys"
			}
			out, errOut, code := runOnTerminal(t, f.env, fake, input, c.args...)
			if code != 0 {
				t.Fatalf("%s, %s: code=%d stderr=%s\n%s", c.name, mode, code, errOut, out)
			}
			if !strings.Contains(out, enterAltScreenSequence) || !strings.Contains(out, heading) {
				t.Errorf("%s, %s: no browser on the alternate screen with the heading %q:\n%s", c.name, mode, heading, out)
			}
			for _, verb := range c.verbs {
				if strings.Contains(out, verb) {
					t.Errorf("%s, %s: a heading led by %q:\n%s", c.name, mode, verb, out)
				}
			}
			// Keys are read only where the terminal can; lines are the fallback.
			hasKeys := strings.Contains(out, "↑↓ move   Enter ")
			asksForWords := strings.Contains(out, "words to filter, or q to quit: ")
			if hasKeys != keyed || asksForWords == keyed || keyed && len(fake.history()) == 0 {
				t.Errorf("%s, %s: key mode %v, line prompt %v\n%s", c.name, mode, hasKeys, asksForWords, out)
			}
			details := "t transcript · b back · q quit"
			if !keyed {
				details = "[t] transcript  [Enter/b] back to list  [q] quit"
			}
			if c.acts == "browse" {
				if !strings.Contains(out, details) {
					t.Errorf("%s, %s: Enter did not show the details:\n%s", c.name, mode, out)
				}
			} else if strings.Contains(out, details) || !strings.Contains(out, c.acts) {
				t.Errorf("%s, %s: Enter did not return the session (%q):\n%s", c.name, mode, c.acts, out)
			}
		}
	}
}

// highlighted is the title of the row a screen of the key browser marks, "" when
// none is.
func highlighted(screen string) string {
	for line := range strings.SplitSeq(screen, "\n") {
		if _, after, ok := strings.Cut(line, cursorMark+" "); ok {
			// Past the number and the agent's dot and name.
			after = strings.TrimLeft(after, " 0123456789")
			for _, name := range []string{"Claude", "Codex", "Cursor"} {
				if rest, ok := strings.CutPrefix(after, agentDot+" "+name+" "); ok {
					after = strings.TrimLeft(rest, " ")
				}
			}
			title, _, _ := strings.Cut(strings.TrimLeft(after, " "), "  ")
			return title
		}
	}
	return ""
}

// `/` opens a filter line that narrows the rows as each character is typed,
// highlights the first row, and lets the arrows move the highlight; Enter
// acts on the highlighted row.
func TestKeyFilterNarrowsAsTypedAndEnterActsOnTheHighlight(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	row, ok, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 40}}, sessions, listFormatOptions{},
		"/", "number ", "1", "\x1b[B", "\r")
	if !ok || row.Index != 10 || row.SessionID != sessions[9].SessionID {
		t.Fatalf("Enter picked %+v ok=%v", row, ok)
	}
	if len(screens) != 5 {
		t.Fatalf("%d screens:\n%s", len(screens), strings.Join(screens, "\n----\n"))
	}
	// Opening the filter highlights the first row, and every row is still shown.
	if got := highlighted(screens[1]); got != "Session number 1" || len(rowNumbers(screens[1])) != 30 || !strings.HasSuffix(screens[1], "\n/") {
		t.Fatalf("filter line opened with %q highlighted:\n%s", got, screens[1])
	}
	// "number " matches every row; "number 1" the rows with a 1 in their number.
	if len(rowNumbers(screens[2])) != 30 || !strings.HasSuffix(screens[2], "\n/number ") {
		t.Fatalf("rows after typing %q:\n%s", "number ", screens[2])
	}
	want := []int{1, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 21}
	if got := rowNumbers(screens[3]); !sameInts(got, want) || highlighted(screens[3]) != "Session number 1" {
		t.Fatalf("rows after typing %q: %v\n%s", "number 1", got, screens[3])
	}
	// ↓ moves the highlight to the next row, which Enter then picked.
	if got := highlighted(screens[4]); got != "Session number 10" {
		t.Fatalf("highlight after ↓ on %q:\n%s", got, screens[4])
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Rows keep the numbers they have unfiltered, in key mode and in line mode,
// and a number seen before filtering picks its row.
func TestFilteredRowsKeepTheirUnfilteredNumbers(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 40}}, sessions, listFormatOptions{}, "/", "number 17", "q")
	if got := rowNumbers(screens[2]); !sameInts(got, []int{17}) {
		t.Fatalf("key mode numbers the filtered row %v:\n%s", got, screens[2])
	}
	// Leaving the filter, a number typed at the prompt is the unfiltered one.
	row, ok, _ := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 40}}, sessions, listFormatOptions{}, "/", "number 17", "\x1b", "3", "\r")
	if !ok || row.Index != 3 {
		t.Fatalf("picked %+v ok=%v", row, ok)
	}
	// Line mode: the filtered table shows 17, and 3, which is not on it,
	// still picks row 3, as 17 does.
	for number, want := range map[string]int{"17": 17, "3": 3} {
		row, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 40}}, sessions, listFormatOptions{}, "number 17\n"+number+"\n")
		if !ok || row.Index != want || row.SessionID != sessions[want-1].SessionID {
			t.Errorf("number %s picked %+v ok=%v", number, row, ok)
		}
		if got := rowNumbers(screens[1]); !sameInts(got, []int{17}) {
			t.Errorf("line mode numbers the filtered row %v:\n%s", got, screens[1])
		}
	}
}

// A row the filter finds past the table's limit has the number it would have
// in the whole table, and is picked in the filter: by the highlight and Enter
// in key mode, by that number in line mode while the page drawn shows it.
// Only the table's own numbers pick once the filter is gone, so a number past
// the table never names a session the person has not seen.
func TestARowPastTheLimitIsPickedInTheFilter(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	format := listFormatOptions{Now: pickerNow, Numbered: true}
	choices := newScopeChoices(sessionScope{}, format, false, archiveRows(sessions, 10, format))
	pickKeys := func(chunks ...string) (listRow, bool, []string) {
		t.Helper()
		var out bytes.Buffer
		picker := &sessionPicker{env: fixedTerminal{120, 40}, keys: startKeys(newFakeKeys(chunks...)), clear: func() { out.WriteString(screenBreak) }}
		defer picker.keys.close()
		row, ok, err := picker.pickScoped(newPrompter(strings.NewReader(""), &out), &out, choices, "show")
		if err != nil {
			t.Fatal(err)
		}
		return row, ok, strings.Split(out.String(), screenBreak)
	}
	row, ok, screens := pickKeys("/", "number 27", "\r")
	if !ok || row.Index != 27 || row.SessionID != sessions[26].SessionID {
		t.Fatalf("Enter in the filter picked %+v ok=%v\n%s", row, ok, strings.Join(screens, "\n----\n"))
	}
	if got := rowNumbers(screens[2]); !sameInts(got, []int{27}) {
		t.Fatalf("the filter numbers the row past the limit %v:\n%s", got, screens[2])
	}
	// After Esc, 27 is refused as any number past the table is, seen in a
	// filter or not, as is a number no session has.
	for _, keys := range [][]string{{"/", "number 27", "\x1b", "27", "\r", "q"}, {"27", "\r", "q"}, {"31", "\r", "q"}} {
		row, ok, screens := pickKeys(keys...)
		if ok {
			t.Fatalf("%q picked row %d", keys, row.Index)
		}
		if out := strings.Join(screens, ""); !strings.Contains(out, "Enter a listed number or unique short SESSION_ID") {
			t.Fatalf("%q was not refused:\n%s", keys, out)
		}
	}

	// Line mode: 27 picks while the filter shows it.
	var out bytes.Buffer
	picker := &sessionPicker{env: fixedTerminal{120, 40}}
	row, ok, err := picker.pickScoped(newPrompter(strings.NewReader("number 27\n27\n"), &out), &out, choices, "show")
	if err != nil || !ok || row.Index != 27 {
		t.Fatalf("line mode 27 in the filter picked %+v ok=%v err=%v\n%s", row, ok, err, out.String())
	}
	// On a page of the filter that does not show 28, 28 is words, which
	// narrow the filter to it; then it picks.
	out.Reset()
	picker = &sessionPicker{env: fixedTerminal{120, 14}}
	row, ok, err = picker.pickScoped(newPrompter(strings.NewReader("number\n28\n28\n"), &out), &out, choices, "show")
	if err != nil || !ok || row.Index != 28 || !strings.Contains(out.String(), `"number 28"`) {
		t.Fatalf("line mode 28 off the page picked %+v ok=%v err=%v\n%s", row, ok, err, out.String())
	}
	if !strings.Contains(out.String(), "Page 1 of") {
		t.Fatalf("the filter is not paged:\n%s", out.String())
	}
}

// In line mode a word too short to be an ID prefix the matcher takes is words
// to filter by, even when a listed session's ID starts with it: "db" or "add"
// is a word, not that session.
func TestLineModeShortHexWordFiltersRatherThanPickingByID(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	sessions[3].SessionID = "db" + sessions[3].SessionID[2:]
	sessions[5].Title = "Session number 6, the db schema"
	row, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 60}}, sessions, listFormatOptions{}, "db\nq\n")
	if ok {
		t.Fatalf("db picked %+v", row)
	}
	if got := rowNumbers(screens[1]); !sameInts(got, []int{6}) || !strings.Contains(screens[1], `1 session matches "db"`) {
		t.Fatalf("db did not filter: %v\n%s", got, screens[1])
	}
	// An ID's first four characters or more still pick its session.
	if row, ok, _ := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 60}}, sessions, listFormatOptions{}, sessions[3].SessionID[:4]+"\n"); !ok || row.Index != 4 {
		t.Fatalf("an ID prefix picked %+v ok=%v", row, ok)
	}
}

// In line mode an answer that is not a number, an ID, or a command word is a
// filter, more words add to it, and an empty answer clears it (and quits when
// there is none).
func TestLineModeWordsFilterAndAnEmptyAnswerClears(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, func(i int) string { return []string{"alpha", "beta"}[i%2] })
	_, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 60}}, sessions, listFormatOptions{},
		"number 1\nbeta\n\nq\n")
	if ok || len(screens) != 4 {
		t.Fatalf("ok=%v, %d screens:\n%s", ok, len(screens), strings.Join(screens, "\n----\n"))
	}
	if got := len(rowNumbers(screens[0])); got != 30 || strings.Contains(screens[0], "match") {
		t.Fatalf("the table before any filter has %d rows:\n%s", got, screens[0])
	}
	if got := rowNumbers(screens[1]); !sameInts(got, []int{1, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 21}) ||
		!strings.Contains(screens[1], `12 sessions match "number 1" · a number, more words, or Enter for all`) {
		t.Fatalf("the table filtered by %q: %v\n%s", "number 1", got, screens[1])
	}
	// More words narrow it further: the even rows (the odd numbers, counting
	// from 0) are in project beta.
	if got := rowNumbers(screens[2]); !sameInts(got, []int{10, 12, 14, 16, 18}) ||
		!strings.Contains(screens[2], `5 sessions match "number 1 beta"`) {
		t.Fatalf("the table filtered by more words: %v\n%s", got, screens[2])
	}
	// An empty answer clears the filter, and a second one (here q) quits.
	if got := len(rowNumbers(screens[3])); got != 30 || strings.Contains(screens[3], "match") {
		t.Fatalf("the table after an empty answer has %d rows:\n%s", got, screens[3])
	}
	// With no filter, an empty answer quits, as before.
	if _, ok, screens := runSessionPicker(t, &sessionPicker{env: fixedTerminal{120, 60}}, sessions, listFormatOptions{}, "\n"); ok || len(screens) != 1 {
		t.Fatalf("an empty answer: ok=%v, %d screens", ok, len(screens))
	}
}

// Esc clears the filter and closes the line; Backspace takes a character off
// and, with none left, closes it too; Enter with nothing matching says so.
func TestKeyFilterEscBackspaceAndNoMatch(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	_, ok, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 40}}, sessions, listFormatOptions{},
		"/", "zzz", "\r", "\x7f\x7f\x7f", "\x7f", "/", "17", "\x1b", "q")
	if ok {
		t.Fatalf("picked a row")
	}
	// zzz: nothing matches, the highlight is on nothing, and Enter says so.
	if len(rowNumbers(screens[2])) != 0 || !strings.Contains(screens[2], `No sessions match "zzz".`) || strings.Contains(screens[2], cursorMark) {
		t.Fatalf("no match:\n%s", screens[2])
	}
	if !strings.Contains(screens[3], "Nothing matches the filter; Esc clears it.") {
		t.Fatalf("Enter with no match:\n%s", screens[3])
	}
	// Three Backspaces empty the filter, which shows every row again; a
	// fourth closes the line, back to the list's own prompt.
	if len(rowNumbers(screens[4])) != 30 || !strings.HasSuffix(screens[4], "\n/") {
		t.Fatalf("after Backspace:\n%s", screens[4])
	}
	if !strings.HasSuffix(screens[5], keyListPrompt) || highlightedRow(screens[5]) != 1 {
		t.Fatalf("after the last Backspace:\n%s", screens[5])
	}
	// Esc clears a filter with words in it, and closes the line; the
	// highlight stays on the row it was on.
	last := screens[len(screens)-1]
	if len(rowNumbers(last)) != 30 || highlightedRow(last) != 17 || !strings.HasSuffix(last, keyListPrompt) {
		t.Fatalf("after Esc:\n%s", last)
	}
}

// / keeps the highlight where it is until words are typed, and Esc out of
// the filter keeps the row it highlights when the whole list has it.
func TestKeyFilterEscKeepsTheHighlightedRow(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, sessions, listFormatOptions{},
		strings.Repeat("\x1b[B", 19), "/", "\x1b", "/", "number 2", "\x1b[B", "\x1b", "q")
	var rows []int
	for _, screen := range screens {
		rows = append(rows, highlightedRow(screen))
	}
	// ↓ ×19, /, Esc, /, the words (2, 12 and 20…29 match; 2 is highlighted), ↓, Esc.
	if want := []int{1, 20, 20, 20, 20, 2, 12, 12}; !slices.Equal(rows, want) {
		t.Fatalf("highlighted %v, want %v:\n%s", rows, want, strings.Join(screens, "\n----\n"))
	}
	if !strings.Contains(screens[len(screens)-1], cursorMark+" 12  ") {
		t.Fatalf("row 12 is not in view:\n%s", screens[len(screens)-1])
	}
}

// Keys that mean something outside the filter are text inside it: a, q, n, p.
func TestKeyFilterTakesLettersThatActOutsideIt(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	scope := sessionScope{Label: "app", ProjectIDs: []string{"id-app"}, Dir: "/w/app"}
	format := listFormatOptions{Now: pickerNow, Numbered: true}
	choices := newScopeChoices(scope, format, false, func(sessionScope) scopeView {
		return scopeView{rows: formatSessionRows(sessions, format), total: len(sessions)}
	})
	fake := newFakeKeys("/", "a", "n", "p", "q", "\x1b", "a", "q")
	picker := &sessionPicker{env: fixedTerminal{120, 40}, keys: startKeys(fake)}
	defer picker.keys.close()
	var out bytes.Buffer
	picker.clear = func() { out.WriteString(screenBreak) }
	if _, ok, err := picker.pickScoped(newPrompter(strings.NewReader(""), &out), &out, choices, "show"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	screens := strings.Split(out.String(), screenBreak)
	// Typing "a" did not switch the scope; the filter now holds "anpq".
	if !strings.HasSuffix(screens[5], "\n/anpq") || strings.Contains(screens[5], "All projects") {
		t.Fatalf("after typing a, n, p, q:\n%s", screens[5])
	}
	// Outside the filter, the same keys act: a asks for the other scope.
	if !strings.Contains(screens[len(screens)-1], "All projects") {
		t.Fatalf("a did not switch the scope outside the filter:\n%s", screens[len(screens)-1])
	}
}

// The heading names Esc, not a, as soon as the filter line is open, before
// a word is typed: a is typed text there. Closing the line brings a back.
func TestKeyFilterHeadingNamesEscBeforeAWordIsTyped(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	scope := sessionScope{Label: "app", ProjectIDs: []string{"id-app"}, Dir: "/w/app"}
	format := listFormatOptions{Now: pickerNow, Numbered: true}
	choices := newScopeChoices(scope, format, false, func(sessionScope) scopeView {
		return scopeView{rows: formatSessionRows(sessions, format), total: len(sessions)}
	})
	fake := newFakeKeys("/", "\x1b", "q")
	picker := &sessionPicker{env: fixedTerminal{120, 40}, keys: startKeys(fake)}
	defer picker.keys.close()
	var out bytes.Buffer
	picker.clear = func() { out.WriteString(screenBreak) }
	if _, ok, err := picker.pickScoped(newPrompter(strings.NewReader(""), &out), &out, choices, "show"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	screens := strings.Split(out.String(), screenBreak)
	if len(screens) != 3 {
		t.Fatalf("want 3 screens (list, filter line, list), got %d:\n%s", len(screens), out.String())
	}
	heading := func(screen string) string {
		first, _, _ := strings.Cut(screen, "\n")
		return first
	}
	if h := heading(screens[1]); !strings.HasSuffix(h, " · Esc clear") || strings.Contains(h, "a all projects") {
		t.Errorf("heading with the filter line open and empty = %q, want it to end in Esc clear", h)
	}
	if h := heading(screens[2]); !strings.HasSuffix(h, " · a all projects") {
		t.Errorf("heading after Esc = %q, want it to end in a all projects", h)
	}
}

// In browse mode Enter shows the details of the highlighted row and the list
// comes back with the filter still on it; in pick mode Enter hands off the
// highlighted row.
func TestKeyFilterEnterActsOnTheHighlightInBrowseAndPickModes(t *testing.T) {
	t.Parallel()
	f, _ := flakyArchive(t)
	// list: the second match, flak0001, opens; b goes back to the filtered
	// list, where Esc clears the filter and q quits.
	fake := newFakeKeys("/", "flaky", "\x1b[B", "\r", "b", "\x1b", "q")
	out, errOut, code := runOnTerminal(t, f.env, fake, "", "list")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	screens := strings.Split(out, clearScreenSequence)
	// Opening, "/", the words, ↓, the details, back, Esc, then the last
	// summary printed after the screen is left.
	if len(screens) < 8 {
		t.Fatalf("%d screens:\n%s", len(screens), out)
	}
	if highlighted(screens[4]) != "Flaky retention test" || !strings.Contains(screens[4], `"flaky" matches 3`) {
		t.Fatalf("after ↓:\n%s", screens[4])
	}
	if !strings.Contains(screens[5], "ID flak0001") || !strings.Contains(screens[5], "t transcript · b back · q quit") {
		t.Fatalf("Enter did not show flak0001:\n%s", screens[5])
	}
	// Back at the list, the filter and the highlight are where they were.
	if highlighted(screens[6]) != "Flaky retention test" || !strings.Contains(screens[6], "\n/flaky") {
		t.Fatalf("back at the list:\n%s", screens[6])
	}
	if len(rowNumbers2(screens[7], "")) < 4 || strings.Contains(screens[7], "\n/flaky") {
		t.Fatalf("after Esc:\n%s", screens[7])
	}

	// handoff: the same keys pick the second match, which is the one handed
	// off. Its rows are this machine's and the archive's, newest first.
	f, _ = flakyArchive(t)
	fake = newFakeKeys("/", "flaky", "\x1b[B", "\r")
	out, errOut, code = runOnTerminal(t, f.env, fake, "p\n", "handoff")
	if code != 0 {
		t.Fatalf("handoff: code=%d stderr=%s\n%s", code, errOut, out)
	}
	screens = strings.Split(out, clearScreenSequence)
	if got := highlighted(screens[len(screens)-1]); got != "Flaky local cleanup · "+notUploadedShortHint {
		t.Fatalf("highlight after ↓ on %q:\n%s", got, out)
	}
	after := out[strings.LastIndex(out, leaveAltScreenSequence):]
	if !strings.Contains(after, "? Continue in") || !strings.Contains(after, "Flaky local cleanup") || strings.Contains(after, "Flaky local rewrite") {
		t.Fatalf("handed off a session other than the second match:\n%s", after)
	}
}

// rowNumbers2 is the numbers of the rows of the table a screen draws, past
// its column header, whose title holds prefix.
func rowNumbers2(screen, prefix string) []int {
	var numbers []int
	// The plain table starts after its header; the width-aware one has none.
	inTable := !strings.HasPrefix(screen, "#") && !strings.Contains(screen, "\n#")
	for line := range strings.SplitSeq(screen, "\n") {
		if strings.HasPrefix(line, "#") {
			inTable = true
			continue
		}
		field, rest, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), cursorMark+" "), " ")
		if n := atoi(field); inTable && n > 0 && strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "session") && strings.Contains(strings.TrimLeft(rest, " ▸"), prefix) {
			numbers = append(numbers, n)
		}
	}
	return numbers
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// A subagent that matches is shown indented under its parent, and the parent
// is shown though it does not match; a subagent has no number, and can be
// picked like any row. The highlight starts on the subagent, the row that
// matches, not on the parent shown for it.
func TestASubagentMatchShowsItsParent(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "parent01", "Orchestrate the release", a.label)
	a.add(t, "child001", "Review and fix PR #208 (5b-1b)", a.label, subagentOf("parent01"))
	a.add(t, "child002", "Write the changelog", a.label, subagentOf("parent01"))
	a.add(t, "other001", "A lone task", a.label)

	// list "208" opens with the words in the filter: the parent, then the one
	// subagent that matches, highlighted, which Enter opens.
	fake := newFakeKeys("\r", "q")
	out, errOut, code := runOnTerminal(t, a.env, fake, "", "list", "208")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	screens := strings.Split(out, clearScreenSequence)
	opening := screens[1]
	if !strings.Contains(opening, `"208" matches 1`) {
		t.Fatalf("heading:\n%s", opening)
	}
	var lines []string
	// The table's rows, above the detail strip that repeats the title.
	table, _, _ := strings.Cut(opening, "\n─")
	for line := range strings.SplitSeq(table, "\n") {
		if strings.Contains(line, "Orchestrate the release") || strings.Contains(line, "(5b-1b)") || strings.Contains(line, "changelog") || strings.Contains(line, "lone task") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "Orchestrate the release") || !strings.Contains(lines[1], "↳ Review and fix PR #208 (5b-1b)") {
		t.Fatalf("rows:\n%s", opening)
	}
	// The parent keeps its number; the subagent has none, and the hint that
	// the parent has subagents stays.
	if fields := strings.Fields(lines[1]); fields[0] != "↳" && fields[0] != cursorMark {
		t.Fatalf("a subagent row starts with %q, want no number:\n%s", fields[0], lines[1])
	}
	if !strings.Contains(lines[0], "Orchestrate the release +2") || highlighted(opening) != "↳ Review and fix PR #208 (5b-1b)" {
		t.Fatalf("parent row %q, highlighted %q", lines[0], highlighted(opening))
	}
	if !strings.Contains(screens[2], "ID child001") {
		t.Fatalf("Enter did not open the subagent:\n%s", screens[2])
	}
	// ↑ moves the highlight onto the parent, which Enter then opens.
	out, _, code = runOnTerminal(t, a.env, newFakeKeys("\x1b[A", "\r", "q"), "", "list", "208")
	screens = strings.Split(out, clearScreenSequence)
	if code != 0 || len(screens) < 4 || highlighted(screens[2]) != "Orchestrate the release +2" || !strings.Contains(screens[3], "ID parent01") {
		t.Fatalf("code=%d after ↑ and Enter:\n%s", code, out)
	}

	// Typed at the filter of the plain list, the same words find the same rows.
	fake = newFakeKeys("/", "208", "\x1b", "q")
	out, _, code = runOnTerminal(t, a.env, fake, "", "list")
	if code != 0 || !strings.Contains(out, "↳ Review and fix PR #208 (5b-1b)") || strings.Contains(out, "changelog") {
		t.Fatalf("code=%d\n%s", code, out)
	}
	// And by lines, the parent comes first and the subagent has no number.
	out, _, code = runOnTerminal(t, a.env, nil, "q\n", "list", "208")
	if code != 0 || !strings.Contains(out, "1 session matches \"208\"") || !strings.Contains(out, "  ↳ Review and fix PR #208 (5b-1b)") {
		t.Fatalf("code=%d\n%s", code, out)
	}
}

func TestFilterRowsNestsMatchingSubagentsUnderTheirParents(t *testing.T) {
	t.Parallel()
	meta := func(id, title, parent string) archive.Metadata {
		return archive.Metadata{SessionID: id + strings.Repeat("0", 24), Title: title, ParentSessionID: parent, Harness: archive.Harness{Name: "claude"}}
	}
	sessions := []archive.Metadata{
		meta("parent01", "Orchestrate the release", ""),
		meta("child001", "Review PR #208", "parent01"+strings.Repeat("0", 24)),
		meta("child002", "Write the changelog", "parent01"+strings.Repeat("0", 24)),
		meta("lone0001", "Handle the 208 report", ""),
		meta("orphan01", "Another #208 review", "gone0001"+strings.Repeat("0", 24)),
	}
	rows := formatSessionRows(sessions, listFormatOptions{Numbered: true})
	shown, matched := filterRows(rows, parseSessionQuery("208"))
	var got []string
	for _, r := range shown {
		got = append(got, strings.TrimSpace(strings.Repeat("  ", r.Depth)+r.Title))
	}
	want := []string{"Orchestrate the release", "Review PR #208", "Handle the 208 report", "Another #208 review"}
	if strings.Join(got, "|") != strings.Join(want, "|") || matched != 3 {
		t.Fatalf("shown %q, matched %d", got, matched)
	}
	if shown[0].Index != 1 || shown[0].Depth != 0 || shown[1].Depth != 1 || shown[2].Index != 4 || shown[3].Depth != 0 {
		t.Fatalf("numbers and depths: %+v", shown)
	}
	// Nothing matches: nothing shown, not the parents.
	if shown, matched := filterRows(rows, parseSessionQuery("nothing")); len(shown) != 0 || matched != 0 {
		t.Fatalf("shown %d, matched %d", len(shown), matched)
	}
}

// list "<words>" on a terminal opens with the words in the filter and the
// rows they match; Esc clears the filter.
func TestListWordsOpenTheKeyBrowserFilteredAndEscClearsThem(t *testing.T) {
	t.Parallel()
	f, label := flakyArchive(t)
	fake := newFakeKeys("\x1b", "q")
	out, errOut, code := runOnTerminal(t, f.env, fake, "", "list", "flaky")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	screens := strings.Split(out, clearScreenSequence)
	if len(screens) != 3 {
		t.Fatalf("%d screens:\n%s", len(screens), out)
	}
	if !strings.Contains(screens[1], label+` · "flaky" matches 3 · codex · Esc clear`) || len(rowNumbers(screens[1])) != 0 && highlighted(screens[1]) == "" {
		t.Fatalf("opening screen:\n%s", screens[1])
	}
	if got := rowNumbers2(screens[1], ""); len(got) != 3 || !strings.HasSuffix(screens[1], "\n/flaky") {
		t.Fatalf("opening rows %v:\n%s", got, screens[1])
	}
	// The highlight stays on the row the filter highlighted.
	if got := rowNumbers2(screens[2], ""); len(got) != 4 || strings.Contains(screens[2], "matches") || highlightedRow(screens[2]) != highlightedRow(screens[1]) || highlightedRow(screens[2]) == 0 {
		t.Fatalf("after Esc: %v\n%s", got, screens[2])
	}
}

// list "<words>" that match only in another project opens on all projects,
// saying nothing in the scope matches while the filter holds those words; the
// scope's sessions are still there: Esc lists all projects under their own
// heading, and a shows the scope.
func TestListWordsFoundOnlyElsewhereKeepTheScopeOneKeyAway(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "mine0001", "Local work here", a.label)
	a.add(t, "else0001", "Elsewhere thing", "billing")
	out, errOut, code := runOnTerminal(t, a.env, newFakeKeys("\x1b", "a", "q"), "", "list", "elsewhere")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	screens := strings.Split(out, clearScreenSequence)
	if len(screens) != 4 {
		t.Fatalf("%d screens:\n%s", len(screens), out)
	}
	if !strings.Contains(screens[1], "Nothing in "+a.label+` · showing all projects · "elsewhere" matches 1`) {
		t.Fatalf("opening screen:\n%s", screens[1])
	}
	if !strings.HasPrefix(screens[2], "All projects · 3 sessions") || !strings.Contains(screens[2], "· a "+a.label) || strings.Contains(screens[2], "Nothing in") {
		t.Fatalf("after Esc:\n%s", screens[2])
	}
	if !strings.HasPrefix(screens[3], a.label+" · 2 sessions") || !strings.Contains(screens[3], "Local work here") || strings.Contains(screens[3], "Elsewhere thing") {
		t.Fatalf("after a:\n%s", screens[3])
	}
}

// What was typed ahead of a pick, in the same burst as its Enter or still
// waiting at the terminal, is handed back for the prompts after the browser:
// text and Enter, with Backspace taking a character back, and keys that move
// dropped. The terminal's input is then not flushed.
func TestPickHandsBackWhatWasTypedAhead(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(30, oneProject)
	// One burst holds the pick's "1" and Enter, then "cx", Backspace, "p",
	// Enter, and an arrow; a second is still at the terminal.
	fake := newFakeKeys("1\rcx\x7fp\r\x1b[B")
	picker := &sessionPicker{env: fixedTerminal{120, 40}, keys: startKeys(fake)}
	var out bytes.Buffer
	row, ok, err := picker.pick(newPrompter(strings.NewReader(""), &out), &out, sessions, listFormatOptions{Now: pickerNow}, "hand off")
	if err != nil || !ok || row.Index != 1 {
		t.Fatalf("picked %+v ok=%v err=%v", row, ok, err)
	}
	var given []byte
	picker.keys.handBack(func(text []byte) { given = append(given, text...) })
	picker.keys.close()
	if string(given) != "cp\n" {
		t.Fatalf("handed back %q, want %q", given, "cp\n")
	}
	if history := fake.history(); strings.Join(history, " ") != "keys lines release" {
		t.Fatalf("terminal calls %v: the input was flushed, or the modes not restored", history)
	}
}

// A handoff picked in key mode still reads the destination prompt's answer
// that was typed ahead, from the same buffer that every prompt reads: the
// answer is not lost to the browser, and nothing else is read.
func TestHandoffPickedByKeysReadsTheTypedAheadDestination(t *testing.T) {
	t.Parallel()
	f, _ := flakyArchive(t)
	// The pick's "1" and Enter, then the answer "p" and Enter, in one burst
	// as a paste or a fast typist sends them; stdin has nothing.
	out, errOut, code := runOnTerminal(t, f.env, newFakeKeys("1\rp\r"), "", "handoff")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	after := out[strings.LastIndex(out, leaveAltScreenSequence):]
	if !strings.Contains(after, "? Continue in") || !strings.Contains(after, "# Handoff: continuing a Codex session") {
		t.Fatalf("the typed-ahead answer was not read:\n%s", after)
	}
	// With none typed ahead, the prompt reads what comes next from stdin.
	out, errOut, code = runOnTerminal(t, f.env, newFakeKeys("1\r"), "p\n", "handoff")
	if code != 0 || !strings.Contains(out, "# Handoff: continuing a Codex session") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	// And a person who quits the browser has none of it handed on.
	out, errOut, code = runOnTerminal(t, f.env, newFakeKeys("q"), "", "handoff")
	if code != 0 || strings.Contains(out, "? Continue in") {
		t.Fatalf("quit: code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// keyScreens runs picker over choices reading keys from chunks, and returns
// what each screen showed.
func keyScreens(t *testing.T, picker *sessionPicker, choices *scopeChoices, action string, chunks ...string) []string {
	t.Helper()
	picker.keys = startKeys(newFakeKeys(chunks...))
	defer picker.keys.close()
	var out bytes.Buffer
	picker.clear = func() { out.WriteString(screenBreak) }
	if _, _, err := picker.pickScoped(newPrompter(strings.NewReader(""), &out), &out, choices, action); err != nil {
		t.Fatal(err)
	}
	return strings.Split(out.String(), screenBreak)
}

// Regenerate with `go test ./internal/cli -run TestKeysListFilteredGolden -update`
// and review the diff.
func TestKeysListFilteredGolden(t *testing.T) {
	t.Parallel()
	sessions := goldenSessions()
	parent := sessions[2]
	sessions = append(sessions, archive.Metadata{
		SessionID: "a1b2c3d4" + strings.Repeat("0", 24), Title: "Review and fix PR #208 (5b-1b)", ParentSessionID: parent.SessionID,
		Harness: parent.Harness, ProjectName: parent.ProjectName, ProjectID: parent.ProjectID, CapturedAt: pickerNow.Add(-3 * time.Hour),
		PullRequests: []archive.PullRequestLink{{Repository: "wangjohn/agent-archive", Number: 208}},
	})
	scope := sessionScope{Label: "agent-archive", RepoKey: scopeKey, Dir: "/w/agent-archive", ProjectIDs: []string{"project-agent-archive"}}
	format := listFormatOptions{Now: pickerNow, GroupByDay: true, Numbered: true}
	choices := newScopeChoices(scope, format, false, archiveRows(sessions, defaultListLimit, format))
	// /208 narrows to the PR-208 reviewer under its parent, and highlights it,
	// the row that matches.
	screens := keyScreens(t, &sessionPicker{env: fixedTerminal{100, 30}}, choices, "show", "/", "208")
	if len(screens) != 3 {
		t.Fatalf("%d screens", len(screens))
	}
	golden.Check(t, filepath.Join("testdata", "browse", "keys-list-filtered.txt"), []byte(screens[2]))
	// The whole list hides the subagent the filter highlighted, so Esc
	// leaves the highlight on its first row.
	choices = newScopeChoices(scope, format, false, archiveRows(sessions, defaultListLimit, format))
	screens = keyScreens(t, &sessionPicker{env: fixedTerminal{100, 30}}, choices, "show", "/", "208", "\x1b", "q")
	if last := screens[len(screens)-1]; highlightedRow(last) != 1 || strings.Contains(last, "#208 (5b-1b)") {
		t.Fatalf("after Esc:\n%s", last)
	}
}

// Regenerate with `go test ./internal/cli -run TestKeysHandoffFilteredGolden -update`
// and review the diff.
func TestKeysHandoffFilteredGolden(t *testing.T) {
	t.Parallel()
	sessions := goldenSessions()
	scope := sessionScope{Label: "agent-archive", RepoKey: scopeKey, Dir: "/w/agent-archive", ProjectIDs: []string{"project-agent-archive"}}
	format := listFormatOptions{Now: pickerNow, GroupByDay: true, Numbered: true}
	rows := make([]handoffPickerRow, len(sessions))
	for i, m := range sessions {
		// The first two are registered here, active just now; the last is on
		// this machine only.
		rows[i] = handoffPickerRow{metadata: m, active: m.CapturedAt, registered: i < 2, notUploaded: i == 4}
	}
	choices := newScopeChoices(scope, format, false, func(sessionScope) scopeView {
		return scopeView{rows: formatHandoffRows(rows, format), total: len(rows)}
	})
	// /the narrows to three sessions, one of them live; ↓ highlights the second.
	screens := keyScreens(t, &sessionPicker{env: fixedTerminal{100, 30}, verb: "Hand off"}, choices, "hand off", "/", "the", "\x1b[B")
	if len(screens) != 4 {
		t.Fatalf("%d screens", len(screens))
	}
	golden.Check(t, filepath.Join("testdata", "browse", "keys-handoff-filtered.txt"), []byte(screens[3]))
}

// pick is pickRows over the rows of sessions, numbered.
func (l *sessionPicker) pick(p *prompter, stdout io.Writer, sessions []archive.Metadata, format listFormatOptions, action string) (listRow, bool, error) {
	format.Numbered = true
	return l.pickRows(p, stdout, formatSessionRows(sessions, format), len(sessions), false, format, action)
}
