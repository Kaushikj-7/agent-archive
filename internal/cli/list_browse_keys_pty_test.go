//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// This child is only launched under a pseudo-terminal by the test below:
// the browser reading keys from a real terminal.
func TestBrowserKeysTerminalChild(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARCHIVE_KEYS_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	env, _, _ := publishedFixture(t)
	env.openKeys = terminalKeys
	// The real terminal, its size, and signals.
	env.IsTerminal, env.TerminalSize, env.Interrupts = nil, nil, nil
	env.LookupEnv = func(name string) (string, bool) {
		if name == "PAGER" {
			return os.LookupEnv("ARCHIVE_KEYS_TEST_PAGER")
		}
		return "", false
	}
	if code := Run([]string{"list"}, os.Stdin, os.Stdout, os.Stderr, env); code != 0 {
		t.Fatalf("code %d", code)
	}
}

// On a real terminal the browser turns off echo, line editing, the
// extended input characters and Ctrl-\ while it reads keys, and turns them
// back on however it ends: quitting, Ctrl-C, or Ctrl-Z (until the shell
// continues it, with the normal screen shown meanwhile). Ctrl-Z while a
// pager runs stops the browser too, so the shell sees the job stop, and
// continuing it finishes the pager. The modes are checked one run at a
// time, each well within its own deadline.
func TestBrowserKeysRestoreTheTerminal(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// One at a time, not as parallel subtests.
	for _, mode := range []string{"quit", "enter", "interrupt", "sigquit", "suspend", "pager"} {
		if out, err := runPTYScript(t, python, keysPTYScript, binary, mode); err != nil {
			t.Errorf("PTY test %s: %v %s", mode, err, out)
		}
	}
}

// ptyKeysHarness is the start of the scripts that run a child under a
// pseudo-terminal and read its screen: the terminal, the child as the foreground
// job, and what to wait for and check. A script goes on with its own
// try block, which says what to type and what must follow.
const ptyKeysHarness = `import fcntl, os, pty, select, signal, struct, subprocess, sys, termios, time
binary, mode = sys.argv[1], sys.argv[2]
child = sys.argv[3] if len(sys.argv) > 3 else 'TestBrowserKeysTerminalChild'
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 100, 0, 0))
# ECHONL on, so turning it off shows; IEXTEN and VQUIT are on by default.
modes = termios.tcgetattr(slave)
modes[3] |= termios.ECHONL | termios.IEXTEN
termios.tcsetattr(slave, termios.TCSANOW, modes)
before = termios.tcgetattr(slave)
vdisable = b'\xff' if sys.platform == 'darwin' else b'\x00'
assert before[6][termios.VQUIT] != vdisable
env = dict(os.environ, ARCHIVE_KEYS_TEST_CHILD='1', TERM='xterm-256color')
if mode == 'pager':
    # A pager that stops its job as Ctrl-Z in less would, then reads its text.
    env['ARCHIVE_KEYS_TEST_PAGER'] = 'kill -TSTP 0; cat >/dev/null'
# As a shell does: this script leads a session whose controlling terminal
# is the pty, and runs the child as the foreground job, so Ctrl-C and
# Ctrl-Z typed on the pty signal it, and the pty outlives it.
os.setsid()
fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
def foreground():
    os.setpgid(0, 0)
    signal.signal(signal.SIGTTOU, signal.SIG_IGN)
    os.tcsetpgrp(0, os.getpid())
    signal.signal(signal.SIGTTOU, signal.SIG_DFL)
p = subprocess.Popen([binary, '-test.run=^' + child + '$'], stdin=slave, stdout=slave, stderr=slave, env=env, preexec_fn=foreground)
deadline = time.monotonic() + 60
output = b''
def pump():
    global output
    if time.monotonic() > deadline: raise RuntimeError('timeout: %r' % output[-400:])
    if select.select([master], [], [], .05)[0]:
        try: output += os.read(master, 65536)
        except OSError: pass
def keys_on():
    m = termios.tcgetattr(slave)
    lflag = m[3]
    off = termios.ECHO | termios.ICANON | termios.ECHONL | termios.IEXTEN
    return not (lflag & off) and lflag & termios.ISIG and m[6][termios.VQUIT] == vdisable
def restored():
    # The system sets PENDIN itself when line editing goes back on.
    m = termios.tcgetattr(slave)
    return m[3] & ~termios.PENDIN == before[3] and m[6][termios.VQUIT] == before[6][termios.VQUIT]
def wait_for(text, start=0):
    # Only output from start on counts, so text drawn earlier does not.
    while output.find(text, start) < 0: pump()
    return output.find(text, start) + len(text)
def wait_until(check, what):
    # The modes change as the prompt is drawn: seconds, even on a loaded
    # machine running the race detector.
    limit = min(deadline, time.monotonic() + 30)
    while not check():
        if time.monotonic() > limit: raise RuntimeError(what, termios.tcgetattr(slave))
        pump()
def drain():
    # Whatever the child wrote is still to be read once it has exited or
    # stopped: the script can be descheduled while the child writes and
    # exits, and on Linux output reaches the master through a worker a
    # moment after the write. A marker written on the slave side queues
    # behind all of it, so the output is complete once the marker is read.
    # Output, unlike input, is not echoed or discarded by the terminal's
    # modes, and pump gives up at the deadline.
    global output
    marker = b'<<end of output>>'
    while not select.select([], [slave], [], 0)[1]: pump()
    os.write(slave, marker)
    while marker not in output: pump()
    output = output.replace(marker, b'')
def finish(code):
    while p.poll() is None: pump()
    drain()
    assert p.returncode == code, (p.returncode, output[-800:])
def wait_stopped():
    limit = min(deadline, time.monotonic() + 30)
    while True:
        pid, status = os.waitpid(p.pid, os.WUNTRACED | os.WNOHANG)
        if pid: break
        if time.monotonic() > limit: raise RuntimeError('the job did not stop', output[-300:])
        pump()
    # What it wrote before it stopped.
    drain()
    return status
`

// keysPTYScript is ptyKeysHarness with the list browser's modes.
const keysPTYScript = ptyKeysHarness + `try:
    wait_for(b'Number or ID to show')
    wait_until(keys_on, 'key mode not on at the list')
    if mode == 'quit':
        os.write(master, b'\x1b[B\x1b[A1\r')
        wait_for(b't transcript')
        assert keys_on(), 'key mode off at the details'
        os.write(master, b'q')
        finish(0)
    elif mode == 'enter':
        # Enter with nothing typed opens the highlighted row, which the
        # detail strip under the list spells out.
        wait_for(b'last active')
        os.write(master, b'\x1b[B\r')
        wait_for(b't transcript')
        os.write(master, b'q')
        finish(0)
    elif mode == 'interrupt':
        os.write(master, b'\x03')
        finish(130)
    elif mode == 'sigquit':
        # From outside (Ctrl-\ is off in key mode): handled like Ctrl-C, no
        # goroutine dump, the terminal given back, the shell's status.
        p.send_signal(signal.SIGQUIT)
        finish(131)
        assert b'goroutine ' not in output and b'SIGQUIT' not in output, output[-400:]
    elif mode == 'pager':
        os.write(master, b'1\r')
        wait_for(b't transcript')
        os.write(master, b't')
        status = wait_stopped()
        assert os.WIFSTOPPED(status), status
        assert restored(), 'terminal modes not restored for the pager'
        os.killpg(p.pid, signal.SIGCONT)
        wait_for(b'back to details')
        wait_until(keys_on, 'key mode not back on after the pager')
        os.write(master, b'q')
        finish(0)
    else:
        os.write(master, b'\x1a')
        status = wait_stopped()
        assert os.WIFSTOPPED(status) and os.WSTOPSIG(status) == signal.SIGSTOP, status
        assert restored(), 'terminal modes not restored while stopped'
        assert output.endswith(b'\x1b[?1049l'), ('alternate screen shown while stopped', output[-200:])
        stopped = len(output)
        p.send_signal(signal.SIGCONT)
        # Key mode goes back on before the screen is drawn again, and the
        # list's prompt is already in the output from before the stop: wait
        # for the alternate screen and then a new prompt after it.
        wait_until(keys_on, 'key mode not back on after continuing')
        wait_for(b'Number or ID to show', wait_for(b'\x1b[?1049h', stopped))
        os.write(master, b'q')
        finish(0)
    assert restored(), ('terminal modes not restored', hex(before[3]), hex(termios.tcgetattr(slave)[3]))
    assert b'\x1b[?1049l' in output, 'alternate screen not left'
finally:
    if p.poll() is None: p.kill(); p.wait()
    # Closing the pty hangs up its session, this script.
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.close(master); os.close(slave)
`

// This child is only launched under a pseudo-terminal by the test below: the
// handoff picker reading keys from a real terminal, and the prompt after it
// reading answers from the same input.
func TestHandoffBrowserTerminalChild(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARCHIVE_KEYS_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	f := newPickerFixture(t)
	env := f.env
	env.openKeys = terminalKeys
	// The real terminal, its size, and signals.
	env.IsTerminal, env.TerminalSize, env.Interrupts = nil, nil, nil
	env.LookupEnv = func(string) (string, bool) { return "", false }
	env.Clipboard = func(text []byte) error {
		fmt.Printf("\nCLIPBOARD HAS %d BYTES\n%s\nCLIPBOARD END\n", len(text), text)
		return nil
	}
	if code := Run([]string{"handoff"}, os.Stdin, os.Stdout, os.Stderr, env); code != 0 {
		t.Fatalf("code %d", code)
	}
}

// A handoff picked in key mode on a real terminal still reads the destination
// prompt's answer that was typed ahead of it: the browser reads the terminal
// a key at a time, so the answer arrives with the pick, and is handed back to
// the one buffer the prompts read. The terminal's modes are back as the
// prompt asks.
func TestHandoffPickedInKeyModeReadsTheTypedAheadAnswer(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runPTYScript(t, python, handoffPTYScript, binary, "handoff", "TestHandoffBrowserTerminalChild"); err != nil {
		t.Errorf("PTY test: %v %s", err, out)
	}
}

// handoffPTYScript types the pick and the destination's answer together: "1"
// and Enter, then "c" and Enter (copy to the clipboard). Nothing is typed
// after the picker is gone, so an answer the browser lost would leave the
// prompt waiting.
const handoffPTYScript = ptyKeysHarness + `try:
    wait_for(b'to hand off')
    wait_until(keys_on, 'key mode not on at the picker')
    os.write(master, b'1\rc\r')
    wait_for(b'? Continue in')
    wait_for(b'CLIPBOARD HAS ')
    finish(0)
    assert b'\x1b[?1049l' in output, 'alternate screen not left'
    assert restored(), ('terminal modes not restored', hex(before[3]), hex(termios.tcgetattr(slave)[3]))
finally:
    if p.poll() is None: p.kill(); p.wait()
    # Closing the pty hangs up its session, this script.
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.close(master); os.close(slave)
`

// On a real terminal ↓ moves the highlight and the detail strip under the
// list follows it; a resize redraws the strip to the new width with the
// same row highlighted; and Enter with nothing typed hands off the
// highlighted row, the one the strip named.
func TestHandoffKeyBrowserCursorAndStripOnATerminal(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runPTYScript(t, python, handoffCursorPTYScript, binary, "cursor", "TestHandoffBrowserTerminalChild"); err != nil {
		t.Errorf("PTY test: %v %s", err, out)
	}
}

// handoffCursorPTYScript moves the highlight to the second row, reads the
// title the strip shows for it, narrows the terminal, and picks the row
// with Enter alone.
const handoffCursorPTYScript = ptyKeysHarness + `import re
rule = '─'.encode()
mark = '▸ 2'.encode()
def strip_title(start):
    # The line after the first rule of the strip drawn from start on.
    at = wait_for(rule * 20, start)
    end = wait_for(b'\n', at)
    title_end = wait_for(b'\n', end)
    return re.sub(rb'\x1b\[[0-9;]*m|\r', b'', output[end:title_end - 1])
try:
    wait_for(b'to hand off')
    wait_until(keys_on, 'key mode not on at the picker')
    first = strip_title(0)
    moved = len(output)
    os.write(master, b'\x1b[B')
    second = strip_title(wait_for(mark, moved))
    assert second and second != first, (first, second)
    resized = len(output)
    # This script is not the terminal's foreground job: SIGTTOU is ignored
    # while it sets the size, as foreground() does to hand the terminal over.
    signal.signal(signal.SIGTTOU, signal.SIG_IGN)
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 70, 0, 0))
    signal.signal(signal.SIGTTOU, signal.SIG_DFL)
    os.kill(p.pid, signal.SIGWINCH)
    # A rule exactly 70 columns wide, not the 100 of before.
    narrow = re.compile(rb'(?<!\xe2\x94\x80)' + rule * 70 + rb'(?!\xe2\x94\x80)')
    while not narrow.search(output, resized): pump()
    after = wait_for(mark, resized)
    assert strip_title(after) == second, 'the strip names another row after the resize'
    os.write(master, b'\r')
    wait_for(b'? Continue in')
    os.write(master, b'c\r')
    copied = wait_for(b'CLIPBOARD HAS ')
    handoff = output[copied:wait_for(b'CLIPBOARD END', copied)]
    finish(0)
    assert b'\x1b[?1049l' in output, 'alternate screen not left'
    assert second in handoff and first not in handoff, ('handed off a row other than the highlighted one', first, second, handoff)
    assert restored(), ('terminal modes not restored', hex(before[3]), hex(termios.tcgetattr(slave)[3]))
finally:
    if p.poll() is None: p.kill(); p.wait()
    # Closing the pty hangs up its session, this script.
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.close(master); os.close(slave)
`
