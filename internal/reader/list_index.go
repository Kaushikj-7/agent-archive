package reader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// RecentResult is a bounded newest-first view. Complete means every matching identity was considered, so TotalMatched
// is exact. Indexed completeness proves discovery coverage, not validation
// of unselected bodies.
type RecentResult struct {
	Sessions     []archive.Metadata
	Hidden       int
	Children     map[string]int
	ScopeEmpty   bool
	Outside      int
	TotalMatched int
	Complete     bool
}

// ListRecent proves coverage from fresh canonical headers before choosing bodies.
// Unsupported predicates, incomplete indexes, and nonpositive limits use the
// exhaustive cache reader for compatibility. SelectMetadata supports indexed
// unlimited queries.
func ListRecent(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions) (RecentResult, error) {
	if CatalogAuthority(store) {
		order := CaptureOrder
		if opts.ActivityOrder {
			order = ActivityOrder
		}
		return SelectMetadata(ctx, store, prefix, MetadataQuery{Filter: filter, Limit: limit, Order: order, TopLevelOnly: opts.TopLevelOnly}, opts)
	}
	if limit <= 0 {
		if opts.CompatibilityScan != nil {
			opts.CompatibilityScan("query requires an exhaustive metadata scan")
		}
		return listRecentFullWithHeaders(ctx, store, prefix, filter, limit, opts, nil)
	}
	order := CaptureOrder
	if opts.ActivityOrder {
		order = ActivityOrder
	}
	return SelectMetadata(ctx, store, prefix, MetadataQuery{Filter: filter, Limit: limit, Order: order, TopLevelOnly: opts.TopLevelOnly}, opts)
}

// QueryOrder selects the ordering used before applying the query limit.
type QueryOrder uint8

const (
	// CaptureOrder sorts newest capture first, with identity ties.
	CaptureOrder QueryOrder = iota
	// ActivityOrder sorts newest activity first, then capture and identity.
	ActivityOrder
)

// MetadataQuery selects metadata from fresh headers before downloading bodies.
// A nonpositive Limit selects every matching session.
type MetadataQuery struct {
	Filter       Filter     `json:"Filter"`
	Limit        int        `json:"Limit"`
	Order        QueryOrder `json:"Order"`
	TopLevelOnly bool       `json:"TopLevelOnly"`
	// IncludeRootChildren retains descendants of date-matched sessions even
	// outside the capture bounds. Other predicates still apply to each member.
	// Stats uses this to keep complete root accounting; ancestors are not added.
	IncludeRootChildren bool `json:"IncludeRootChildren"`
}

// SelectMetadata proves discovery coverage and supports unlimited date queries.
// Unsupported predicates and incomplete summaries use the exhaustive reader.
func SelectMetadata(ctx context.Context, store storage.ObjectStore, prefix string, query MetadataQuery, opts ListOptions) (RecentResult, error) {
	if CatalogAuthority(store) {
		opts.Cache.maintain(ctx)
		opts.ActivityOrder = query.Order == ActivityOrder
		opts.TopLevelOnly = query.TopLevelOnly
		opts.includeRootChildren = query.IncludeRootChildren
		return selectCatalogMetadata(ctx, store, prefix, query, opts)
	}
	filter, limit := query.Filter, query.Limit
	opts.ActivityOrder, opts.TopLevelOnly = query.Order == ActivityOrder, query.TopLevelOnly
	opts.includeRootChildren = query.IncludeRootChildren
	var snapshot *HeaderSnapshot
	opts.Cache.maintain(ctx)
	fallback := func(reason string) (RecentResult, error) {
		if opts.CompatibilityScan != nil {
			opts.CompatibilityScan(reason)
		}
		return listRecentFullWithHeaders(ctx, store, prefix, filter, limit, opts, snapshot)
	}
	getter, ok := store.(storage.VersionedGetter)
	if !ok || filter.Model != "" || filter.Skill != "" || filter.SkillSHA256 != "" || filter.RequireCompleteCoverage {
		return fallback("query requires an exhaustive metadata scan")
	}
	headers, err := discoverHeaders(ctx, store, prefix, filter, opts.Cache)
	if err != nil {
		return RecentResult{}, err
	}
	snapshot = &headers
	if headers.incompleteReason != "" {
		return fallback(headers.incompleteReason)
	}
	objects := headers.Canonical
	selectionFilter := filter
	if query.IncludeRootChildren {
		objects, err = selectRootChildHeaders(objects, headers.Revisions, filter)
		if err != nil {
			return fallback(err.Error())
		}
		selectionFilter.From, selectionFilter.To = time.Time{}, time.Time{}
	}
	selected, result, err := selectListingRevisions(objects, headers.Revisions, selectionFilter, limit, opts)
	if err != nil {
		return fallback(err.Error())
	}
	reads := readSelected(ctx, getter, selected, opts)
	for _, read := range reads {
		if read.Err != nil {
			return RecentResult{}, read.Err
		}
		result.Sessions = append(result.Sessions, read.Metadata)
	}
	if err := ctx.Err(); err != nil {
		return RecentResult{}, err
	}
	return result, nil
}

// selectedRead occupies the same slot as its revision in selection order.
type selectedRead struct {
	Metadata archive.Metadata
	Cached   bool
	Err      error
}

// readSelected stops assigning work on failure and joins every started read.
// In-flight reads retain the caller context: an internal cancellation must not
// hide an earlier selected revision's real error. Progress runs serially on
// the caller as reads finish; BodyRead runs in selection order after the join.
func readSelected(ctx context.Context, getter storage.VersionedGetter, revisions []listingindex.Revision, opts ListOptions) []selectedRead {
	return readSelectedRows(ctx, len(revisions), opts, func(i int) (selectedRead, bool) {
		return readSelectedRevision(ctx, getter, revisions[i], opts.Cache)
	}, func(i int) string { return revisions[i].MetadataKey })
}

// readSelectedRows shares the bounded scheduler while each authority supplies
// its exact selected-body reader. Progress runs on the caller as reads finish;
// selected body observers run in order after every worker has joined.
func readSelectedRows(ctx context.Context, count int, opts ListOptions, read func(int) (selectedRead, bool), key func(int) string) []selectedRead {
	reads := make([]selectedRead, count)
	observed := make([]bool, count)
	var mu sync.Mutex
	var wg sync.WaitGroup
	completed := make(chan struct{}, count)
	next, failed := 0, false
	for range min(listConcurrency, count) {
		wg.Go(func() {
			for {
				mu.Lock()
				if failed || ctx.Err() != nil || next == count {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				reads[i], observed[i] = read(i)
				completed <- struct{}{}
				if reads[i].Err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
					return
				}
			}
		})
	}
	go func() {
		wg.Wait()
		close(completed)
	}()
	finished := 0
	for range completed {
		finished++
		if opts.Progress != nil {
			opts.Progress(finished, count)
		}
	}
	for i := range next {
		if observed[i] && opts.BodyRead != nil {
			opts.BodyRead(key(i), reads[i].Cached)
		}
	}
	return reads[:next]
}

func readSelectedRevision(ctx context.Context, getter storage.VersionedGetter, r listingindex.Revision, cache *MetadataCache) (selectedRead, bool) {
	data, cached := cache.get(r.MetadataKey, r.ETag)
	if cached && storage.SHA256Hex(data) != r.Hash {
		cached = false
	}
	if !cached {
		var validator string
		var err error
		data, validator, err = getter.GetVersioned(ctx, r.MetadataKey)
		if err != nil {
			return selectedRead{Err: fmt.Errorf("incomplete listing: selected metadata %q changed or cannot be read; retry or use --limit 0: %w", r.MetadataKey, err)}, false
		}
		if validator != r.ETag || storage.SHA256Hex(data) != r.Hash {
			return selectedRead{Err: fmt.Errorf("incomplete listing: metadata %q changed during query; retry or use --limit 0", r.MetadataKey)}, false
		}
	}
	metadata, err := decodeMetadata(r.MetadataKey, data)
	if err != nil {
		return selectedRead{Cached: cached, Err: err}, true
	}
	if err := r.ValidateMetadata(data); err != nil {
		return selectedRead{Cached: cached, Err: fmt.Errorf("incomplete listing: invalid revision summary for %q", r.MetadataKey)}, true
	}
	if !cached {
		cache.putVerified(r.MetadataKey, r.ETag, data)
	}
	return selectedRead{Metadata: metadata, Cached: cached}, true
}

func listRecentFullWithHeaders(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions, snapshot *HeaderSnapshot) (RecentResult, error) {
	var all []archive.Metadata
	var err error
	readFilter := filter
	if opts.includeRootChildren {
		readFilter.From, readFilter.To = time.Time{}, time.Time{}
	}
	if snapshot == nil {
		all, err = ListMetadataWithOptions(ctx, store, prefix, readFilter, opts)
	} else {
		all, err = listMetadataFromHeaders(ctx, store, readFilter, opts, snapshot.Canonical, snapshot.knownCanonical)
	}
	if err != nil {
		return RecentResult{}, err
	}
	if opts.includeRootChildren {
		all = SelectRootChildren(all, filter, func(m archive.Metadata) (string, string, time.Time) {
			return m.SessionID, m.ParentSessionID, m.CapturedAt
		})
	}
	allChildren := make(map[string]int)
	for _, m := range all {
		if m.ParentSessionID != "" {
			allChildren[m.Harness.Name+"/"+m.ParentSessionID]++
		}
	}
	scopeEmpty, outside := false, 0
	if opts.ScopeMatch != nil {
		scoped := make([]archive.Metadata, 0, len(all))
		for _, m := range all {
			if opts.ScopeMatch(m) {
				scoped = append(scoped, m)
			}
		}
		countScoped := len(scoped)
		if opts.TopLevelOnly {
			countScoped = 0
			for _, m := range scoped {
				if !m.IsChild() {
					countScoped++
				}
			}
		}
		outside = len(all) - len(scoped)
		if countScoped == 0 {
			scopeEmpty = true
		} else {
			all = scoped
		}
	}
	result := RecentResult{Complete: true, Children: allChildren, ScopeEmpty: scopeEmpty, Outside: outside}
	for _, m := range all {
		if m.IsChild() {
			if opts.TopLevelOnly {
				result.Hidden++
			}
		}
	}
	if opts.TopLevelOnly {
		top := all[:0]
		for _, m := range all {
			if !m.IsChild() {
				top = append(top, m)
			}
		}
		all = top
	}
	if opts.ActivityOrder {
		sort.SliceStable(all, func(i, j int) bool { return listingindex.ActivityTime(all[i]).After(listingindex.ActivityTime(all[j])) })
	}
	result.TotalMatched = len(all)
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	result.Sessions = all
	return result, nil
}

// RebuildIndex validates canonical bodies and writes only auxiliary revisions.
// Each invocation resumes through convergent auxiliary writes; no ready marker is used.
func RebuildIndex(ctx context.Context, store storage.ObjectStore, prefix string) (int, error) {
	getter, ok := store.(storage.VersionedGetter)
	if !ok {
		return 0, errors.New("store cannot return metadata revision validators")
	}
	legacy, err := listingindex.LegacyEntries(ctx, store)
	if err != nil {
		return 0, err
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		data, etag, err := getter.GetVersioned(ctx, obj.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return count, err
		}
		r, err := listingindex.NewRevision(obj.Key, data, etag)
		if err != nil {
			return count, fmt.Errorf("rebuild index %q: %w", obj.Key, err)
		}
		if err = listingindex.RebuildRevision(ctx, store, r, legacy[r.MetadataKey]); err != nil {
			return count, err
		}
		count++
	}
	if err := cleanupRevisionHeaders(ctx, store, legacy); err != nil {
		return count, err
	}
	return count, nil
}

// cleanupRevisionHeaders bounds explicit orphan/malformed cleanup. Repeating a
// partial rebuild resumes from the remaining immutable headers.
func cleanupRevisionHeaders(ctx context.Context, store storage.ObjectStore, legacy map[string][]storage.Object) error {
	hints, err := listRevisionHeaders(ctx, store)
	if err != nil {
		return err
	}
	bySession := make(map[string][]storage.Object)
	cleaned := 0
	for _, hint := range hints {
		r, parseErr := listingindex.ParseRevision(hint.Key)
		if parseErr == nil {
			bySession[r.MetadataKey] = append(bySession[r.MetadataKey], hint)
			continue
		}
		if cleaned == 32 {
			return errors.New("listing cleanup remains pending; rerun rebuild")
		}
		if err := store.Delete(ctx, hint.Key); err != nil {
			return err
		}
		cleaned++
	}
	for key, objects := range legacy {
		for _, obj := range objects {
			if strings.HasPrefix(obj.Key, "listing/by-session-v2/") {
				bySession[key] = append(bySession[key], obj)
			}
		}
	}
	statter, ok := store.(storage.ObjectStatter)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(bySession))
	for key := range bySession {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, err := statter.Stat(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			if err := listingindex.RetireSnapshot(ctx, store, key, bySession[key]); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// selectListingRevisions proves canonical coverage and applies summary predicates before body reads.
func selectListingRevisions(objects []storage.Object, revisions map[RevisionID]listingindex.Revision, filter Filter, limit int, opts ListOptions) ([]listingindex.Revision, RecentResult, error) {
	var selected, all []listingindex.Revision
	allChildren := make(map[string]int)
	scopedHidden, allHidden := 0, 0
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		r, exists := revisions[RevisionID{Key: obj.Key, ETag: obj.ETag}]
		if obj.ETag == "" || !exists {
			return nil, RecentResult{}, errors.New("listing index does not cover current metadata; run list --rebuild-index")
		}
		if filter.Harness != "" && !strings.HasPrefix(r.MetadataKey, "sessions/"+filter.Harness+"/") {
			continue
		}
		if !filter.From.IsZero() && r.CapturedAt.Before(filter.From) || !filter.To.IsZero() && r.CapturedAt.After(filter.To) {
			continue
		}
		if filter.Replays == ReplaysHidden && r.Replay || filter.Replays == ReplaysOnly && !r.Replay {
			continue
		}
		parts := strings.Split(r.MetadataKey, "/")
		inScope := opts.ScopeMatch == nil || opts.ScopeMatch(archive.Metadata{ProjectID: r.ProjectID, RepoKey: r.RepoKey})
		if r.NativeChild || r.Parent != "" {
			if r.Parent != "" {
				allChildren[parts[1]+"/"+r.Parent]++
			}
			if opts.TopLevelOnly {
				allHidden++
				if inScope {
					scopedHidden++
				}
				continue
			}
		}
		all = append(all, r)
		if inScope {
			selected = append(selected, r)
		}
	}
	scopeEmpty := opts.ScopeMatch != nil && len(selected) == 0
	hidden := scopedHidden
	outside := len(all) - len(selected)
	if scopeEmpty {
		selected = all
		hidden = allHidden
	}
	result := RecentResult{Complete: true, Children: allChildren, Hidden: hidden, Outside: outside, ScopeEmpty: scopeEmpty, TotalMatched: len(selected)}
	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i], selected[j]
		if opts.ActivityOrder && !a.Activity.Equal(b.Activity) {
			return a.Activity.After(b.Activity)
		}
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		return a.MetadataKey < b.MetadataKey
	})
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	return selected, result, nil
}

// listRevisionHeaders reads discovery summaries without downloading hint bodies.
func listRevisionHeaders(ctx context.Context, store storage.ObjectStore) ([]storage.Object, error) {
	objects, _, err := listHeaderGroups(ctx, store, "", nil, false, listingindex.V3Prefix)
	if err != nil {
		return nil, err
	}
	return append(objects[1], objects[2]...), nil
}
