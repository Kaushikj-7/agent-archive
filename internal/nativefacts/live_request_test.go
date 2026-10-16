package nativefacts

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Mechanism fixture uses private state rather than opening the exact state-only
// constructor to test callers. It cannot mint a native capture proof.
func liveMechanismFixture(t *testing.T) (*LiveCaptureRequest, *CaptureRequest, *int, *int) {
	t.Helper()
	original := requestMechanismFixture(t, agentapi.NewNativeReadBudget(agentapi.PassReadBudget))
	captured, err := original.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	calls, cleanups := new(int), new(int)
	ctx, cancel := context.WithCancelCause(captured.Context())
	state := &liveRequestState{original: original, captured: captured, ctx: ctx, cancel: cancel,
		reserve: func(int64) error { *calls++; return nil }, open: func() (*os.Root, error) { *calls++; return nil, errors.New("unused test open") },
		cleanup: func() error { *cleanups++; return nil }, check: func() error { return nil }}
	live := &LiveCaptureRequest{state: state}
	t.Cleanup(func() {
		if err := live.Close(); err != nil {
			t.Error(err)
		}
	})
	return live, original, calls, cleanups
}

func TestLiveCaptureCancelledOriginalRefusesBeforeWorkspaceCallbacks(t *testing.T) {
	t.Parallel()
	live, original, calls, _ := liveMechanismFixture(t)
	original.Close()
	if _, err := live.Borrow(); err == nil {
		t.Fatal("closed original borrowed")
	}
	if *calls != 0 {
		t.Fatal("closed original reached workspace", *calls)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveCaptureCopiedBindingIsOneShotAndCleanupWaitsBorrower(t *testing.T) {
	t.Parallel()
	live, _, _, cleanups := liveMechanismFixture(t)
	copy := *live
	lease, err := live.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = copy.Borrow(); err == nil {
		t.Fatal("copy restarted capture")
	}
	if err = copy.Close(); err != nil {
		t.Fatal(err)
	}
	if *cleanups != 0 {
		t.Fatal("cleaned while SQL borrower active")
	}
	if err = lease.Err(); err == nil {
		t.Fatal("active lease missed binding close")
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if *cleanups != 1 {
		t.Fatal(*cleanups)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err = live.Close(); err != nil {
		t.Fatal(err)
	}
	if *cleanups != 1 {
		t.Fatal("duplicate cleanup", *cleanups)
	}
}

func TestLiveCaptureLeaseCleanupFailureRemainsRetryable(t *testing.T) {
	t.Parallel()
	live, _, _, _ := liveMechanismFixture(t)
	refusal := errors.New("synthetic unlink failure")
	attempts := 0
	live.state.cleanup = func() error {
		attempts++
		if attempts == 1 {
			return refusal
		}
		return nil
	}
	lease, err := live.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	if live.state.cleaned {
		t.Fatal("failed cleanup released captured lease")
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if !live.state.cleaned || attempts != 2 {
		t.Fatal(live.state.cleaned, attempts)
	}
}
