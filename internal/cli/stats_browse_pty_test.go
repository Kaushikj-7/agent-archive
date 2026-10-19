//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"testing"
)

// This child is only launched under a pseudo-terminal by the test below: the
// interactive stats screen on a real terminal, reading real keys.
func TestStatsScreenTerminalChild(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARCHIVE_STATS_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	env.openKeys = terminalKeys
	// The real terminal, its size, and signals.
	env.IsTerminal, env.TerminalSize, env.Interrupts, env.WorkingDir = nil, nil, nil, nil
	env.LookupEnv = func(string) (string, bool) { return "", false }
	if code := Run([]string{"stats"}, os.Stdin, os.Stdout, os.Stderr, env); code != 0 {
		t.Fatalf("code %d", code)
	}
}

// On a real terminal the stats screen turns off echo and line editing while it
// reads keys, hides the cursor, and gives everything back however it ends:
// quitting, Ctrl-C, SIGTERM, SIGHUP, Ctrl-Z (until the shell continues it).
// It redraws when the window is resized, and h saves the page. Each scenario
// waits for what the screen draws, never for a time.
func TestStatsScreenOnARealTerminal(t *testing.T) {
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
	for _, mode := range []string{"keys", "quit", "interrupt", "term", "hup", "sigquit", "savequit", "resize", "suspend", "save"} {
		dir := t.TempDir()
		out, err := runPTYScript(t, python, statsPTYScript, binary, mode, dir)
		if err != nil {
			t.Errorf("PTY test %s: %v %s", mode, err, out)
		}
	}
}

const statsPTYScript = `import fcntl, os, pty, re, select, signal, struct, subprocess, sys, termios, time
binary, mode, cwd = sys.argv[1], sys.argv[2], sys.argv[3]
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 100, 0, 0))
modes = termios.tcgetattr(slave)
modes[3] |= termios.ECHONL | termios.IEXTEN
termios.tcsetattr(slave, termios.TCSANOW, modes)
before = termios.tcgetattr(slave)
vdisable = b'\xff' if sys.platform == 'darwin' else b'\x00'
env = dict(os.environ, ARCHIVE_STATS_TEST_CHILD='1', TERM='xterm-256color', NO_COLOR='1')
env.pop('AGENT_ARCHIVE_NONINTERACTIVE', None)
os.setsid()
fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
def foreground():
    os.setpgid(0, 0)
    signal.signal(signal.SIGTTOU, signal.SIG_IGN)
    os.tcsetpgrp(0, os.getpid())
    signal.signal(signal.SIGTTOU, signal.SIG_DFL)
p = subprocess.Popen([binary, '-test.run=^TestStatsScreenTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env, cwd=cwd, preexec_fn=foreground)
deadline = time.monotonic() + 60
output = b''
CLEAR = b'\x1b[H\x1b[2J'
LEAVE = b'\x1b[?1049l'
ENTER = b'\x1b[?1049h'
ANSI = re.compile(rb'\x1b\[[0-9;?]*[A-Za-z]')
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
    m = termios.tcgetattr(slave)
    return m[3] & ~termios.PENDIN == before[3] and m[6][termios.VQUIT] == before[6][termios.VQUIT]
def wait_until(check, what):
    limit = min(deadline, time.monotonic() + 30)
    while not check():
        if time.monotonic() > limit: raise RuntimeError(what, termios.tcgetattr(slave), output[-300:])
        pump()
def frames():
    # Each frame, as the text a terminal would show: no escape sequences.
    body = output.split(ENTER)[-1] if ENTER in output else output
    parts = body.split(CLEAR)[1:]
    return [ANSI.sub(b'', part.split(LEAVE)[0]).decode('utf-8', 'replace').replace('\r', '') for part in parts]
def wait_frame(text, after, end='q quit'):
    # A frame after the first "after" frames that has text and ends as end
    # says (the key bar, by default): the last row is drawn last, so a frame
    # with it is whole.
    def ready():
        f = frames()
        return len(f) > after and text in f[-1] and end in f[-1]
    wait_until(ready, 'no frame with %r after %d frames' % (text, after))
    return len(frames())
def wait_for(text, start=0):
    while output.find(text, start) < 0: pump()
    return output.find(text, start) + len(text)
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
def resize(rows, cols):
    # As the terminal emulator does: the size changes and the foreground job
    # gets SIGWINCH. This script is a background job on the terminal, so it
    # takes the foreground for the ioctl and gives it back.
    signal.signal(signal.SIGTTOU, signal.SIG_IGN)
    os.tcsetpgrp(slave, os.getpgrp())
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, 0, 0))
    os.tcsetpgrp(slave, p.pid)
    signal.signal(signal.SIGTTOU, signal.SIG_DFL)
    p.send_signal(signal.SIGWINCH)
def check_restored():
    assert restored(), ('terminal modes not restored', hex(before[3]), hex(termios.tcgetattr(slave)[3]))
    assert output.endswith(LEAVE) or LEAVE in output, 'alternate screen not left'
    tail = output[output.rindex(LEAVE) - 6:]
    assert tail.startswith(b'\x1b[?25h' + LEAVE) , ('cursor not shown before leaving', tail[:60])
try:
    n = wait_frame('q quit', 0)
    wait_until(keys_on, 'key mode not on')
    assert b'\x1b[?25l' in output, 'the cursor was not hidden'
    if mode == 'keys':
        os.write(master, b'd'); n = wait_frame('agent-archive stats · detail · last 30 days', n)
        os.write(master, b'w'); n = wait_frame('last 90 days', n)
        assert 'w window 90d>7d' in frames()[-1], frames()[-1]
        os.write(master, b'p'); n = wait_frame('projects', n)
        os.write(master, b'?'); n = wait_frame('agent-archive stats: keys', n, 'closes the help')
        os.write(master, b'x'); n = wait_frame('q quit', n)
        assert 'agent-archive stats: keys' not in frames()[-1]
        # A wheel: several arrows at once, one redraw.
        os.write(master, b'\x1b[B\x1b[B\x1b[B'); n = wait_frame('q quit', n)
        os.write(master, b'q')
        finish(0)
        check_restored()
    elif mode == 'quit':
        os.write(master, b'q')
        finish(0)
        check_restored()
    elif mode == 'interrupt':
        os.write(master, b'\x03')
        finish(130)
        check_restored()
    elif mode == 'term':
        p.send_signal(signal.SIGTERM)
        finish(143)
        check_restored()
    elif mode == 'hup':
        p.send_signal(signal.SIGHUP)
        finish(129)
        check_restored()
    elif mode == 'sigquit':
        # From outside (Ctrl-\ is off in key mode): handled like the others,
        # no goroutine dump, the terminal given back, the shell's status.
        p.send_signal(signal.SIGQUIT)
        finish(131)
        assert b'goroutine ' not in output and b'SIGQUIT' not in output, output[-400:]
        check_restored()
    elif mode == 'savequit':
        # A page saved with h, then a signal from outside: the terminal is
        # given back first, and then the path is printed on the normal screen,
        # as after a quit.
        os.write(master, b'h'); n = wait_frame('Save redacted HTML as', n, 'Esc cancels): ')
        os.write(master, b'saved.html\r'); n = wait_frame('Saved ', n, 'names replaced)')
        assert os.path.exists(os.path.join(cwd, 'saved.html'))
        p.send_signal(signal.SIGQUIT)
        finish(131)
        assert b'goroutine ' not in output and b'SIGQUIT' not in output, output[-400:]
        wait_for(b'saved.html\r\n', output.rindex(LEAVE))
        assert re.search(rb'\x1b\[\?25h\x1b\[\?1049lWrote [^\n]*saved\.html\r?\n$', output), output[-300:]
        assert output.count(b'Wrote ') == 1, output[-300:]
        assert os.listdir(cwd) == ['saved.html'], os.listdir(cwd)
        check_restored()
    elif mode == 'resize':
        resize(12, 60)
        n = wait_frame('q quit', n)
        f = frames()[-1].split('\n')
        assert len(f) == 12, (len(f), f)
        assert all(len(row) <= 60 for row in f), f
        resize(30, 120)
        n = wait_frame('q quit', n)
        f = frames()[-1].split('\n')
        assert len(f) == 30, (len(f), f)
        os.write(master, b'q')
        finish(0)
        check_restored()
    elif mode == 'suspend':
        os.write(master, b'\x1a')
        status = wait_stopped()
        assert os.WIFSTOPPED(status) and os.WSTOPSIG(status) == signal.SIGSTOP, status
        assert restored(), 'terminal modes not restored while stopped'
        assert output.endswith(b'\x1b[?25h' + LEAVE), ('screen or cursor not restored while stopped', output[-200:])
        stopped = len(output)
        continue_job(p.pid, keys_on, pump)
        wait_for(b'q quit', wait_for(ENTER, stopped))
        os.write(master, b'q')
        finish(0)
        check_restored()
    elif mode == 'save':
        os.write(master, b'h'); n = wait_frame('Save redacted HTML as', n, 'Esc cancels): ')
        os.write(master, b'saved.html'); n = wait_frame('Save redacted HTML as', n, 'saved.html')
        os.write(master, b'\r'); n = wait_frame('Saved ', n, 'names replaced)')
        assert os.path.exists(os.path.join(cwd, 'saved.html'))
        assert os.stat(os.path.join(cwd, 'saved.html')).st_mode & 0o777 == 0o600
        os.write(master, b'q')
        finish(0)
        assert re.search(rb'\x1b\[\?1049lWrote [^\n]*saved\.html\r?\n', output), output[-300:]
        check_restored()
    else:
        raise RuntimeError('unknown mode ' + mode)
finally:
    if p.poll() is None: p.kill(); p.wait()
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.close(master); os.close(slave)
`
