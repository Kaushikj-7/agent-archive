package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ptyHarnessTimeout bounds one run of a Python PTY harness script. Each
// script gives up by itself after 60 seconds, with what it saw, before
// this.
const ptyHarnessTimeout = 90 * time.Second

// ptyHarnessMargin is the time left before the test binary's own deadline
// (go test -timeout) when a harness is stopped, so a stuck harness fails
// its test with a message rather than ending the whole package in a
// timeout panic.
const ptyHarnessMargin = 30 * time.Second

// ptyHarnessStopGrace is how long a stopped harness has to stop its child
// and print what it saw before it is killed.
const ptyHarnessStopGrace = 10 * time.Second

// ptyHarnessPrelude starts every harness script. SIGTERM, which
// runPTYScript sends at its deadline, raises an exception where the script
// is waiting, so its finally block stops the child under test and the
// traceback says where it was. A harness killed outright would leave its
// child running with nobody to stop it.
//
// continue_job continues a child that waitpid reported stopped and that
// catches SIGCONT (ttyKeys.stop does while it is stopped), sending SIGCONT
// again until running() says it runs, pumping output with pump. macOS marks
// a process stopped, which waitpid reports, before it suspends the
// process's threads; a SIGCONT in between finds nothing to resume, and the
// process then suspends with nobody to continue it. A shell's fg comes long
// after the stop; a script's comes at once. For a caught SIGCONT, macOS
// resumes the threads on every one, so the next SIGCONT continues such a
// process, and to one already running it is harmless. A process that stops
// itself again is reported by waitpid, which is asked before running() and
// before each SIGCONT after the first, so such a stop fails here instead of
// being continued.
const ptyHarnessPrelude = `import os as _os, signal as _signal
def _stopped(signum, frame): raise RuntimeError("stopped at the Go test's deadline")
_signal.signal(_signal.SIGTERM, _stopped)
def continue_job(pid, running, pump):
    _os.kill(pid, _signal.SIGCONT)
    while True:
        pump()
        changed, status = _os.waitpid(pid, _os.WUNTRACED | _os.WNOHANG)
        if changed: raise RuntimeError('the job stopped or ended again after continuing', status)
        if running(): return
        _os.kill(pid, _signal.SIGCONT)
`

// runPTYScript runs a Python PTY harness script with args and returns what
// it printed. See ptyHarnessTimeout and ptyHarnessMargin for its deadline.
func runPTYScript(t *testing.T, python, script string, args ...string) ([]byte, error) {
	t.Helper()
	deadline := time.Now().Add(ptyHarnessTimeout)
	if end, ok := t.Deadline(); ok && end.Add(-ptyHarnessMargin).Before(deadline) {
		deadline = end.Add(-ptyHarnessMargin)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return runPTYScriptUntil(ctx, python, script, args...)
}

// runPTYScriptUntil runs the script until it ends or ctx is done, when it
// is stopped (see ptyHarnessPrelude) and then, after ptyHarnessStopGrace,
// killed.
func runPTYScriptUntil(ctx context.Context, python, script string, args ...string) ([]byte, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, python, append([]string{"-c", ptyHarnessPrelude + script}, args...)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = ptyHarnessStopGrace
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("PTY harness stopped at its deadline, after %v: %w", time.Since(start).Round(time.Second), err)
	}
	return out, err
}

// A harness still running at its deadline is stopped so that it stops its
// own child and says where it was. Killed outright, it left the child
// running with nobody to stop it (one such child, a secret prompt, spun
// at full CPU after its test run had ended) and said only "signal:
// killed".
func TestPTYHarnessStopsItsChildAtItsDeadline(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	ready := filepath.Join(t.TempDir(), "child")
	script := `import os, subprocess, sys, time
p = subprocess.Popen(['sleep', '600'])
print('child', p.pid, flush=True)
with open(sys.argv[1] + '.tmp', 'w') as f: f.write(str(p.pid))
os.rename(sys.argv[1] + '.tmp', sys.argv[1])
try:
    while True: time.sleep(.05)
finally:
    if p.poll() is None: p.kill(); p.wait()
`
	// The deadline comes once the child is running.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for limit := time.Now().Add(60 * time.Second); time.Now().Before(limit); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				return
			}
		}
	}()
	out, err := runPTYScriptUntil(ctx, python, script, ready)
	var pid int
	if _, scanErr := fmt.Sscanf(string(out), "child %d", &pid); scanErr != nil {
		t.Fatalf("no child pid: %v %v %s", scanErr, err, out)
	}
	alive := syscall.Kill(pid, 0) == nil
	if alive {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if err == nil || !strings.Contains(err.Error(), "stopped at its deadline") {
		t.Errorf("error %v, want the harness stopped at its deadline", err)
	}
	if !strings.Contains(string(out), "stopped at the Go test's deadline") {
		t.Errorf("the harness did not say where it was stopped:\n%s", out)
	}
	if alive {
		t.Errorf("the harness's child %d outlived it", pid)
	}
}

// continue_job keeps continuing a stopped job until it runs, as when macOS
// loses the first SIGCONT, and never continues a stop that comes after: one
// that comes even after the job looks running, as key mode stays on while
// a job is stopped, is left for the script to see. The child here stops
// itself and runs on only after a second SIGCONT, simulating a first one
// that did nothing without the kernel's race.
func TestPTYHarnessContinuesAStoppedJobUntilItRuns(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	script := `import os, select, signal, subprocess, sys
child = r'''
import os, signal, sys
continued = [0]
signal.signal(signal.SIGCONT, lambda *a: continued.__setitem__(0, continued[0] + 1))
os.kill(os.getpid(), signal.SIGSTOP)
while continued[0] < 2: signal.pause()
os.write(1, b'running')
if sys.argv[1] == 'stops-again':
    os.kill(os.getpid(), signal.SIGSTOP)
    os.write(1, b' and continued again')
'''
p = subprocess.Popen([sys.executable, '-c', child, sys.argv[1]], stdout=subprocess.PIPE)
out = b''
def pump():
    global out
    if select.select([p.stdout], [], [], .05)[0]: out += os.read(p.stdout.fileno(), 100)
try:
    _, status = os.waitpid(p.pid, os.WUNTRACED)
    assert os.WIFSTOPPED(status), status
    try:
        continue_job(p.pid, lambda: b'running' in out, pump)
    except RuntimeError as e:
        print('stopped again', e); sys.exit()
    while sys.argv[1] == 'stops-again':
        pid, status = os.waitpid(p.pid, os.WUNTRACED | os.WNOHANG)
        if pid: print('stopped again', status); sys.exit()
        if b'again' in out: print('continued twice', out); sys.exit()
        pump()
    print('continued')
finally:
    p.kill()
`
	for mode, want := range map[string]string{"runs-after-two": "continued", "stops-again": "stopped again"} {
		out, err := runPTYScript(t, python, script, mode)
		if err != nil || !strings.HasPrefix(string(out), want) {
			t.Errorf("%s: %v %s, want %q", mode, err, out, want)
		}
	}
}
