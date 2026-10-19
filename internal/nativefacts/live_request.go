package nativefacts

import (
	"context"
	"errors"
	"os"
	"sync"
)

// LiveCaptureRequest binds one actual original request to its issued scratch
// owner. It is not capture proof; copies share one-shot borrowing and Close.
type LiveCaptureRequest struct{ state *liveRequestState }

type liveRequestState struct {
	mu       sync.Mutex
	original *CaptureRequest
	captured *RequestLease
	ctx      context.Context
	cancel   context.CancelCauseFunc
	reserve  func(int64) error
	open     func() (*os.Root, error)
	cleanup  func() error
	check    func() error
	borrowed bool
	active   bool
	closed   bool
	cleaned  bool
}

// BindLiveCaptureRequest transfers the exact captured lease and real scratch
// owner callbacks from the single state issuer. Whole-source type/object checks
// restrict this constructor to that issuer, including tests and build profiles.
// It never accepts a native locator or caller-supplied captured bytes.
func BindLiveCaptureRequest(original *CaptureRequest, captured *RequestLease, reserve func(int64) error, open func() (*os.Root, error), cleanup func() error, check func() error) (*LiveCaptureRequest, error) {
	if original == nil || original.state == nil || captured == nil || captured.lease == nil || captured.lease.state != original.state || reserve == nil || open == nil || cleanup == nil || check == nil {
		return nil, errors.New("live capture requires the original issued owner")
	}
	// A cancellation racing issuance still returns the cleanup handle; Borrow
	// checks the captured lifetime before granting the sole attempt.
	ctx, cancel := context.WithCancelCause(captured.Context())
	return &LiveCaptureRequest{state: &liveRequestState{original: original, captured: captured, ctx: ctx, cancel: cancel, reserve: reserve, open: open, cleanup: cleanup, check: check}}, nil
}

// LiveCaptureLease permits only one actual live-image attempt on its owner.
// It closes all SQL/file readers before returning owned scratch through Close.
type LiveCaptureLease struct{ state *liveRequestState }

// Borrow obtains the sole attempt; copies cannot restart or retarget the owner.
func (r *LiveCaptureRequest) Borrow() (*LiveCaptureLease, error) {
	if r == nil || r.state == nil {
		return nil, os.ErrClosed
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.borrowed {
		return nil, os.ErrClosed
	}
	if err := errors.Join(s.ctx.Err(), s.captured.Check(), s.check()); err != nil {
		return nil, err
	}
	s.borrowed = true
	s.active = true
	return &LiveCaptureLease{state: s}, nil
}

// Original returns the exact original request for a separately retained proof.
func (l *LiveCaptureLease) Original() *CaptureRequest { return l.state.original }

// Context retains both the original lifetime and this binding's cancellation.
func (l *LiveCaptureLease) Context() context.Context { return l.state.ctx }

// Err checks both issued lifetimes and the scratch owner's original home.
func (l *LiveCaptureLease) Err() error {
	if l == nil || l.state == nil {
		return os.ErrClosed
	}
	s := l.state
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked()
}

func (s *liveRequestState) checkLocked() error {
	if s.closed || !s.active {
		return os.ErrClosed
	}
	return errors.Join(s.ctx.Err(), s.captured.Check(), s.check())
}

// Reserve persists capacity on the original issued owner before image creation.
func (l *LiveCaptureLease) Reserve(n int64) error {
	s := l.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	return s.reserve(n)
}

// OpenWorkspace borrows the issued rooted workspace; the borrower closes it.
func (l *LiveCaptureLease) OpenWorkspace() (*os.Root, error) {
	s := l.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return nil, err
	}
	return s.open()
}

// Close ends the actual attempt after its SQL/file handles have closed. Failed
// cleanup stays retryable and retains the captured lease and durable receipt.
func (l *LiveCaptureLease) Close() error {
	if l == nil || l.state == nil {
		return nil
	}
	s := l.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return s.finishLocked()
	}
	s.active = false
	return s.finishLocked()
}

// Close cancels an attempt but does not clean a workspace still borrowed by SQL.
// The last actual lease performs cleanup; cleanup errors remain retryable.
func (r *LiveCaptureRequest) Close() error {
	if r == nil || r.state == nil {
		return nil
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cancel(os.ErrClosed)
	if s.active {
		return nil
	}
	return s.finishLocked()
}

func (s *liveRequestState) finishLocked() error {
	if s.cleaned {
		return nil
	}
	if err := s.cleanup(); err != nil {
		return &LiveCaptureCleanupError{custody: &liveCleanupCustody{state: s, cause: err}}
	}
	s.cleaned = true
	s.captured.Close()
	s.reserve = nil
	s.open = nil
	s.cleanup = nil
	s.check = nil
	return nil
}
