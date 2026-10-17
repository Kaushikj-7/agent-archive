package reader

import (
	"context"
	"sort"
	"sync"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// rangeSidecars is the fallback size when no complete-key hint exists. A session is about 3.5 listed keys (its sidecar, its current
// source, and superseded sources in their grace period), so a range is about
// 700 keys: one provider page of 1,000.
const rangeSidecars = 200

// rangeConcurrency bounds how many ranges are listed at once. On R2 a page of
// 1,000 keys takes 0.5-0.9 s and pages of one listing are strictly
// sequential, so the ranges in flight are what divide a full listing's time.
const rangeConcurrency = 16

// listObjects lists everything under listPrefix. When the store can list key
// ranges, it prefers boundaries balanced from the previous complete listing,
// falling back to cached sidecar boundaries. Without boundaries it uses the
// store's single sequential List. Both
// return the same objects in the same key order.
func listObjects(ctx context.Context, store storage.ObjectStore, listPrefix string, known []string, cache *MetadataCache) ([]storage.Object, error) {
	return listObjectsWithSlots(ctx, store, listPrefix, known, cache, make(chan struct{}, rangeConcurrency))
}

func listObjectsWithSlots(ctx context.Context, store storage.ObjectStore, listPrefix string, known []string, cache *MetadataCache, slots chan struct{}) ([]storage.Object, error) {
	span := trace.Start("list objects")
	defer span.End()
	bounds, hinted := cache.rangeBounds(listPrefix)
	if !hinted {
		bounds = planRanges(known)
	}
	var objects []storage.Object
	var err error
	if ranger, ok := store.(storage.RangeLister); ok && len(bounds) > 0 {
		span.Count("ranges", len(bounds)+1)
		objects, err = listRangesWithSlots(ctx, ranger, listPrefix, bounds, span, slots)
	} else {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		objects, err = store.List(ctx, listPrefix)
	}
	if err == nil && ctx.Err() == nil {
		cache.saveRangeBounds(listPrefix, objects)
	}
	return objects, err
}

// planRanges picks the boundaries that split a listing into ranges of about
// rangeSidecars known sidecars each: every rangeSidecars-th key, sorted.
// The ranges they make, (-inf, b1], (b1, b2], ..., (bn, +inf), cover every
// possible key, so a boundary only decides how evenly the work is spread,
// never what is listed. Keys the cache has never seen (new sessions, a
// harness this cache has not listed) land in whichever range contains them.
// Fewer than two ranges' worth of known sidecars gets no boundaries: a
// listing that small is one page anyway.
func planRanges(known []string) []string {
	if len(known) < 2*rangeSidecars {
		return nil
	}
	sorted := append([]string(nil), known...)
	sort.Strings(sorted)
	var bounds []string
	for i := rangeSidecars - 1; i < len(sorted)-1; i += rangeSidecars {
		bounds = append(bounds, sorted[i])
	}
	return bounds
}

// listRangesWithSlots lists the ranges bounds make under the shared slot
// budget, each a trace child of span, and joins them in range order, so the
// result is in key order like a single listing. The first range to fail
// cancels the rest, and its error is the one returned: errors that arrive
// after it, whatever they wrap, may be only the echo of that cancellation.
// It returns only after every range it started has finished.
func listRangesWithSlots(ctx context.Context, store storage.RangeLister, prefix string, bounds []string, span *trace.Span, slots chan struct{}) ([]storage.Object, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	count := len(bounds) + 1
	results := make([][]storage.Object, count)
	var failed sync.Once
	var failure error
	var wg sync.WaitGroup
	dispatched := 0
	for index := range count {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			if failure != nil {
				return nil, failure
			}
			return nil, ctx.Err()
		}
		if ctx.Err() != nil {
			<-slots
			break
		}
		after, through := "", ""
		if index > 0 {
			after = bounds[index-1]
		}
		if index < len(bounds) {
			through = bounds[index]
		}
		dispatched++
		wg.Add(1)
		child := span.Child("range")
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer child.End()
			objects, err := store.ListRange(ctx, prefix, after, through)
			child.Count("keys", len(objects))
			if err != nil {
				failed.Do(func() {
					failure = err
					cancel()
				})
				return
			}
			results[index] = objects
		}()
	}
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	if dispatched < count {
		// Dispatch stopped early without a range failing: the caller's
		// context is done.
		return nil, ctx.Err()
	}
	total := 0
	for _, objects := range results {
		total += len(objects)
	}
	joined := make([]storage.Object, 0, total)
	for _, objects := range results {
		joined = append(joined, objects...)
	}
	return joined, nil
}
