package reader

import (
	"context"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// OpenRemoteSessionCatalog opens and refreshes the complete local summary
// universe for a qualified catalog destination. A nil handle means that the
// disposable cache is unavailable; callers must use the complete remote fallback.
// Remote authority and reconciliation failures are returned, never hidden by
// cached summaries. Callers retain the read-view context and close the handle.
func OpenRemoteSessionCatalog(ctx context.Context, store storage.ObjectStore, opts ListOptions) (*SQLiteSessionCatalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Cache == nil || !CatalogAuthority(store) {
		return nil, nil
	}
	c, err := OpenSessionCatalog(ctx, opts.Cache, store, opts)
	if err != nil {
		return nil, ctx.Err()
	}
	if err = c.RefreshRemote(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// CatalogSummariesWithOptions discovers the complete fresh summary universe,
// using verified remote deltas and the private local query engine when available.
// Refresh still validates all local row tuples and inventories cached bodies in
// O(N). Without a cache it retains the complete in-memory remote fallback.
// Results retain newest activity/capture order, with harness and ID breaking ties.
func CatalogSummariesWithOptions(ctx context.Context, store storage.ObjectStore, filter Filter, opts ListOptions) ([]archive.Metadata, error) {
	ctx = catalog.WithReadView(ctx)
	// The private projection omits token observations, which may carry the
	// only evidence for a model match. Keep that predicate on complete entries.
	if filter.Model != "" {
		return catalogSummariesUncached(ctx, store, filter)
	}
	c, err := OpenRemoteSessionCatalog(ctx, store, opts)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return catalogSummariesUncached(ctx, store, filter)
	}
	defer func() { _ = c.Close() }()
	rows, err := queryRemoteSummaryRows(ctx, c, MetadataQuery{Filter: filter, Order: ActivityOrder})
	if err != nil {
		return nil, err
	}
	result := make([]archive.Metadata, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.Summary.Metadata())
	}
	// SQL ties use full object keys. Punctuation before the slash separator
	// can change prefix identity ordering, so retain the API's exact comparator.
	sortCatalogSummaries(result)
	return result, nil
}

func queryRemoteSummaryRows(ctx context.Context, c *SQLiteSessionCatalog, query MetadataQuery) ([]CatalogRow, error) {
	q := CatalogQuery{Metadata: query}
	var result []CatalogRow
	for {
		page, err := c.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		if !page.Complete {
			return nil, errors.New("catalog summary universe is incomplete; refresh the view")
		}
		if result == nil {
			result = page.Rows
		} else {
			result = append(result, page.Rows...)
		}
		if page.Next == "" {
			return result, nil
		}
		q.Cursor = page.Next
	}
}

// Complete matching and child/scope accounting still see every eligible local
// summary. Only selected rows return to the authenticated remote identity tree.
func selectRemoteSummaryMetadata(ctx context.Context, store storage.ObjectStore, prefix string, q MetadataQuery, opts ListOptions) (RecentResult, bool, error) {
	if q.Filter.Model != "" {
		return RecentResult{}, false, nil
	}
	c, err := OpenRemoteSessionCatalog(ctx, store, opts)
	if err != nil || c == nil {
		return RecentResult{}, false, err
	}
	defer func() { _ = c.Close() }()
	rows, err := queryRemoteSummaryRows(ctx, c, MetadataQuery{Filter: q.Filter, Order: q.Order})
	if err != nil {
		return RecentResult{}, true, err
	}
	objects := make([]storage.Object, 0, len(rows))
	revisions := make(map[RevisionID]listingindex.Revision, len(rows))
	for _, row := range rows {
		if !strings.HasPrefix(row.Key, canonicalCatalogPrefix(prefix)) {
			continue
		}
		m := row.Summary.Metadata()
		objects = append(objects, storage.Object{Key: row.Key, ETag: row.ETag})
		revisions[RevisionID{Key: row.Key, ETag: row.ETag}] = listingindex.Revision{MetadataKey: row.Key, ETag: row.ETag, Hash: row.Hash, CapturedAt: m.CapturedAt, Activity: listingindex.ActivityTime(m), Parent: m.ParentSessionID, NativeChild: m.NativeChild, Replay: m.IsReplay(), ProjectID: m.ProjectID, RepoKey: m.RepoKey}
	}
	selected, result, err := selectListingRevisions(objects, revisions, q.Filter, q.Limit, opts)
	if err != nil {
		return result, true, err
	}
	selectedRows, err := remoteSelectedCatalogRows(ctx, c.remoteSnapshot, selected)
	if err != nil {
		return result, true, err
	}
	result.Sessions, err = hydrateCatalogRows(ctx, c.remoteSnapshot, selectedRows, opts)
	return result, true, err
}

func remoteSelectedCatalogRows(ctx context.Context, snapshot *catalog.Snapshot, selected []listingindex.Revision) ([]catalog.Row, error) {
	span := trace.Start("catalog selected identity lookup")
	defer span.End()
	span.Count("selected identities", len(selected))
	selectedRows := make([]catalog.Row, 0, len(selected))
	for _, revision := range selected {
		entry, err := snapshot.Find(ctx, revision.MetadataKey)
		if err != nil {
			return nil, err
		}
		if entry == nil || entry.Revision != revision.ETag || entry.Metadata.SHA256 != revision.Hash {
			return nil, ErrStaleCatalogCursor
		}
		selectedRows = append(selectedRows, catalog.Row{Key: revision.MetadataKey, Entry: *entry})
	}
	return selectedRows, snapshot.ValidateRead(ctx)
}

func remoteSummaryDelta(ctx context.Context, snapshot *catalog.Snapshot, prior catalog.ObjectRef) (catalog.Delta, error) {
	span := trace.Start("catalog tree delta")
	defer span.End()
	return snapshot.Delta(ctx, prior)
}

func remoteSummaryCount(ctx context.Context, snapshot *catalog.Snapshot) (uint64, error) {
	span := trace.Start("catalog tree count validation")
	defer span.End()
	return snapshot.Count(ctx, catalog.Query{Index: catalog.CaptureIndex})
}
