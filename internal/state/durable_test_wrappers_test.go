package state

import (
	"context"
	"io"

	"github.com/wangjohn/agent-archive/internal/local"
)

func (s *Store) inspectPendingRoot(home *local.RootedHome, name string, remaining *int, legacy bool) ([]DurableStorageObligation, bool, error) {
	matched := false
	return s.inspectPendingRootWitnessed(home, name, remaining, legacy, &matched)
}

func (s *Store) validateDurableReadPending(id string, history bool, owner *publicationReadOwner) error {
	return s.validateDurableReadPendingSelection(id, history, owner, nil)
}

func (q *durableQuota) writeContext(ctx context.Context, path string, n int64, write func(io.Writer) error) error {
	return q.writeWithHeadroomContext(ctx, path, n, write, nil, 0)
}
