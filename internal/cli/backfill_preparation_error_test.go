package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestPreparationStopsOnlyForPureInventoryNoProgress(t *testing.T) {
	noProgress := agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, errors.New("synthetic private inventory")), agentapi.CauseLookupNoProgress)
	for _, tc := range []struct {
		name string
		err  error
		stop bool
	}{
		{name: "no progress", err: noProgress, stop: true},
		{name: "wrapped", err: fmt.Errorf("synthetic wrapper: %w", noProgress), stop: true},
		{name: "joined no progress", err: errors.Join(noProgress, noProgress), stop: true},
		{name: "cleanup", err: errors.Join(noProgress, agentapi.Wrap(agentapi.Cleanup, io.ErrUnexpectedEOF))},
		{name: "capacity", err: errors.Join(noProgress, agentapi.Wrap(agentapi.Limit, io.ErrUnexpectedEOF))},
		{name: "bounded I/O timeout", err: errors.Join(noProgress, agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, io.ErrUnexpectedEOF), agentapi.CauseLookupIOTimeout))},
		{name: "I/O", err: errors.Join(noProgress, io.ErrUnexpectedEOF)},
		{name: "cancel", err: errors.Join(noProgress, context.Canceled)},
		{name: "deadline", err: errors.Join(noProgress, context.DeadlineExceeded)},
		{name: "nested cleanup", err: agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, agentapi.Wrap(agentapi.Cleanup, io.ErrUnexpectedEOF)), agentapi.CauseLookupNoProgress)},
		{name: "ordinary unavailable", err: agentapi.Wrap(agentapi.Unavailable, io.ErrUnexpectedEOF)},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lookupNoProgressOnly(tc.err); got != tc.stop {
				t.Fatalf("inventory stop=%v want=%v", got, tc.stop)
			}
		})
	}
}
