package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// pagerCall is one page the browser opened.
type pagerCall struct {
	command string
	text    string
}

// browse runs `list` (or args) interactively on input, with a recording
// pager, and returns what was printed.
func browse(t *testing.T, input string, args ...string) (out, errOut string, pages []pagerCall, id string) {
	t.Helper()
	return browseSized(t, fixedTerminal{}, input, args...)
}

// browseSized is browse on a terminal of the given size.
func browseSized(t *testing.T, size fixedTerminal, input string, args ...string) (out, errOut string, pages []pagerCall, id string) {
	t.Helper()
	env, _, id := publishedFixture(t)
	env.TerminalSize = size.terminalSize
	stdin := strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&stdout)
	}
	env.RunPager = func(_ context.Context, command string, _ []string, in io.Reader, _, _ io.Writer) error {
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
	return stdout.String(), stderr.String(), pages, id
}

const detailsPrompt = "[t] transcript  [Enter/b] back to list  [q] quit"

func TestBrowserBackReturnsToList(t *testing.T) {
	t.Parallel()
	out, errOut, _, _ := browse(t, "1\nb\n1\n\nq\n")
	if errOut != "" {
		t.Fatalf("stderr: %s", errOut)
	}
	if n := strings.Count(out, "Enter number"); n != 3 {
		t.Fatalf("list shown %d times, want 3:\n%s", n, out)
	}
	if n := strings.Count(out, detailsPrompt); n != 2 {
		t.Fatalf("details shown %d times, want 2:\n%s", n, out)
	}
	// Each view starts on a cleared screen instead of below the last one.
	if n := strings.Count(out, clearScreenSequence); n != 5 {
		t.Fatalf("screen cleared %d times, want 5:\n%q", n, out)
	}
}

func TestBrowserQuitFromDetails(t *testing.T) {
	t.Parallel()
	out, _, pages, id := browse(t, "1\nq\n")
	if strings.Count(out, "Enter number") != 1 || strings.Count(out, detailsPrompt) != 1 || len(pages) != 0 {
		t.Fatalf("q did not quit from the details:\n%s", out)
	}
	if !strings.HasPrefix(out, enterAltScreenSequence) || strings.Count(out, leaveAltScreenSequence) != 1 {
		t.Fatalf("alternate screen not entered and left once:\n%q", out)
	}
	_, after, _ := strings.Cut(out, leaveAltScreenSequence)
	if !strings.Contains(after, "ID "+id) || !strings.Contains(after, "--transcript") {
		t.Fatalf("last summary not printed to the normal screen:\n%q", after)
	}
}

func TestBrowserTranscriptOpensPager(t *testing.T) {
	t.Parallel()
	out, _, pages, _ := browse(t, "1\nt\nq\n")
	if len(pages) != 1 {
		t.Fatalf("pager ran %d times:\n%s", len(pages), out)
	}
	// The default pager waits for q even for a short transcript, since the
	// details are redrawn when it exits.
	if !strings.HasPrefix(pages[0].command, "less -RX --mouse ") || !strings.HasSuffix(pages[0].command, "q back' -+F") || !strings.Contains(pages[0].text, "visible") {
		t.Fatalf("pager = %q:\n%s", pages[0].command, pages[0].text)
	}
	// After the pager, the details are drawn again.
	if n := strings.Count(out, detailsPrompt); n != 2 || strings.Contains(out, "visible") {
		t.Fatalf("details shown %d times after the pager:\n%s", n, out)
	}
	// The browser enters the alternate screen again after the pager, in
	// case the pager left it.
	if n := strings.Count(out, enterAltScreenSequence); n != 2 {
		t.Fatalf("alternate screen entered %d times, want 2:\n%q", n, out)
	}
}

func TestBrowserTranscriptWithoutPagerWaits(t *testing.T) {
	t.Parallel()
	out, _, pages, _ := browse(t, "1\nt\nx\n\nq\n", "list", "--no-pager")
	if len(pages) != 0 || !strings.Contains(out, "visible") {
		t.Fatalf("transcript not printed directly (pages=%d):\n%s", len(pages), out)
	}
	if !strings.Contains(out, "[Enter/b] back to details  [q] quit") || !strings.Contains(out, "Enter b (or just Enter) for the details") {
		t.Fatalf("no prompt after the transcript:\n%s", out)
	}
	if n := strings.Count(out, detailsPrompt); n != 2 {
		t.Fatalf("details shown %d times, want 2:\n%s", n, out)
	}
}

func TestBrowserEndOfInputRestoresScreen(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "1\n", "1\nt\n"} {
		out, _, _, _ := browse(t, input, "list", "--no-pager")
		if strings.Count(out, enterAltScreenSequence) != 1 || strings.Count(out, leaveAltScreenSequence) != 1 {
			t.Fatalf("%q: alternate screen not restored:\n%q", input, out)
		}
		_, after, _ := strings.Cut(out, leaveAltScreenSequence)
		if input == "" && after != "" {
			t.Fatalf("%q: printed after leaving with nothing viewed: %q", input, after)
		}
		if input != "" && !strings.Contains(after, "--transcript") {
			t.Fatalf("%q: last summary missing: %q", input, after)
		}
	}
}

func TestBrowserInvalidInputAsksAgain(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browse(t, "1\nzz\nq\n")
	if !strings.Contains(out, "Enter t for the transcript, b (or just Enter) for the list, or q to quit.") || strings.Count(out, detailsPrompt) != 2 {
		t.Fatalf("invalid answer not re-prompted:\n%s", out)
	}
	// Asking again keeps the summary on screen: no redraw.
	if n := strings.Count(out, clearScreenSequence); n != 2 {
		t.Fatalf("screen cleared %d times, want 2:\n%q", n, out)
	}
}

type screenStub struct {
	terminal bool
	signals  chan os.Signal
	stopped  bool
}

func (s *screenStub) interactive(any) bool { return s.terminal }

func (s *screenStub) interrupts() (<-chan os.Signal, func()) {
	return s.signals, func() { s.stopped = true }
}

func TestAltScreenOnlyOnATerminal(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	s := enterAltScreen(&out, &screenStub{})
	s.clear()
	s.leave()
	if out.Len() != 0 {
		t.Fatalf("wrote to a non-terminal: %q", out.String())
	}
}

// An interrupt restores the screen before the process exits. While a pager
// owns the terminal, Ctrl-C is left to it and any other signal stops the
// pager first; the browser exits once the pager has.
func TestAltScreenRestoresOnInterrupt(t *testing.T) {
	t.Parallel()
	var out syncBuffer
	stub := &screenStub{terminal: true, signals: make(chan os.Signal, 2)}
	exited := make(chan int, 1)
	s := &altScreen{out: &out, exit: func(code int) { exited <- code }}
	s.start(stub.signals, func() { stub.stopped = true })

	stopped := make(chan struct{})
	s.startPaging(func() { close(stopped) })
	stub.signals <- os.Interrupt // the pager's
	stub.signals <- syscall.SIGTERM
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not stop the pager")
	}
	select {
	case code := <-exited:
		t.Fatalf("exited %d while the pager ran", code)
	default:
	}
	sig := s.endPaging()
	if sig != syscall.SIGTERM || out.String() != enterAltScreenSequence {
		t.Fatalf("pending %v, output %q", sig, out.String())
	}
	s.exitForSignal(sig)
	if code := <-exited; code != 128+int(syscall.SIGTERM) || out.String() != enterAltScreenSequence+leaveAltScreenSequence || !stub.stopped {
		t.Fatalf("exit %d, output %q, stopped %v", code, out.String(), stub.stopped)
	}
	s.leave() // a second leave writes nothing
	if out.String() != enterAltScreenSequence+leaveAltScreenSequence {
		t.Fatalf("output %q", out.String())
	}
}

func TestAltScreenExitsOnInterruptOutsideThePager(t *testing.T) {
	t.Parallel()
	var out syncBuffer
	signals := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	s := &altScreen{out: &out, exit: func(code int) { exited <- code }}
	s.start(signals, func() {})
	signals <- os.Interrupt
	select {
	case code := <-exited:
		if code != 130 || out.String() != enterAltScreenSequence+leaveAltScreenSequence {
			t.Fatalf("exit %d, output %q", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit after Ctrl-C")
	}
}

// Stopping a pager signals it and waits for it to exit.
func TestPagerStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Env{}.runPager(ctx, "sleep 30", nil, strings.NewReader(""), io.Discard, io.Discard)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(pagerStopDelay + 5*time.Second):
		t.Fatal("pager still running after cancel")
	}
}

// For the browser, less is told to wait for q even when the text fits on
// one screen; another pager is run as given.
func TestBrowserPagerCommand(t *testing.T) {
	t.Parallel()
	for pager, want := range map[string]string{
		"":                     "less -RX --mouse --wheel-lines=3 " + lessPrompts(1, true) + " -+F",
		"less -FR":             "less -FR -+F",
		"/usr/bin/less":        "/usr/bin/less -RX --mouse --wheel-lines=3 " + lessPrompts(1, true) + " -+F",
		"less -R":              "less -R -+F",
		"most":                 "most",
		"less -R | tee /tmp/x": "less -R | tee /tmp/x",
		"less -R # note":       "less -R # note",
		"less -- -R":           "less -- -R",
		"less 'prompt'":        "less 'prompt'",
		"less -R\\":            "less -R\\",
	} {
		env := testEnv(t, t.TempDir(), time.Now())
		var out bytes.Buffer
		env.IsTerminal = func(any) bool { return true }
		env.LookupEnv = func(key string) (string, bool) { return pager, key == "PAGER" && pager != "" }
		var got string
		env.RunPager = func(_ context.Context, command string, _ []string, _ io.Reader, _, _ io.Writer) error {
			got = command
			return nil
		}
		paged, waited, err := pageText(context.Background(), &out, io.Discard, env, false, true, []byte("x"))
		if err != nil || !paged || got != want || waited != strings.Contains(want, "-+F") {
			t.Errorf("%q: ran %q (waited %v), want %q", pager, got, waited, want)
		}
	}
}

// A pager other than less may return at once, so the browser waits before
// redrawing; after it, the browser enters the alternate screen again in
// case the pager left it.
func TestBrowserWaitsAfterOtherPagers(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	stdin := strings.NewReader("1\nt\n\nq\n")
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.LookupEnv = func(key string) (string, bool) { return "most", key == "PAGER" }
	pages := 0
	env.RunPager = func(_ context.Context, command string, _ []string, _ io.Reader, _, _ io.Writer) error {
		pages++
		if command != "most" {
			t.Errorf("pager %q", command)
		}
		return nil
	}
	if code := Run([]string{"list"}, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if pages != 1 || !strings.Contains(out, "[Enter/b] back to details  [q] quit") || strings.Count(out, detailsPrompt) != 2 || !strings.Contains(out, "t opens the transcript in your pager; quit it to return here.") {
		t.Fatalf("pages=%d:\n%s", pages, out)
	}
	if n := strings.Count(out, enterAltScreenSequence); n != 2 {
		t.Fatalf("alternate screen entered %d times, want 2:\n%q", n, out)
	}
}

func (s *screenStub) exit(int) {}

// A signal while the transcript's pager runs stops the pager, then the
// browser restores the screen and exits as the signal would have, without
// printing the transcript as a failed pager's fallback. The whole summary
// (m) is paged the same way.
func TestBrowserSignalDuringTranscript(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"t", "m"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			testBrowserSignalWhilePaging(t, key)
		})
	}
}

func testBrowserSignalWhilePaging(t *testing.T, key string) {
	t.Helper()
	env, _, _ := publishedFixture(t)
	// Short enough that the details are cut, so m is offered.
	env.TerminalSize = fixedTerminal{100, 9}.terminalSize
	stdin := strings.NewReader("1\n" + key + "\nq\n")
	var stdout, stderr syncBuffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	signals := make(chan os.Signal, 1)
	env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	exited := make(chan int, 1)
	env.exitProcess = func(code int) { exited <- code }
	cancelled := make(chan struct{})
	env.RunPager = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
		signals <- syscall.SIGTERM
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	done := make(chan int, 1)
	go func() { done <- Run([]string{"list"}, stdin, &stdout, &stderr, env) }()
	select {
	case code := <-exited:
		if code != 128+int(syscall.SIGTERM) {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", stdout.String())
	}
	<-cancelled
	<-done
	out := stdout.String()
	// The fake exit returns, so what follows it is not checked.
	if strings.Count(out, leaveAltScreenSequence) != 1 || strings.Contains(out, "visible") || strings.Contains(stderr.String(), "printing directly") {
		t.Fatalf("output %q, stderr %q", out, stderr.String())
	}
}

const cutDetailsPrompt = "[t] transcript  [m] more  [Enter/b] back to list  [q] quit"

// detailsScreens is each drawing of the details in out, as far as its
// prompt.
func detailsScreens(out string) []string {
	const end = "back to list  [q] quit: "
	var screens []string
	for screen := range strings.SplitSeq(out, clearScreenSequence) {
		screen, _, _ = strings.Cut(screen, leaveAltScreenSequence)
		if at := strings.Index(screen, end); at >= 0 {
			screens = append(screens, screen[:at+len(end)])
		}
	}
	return screens
}

// Details taller than the terminal are cut to fit above the prompt, and m
// shows the whole summary through the pager, as t shows the transcript.
func TestBrowserDetailsCutToFitTheWindow(t *testing.T) {
	t.Parallel()
	out, _, pages, id := browseSized(t, fixedTerminal{100, 9}, "1\nm\nq\n")
	screens := detailsScreens(out)
	if len(screens) != 2 {
		t.Fatalf("details drawn %d times, want 2:\n%s", len(screens), out)
	}
	for _, screen := range screens {
		if n := displayLines(screen, 100); n != 9 || !strings.Contains(screen, cutDetailsPrompt) || !strings.Contains(screen, " more lines\n") || strings.Contains(screen, "ID "+id) {
			t.Fatalf("cut details take %d rows of 9:\n%s", n, screen)
		}
	}
	if len(pages) != 1 || !strings.HasPrefix(pages[0].command, "less -RX --mouse") || !strings.HasSuffix(pages[0].command, "q back' -+F") || !strings.Contains(pages[0].text, "ID "+id) || strings.Contains(pages[0].text, "visible") {
		t.Fatalf("m paged %+v", pages)
	}
}

func TestBrowserDetailsDrawnWholeWhenTheyFit(t *testing.T) {
	t.Parallel()
	for _, size := range []fixedTerminal{{}, {100, 60}} {
		out, _, _, id := browseSized(t, size, "1\nq\n")
		screens := detailsScreens(out)
		if len(screens) != 1 || !strings.Contains(screens[0], detailsPrompt) || strings.Contains(screens[0], "[m]") || strings.Contains(screens[0], "more line") || !strings.Contains(screens[0], "ID "+id) {
			t.Fatalf("%v: details:\n%s", size, out)
		}
	}
}

// A cut summary says how to see the rest when an answer is not understood.
func TestBrowserCutDetailsInvalidInputMentionsMore(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browseSized(t, fixedTerminal{100, 9}, "1\nzz\nq\n")
	if !strings.Contains(out, "Enter t for the transcript, m for the whole summary, b (or just Enter) for the list, or q to quit.") {
		t.Fatalf("no hint about m:\n%s", out)
	}
}

// The details say how to scroll and leave the transcript, when a pager
// shows it.
func TestBrowserDetailsExplainTheTranscriptPager(t *testing.T) {
	t.Parallel()
	const lessHint = "t opens the transcript: scroll with the wheel or arrows, q returns here."
	out, _, _, _ := browse(t, "1\nq\n")
	if !strings.Contains(out, "\n\n"+lessHint+"\n"+detailsPrompt) {
		t.Fatalf("no pager hint:\n%s", out)
	}
	out, _, _, _ = browse(t, "1\nq\n", "list", "--no-pager")
	if strings.Contains(out, "t opens") {
		t.Fatalf("pager hint without a pager:\n%s", out)
	}
}

// Regenerate with `go test ./internal/cli -run TestBrowserCutDetailsGolden
// -update` and review the diff.
func TestBrowserCutDetailsGolden(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), summaryNow)
	var out bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(&out) }
	env.TerminalSize = fixedTerminal{80, 14}.terminalSize
	b := &sessionBrowser{env: env, prompt: newPrompter(strings.NewReader("q\n"), &out), stdout: &out, screen: &altScreen{}}
	summary := renderSummaryText(summaryFixture(), summaryOptions{Now: summaryNow, Location: time.UTC})
	hint := b.transcriptHint()
	rest, notice, redraw := b.drawDetails(summary, hint, browseNotice{})
	if action, _, err := b.detailsPrompt(context.Background(), listRow{}, []byte(summary), rest, hint, notice, redraw); err != nil || action != browseQuit || rest == "" {
		t.Fatalf("action %v, err %v, rest %q", action, err, rest)
	}
	if n := displayLines(out.String(), 80); n != 14 {
		t.Fatalf("cut details take %d rows of 14:\n%s", n, out.String())
	}
	golden.Check(t, filepath.Join("testdata", "browse", "details-cut.txt"), out.Bytes())
}

// Without a pager, m prints only the lines the details left out, below
// them, and asks again.
func TestBrowserMoreWithoutPagerPrintsTheRest(t *testing.T) {
	t.Parallel()
	out, _, pages, id := browseSized(t, fixedTerminal{100, 9}, "1\nm\nq\n", "list", "--no-pager")
	if len(pages) != 0 {
		t.Fatalf("paged %+v", pages)
	}
	screens := strings.Split(out, clearScreenSequence)
	details, _, _ := strings.Cut(screens[len(screens)-1], leaveAltScreenSequence)
	shown, after, ok := strings.Cut(details, cutDetailsPrompt+": ")
	if !ok || strings.Count(details, "\n"+detailsPrompt+": ") != 1 {
		t.Fatalf("no prompt after the rest:\n%s", details)
	}
	cutAt := strings.Index(shown, "\n… ")
	if cutAt < 0 {
		t.Fatalf("details not cut:\n%s", shown)
	}
	lastShown := shown[strings.LastIndex(shown[:cutAt], "\n")+1 : cutAt]
	if !strings.Contains(after, "ID "+id) || strings.Contains(after, lastShown) || strings.Contains(after, " more lines") {
		t.Fatalf("m printed more than the rest:\n%s", after)
	}
}

// On a screen that can be cleared, a message about an answer replaces the
// blank line above the redrawn details instead of scrolling them.
func TestBrowserDetailsRedrawWithAMessage(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browseSized(t, fixedTerminal{100, 9}, "1\nzz\nq\n")
	screens := detailsScreens(out)
	if len(screens) != 2 {
		t.Fatalf("details drawn %d times, want 2:\n%s", len(screens), out)
	}
	const message = "Enter t for the transcript, m for the whole summary, b (or just Enter) for the list, or q to quit.\nt opens"
	if n := displayLines(screens[1], 100); n != 9 || !strings.Contains(screens[1], " more lines\n"+message) {
		t.Fatalf("redrawn details take %d rows of 9:\n%s", n, screens[1])
	}
}

// A message that wraps is cut to one row when the summary is already at its
// fewest lines, so the redrawn details still fit.
func TestBrowserDetailsCutAWrappedMessageToFit(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browseSized(t, fixedTerminal{80, 9}, "1\nzz\nq\n")
	screens := detailsScreens(out)
	if len(screens) != 2 {
		t.Fatalf("details drawn %d times, want 2:\n%s", len(screens), out)
	}
	if n := displayLines(screens[1], 80); n != 9 || !strings.Contains(screens[1], " more lines\nEnter t for the transcript, m for") || !strings.Contains(screens[1], "…\nt opens") {
		t.Fatalf("redrawn details take %d rows of 9:\n%s", n, screens[1])
	}
}

// Once m has printed the rest without a pager, the details are not drawn
// again (that would cut the summary again), and m is no longer offered.
func TestBrowserMoreWithoutPagerKeepsTheWholeSummary(t *testing.T) {
	t.Parallel()
	out, _, _, id := browseSized(t, fixedTerminal{100, 9}, "1\nm\nm\nq\n", "list", "--no-pager")
	if screens := detailsScreens(out); len(screens) != 1 {
		t.Fatalf("details drawn %d times, want 1:\n%s", len(screens), out)
	}
	_, after, _ := strings.Cut(out, cutDetailsPrompt+": ")
	if !strings.Contains(after, "ID "+id) || !strings.Contains(after, "Enter t for the transcript, b (or just Enter) for the list, or q to quit.\n\n"+detailsPrompt+": ") || strings.Contains(after, cutDetailsPrompt) {
		t.Fatalf("second m:\n%s", after)
	}
}

// However short the terminal, the details show a few lines of the summary.
func TestBrowserDetailsShowAFewLinesOnATinyTerminal(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browseSized(t, fixedTerminal{100, 3}, "1\nq\n")
	screens := detailsScreens(out)
	if len(screens) != 1 {
		t.Fatalf("details:\n%s", out)
	}
	shown, _, ok := strings.Cut(screens[0], "… ")
	if !ok || strings.Count(shown, "\n") != minDetailLines {
		t.Fatalf("want %d summary lines:\n%s", minDetailLines, screens[0])
	}
}

// Only the default less is set up to scroll on the wheel, so the hint for a
// less the user chose names the keys instead.
func TestBrowserTranscriptHintFollowsThePager(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pager string
		want  string
	}{
		{"", "t opens the transcript: scroll with the wheel or arrows, q returns here."},
		{"less -R", "t opens the transcript: scroll with the arrows or space, q returns here."},
		{"most", "t opens the transcript in your pager; quit it to return here."},
	} {
		env := testEnv(t, t.TempDir(), summaryNow)
		var out bytes.Buffer
		env.IsTerminal = func(stream any) bool { return stream == any(&out) }
		env.LookupEnv = func(key string) (string, bool) { return tc.pager, tc.pager != "" && key == "PAGER" }
		b := &sessionBrowser{env: env, stdout: &out}
		if got := b.transcriptHint(); got != tc.want {
			t.Errorf("PAGER=%q: hint %q, want %q", tc.pager, got, tc.want)
		}
	}
}
