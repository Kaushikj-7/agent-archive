package nativefacts

import "errors"

// LiveCaptureCleanupError retains explicit retry custody after failed cleanup.
// Copies share the same original owner; it grants no read or capture authority.
type LiveCaptureCleanupError struct{ custody *liveCleanupCustody }

type liveCleanupCustody struct {
	state *liveRequestState
	cause error
}

// Error explains the retained obligation without discarding its retry handle.
func (e *LiveCaptureCleanupError) Error() string {
	if e == nil || e.custody == nil {
		return "live Cursor scratch cleanup remains incomplete"
	}
	return "live Cursor scratch cleanup remains incomplete: " + e.custody.cause.Error()
}

// Unwrap preserves the original filesystem/accounting cause through error chains.
func (e *LiveCaptureCleanupError) Unwrap() error {
	if e == nil || e.custody == nil {
		return nil
	}
	return e.custody.cause
}

// Close retries cleanup only after the actual SQL/image borrower has ended.
// Failed retries preserve both the durable receipt and original captured scope.
func (e *LiveCaptureCleanupError) Close() error {
	if e == nil || e.custody == nil {
		return nil
	}
	s := e.custody.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return errors.New("live Cursor image reader still owns scratch")
	}
	s.closed = true
	s.cancel(e.custody.cause)
	return s.finishLocked()
}
