package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
	"io"
)

// catalogSessions supplies the existing CLI matcher with typed projections.
// Paging happens after the final matcher, so exact IDs, configured labels and
// scope fallback cannot be lost to a reader-side candidate limit.
func catalogSessions(env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string, words []string) ([]archive.Metadata, bool, error) {
	return catalogSessionsInContext(context.Background(), env, store, opts, stderr, command, words)
}

func catalogSessionsInContext(ctx context.Context, env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string, words []string) ([]archive.Metadata, bool, error) {
	sessions, used, _, err := catalogCandidateSessions(ctx, env, store, opts, stderr, command, words, nil)
	return sessions, used, err
}

func catalogCandidateSessions(ctx context.Context, env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string, words []string, reuse *showBodyReuse) ([]archive.Metadata, bool, bool, error) {
	if reader.CatalogAuthority(store) {
		cache := listCache(env, opts.noCache)
		if cache != nil {
			c, err := reader.OpenSessionCatalog(ctx, cache, store, reader.ListOptions{Cache: cache, BodyRead: showBodyObserver(reuse, catalogBodyObserver(env))})
			if err == nil {
				defer func() {
					if reuse == nil || reuse.close == nil {
						_ = c.Close()
					}
				}()
				if err = c.RefreshRemote(ctx); err != nil {
					return nil, true, false, err
				}
				page, e := c.Query(ctx, reader.CatalogQuery{Words: words, Metadata: reader.MetadataQuery{Filter: opts.filter, Order: reader.ActivityOrder}})
				if e == nil {
					var sessions []archive.Metadata
					for _, row := range page.Rows {
						sessions = append(sessions, row.Summary.Metadata())
					}
					if reuse != nil {
						reuse.read = c.ReadCachedMetadata
						reuse.close = c.Close
					}
					return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, false, nil
				}
			}
		}
		sessions, err := reader.CatalogSummaries(ctx, store, opts.filter)
		return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, false, err
	}

	cache := listCache(env, opts.noCache)
	if cache == nil {
		return nil, false, false, nil
	}
	span := trace.Start("list metadata")
	defer span.End()
	readOpts := reader.ListOptions{Cache: cache, Skipped: warnSkippedSidecar(stderr, command), BodyRead: showBodyObserver(reuse, catalogBodyObserver(env))}
	catalog, err := reader.OpenSessionCatalog(ctx, cache, store, readOpts)
	if err != nil {
		return nil, false, false, nil
	}
	defer func() {
		if reuse == nil || reuse.close == nil {
			_ = catalog.Close()
		}
	}()
	headers, err := reader.DiscoverCatalogHeaders(ctx, store, readOpts)
	if err != nil {
		return nil, true, false, err
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		sessions, fallbackErr := reader.ListMetadataFromSnapshot(ctx, store, headers, opts.filter, readOpts)
		sortByActivity(sessions)
		return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, true, fallbackErr
	}
	query := reader.CatalogQuery{Words: words, Metadata: reader.MetadataQuery{Filter: opts.filter, Order: reader.ActivityOrder}}
	var sessions []archive.Metadata
	for {
		page, e := catalog.Query(ctx, query)
		if e != nil {
			// A damaged disposable index must not hide current archive sessions.
			// Reuse this command's fresh headers for the verified legacy fallback.
			sessions, fallbackErr := reader.ListMetadataFromSnapshot(ctx, store, headers, opts.filter, readOpts)
			sortByActivity(sessions)
			return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, true, fallbackErr
		}
		for _, row := range page.Rows {
			sessions = append(sessions, row.Summary.Metadata())
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if reuse != nil {
		reuse.read = catalog.ReadCachedMetadata
		reuse.close = catalog.Close
	}
	return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, false, nil
}

// readListCandidates retains selected reads for simple bounded listings and
// uses the summary catalog where exhaustive metadata used to be necessary.
func readListCandidates(env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, limit int, readOpts reader.ListOptions, full bool, stderr io.Writer) (reader.RecentResult, bool, error) {
	needsCatalog := opts.filter.Model != "" || opts.filter.Skill != "" || opts.filter.SkillSHA256 != "" || opts.filter.RequireCompleteCoverage
	if !opts.jsonOut && (full || needsCatalog) {
		// Complete summaries preserve child hints, scope tiers and ambiguity.
		sessions, used, err := catalogSessions(env, store, opts, stderr, "list", nil)
		if used {
			return reader.RecentResult{Sessions: sessions, Complete: true, TotalMatched: len(sessions)}, true, err
		}
	}
	listed, err := reader.ListRecent(context.Background(), store, archiveSessionsPrefix, opts.filter, limit, readOpts)
	return listed, full, err
}

// readShowCandidates distinguishes search projections from verified full bodies.
// The resolver may reuse only the latter as selected metadata authority.
func readShowCandidates(ctx context.Context, store storage.ObjectStore, env metadataCacheDependencies, harness, query string, stderr io.Writer) (showCandidates, error) {
	reuse := &showBodyReuse{verified: map[string]bool{}}
	opts := listOptions{filter: reader.Filter{Harness: harness}}
	sessions, used, fullBodies, err := catalogCandidateSessions(ctx, env, store, opts, stderr, "show", nil, reuse)
	if used {
		return showCandidates{sessions: sessions, fullBodies: fullBodies, reuse: reuse}, err
	}
	readOpts := reader.ListOptions{Cache: listCache(env, false), Skipped: warnSkippedSidecar(stderr, "show"), BodyRead: catalogBodyObserver(env)}
	sessions, err = reader.FindMetadataPrefix(ctx, store, archiveSessionsPrefix, query, opts.filter, readOpts, func(archive.Metadata) bool { return true })
	return showCandidates{sessions: sessions, fullBodies: true}, err
}

type showBodyReuse struct {
	verified map[string]bool
	read     func(context.Context, string) (reader.MetadataLookup, bool, error)
	close    func() error
}

type showCandidates struct {
	sessions   []archive.Metadata
	fullBodies bool
	reuse      *showBodyReuse
}

func (c showCandidates) close() {
	if c.reuse != nil && c.reuse.close != nil {
		_ = c.reuse.close()
	}
}

func (c showCandidates) lookup(ctx context.Context, metadata archive.Metadata) (showLookup, error) {
	if c.fullBodies {
		return showMetadataLookup(metadata), nil
	}
	result := showLookup{SessionID: metadata.SessionID, Harness: metadata.Harness.Name}
	key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil {
		return result, err
	}
	if c.reuse != nil && c.reuse.read != nil && c.reuse.verified[key] {
		lookup, found, err := c.reuse.read(ctx, key)
		if err != nil {
			return result, err
		}
		if found {
			result.Metadata = lookup
		}
	}
	return result, nil
}

func showBodyObserver(reuse *showBodyReuse, observer func(string, bool)) func(string, bool) {
	if reuse == nil {
		return observer
	}
	return func(key string, cached bool) {
		if observer != nil {
			observer(key, cached)
		}
		reuse.verified[key] = true
	}
}

func catalogBodyObserver(env metadataCacheDependencies) func(string, bool) {
	if observer, ok := env.(interface{ listBodyObserver() func(string, bool) }); ok {
		return observer.listBodyObserver()
	}
	return nil
}

type catalogBrowseCommand string

const (
	catalogBrowseShow catalogBrowseCommand = "show"
	catalogBrowseList catalogBrowseCommand = "list"
)
