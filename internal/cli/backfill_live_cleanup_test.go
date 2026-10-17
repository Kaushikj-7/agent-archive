package cli

import (
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

func TestImportFailureWrappersPreserveCleanupCustody(t *testing.T) {
	cleanup := &nativefacts.LiveCaptureCleanupError{}
	stopped := &importStoppedError{cause: errors.Join(backfill.ErrStopped, cleanup), batchID: "synthetic"}
	failure := &importRegistrationError{err: stopped}
	var retained *nativefacts.LiveCaptureCleanupError
	if !errors.As(failure, &retained) || retained != cleanup {
		t.Fatal("registration wrapper lost cleanup custody")
	}
	if !errors.Is(failure, backfill.ErrStopped) {
		t.Fatal("stopped wrapper lost original cause")
	}
	if err := retained.Close(); err != nil {
		t.Fatal("zero custody gained authority", err)
	}
}
