//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// The parent launches this child only on a private pseudo-terminal. Archive,
// setup, credentials and scheduler effects remain the synthetic fixture's.
func TestSearchBrowserTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_KEYS_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	env, _, _ := prepareSearchBrowser(t)
	env.openKeys = terminalKeys
	env.IsTerminal, env.TerminalSize, env.Interrupts = nil, nil, nil
	env.LookupEnv = func(string) (string, bool) { return "", false }
	env.Clipboard = func(text []byte) error {
		fmt.Printf("\nSEARCH CLIPBOARD HAS %d BYTES\n", len(text))
		return nil
	}
	if code := Run([]string{"search", "visible", "--all-projects"}, os.Stdin, os.Stdout, os.Stderr, env); code != 0 {
		t.Fatalf("code %d", code)
	}
}

func TestSearchBrowserOnARealTerminal(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []string{"keys", "resize", "quit", "interrupt", "handoff"} {
		t.Run(mode, func(t *testing.T) {
			if out, err := runPTYScript(t, python, searchBrowserPTYScript, binary, mode, "TestSearchBrowserTerminalChild"); err != nil {
				t.Fatalf("search terminal %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const searchBrowserPTYScript = ptyKeysHarness + `try:
    wait_for(b'Content query:')
    wait_until(keys_on, 'key mode not on in search')
    if mode == 'interrupt':
        os.write(master, b'\x03')
        finish(130)
    elif mode == 'quit':
        os.write(master, b'q')
        finish(0)
    else:
        os.write(master, b'1\r')
        wait_for(b'Hand off current session')
        assert keys_on(), 'key mode off in search context'
        if mode == 'handoff':
            # One burst crosses browser and ordinary destination prompt ownership.
            os.write(master, b'hc\r')
            wait_for(b'? Continue in')
            wait_for(b'SEARCH CLIPBOARD HAS ')
            finish(0)
        else:
            if mode == 'resize':
                start = len(output)
                signal.signal(signal.SIGTTOU, signal.SIG_IGN)
                fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 20, 36, 0, 0))
                signal.signal(signal.SIGTTOU, signal.SIG_DFL)
                p.send_signal(signal.SIGWINCH)
                wait_for(b'Hand off current session', start)
                assert keys_on(), 'resize left key mode'
                os.write(master, b'\x1b[F\x1b[H')
            else:
                start = len(output)
                os.write(master, b'n')
                wait_for(b'no further matching body passage.', start)
                start = len(output)
                os.write(master, b'p')
                wait_for(b'no further matching body passage.', start)
            start = len(output)
            os.write(master, b'b')
            wait_for(b'Content query:', start)
            os.write(master, b'q')
            finish(0)
    assert restored(), ('search terminal modes not restored', hex(before[3]), hex(termios.tcgetattr(slave)[3]))
    assert b'\x1b[?1049l' in output, 'search alternate screen not left'
finally:
    if p.poll() is None: p.kill(); p.wait()
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.close(master); os.close(slave)
`
