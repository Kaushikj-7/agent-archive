package state

import (
	"context"
	"reflect"
	"time"
)

// withDurableContext borrows the original reservation and release owner while
// keeping both the explicit caller and an existing captured read scope alive.
func (s *Store) withDurableContext(ctx context.Context) (*Store, context.Context, func()) {
	original := s.resourceContext
	if original != nil && reflect.ValueOf(original).Comparable() && original == ctx {
		return s, ctx, func() {}
	}
	scoped := *s
	if original == nil {
		scoped.resourceContext = ctx
		return &scoped, ctx, func() {}
	}
	joined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(original, cancel)
	if original.Err() != nil {
		cancel()
	}
	scoped.resourceContext = durableJoinedContext{Context: joined, original: original}
	return &scoped, scoped.resourceContext, func() { stop(); cancel() }
}

// durableJoinedContext keeps both cancellation owners observable at each read.
type durableJoinedContext struct {
	context.Context
	original context.Context
}

// Err checks the original synchronously as well as through AfterFunc, so a
// cancelled read owner cannot be revived before its cancellation callback runs.
func (c durableJoinedContext) Err() error {
	if err := c.original.Err(); err != nil {
		return err
	}
	return c.Context.Err()
}

func (c durableJoinedContext) Deadline() (time.Time, bool) {
	deadline, limited := c.Context.Deadline()
	original, haveOriginal := c.original.Deadline()
	if haveOriginal && (!limited || original.Before(deadline)) {
		return original, true
	}
	return deadline, limited
}
