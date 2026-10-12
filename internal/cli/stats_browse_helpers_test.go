package cli

import (
	"github.com/wangjohn/agent-archive/internal/stats"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// statsScreenOutput is a terminal for the interactive stats screen's tests: it can
// be read while the screen writes, and says whether it has color.
type statsScreenOutput struct {
	syncBuffer
	color bool
}

func (s *statsScreenOutput) colorTerminal() bool { return s.color }

// screenOptions is how a test runs the interactive screen.
type screenOptions struct {
	width  int
	height int
	color  bool
	// days is the window shown first; 0 is 30.
	days int
	// dir is the folder h saves into.
	dir string
	// size, when set, is the terminal's size, which the test may change.
	size *adjustableTerminal
	// fake, when set, is the terminal's keys.
	fake *fakeKeys
	// tweak, when set, changes the Env before the screen runs.
	tweak func(*Env)
	// inputs, when set, are the sessions to count instead of the fixture's.
	inputs *statsInputs
	// filters, when set, are echoed in the title as --harness and --model are.
	filters statsFilters
}

// screenRun is one run of the interactive screen.
type screenRun struct {
	out     *statsScreenOutput
	fake    *fakeKeys
	code    int
	ran     bool
	stderr  string
	view    statsView
	inputs  statsInputs
	stopped bool
	frames  [][]string
}

// screenInputs are the fixture's sessions, counted as the stats tests do.
func screenInputs(tb testing.TB) statsInputs {
	tb.Helper()
	var sessions []archive.Metadata
	for _, s := range statsFixtureSessions() {
		sessions = append(sessions, s.build())
	}
	return statsInputs{hasSessions: len(sessions) > 0, prepared: stats.Prepare(sessions, stats.PrepareOptions{Location: statsNow.Location()}), now: statsNow, location: statsNow.Location()}
}

// runScreen shows the interactive screen with the keys of chunks, as
// runStatsBrowser does for the command, and returns what it drew.
func runScreen(t *testing.T, o screenOptions, chunks ...string) screenRun {
	t.Helper()
	if o.width == 0 {
		o.width, o.height = 80, 24
	}
	if o.days == 0 {
		o.days = 30
	}
	if o.size == nil {
		o.size = &adjustableTerminal{size: fixedTerminal{o.width, o.height}}
	}
	if o.fake == nil {
		o.fake = newFakeKeys(chunks...)
	}
	env := testEnv(t, t.TempDir(), statsNow)
	stdin := strings.NewReader("")
	out := &statsScreenOutput{color: o.color}
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(out) }
	env.TerminalSize = o.size.terminalSize
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return o.fake, true }
	stopped := make(chan struct{})
	env.Interrupts = func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() { close(stopped) } }
	if o.dir != "" {
		env.WorkingDir = func() (string, error) { return o.dir, nil }
	}
	if o.tweak != nil {
		o.tweak(&env)
	}
	inputs := screenInputs(t)
	if o.inputs != nil {
		inputs = *o.inputs
	}
	inputs.filters = o.filters
	windows, index := statsWindowCycle(o.days)
	view := statsView{style: textStyle{color: o.color}, glyphs: unicodeGlyphs, filters: o.filters}
	var stderr strings.Builder
	code, ran := runStatsBrowser(env, stdin, out, &stderr, statsBrowserStart{
		inputs: inputs, windows: windows, window: index, first: inputs.compute(o.days, true), view: view,
	})
	run := screenRun{out: out, fake: o.fake, code: code, ran: ran, stderr: stderr.String(), view: view, inputs: inputs}
	select {
	case <-stopped:
		run.stopped = true
	default:
	}
	run.frames = screenFrames(out.String())
	return run
}

// screenFrames are the frames drawn, each as its rows: the output between
// clears, less the sequences that hide and show the cursor and what follows
// leaving the screen.
func screenFrames(out string) [][]string {
	parts := strings.Split(out, clearScreenSequence)
	var frames [][]string
	for _, part := range parts[1:] {
		part, _, _ = strings.Cut(part, leaveAltScreenSequence)
		part = strings.ReplaceAll(part, hideCursorSequence, "")
		part = strings.ReplaceAll(part, showCursorSequence, "")
		frames = append(frames, strings.Split(part, "\n"))
	}
	return frames
}

// expectedLines is the page as the screen lays it out for width columns:
// what the static command prints, with the interactive footer.
func (r screenRun) expectedLines(page statsPage, days, width int) []string {
	view := r.view
	view.width = width
	view.interactive = true
	lines := renderPage(page, r.inputs.compute(days, true), view)
	for i, line := range lines {
		lines[i] = truncateVisible(line, width)
	}
	return lines
}

// checkFrame fails unless rows are a frame of a terminal width by height
// that shows lines from top on above a last row that satisfies bar.
func checkFrame(t *testing.T, rows, lines []string, top, width, height int, bar func(string) bool) {
	t.Helper()
	if len(rows) != height {
		t.Fatalf("the frame has %d rows, want %d:\n%s", len(rows), height, strings.Join(rows, "\n"))
	}
	for i, row := range rows {
		if w := visibleWidth(row); w > width {
			t.Errorf("row %d is %d columns, over %d: %q", i, w, width, row)
		}
	}
	body := height - 1
	for i := range body {
		want := ""
		if top+i < len(lines) {
			want = lines[top+i]
		}
		if rows[i] != want {
			t.Fatalf("row %d = %q, want %q", i, rows[i], want)
		}
	}
	if !bar(rows[height-1]) {
		t.Errorf("the last row is %q", rows[height-1])
	}
}

// anyBar accepts any last row.
func anyBar(string) bool { return true }

// barHas accepts a last row with all of parts in it.
func barHas(parts ...string) func(string) bool {
	return func(row string) bool {
		for _, part := range parts {
			if !strings.Contains(row, part) {
				return false
			}
		}
		return true
	}
}
