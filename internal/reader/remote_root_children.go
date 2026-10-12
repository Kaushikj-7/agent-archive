package reader

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Root accounting needs the complete non-date universe, not a date range.
// Envelope fields prove empty windows without reading unselected overflow bodies.
func selectRootCatalogMetadata(ctx context.Context, snapshot *catalog.Snapshot, store storage.ObjectStore, prefix string, q MetadataQuery, opts ListOptions) (RecentResult, error) {
	if result, used, err := reusableRootCatalog(ctx, snapshot, store, prefix, q, opts); used || err != nil {
		return result, err
	}
	rows, err := lazyCatalogRows(ctx, snapshot)
	if err != nil {
		return RecentResult{}, err
	}
	candidates, hasSeed, ambiguous := rootCatalogHeaders(rows, prefix, q.Filter)
	if !hasSeed {
		_, result, err := selectRootCatalogRows(nil, q, opts)
		if err != nil {
			return result, err
		}
		return result, snapshot.ValidateRead(ctx)
	}
	if ambiguous || catalogRequiresSummaryFilter(q.Filter) || opts.ScopeMatch != nil {
		return fullRootCatalogMetadata(ctx, snapshot, candidates, q, opts)
	}
	candidates = SelectRootChildren(candidates, q.Filter, rootCatalogFields)
	selected, result, err := selectRootCatalogRows(candidates, q, opts)
	if err != nil {
		return result, err
	}
	result.Sessions, err = hydrateCatalogRows(ctx, snapshot, selected, opts)
	return result, err
}

func rootCatalogHeaders(rows []catalog.Row, prefix string, filter Filter) ([]catalog.Row, bool, bool) {
	var candidates []catalog.Row
	identities := make(map[string]bool)
	hasSeed, ambiguous := false, false
	nonDate := filter
	nonDate.From, nonDate.To = time.Time{}, time.Time{}
	for _, row := range rows {
		m := row.Entry.Summary
		if !strings.HasPrefix(row.Key, canonicalCatalogPrefix(prefix)) || !matchesCapture(m, nonDate) {
			continue
		}
		if identities[m.SessionID] || m.SessionID == "" {
			ambiguous = true
		}
		identities[m.SessionID] = true
		hasSeed = hasSeed || matchesCapture(m, filter)
		candidates = append(candidates, row)
	}
	return candidates, hasSeed, ambiguous
}

func rootCatalogFields(row catalog.Row) (string, string, time.Time) {
	return row.Entry.Summary.SessionID, row.Entry.Summary.ParentSessionID, row.Entry.Summary.CapturedAt
}

func rootCatalogRevision(row catalog.Row) listingindex.Revision {
	m := row.Entry.Summary
	return listingindex.Revision{MetadataKey: row.Key, ETag: row.Entry.Revision, Hash: row.Entry.Metadata.SHA256, CapturedAt: m.CapturedAt, Activity: listingindex.ActivityTime(m), Parent: m.ParentSessionID, NativeChild: m.NativeChild, Replay: m.IsReplay(), ProjectID: m.ProjectID, RepoKey: m.RepoKey}
}

func selectRootCatalogRows(rows []catalog.Row, q MetadataQuery, opts ListOptions) ([]catalog.Row, RecentResult, error) {
	objects := make([]storage.Object, 0, len(rows))
	revisions := make(map[RevisionID]listingindex.Revision, len(rows))
	byKey := make(map[string]catalog.Row, len(rows))
	for _, row := range rows {
		objects = append(objects, storage.Object{Key: row.Key, ETag: row.Entry.Revision})
		revisions[RevisionID{Key: row.Key, ETag: row.Entry.Revision}] = rootCatalogRevision(row)
		byKey[row.Key] = row
	}
	filter := q.Filter
	filter.From, filter.To = time.Time{}, time.Time{}
	selected, result, err := selectListingRevisions(objects, revisions, filter, q.Limit, opts)
	var selectedRows []catalog.Row
	for _, revision := range selected {
		selectedRows = append(selectedRows, byKey[revision.MetadataKey])
	}
	return selectedRows, result, err
}

// Unsupported predicates and ambiguous header identity require complete bodies.
// This stays pinned to catalog authority and never calls canonical LIST.
func fullRootCatalogMetadata(ctx context.Context, snapshot *catalog.Snapshot, rows []catalog.Row, q MetadataQuery, opts ListOptions) (RecentResult, error) {
	bodies, err := hydrateCatalogRows(ctx, snapshot, rows, opts)
	if err != nil {
		return RecentResult{}, err
	}
	nonDate := q.Filter
	nonDate.From, nonDate.To = time.Time{}, time.Time{}
	var eligible []catalog.Row
	byKey := make(map[string]archive.Metadata)
	for i, body := range bodies {
		if !matches(body, nonDate) {
			continue
		}
		row := rows[i]
		row.Entry.Summary = body
		row.Entry.SummaryOverflow = ""
		eligible = append(eligible, row)
		byKey[row.Key] = body
	}
	eligible = SelectRootChildren(eligible, q.Filter, rootCatalogFields)
	selected, result, err := selectRootCatalogRows(eligible, q, opts)
	if err != nil {
		return result, err
	}
	for _, row := range selected {
		result.Sessions = append(result.Sessions, byKey[row.Key])
	}
	return result, snapshot.ValidateRead(ctx)
}

// Reuse is limited to an existing complete SQL universe for this captured root.
// Cold stats requests do not create a catalog or resolve unselected overflow.
func reusableRootCatalog(ctx context.Context, snapshot *catalog.Snapshot, store storage.ObjectStore, prefix string, q MetadataQuery, opts ListOptions) (RecentResult, bool, error) {
	if opts.Cache == nil || canonicalCatalogPrefix(prefix) != "sessions/" || opts.ScopeMatch != nil {
		return RecentResult{}, false, nil
	}
	path := filepath.Join(filepath.Dir(opts.Cache.dir), "catalog", "sessions.sqlite")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RecentResult{}, false, nil
	}
	if err != nil {
		return RecentResult{}, false, err
	}
	if !info.Mode().IsRegular() {
		return RecentResult{}, false, errors.New("catalog requires a regular database")
	}
	local, err := openExistingRootCatalog(ctx, store, opts)
	if err != nil {
		return RecentResult{}, false, err
	}
	if local == nil {
		return RecentResult{}, false, nil
	}
	defer func() { _ = local.Close() }()
	root := snapshot.Root()
	used, err := local.refreshRemote(ctx, &root)
	if !used || err != nil {
		return RecentResult{}, false, err
	}
	query := q
	query.Limit, query.TopLevelOnly = 0, false
	page, err := local.Query(ctx, CatalogQuery{Metadata: query})
	if err != nil {
		return RecentResult{}, false, err
	}
	if !page.Complete {
		return RecentResult{}, false, nil
	}
	objects := make([]storage.Object, 0, len(page.Rows))
	revisions := make(map[RevisionID]listingindex.Revision, len(page.Rows))
	for _, row := range page.Rows {
		m := row.Summary.Metadata()
		objects = append(objects, storage.Object{Key: row.Key, ETag: row.ETag})
		revisions[RevisionID{Key: row.Key, ETag: row.ETag}] = listingindex.Revision{MetadataKey: row.Key, ETag: row.ETag, Hash: row.Hash, CapturedAt: m.CapturedAt, Activity: listingindex.ActivityTime(m), Parent: m.ParentSessionID, NativeChild: m.NativeChild, Replay: m.IsReplay(), ProjectID: m.ProjectID, RepoKey: m.RepoKey}
	}
	filter := q.Filter
	filter.From, filter.To = time.Time{}, time.Time{}
	selected, result, err := selectListingRevisions(objects, revisions, filter, q.Limit, opts)
	if err != nil {
		return result, true, err
	}
	getter, ok := store.(storage.VersionedGetter)
	if !ok {
		return result, true, errors.New("catalog cannot return metadata revision validators")
	}
	reads := readSelectedRows(ctx, len(selected), opts, func(i int) (selectedRead, bool) {
		if err := snapshot.ValidateRead(ctx); err != nil {
			return selectedRead{Err: err}, false
		}
		read, observed := readSelectedRevision(ctx, getter, selected[i], opts.Cache)
		if read.Err == nil {
			read.Err = snapshot.ValidateRead(ctx)
		}
		return read, observed
	}, func(i int) string { return selected[i].MetadataKey })
	for _, read := range reads {
		if read.Err != nil {
			return result, true, read.Err
		}
		result.Sessions = append(result.Sessions, read.Metadata)
	}
	return result, true, snapshot.ValidateRead(ctx)
}

// A reuse probe never creates, migrates, retires or repairs a database.
func openExistingRootCatalog(ctx context.Context, store storage.ObjectStore, opts ListOptions) (*SQLiteSessionCatalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bound, err := boundCatalogCache(opts.Cache, store, opts)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(opts.Cache.dir), "catalog")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, ctx.Err()
	}
	path := filepath.Join(dir, "sessions.sqlite")
	for _, candidate := range []string{path, path + "-wal", path + "-shm", filepath.Join(dir, "open.lock")} {
		info, err = os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) && candidate != path {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, ctx.Err()
		}
	}
	unlock, err := lockCatalogOpen(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, "SELECT key,etag,hash,capture,activity,summary,search,lowerid,unlabeled,summary_hash FROM sessions LIMIT 0")
	if err != nil {
		_ = db.Close()
		return nil, ctx.Err()
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		_ = db.Close()
		return nil, ctx.Err()
	}
	if err = rows.Err(); err != nil {
		_ = db.Close()
		// A failed local schema probe cannot authorize reuse. Cancellation
		// remains an error; unavailable cache state uses lazy selection.
		return nil, ctx.Err()
	}
	if err = rows.Close(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteSessionCatalog{db: db, store: store, opts: bound}, nil
}
