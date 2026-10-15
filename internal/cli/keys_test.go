package cli

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// An escape sequence split by a slow link still scrolls: within the wait
// the two parts are one burst, and after it the rest, starting the next
// burst, is read as the key rather than typed. A lone Esc followed later
// by other text stays text.
func TestKeysReadASplitEscapeSequenceAsItsKey(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(50, oneProject)
	for _, c := range []struct {
		chunks  []string
		screens int
	}{
		// Within the wait: one burst, drawn once.
		{[]string{"\x1b", later(50*time.Millisecond, "[B"), "q"}, 2},
		{[]string{"\x1bO", later(50*time.Millisecond, "B"), "q"}, 2},
		// Waited out: the Esc on its own, then the arrow.
		{[]string{"\x1b", later(300*time.Millisecond, "[B"), "q"}, 3},
		{[]string{"\x1b[", later(300*time.Millisecond, "B"), "q"}, 2},
	} {
		_, ok, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{}, c.chunks...)
		last := screens[len(screens)-1]
		if ok || len(screens) != c.screens || rowSpan(last) != "2-16" || !strings.HasSuffix(last, " to quit: ") {
			t.Errorf("%q: %d screens, want %d:\n%s", c.chunks, len(screens), c.screens, strings.Join(screens, "\n----\n"))
		}
	}
	// Esc then O and x typed; a cut-off ESC [ then B typed long after, or
	// after a resize.
	for _, chunks := range [][]string{
		{"\x1b", later(300*time.Millisecond, "Ox")},
		{"\x1b[", later(time.Minute, "Ox")},
		{"\x1b[", string(fakeResize), "Ox"},
		{"\x1b[", string(fakeSuspend), "Ox"},
	} {
		_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{}, append(chunks, "\x1b")...)
		typed := screens[len(screens)-2]
		if !strings.HasSuffix(typed, " to quit: Ox") || rowSpan(typed) != "1-15" {
			t.Errorf("%q:\n%s", chunks, strings.Join(screens, "\n----\n"))
		}
	}
}

// Esc on the details does nothing: it may be a wheel's arrow cut in two,
// and Enter, Backspace, and b go back.
func TestKeyDetailsIgnoreEsc(t *testing.T) {
	t.Parallel()
	out, _, _ := browseKeys(t, fixedTerminal{100, 9}, newFakeKeys("1\r", "\x1b", later(300*time.Millisecond, "[B"), "q"))
	screens := keyDetailsScreens(out)
	if strings.Count(out, "Enter number") != 1 || len(screens) != 3 || !strings.Contains(screens[2], "↑ 1 line above") {
		t.Fatalf("details:\n%s", strings.Join(screens, "\n----\n"))
	}
}

// Keys of one burst move on from each other: PgDn PgDn goes two screens,
// and ↓↓ then PgDn a screen from the third row.
func TestKeyPickerBurstsMoveOnFromEachKey(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(80, oneProject)
	for chunk, want := range map[string]string{
		"\x1b[6~\x1b[6~":             "31-45",
		"\x1b[B\x1b[B\x1b[6~":        "18-32",
		"\x1b[6~\x1b[6~\x1b[5~":      "16-30",
		"\x1b[F\x1b[5~\x1b[A":        "50-64",
		"  \x1b[B":                   "32-46",
		"\x1b[6~\x1b[B\x1b[B\x1b[6~": "33-47",
	} {
		_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 19}}, sessions, listFormatOptions{}, chunk, "q")
		if len(screens) != 2 || rowSpan(screens[1]) != want {
			t.Errorf("%q: rows %s, want %s", chunk, rowSpan(screens[len(screens)-1]), want)
		}
	}
}

// A character cut in two by the cap on a burst is read whole with the rest.
func TestKeysCarryWhatTheBurstCapCuts(t *testing.T) {
	t.Parallel()
	_, _, screens := runKeyPicker(t, &sessionPicker{env: fixedTerminal{120, 20}}, pickerSessions(50, oneProject), listFormatOptions{}, strings.Repeat(" ", maxBurst-1)+"é", "\x7f", "q")
	if len(screens) < 2 || !strings.HasSuffix(screens[len(screens)-2], " to quit: é") {
		t.Fatalf("screens:\n%s", strings.Join(screens, "\n----\n"))
	}
}

// adjustableTerminal is a terminal whose size a test changes.
type adjustableTerminal struct {
	mu   sync.Mutex
	size fixedTerminal
}

func (a *adjustableTerminal) terminalSize(out io.Writer) (int, int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.size.terminalSize(out)
}

func (a *adjustableTerminal) resize(size fixedTerminal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.size = size
}

// A window made taller scrolls less far: the list's first row and the
// summary's top line are clamped to the new last screen.
func TestKeyScreensClampAfterAResize(t *testing.T) {
	t.Parallel()
	size := &adjustableTerminal{size: fixedTerminal{120, 11}}
	fake := newFakeKeys("\x1b[F", string(fakeResize), "q")
	fake.onResize = func() { size.resize(fixedTerminal{120, 29}) }
	keys := startKeys(fake)
	var out bytes.Buffer
	picker := &sessionPicker{env: size, keys: keys, clear: func() { out.WriteString(screenBreak) }}
	if _, _, err := picker.pick(newPrompter(strings.NewReader(""), &out), &out, pickerSessions(50, oneProject), listFormatOptions{Now: pickerNow}, "show"); err != nil {
		t.Fatal(err)
	}
	keys.close()
	screens := strings.Split(out.String(), screenBreak)
	if len(screens) != 3 || rowSpan(screens[1]) != "44-50" || rowSpan(screens[2]) != "26-50" || !strings.HasPrefix(statusLine(screens[2]), "Bottom") {
		t.Fatalf("screens:\n%s", strings.Join(screens, "\n----\n"))
	}
	checkKeyScreensFit(t, screens[2:], 120, 29)

	env, _, _ := publishedFixture(t)
	details := &adjustableTerminal{size: fixedTerminal{100, 9}}
	env.TerminalSize = details.terminalSize
	fake = newFakeKeys("1\r", "\x1b[F", string(fakeResize), "q")
	fake.onResize = func() { details.resize(fixedTerminal{100, 12}) }
	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return fake, true }
	if code := Run([]string{"list"}, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	drawn := keyDetailsScreens(stdout.String())
	// 10 lines: 5 fit in 9 rows, 8 in 12.
	if len(drawn) != 3 || !strings.Contains(drawn[1], "\n↑ 5 lines above\n") || !strings.Contains(drawn[2], "\n↑ 2 lines above\n") || displayLines(drawn[2], 100) != 12 {
		t.Fatalf("details:\n%s", strings.Join(drawn, "\n----\n"))
	}
}

// Once closed, key mode stays off: the interrupt handler's restore cannot
// be undone by the browser turning keys back on after a pager.
func TestKeysStayOffOnceClosed(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys()
	keys := startKeys(fake)
	keys.close()
	if err := keys.resume(); err != nil || fake.keyMode() {
		t.Fatalf("resumed after close: %v %v", err, fake.history())
	}
	keys.close()
	if got, want := fake.history(), []string{"keys", "flush", "lines", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal modes %v, want %v", got, want)
	}
}

// Ctrl-Z shows the normal screen while the process is stopped, and the
// browser's again, redrawn, once it continues.
func TestKeysSuspendLeavesTheAlternateScreen(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys(string(fakeSuspend), "q")
	out, _, _ := browseKeys(t, fixedTerminal{100, 20}, fake)
	stop := strings.Index(out, leaveAltScreenSequence)
	if stop < 0 || !strings.HasPrefix(out[stop:], leaveAltScreenSequence+enterAltScreenSequence+clearScreenSequence) {
		t.Fatalf("output %q", out)
	}
	if got, want := fake.history(), []string{"keys", "lines", "stop", "keys", "flush", "lines", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal modes %v, want %v", got, want)
	}
}
