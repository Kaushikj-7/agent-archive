package reader

import (
	"context"
	"sync"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// RevisionID identifies a summary for an exact canonical object revision.
type RevisionID struct {
	Key  string
	ETag string
}

// HeaderSnapshot holds fresh headers for one query. Canonical is a complete
// listing, including objects without summaries; it is never cached as existence
// authority for another query.
type HeaderSnapshot struct {
	// CanonicalComplete proves Canonical came from one successful complete
	// discovery, independently of auxiliary revision coverage. Partial deltas
	// must not claim this proof or reconcile deletion in a local catalog.
	CanonicalComplete bool
	Canonical         []storage.Object
	Revisions         map[RevisionID]listingindex.Revision

	incompleteReason string
	knownCanonical   []string
}

// discoverHeaders overlaps independent canonical and auxiliary listings. Only
// canonical cache keys plan ranges; v2 must remain global for legacy coverage.
func discoverHeaders(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, cache *MetadataCache) (HeaderSnapshot, error) {
	canonicalPrefix := listPrefixFor(prefix, filter.Harness)
	known := cache.keys(canonicalPrefix)
	groups, canonicalComplete, err := listHeaderGroups(ctx, store, canonicalPrefix, known, true, listPrefixFor(listingindex.V3Prefix, filter.Harness))
	// Complete canonical headers prove absence independently of auxiliary
	// discovery. Evict deleted cache entries even if an index listing failed.
	if canonicalComplete && cache != nil {
		cache.evictUnlisted(known, groups[0])
	}
	if err != nil {
		return HeaderSnapshot{}, err
	}
	snapshot := headerSnapshotFromGroups(groups)
	snapshot.CanonicalComplete = canonicalComplete
	snapshot.knownCanonical = known
	return snapshot, nil
}

// headerSnapshotFromGroups preserves complete canonical headers even when
// auxiliary summaries cannot safely select metadata bodies.
func headerSnapshotFromGroups(groups [3][]storage.Object) HeaderSnapshot {
	snapshot := HeaderSnapshot{Canonical: groups[0], Revisions: make(map[RevisionID]listingindex.Revision)}
hints:
	for _, group := range groups[1:] {
		for _, hint := range group {
			r, err := listingindex.ParseRevision(hint.Key)
			if err != nil {
				snapshot.incompleteReason = "listing index contains an unsupported or damaged entry; run list --rebuild-index"
				break hints
			}
			id := RevisionID{Key: r.MetadataKey, ETag: r.ETag}
			if prior, exists := snapshot.Revisions[id]; exists && !prior.SameSummary(r) {
				snapshot.incompleteReason = "listing index has conflicting revision summaries"
				break hints
			}
			snapshot.Revisions[id] = r
		}
	}
	return snapshot
}

// listHeaderGroups joins all started listings before returning. The first
// provider error cancels siblings and remains the returned error even when
// those siblings subsequently report cancellation.
// canonicalComplete distinguishes a successful empty listing from a partial
// or failed canonical discovery, including when an auxiliary listing fails.
func listHeaderGroups(ctx context.Context, store storage.ObjectStore, canonicalPrefix string, known []string, includeCanonical bool, v3Prefix string) ([3][]storage.Object, bool, error) {
	caller := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var groups [3][]storage.Object
	var canonicalComplete bool
	var wg sync.WaitGroup
	var failed sync.Once
	var failure error
	for i, prefix := range []string{canonicalPrefix, listingindex.V2Prefix, v3Prefix} {
		if i == 0 && !includeCanonical {
			continue
		}
		wg.Go(func() {
			var objects []storage.Object
			var err error
			if i == 0 {
				objects, err = listObjects(ctx, store, prefix, known)
			} else {
				objects, err = store.List(ctx, prefix)
			}
			if err != nil {
				failed.Do(func() {
					failure = err
					cancel()
				})
				return
			}
			groups[i] = objects
			if i == 0 {
				canonicalComplete = true
			}
		})
	}
	wg.Wait()
	if failure != nil {
		return groups, canonicalComplete, failure
	}
	return groups, canonicalComplete, caller.Err()
}
