package state

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"testing"
)

func TestPreparedNativeAdmissionZeroCloseIsSafeAndCannotCommit(t *testing.T) {
	t.Parallel()
	var prepared PreparedNativeAdmission
	if _, err := prepared.Filtered(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("zero prepared output: %v", err)
	}
	s := newTestStore(t)
	if _, err := s.CommitNativeAdmission(t.Context(), &prepared); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("zero prepared commit: %v", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
}
