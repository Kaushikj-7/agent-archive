package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// withPager writes through write. When stdout is a terminal and paging is
// not disabled, the written bytes go to $AGENT_ARCHIVE_PAGER, else $PAGER,
// else less (see defaultPagerCommand), so a long display can be scrolled
// and quit with q. Piped or redirected stdout is never paged; a pager that
// cannot be started falls back to writing stdout directly after a stderr
// warning. Cancelling ctx stops the pager.
//
// While the pager runs, Ctrl-C belongs to it (less uses it to cancel a
// search), so agent-archive neither exits nor leaves the pager behind on
// the terminal; SIGTERM, SIGHUP or SIGQUIT stops the pager, then exits as the signal
// would have.
func withPager(ctx context.Context, stdout, stderr io.Writer, env pagerDependencies, noPager bool, write func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return err
	}
	// The pager is chosen, and less's version asked, before Ctrl-C is
	// left to it.
	run, page := choosePager(env, noPager, false, stdout, buf.Bytes())
	if !page {
		_, err := stdout.Write(buf.Bytes())
		return err
	}
	owner := readPhase(ctx)
	if owner != nil {
		owner.Pause()
		defer owner.Resume()
	}
	pagerCtx, stopPager := context.WithCancel(ctx)
	defer stopPager()
	watch := watchPagerSignals(env, stopPager)
	_, _, err := run.page(pagerCtx, stdout, stderr, env, buf.Bytes(), watch.interrupted)
	if sig := watch.stop(); sig != nil {
		if owner != nil {
			owner.cancel()
		}
		env.exit(signalExitCode(sig))
	}
	return err
}

// pagerWatch is watchPagerSignals's watch.
type pagerWatch struct {
	// stop ends the watch and returns the signal that stopped the pager,
	// if one did.
	stop func() os.Signal
	// interrupted reports whether Ctrl-C came while the pager ran.
	interrupted func() bool
}

// watchPagerSignals handles interrupts while a pager runs: Ctrl-C is caught
// and dropped, so it reaches only the pager (a caught signal, unlike an
// ignored one, is reset for the pager when it starts), and any other signal
// calls stopPager.
func watchPagerSignals(env pagerDependencies, stopPager func()) pagerWatch {
	signals, stopSignals := env.interrupts()
	done := make(chan struct{})
	result := make(chan os.Signal, 1)
	var interrupted atomic.Bool
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig == os.Interrupt {
					interrupted.Store(true)
					continue
				}
				stopPager()
				result <- sig
				return
			case <-done:
				result <- nil
				return
			}
		}
	}()
	return pagerWatch{
		stop: func() os.Signal {
			close(done)
			sig := <-result
			stopSignals()
			return sig
		},
		interrupted: interrupted.Load,
	}
}

// lessMouseVersion is the first less with --mouse and --wheel-lines, with
// which the wheel scrolls the text while -X keeps it on the normal screen
// after less quits.
const lessMouseVersion = 551

// lessFitVersion is the first less whose -F prints a text that fits on one
// screen without switching to the alternate screen and back; before it, -F
// without -X showed a short text and wiped it at once.
const lessFitVersion = 530

// defaultPagerCommand is the pager when neither AGENT_ARCHIVE_PAGER nor
// PAGER is set, or one is a bare less: program (less, or the path the user
// gave), with -F to quit at once when the text fits on one screen, -R to
// show color, and a prompt naming the keys (lessPrompt).
//
// The mouse wheel should scroll the text, not the terminal's history:
//   - less 551 and later get --mouse (the wheel scrolls three lines) and -X
//     (the text stays on the screen after q).
//   - less 530 to 550 runs without -X, on the alternate screen, where
//     terminals turn the wheel into arrow keys.
//   - A less older than 530 keeps -X, since without it -F wipes a short
//     text as soon as it is shown.
//   - A less whose version is unknown (BusyBox's, say) gets only -FR: it
//     may not understand the prompt or the mouse options.
//
// stayOpen is for the session browser, which redraws the screen when the
// pager exits: no -F, and -+F overrides an F from $LESS, so less waits for
// q even for a short text; the prompt then says q goes back.
func defaultPagerCommand(env pagerDependencies, program string, stayOpen bool, text []byte) string {
	version, known := env.lessVersion(expandHome(env, program))
	mouse := known && version >= lessMouseVersion
	flags := "-"
	if !stayOpen {
		flags += "F"
	}
	flags += "R"
	if mouse || (known && version < lessFitVersion) {
		flags += "X"
	}
	args := []string{shellQuote(program), flags}
	if mouse {
		args = append(args, "--mouse", "--wheel-lines=3")
	}
	if known {
		// The short, medium (-m), and long (-M) prompts, so the keys show
		// whichever $LESS picks.
		prompt := lessPrompt(countLines(text), stayOpen)
		for _, option := range []string{"-Ps", "-Pm", "-PM"} {
			args = append(args, shellQuote(option+prompt))
		}
	}
	if stayOpen {
		args = append(args, "-+F")
	}
	return strings.Join(args, " ")
}

// lessPrompt is less's prompt for a text of lines lines, in less's prompt
// language (less(1), PROMPTS): "lines 1-48 of 1210 (4%) - " while less
// knows which lines are on the screen, with "(END)" in place of the
// percentage at the end, and the percentage left out until less knows it
// (after reading piped text to its end); then the keys: "arrows/space
// scroll, / search, q quit". Without line numbers (less -n) only the keys
// remain. It is ASCII with no quote or dollar sign, so it passes through
// shellQuote, sh, and less's option parsing unchanged.
func lessPrompt(lines int, stayOpen bool) string {
	quit := "q quit"
	if stayOpen {
		quit = "q back"
	}
	return fmt.Sprintf(`?ltlines %%lt-%%lb of %d ?e(END) :?Pb(%%Pb\%%) ..- .arrows/space scroll, / search, %s`, lines, quit)
}

// countLines is the number of lines less shows for text, before wrapping.
func countLines(text []byte) int {
	n := bytes.Count(text, []byte("\n"))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}

// pageText writes text through the pager withPager would choose, and
// reports whether a pager showed it. Cancelling ctx stops the pager.
//
// stayOpen is for the session browser, which redraws the screen when the
// pager exits. The default pager then waits for q even when the text fits
// on one screen (see defaultPagerCommand), and so does a less the user
// chose (-+F overrides an -F from the command or $LESS); waited reports
// that the pager is known to wait for the user. Another pager may have
// returned at once, so the browser waits itself.
func pageText(ctx context.Context, stdout, stderr io.Writer, env pagerDependencies, noPager, stayOpen bool, text []byte) (paged, waited bool, err error) {
	run, page := choosePager(env, noPager, stayOpen, stdout, text)
	if !page {
		_, err := stdout.Write(text)
		return false, false, err
	}
	paged, clean, err := run.page(ctx, stdout, stderr, env, text, nil)
	// A pager that failed (less given an option it does not know) may have
	// exited without waiting, its error on the screen: the browser then
	// waits itself, so the error stays readable.
	return paged, paged && clean && run.waited, err
}

// pagerRun is a pager to run, as choosePager chose it.
type pagerRun struct {
	// command is run by sh, with environment added to the process's own.
	command     string
	environment []string
	// program names the pager in a warning.
	program string
	// waited is whether the pager is known to wait for the user.
	waited bool
}

// choosePager chooses the pager for text, and reports whether it is to be
// paged at all (see pageText).
func choosePager(env pagerDependencies, noPager, stayOpen bool, stdout io.Writer, text []byte) (pagerRun, bool) {
	command, chosen, page := resolvePagerCommand(env, noPager, stdout)
	if !page {
		return pagerRun{}, false
	}
	if !chosen {
		program := command
		if program == "" {
			program = "less"
		}
		return pagerRun{command: defaultPagerCommand(env, program, stayOpen, text), program: program, waited: stayOpen}, true
	}
	program := command
	if fields := strings.Fields(command); len(fields) > 0 {
		program = fields[0]
	}
	waited := stayOpen && isLess(command)
	if waited {
		command += " -+F"
	}
	return pagerRun{command: command, environment: userPagerEnvironment(env), program: program, waited: waited}, true
}

// page runs the pager on text, and reports whether it showed it, and
// whether it exited cleanly (status 0). Only a
// pager that could not be started (sh could not find or run it) falls
// back to writing text to stdout, as git does: one that exited with an
// error, or was killed, showed the text already, and less exits 2 when
// Ctrl-C quits it (LESS=-K). interrupted, when not nil, reports whether
// Ctrl-C came while the pager ran; then it never falls back. A pager
// stopped because ctx was cancelled (a signal) did run too.
func (run pagerRun) page(ctx context.Context, stdout, stderr io.Writer, env pagerDependencies, text []byte, interrupted func() bool) (paged, clean bool, err error) {
	runErr := env.runPager(ctx, run.command, run.environment, bytes.NewReader(text), stdout, stderr)
	failure := startFailure(runErr)
	if failure == "" || cancelled(ctx) || (interrupted != nil && interrupted()) {
		return true, runErr == nil, nil
	}
	terminal.Printf(stderr, "agent-archive: warning: pager %q failed (%s); printing directly\n", run.program, failure)
	_, copyErr := stdout.Write(text)
	return false, false, copyErr
}

// cancelled reports whether ctx is done.
func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// startFailure describes err from runPager when it means the pager never
// ran: sh itself could not start, or sh exited 127 (command not found) or
// 126 (not executable). It is "" when the pager ran: no error, another
// exit status, or death by a signal, which are the pager's own, or a
// process the pager left behind holding its input past the wait delay.
func startFailure(err error) string {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return ""
	}
	exit, ok := errors.AsType[*exec.ExitError](err)
	if ok && exit.ExitCode() != 126 && exit.ExitCode() != 127 {
		return ""
	}
	return err.Error()
}

// userPagerEnvironment is what a pager the user chose gets added to its
// environment, as git does: LESS=FRX when LESS is unset, so a less quits
// at once on a short text and leaves the text on the screen, and LV=-c
// when LV is unset, so lv shows color.
func userPagerEnvironment(env pagerDependencies) []string {
	var added []string
	if _, set := env.lookupEnv("LESS"); !set {
		added = append(added, "LESS=FRX")
	}
	if _, set := env.lookupEnv("LV"); !set {
		added = append(added, "LV=-c")
	}
	return added
}

// isLess reports whether command runs less by itself, so options can be
// appended to it: no shell syntax (pipes, redirection, substitution,
// quoting, comments, line breaks) and no "--" ending its options.
func isLess(command string) bool {
	if strings.ContainsAny(command, "|;&<>`$()#\\\n'\"") {
		return false
	}
	fields := strings.Fields(command)
	return len(fields) > 0 && filepath.Base(fields[0]) == "less" && !slices.Contains(fields, "--")
}

// resolvePagerCommand returns the pager to run. page is false when nothing
// is to be paged: --no-pager, a non-terminal stdout (or AGENT_ARCHIVE_NONINTERACTIVE; see interactive), or an empty
// AGENT_ARCHIVE_PAGER or PAGER, or the value "cat". chosen is false when
// the default command (defaultPagerCommand) runs: when the user set no
// pager (command is then ""), or set a bare less with no options (command
// is then that program, "less" or a path to it). Otherwise command is the
// user's, run as given.
func resolvePagerCommand(env pagerDependencies, noPager bool, stdout io.Writer) (command string, chosen, page bool) {
	if noPager || !env.interactive(stdout) {
		return "", false, false
	}
	for _, name := range []string{"AGENT_ARCHIVE_PAGER", "PAGER"} {
		if value, set := env.lookupEnv(name); set {
			if value == "" || value == "cat" {
				return "", false, false
			}
			if isLess(value) && len(strings.Fields(value)) == 1 {
				return strings.TrimSpace(value), false, true
			}
			return value, true, true
		}
	}
	return "", false, true
}

// expandHome expands a leading ~/ in program with $HOME, as sh does for the
// pager's command, so its version can be asked of the same file.
func expandHome(env interface{ lookupEnv(string) (string, bool) }, program string) string {
	rest, ok := strings.CutPrefix(program, "~/")
	if !ok {
		return program
	}
	home, set := env.lookupEnv("HOME")
	if !set || home == "" {
		return program
	}
	return filepath.Join(home, rest)
}

// lessVersion is the version of program (less, or a path to it), as
// `less --version` reports it ("less 668 (...)"); known is false when it
// cannot be run or says something else.
func (e Env) lessVersion(program string) (version int, known bool) {
	if e.LessVersion != nil {
		return e.LessVersion(program)
	}
	return detectLessVersion(program)
}

// lessVersions caches detectLessVersion's answers by program.
var lessVersions sync.Map

// lessVersionResult is one cached answer of detectLessVersion.
type lessVersionResult struct {
	version int
	known   bool
}

// detectLessVersion runs `program --version` once per process and program,
// with a short timeout: Env.LessVersion's default. The package's tests
// replace it with one that stops the test, so no test runs the real less.
var detectLessVersion = func(program string) (int, bool) {
	if cached, ok := lessVersions.Load(program); ok {
		result := cached.(lessVersionResult)
		return result.version, result.known
	}
	ctx, cancel := context.WithTimeout(context.Background(), lessVersionTimeout)
	defer cancel()
	version, known := 0, false
	cmd := exec.CommandContext(ctx, program, "--version")
	cmd.WaitDelay = lessVersionTimeout
	if out, err := cmd.Output(); err == nil {
		version, known = parseLessVersion(string(out))
	}
	lessVersions.Store(program, lessVersionResult{version: version, known: known})
	return version, known
}

// lessVersionTimeout bounds `less --version`, which a broken less on PATH
// could otherwise hang on.
const lessVersionTimeout = 2 * time.Second

// parseLessVersion reads the version number from `less --version` output:
// "less 668 (POSIX regular expressions)", or "less 551" alone.
func parseLessVersion(out string) (int, bool) {
	first, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "less" {
		return 0, false
	}
	number, _, _ := strings.Cut(fields[1], ".")
	version, err := strconv.Atoi(number)
	if err != nil || version <= 0 {
		return 0, false
	}
	return version, true
}

// pagerStopDelay is how long a pager asked to stop may take before it is
// killed.
const pagerStopDelay = 3 * time.Second

// runPager runs command through sh, with environment ("NAME=value") added
// to the process's own. Cancelling ctx sends the pager SIGTERM (sh execs a
// lone command, so the signal reaches the pager itself) and waits for it to
// exit, killing it after pagerStopDelay, so no pager is left behind on the
// terminal.
func (e Env) runPager(ctx context.Context, command string, environment []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if e.RunPager != nil {
		return e.RunPager(ctx, command, environment, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = pagerStopDelay
	if len(environment) > 0 {
		cmd.Env = append(os.Environ(), environment...)
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
