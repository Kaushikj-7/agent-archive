package cli

import (
	"context"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

type phaseSignalFixture struct {
	mu            sync.Mutex
	registrations []chan os.Signal
	stopped       int
}

func (f *phaseSignalFixture) interrupts() (<-chan os.Signal, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan os.Signal, 2)
	f.registrations = append(f.registrations, ch)
	var once sync.Once
	return ch, func() { once.Do(func() { f.mu.Lock(); f.stopped++; f.mu.Unlock() }) }
}

func TestReadPhaseTransfersPagerSignalsWithoutEndingView(t *testing.T) {
	source := &phaseSignalFixture{}
	ctx, closeReads := readCommandContext(source)
	defer closeReads()
	owner := readPhase(ctx)
	screenSignals := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	screen := &altScreen{out: io.Discard, exit: func(code int) { exited <- code }, cancelReads: owner.cancel}
	screen.start(screenSignals, func() {})
	defer screen.leave()
	owner.Pause()
	owner.Pause()
	source.mu.Lock()
	stopped, oldSignals := source.stopped, source.registrations[0]
	source.mu.Unlock()
	if stopped != 1 || ctx.Err() != nil {
		t.Fatal("discovery pause lost retained view or failed to join", stopped, ctx.Err())
	}
	pagerCtx, stopPager := context.WithCancel(ctx)
	defer stopPager()
	screen.startPaging(stopPager)
	// Model independent signal.Notify registrations receiving the same broadcast.
	// A stale discovery registration must have no remaining consumer.
	oldSignals <- os.Interrupt
	screenSignals <- os.Interrupt
	select {
	case <-ctx.Done():
		t.Fatal("pager Ctrl-C canceled retained view")
	case <-pagerCtx.Done():
		t.Fatal("pager Ctrl-C bypassed screen exemption")
	case <-time.After(20 * time.Millisecond):
	}
	screenSignals <- syscall.SIGTERM
	select {
	case <-pagerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("termination did not stop pager")
	}
	sig := screen.endPaging()
	if sig != syscall.SIGTERM {
		t.Fatal("pager lost termination signal", sig)
	}
	screen.exitForSignal(sig)
	select {
	case code := <-exited:
		if code != signalExitCode(syscall.SIGTERM) || ctx.Err() != context.Canceled {
			t.Fatal("screen failed to cancel read owner before exit", code, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("screen failed to exit")
	}
	owner.Resume()
	source.mu.Lock()
	count := len(source.registrations)
	source.mu.Unlock()
	if count != 1 {
		t.Fatal("canceled view resumed discovery watcher", count)
	}
}

func TestReadPhaseResumesSameViewForSelectedReads(t *testing.T) {
	source := &phaseSignalFixture{}
	ctx, closeReads := readCommandContext(source)
	owner := readPhase(ctx)
	owner.Pause()
	owner.Resume()
	if readPhase(ctx) != owner || ctx.Err() != nil {
		t.Fatal("phase transition replaced view authority")
	}
	source.mu.Lock()
	next := source.registrations[1]
	source.mu.Unlock()
	next <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("resumed selected reads ignored interrupt")
	}
	closeReads()
	owner.Resume()
	source.mu.Lock()
	count := len(source.registrations)
	source.mu.Unlock()
	if count != 2 {
		t.Fatal("closed owner started another watcher", count)
	}
}
