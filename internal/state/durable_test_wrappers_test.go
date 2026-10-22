package state

import (
	"context"
	"io"
)

func (s *Store) validateDurableReadPending(id string, history bool, owner *publicationReadOwner) error {
	return s.validateDurableReadPendingSelection(id, history, owner, nil)
}

func (q *durableQuota) writeContext(ctx context.Context, path string, n int64, write func(io.Writer) error) error {
	return q.writeWithHeadroomContext(ctx, path, n, write, nil, 0)
}
