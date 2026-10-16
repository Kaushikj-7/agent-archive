package storage

import "context"

type listObserverKey struct{}

// WithListObserver returns ctx carrying observe, which a store that lists
// page by page (S3Store's List and ListRange) calls with the objects of each
// page as it arrives, before the listing returns. It is how a command shows
// a listing's progress without every layer between it and the store passing
// a callback along. Listings run concurrently, so observe must be safe to call
// from several goroutines at once, and it must not block or keep objects.
func WithListObserver(ctx context.Context, observe func(objects []Object)) context.Context {
	return context.WithValue(ctx, listObserverKey{}, observe)
}

// ObserveListPage reports one listed page to the observer ctx carries, if
// any. A store that lists page by page calls it once per page; one that
// lists in a single step, such as a test's in-memory store, once per listing.
func ObserveListPage(ctx context.Context, objects []Object) {
	if observe, ok := ctx.Value(listObserverKey{}).(func([]Object)); ok && len(objects) > 0 {
		observe(objects)
	}
}
