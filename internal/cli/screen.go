package cli

import (
	"context"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Terminal control sequences the session browser writes: switch to and from
// the alternate screen buffer, and clear the screen with the cursor at the
// top left.
const (
	enterAltScreenSequence = "\x1b[?1049h"
	leaveAltScreenSequence = "\x1b[?1049l"
	clearScreenSequence    = "\x1b[H\x1b[2J"
)

type altScreenDependencies interface {
	interactive(any) bool
	interrupts() (<-chan os.Signal, func())
	exit(int)
}

// altScreen is the browser's full-screen view: the list and a session's
// details replace each other in the terminal's alternate screen instead of
// piling up in scrollback, and leaving it restores the screen as it was. It
// does nothing when out is not a terminal. leave is safe to call more than
// once and from the interrupt handler, which restores the screen before
// the process exits on Ctrl-C, SIGTERM, SIGHUP, or SIGQUIT.
//
// While a pager owns the terminal, the handler leaves Ctrl-C to it (less
// uses it to cancel a search) and answers any other signal by stopping the
// pager; the browser exits once the pager has, so none is left behind.
type altScreen struct {
	out    io.Writer
	mu     sync.Mutex
	active bool
	stop   func()
	done   chan struct{}
	exit   func(int)
	// stopPager is set while a pager runs; pending is a signal that asked
	// it to stop.
	stopPager func()
	pending   os.Signal
	// restoreInput, when set, gives the terminal its line input back as
	// the screen is left, on every way out: quitting, an error, a panic,
	// or a signal.
	restoreInput func()
	// afterSignal, when set, runs once the screen is left for a signal and
	// before the process exits, so what must stay in the scrollback (the
	// files stats saved) is still printed.
	afterSignal func()
	cancelReads context.CancelFunc
}

func enterAltScreen(out io.Writer, env altScreenDependencies) *altScreen {
	return enterAltScreenWithReadCancel(out, env, nil)
}

func enterAltScreenWithReadCancel(out io.Writer, env altScreenDependencies, cancel context.CancelFunc) *altScreen {
	s := &altScreen{out: out, exit: env.exit, cancelReads: cancel}
	if !env.interactive(out) {
		return s
	}
	signals, stop := env.interrupts()
	s.start(signals, stop)
	return s
}

// start switches to the alternate screen and handles interrupts until
// leave.
func (s *altScreen) start(signals <-chan os.Signal, stop func()) {
	s.mu.Lock()
	terminal.Print(s.out, enterAltScreenSequence)
	s.active, s.stop, s.done = true, stop, make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		for {
			select {
			case sig := <-signals:
				if s.pagerSignal(sig) {
					continue
				}
				s.exitForSignal(sig)
				return
			case <-done:
				return
			}
		}
	}()
}

// pagerSignal handles sig while a pager runs, reporting whether it did.
func (s *altScreen) pagerSignal(sig os.Signal) bool {
	s.mu.Lock()
	stopPager := s.stopPager
	if stopPager == nil {
		s.mu.Unlock()
		return false
	}
	if sig != os.Interrupt {
		s.pending = sig
	}
	s.mu.Unlock()
	if sig != os.Interrupt {
		stopPager()
	}
	return true
}

// startPaging marks a pager as running; stop asks it to exit.
func (s *altScreen) startPaging(stop func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopPager, s.pending = stop, nil
}

// endPaging marks the pager as done. It returns the signal that stopped the
// pager, if any: the caller then exits with exitForSignal.
func (s *altScreen) endPaging() os.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.stopPager, s.pending = nil, nil
	return pending
}

// reenter switches to the alternate screen again after a pager ran: one
// that uses the alternate screen itself (less without -X, most, vim) leaves
// it when it exits, which would put the browser on the normal screen.
// Entering it again may clear it, so the caller redraws next.
func (s *altScreen) reenter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		terminal.Print(s.out, enterAltScreenSequence)
	}
}

// hide shows the normal screen while the process is stopped (Ctrl-Z);
// reenter shows the browser's again.
func (s *altScreen) hide() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		terminal.Print(s.out, leaveAltScreenSequence)
	}
}

// exitForSignal restores the screen and exits as sig would have.
func (s *altScreen) exitForSignal(sig os.Signal) {
	if s.cancelReads != nil {
		s.cancelReads()
	}
	s.leave()
	s.mu.Lock()
	after := s.afterSignal
	s.mu.Unlock()
	if after != nil {
		after()
	}
	s.exit(signalExitCode(sig))
}

// printAfterSignal has f run when a signal ends the process, once the
// screen is left.
func (s *altScreen) printAfterSignal(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterSignal = f
}

func (e Env) exit(code int) {
	if e.exitProcess != nil {
		e.exitProcess(code)
		return
	}
	os.Exit(code)
}

func signalExitCode(sig os.Signal) int {
	if number, ok := sig.(syscall.Signal); ok {
		return 128 + int(number)
	}
	return 130
}

// draw writes one frame while the screen is up, and nothing once it has been
// left: a frame that was being drawn as a signal restored the terminal must
// not land on the normal screen after it.
func (s *altScreen) draw(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		terminal.Print(s.out, frame)
	}
}

// clears reports whether clear blanks the screen, so a view can be drawn
// again in place instead of below itself.
func (s *altScreen) clears() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// clear blanks the screen before the next view is drawn.
func (s *altScreen) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		terminal.Print(s.out, clearScreenSequence)
	}
}

// restoreOnLeave has leave call restore, before it returns to the normal
// screen.
func (s *altScreen) restoreOnLeave(restore func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreInput = restore
}

// leave returns to the normal screen, where the terminal shows what it
// showed before the browser started, and restores line input.
func (s *altScreen) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restoreInput != nil {
		s.restoreInput()
	}
	if !s.active {
		return
	}
	s.active = false
	terminal.Print(s.out, leaveAltScreenSequence)
	close(s.done)
	if s.stop != nil {
		s.stop()
	}
}
