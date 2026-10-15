package search

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// LockWork acquires the dedicated, nonblocking index work lock. Composition must
// hold it from authoritative metadata discovery through Observe and Run, then
// defer release on every path. Queries and content-free hints do not take it.
// Never acquire it while holding the collector lock; Run does not nest this lock.
func (s *Store) LockWork(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := local.NamedLock(s.lockDirectoryForTest(ctx), "search-worker.lock")
	if errors.Is(err, local.ErrBusy) {
		return nil, ErrWorkBusy
	}
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (s *Store) lockDirectoryForTest(ctx context.Context) string {
	var seq int
	var name, path string
	_ = s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path)
	return filepath.Dir(path)
}
