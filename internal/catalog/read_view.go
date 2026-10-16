package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type readViewKey struct{}

type readView struct {
	mu       sync.Mutex
	started  time.Time
	scope    string
	snapshot *Snapshot
}

// WithReadView starts a destination-bound request lifetime. Metadata, linked
// children and source resolution can share its immutable root. Callers create
// a new context for a fresh browser view; wrapping an existing view keeps its
// original request start and cannot extend its retention lifetime.
func WithReadView(ctx context.Context) context.Context {
	if _, ok := ctx.Value(readViewKey{}).(*readView); ok {
		return ctx
	}
	return NewReadView(ctx)
}

// NewReadView starts a fresh request while preserving the parent's cancellation
// and deadline. An earlier view and its cursors retain their original lifetime.
func NewReadView(ctx context.Context) context.Context {
	return context.WithValue(ctx, readViewKey{}, &readView{started: time.Now()})
}

func (v *readView) capture(ctx context.Context, store storage.ObjectStore, cache *NodeCache) (*Snapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Qualified wrappers forward their authority's opaque destination key.
	// Missing capabilities conservatively bind to this exact client instance.
	scope := ""
	if authority, ok := store.(interface{ CatalogReadScope() string }); ok {
		scope = authority.CatalogReadScope()
	}
	if scope == "" {
		scope = fmt.Sprintf("%T/%p", store, store)
	}
	if v.scope != "" && v.scope != scope {
		return nil, errors.New("catalog read view belongs to another destination")
	}
	if time.Since(v.started) >= SnapshotLifetime {
		return nil, ErrStaleCursor
	}
	v.scope = scope
	if v.snapshot != nil {
		return v.snapshot, v.snapshot.check(ctx)
	}
	snapshot, err := openSnapshotStarted(ctx, store, cache, true, v.started)
	if err != nil {
		return nil, err
	}
	v.snapshot = snapshot
	return snapshot, nil
}

func (s *Store) findForRead(ctx context.Context, key string) (*CatalogEntry, string, error) {
	if _, ok := ctx.Value(readViewKey{}).(*readView); !ok {
		return s.Writer.Find(ctx, key)
	}
	snapshot, err := OpenSnapshot(ctx, s, nil)
	if err != nil {
		return nil, "", err
	}
	entry, err := snapshot.findPinned(ctx, key)
	if err != nil || entry == nil {
		return entry, "", err
	}
	return entry, entry.Revision, nil
}

func (s *Store) checkReadView(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	if v, ok := ctx.Value(readViewKey{}).(*readView); ok {
		v.mu.Lock()
		defer v.mu.Unlock()
		if time.Since(v.started) >= SnapshotLifetime {
			return ErrStaleCursor
		}
	}
	return ctx.Err()
}
