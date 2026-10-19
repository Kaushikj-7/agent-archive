package cli

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type readDiscoveryKey struct{}

type readDiscovery struct {
	done    chan struct{}
	headers reader.HeaderSnapshot
	err     error
}

// startReadDiscovery overlaps fresh legacy headers with independent local work.
// The caller must stop on every return, including local errors: stop cancels and
// joins the worker, while the caller retains its established error precedence.
// Catalog-authoritative reads retain their own snapshot and freshness protocol.
func startReadDiscovery(ctx context.Context, env metadataCacheDependencies, store storage.ObjectStore, noCache bool) (context.Context, func()) {
	if store == nil || reader.CatalogAuthority(store) || ctx.Value(readDiscoveryKey{}) != nil {
		return ctx, func() {}
	}
	cache := listCache(env, noCache)
	if cache == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	discovery := &readDiscovery{done: make(chan struct{})}
	ctx = context.WithValue(ctx, readDiscoveryKey{}, discovery)
	go func() {
		defer close(discovery.done)
		discovery.headers, discovery.err = reader.DiscoverCatalogHeaders(ctx, store, reader.ListOptions{Cache: cache})
	}()
	return ctx, func() { cancel(); <-discovery.done }
}

func discoverReadHeaders(ctx context.Context, store storage.ObjectStore, opts reader.ListOptions) (reader.HeaderSnapshot, error) {
	if discovery, ok := ctx.Value(readDiscoveryKey{}).(*readDiscovery); ok {
		// Joining also publishes the result without additional synchronization.
		<-discovery.done
		return discovery.headers, discovery.err
	}
	return reader.DiscoverCatalogHeaders(ctx, store, opts)
}
