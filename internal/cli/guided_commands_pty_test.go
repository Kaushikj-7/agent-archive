package cli

import (
	"os"
	"os/exec"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// The child traverses guided handoff/file input and pairing's terminal owner,
// then hidden management input. It opens no installations or providers.
func TestGuidedCommandsTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_GUIDED_COMMAND_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	defer p.close()
	choice, err := chooseDestination(p, nil, "", false, productionAgents.Catalog(), nil)
	must(t, err)
	if choice.action != handoffWrite {
		t.Fatal("wrong destination")
	}
	// q is consumed by the same buffer, never a second stdin reader.
	must(t, writeHandoffChoice(p, []byte("synthetic handoff"), handoffTarget{}, t.TempDir(), os.Stdout, Env{}))
	must(t, showPairingCode(p, "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding", Env{LookupEnv: noEnv, Interrupts: noInterrupts}))
	token, err := p.guidedText(promptModel{Question: "Management credential (hidden)", Secret: true})
	must(t, err)
	if token != "synthetic-secret-canary" {
		t.Fatal("hidden input changed")
	}
	terminal.Println(p.out, "DONE")
}

func TestGuidedCommandsTerminalOwnership(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY requires Python")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []string{"normal", "term", "pairing-suspend"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			if out, err := runPTYScript(t, python, guidedCommandsPTY, binary, mode); err != nil {
				t.Fatalf("guided command PTY %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const guidedCommandsPTY = `import os, pty, re, select, signal, struct, subprocess, sys, termios, time, fcntl
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH',24,80,0,0))
before=termios.tcgetattr(slave)
env=dict(os.environ,TERM='xterm-256color',ARCHIVE_GUIDED_COMMAND_CHILD='1')
p=subprocess.Popen([sys.argv[1],'-test.run=^TestGuidedCommandsTerminalChild$'],stdin=slave,stdout=slave,stderr=slave,env=env)
output=b''
deadline=time.monotonic()+60
def wait_for(text):
 global output
 while text not in output:
  if (p.poll() is not None and text!=b'<<drained>>') or time.monotonic()>deadline: raise RuntimeError('waiting for',text,output)
  if select.select([master],[],[],.05)[0]:
   try: output+=os.read(master,65536)
   except OSError: pass
try:
 wait_for(b'Choose:')
 os.write(master,b'w\nq\n')
 wait_for(b'Press Enter to hide.')
 if sys.argv[2]=='pairing-suspend':
  p.send_signal(signal.SIGTSTP)
  stop_deadline=time.monotonic()+5
  while True:
   pid,status=os.waitpid(p.pid,os.WNOHANG|os.WUNTRACED)
   if pid and os.WIFSTOPPED(status): break
   if time.monotonic()>stop_deadline: raise RuntimeError('pairing display did not suspend',output)
   time.sleep(.05)
  assert termios.tcgetattr(slave)==before,('suspended modes changed',output)
  wait_for(b'\x1b[?1049l')
  assert output.count(b'\x1b[?1049h')==1,('screen remained active while stopped',output)
  os.write(slave,b'SHELL SENTINEL\n')
  p.send_signal(signal.SIGCONT)
  wait_for(b'SHELL SENTINEL')
  while output.count(b'\x1b[?1049h')<2:
   if time.monotonic()>deadline: raise RuntimeError('pairing screen not resumed',output)
   if select.select([master],[],[],.05)[0]: output+=os.read(master,65536)
 os.write(master,b'\n')
 wait_for(b'Management credential (hidden)')
 if sys.argv[2]=='pairing-suspend':
  p.send_signal(signal.SIGTSTP)
  stop_deadline=time.monotonic()+5
  while True:
   pid,status=os.waitpid(p.pid,os.WNOHANG|os.WUNTRACED)
   if pid and os.WIFSTOPPED(status): break
   if time.monotonic()>stop_deadline: raise RuntimeError('following hidden prompt did not suspend',output)
   time.sleep(.05)
  assert termios.tcgetattr(slave)==before,('hidden suspension did not restore modes',output)
  p.send_signal(signal.SIGCONT)
  while termios.tcgetattr(slave)[3] & termios.ECHO:
   if time.monotonic()>deadline: raise RuntimeError('hidden input did not resume',output)
   time.sleep(.05)
 if sys.argv[2]=='term': p.send_signal(signal.SIGTERM)
 else: os.write(master,b'synthetic-secret-canary\n')
 while p.poll() is None:
  if time.monotonic()>deadline: raise RuntimeError('child exit',output)
  if select.select([master],[],[],.05)[0]:
   try: output+=os.read(master,65536)
   except OSError: pass
 os.write(slave,b'<<drained>>')
 wait_for(b'<<drained>>')
 assert p.returncode==(143 if sys.argv[2]=='term' else 0),(p.returncode,output)
 assert b'synthetic-secret-canary' not in output,output
 assert output.index(b'\x1b[?1049h')<output.index(b'2. Enter the pairing code on the other machine')<output.index(b'\x1b[?1049l'),output
 assert output.count(b'aardvark abandoned abbreviate abdomen abhorrence abiding')==(2 if sys.argv[2]=='pairing-suspend' else 1),output
 if sys.argv[2]=='pairing-suspend':
  alternate=False
  for part in re.split(b'(\x1b\[\?1049[hl])',output):
   if part==b'\x1b[?1049h': alternate=True
   elif part==b'\x1b[?1049l': alternate=False
   else:
    if b'2. Enter the pairing code on the other machine' in part: assert alternate,('code escaped alternate screen',output)
    if b'SHELL SENTINEL' in part: assert not alternate,('shell remained in pairing screen',output)
  assert output.count(b'\x1b[?1049h')==2,('pairing callbacks survived display exit',output)
 after=termios.tcgetattr(slave)
 assert before==after,('terminal not restored',before,after,output)
 if sys.argv[2]!='term': assert b'Credential received' in output and b'DONE' in output,output
finally:
 if p.poll() is None: p.kill();p.wait()
 os.close(master);os.close(slave)
`
