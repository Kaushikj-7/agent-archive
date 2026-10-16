package storagetest

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// ListingLatencyModel separates page overhead from transferred-key costs. The
// observed 400–1000 ms full pages do not determine either component alone.
// Leave delays zero for deterministic operation gates; enable them for timing.
// Configure before starting readers and do not mutate while reads are active.
type ListingLatencyModel struct {
	// Headers optionally pins a sorted fixture snapshot to exclude repeated in-memory sorting.
	Headers       []storage.Object
	PageSize      int
	ListPageDelay time.Duration
	ListKeyDelay  time.Duration
	// StopAtBoundary models the corrected exact-boundary exit. False preserves
	// the historical behavior, which needs a key strictly beyond the boundary.
	StopAtBoundary bool
}

// ListingPageCost records actual provider pages, including discarded suffixes.
type ListingPageCost struct {
	Pages          int64
	Keys           int64
	Bytes          int64
	DiscardedBytes int64
}

// ModelListingRange models StartAfter pagination over sorted prefix headers.
// An empty prefix still requires one request. Returned headers are independent
// of the early-exit policy; only transferred pages and bytes can differ.
func ModelListingRange(headers []storage.Object, after, through string, model ListingLatencyModel) ([]storage.Object, ListingPageCost) {
	size := model.PageSize
	if size <= 0 {
		size = 1000
	}
	start := sort.Search(len(headers), func(i int) bool { return headers[i].Key > after })
	end := len(headers)
	if through != "" {
		end = start + sort.Search(len(headers)-start, func(i int) bool { return headers[start+i].Key > through })
	}
	scanned := end - start
	if end < len(headers) && !(model.StopAtBoundary && end > start && headers[end-1].Key == through) {
		scanned++
	}
	pages := max(1, (scanned+size-1)/size)
	transferred := min(pages*size, len(headers)-start)
	cost := ListingPageCost{Pages: int64(pages), Keys: int64(transferred)}
	for i, object := range headers[start : start+transferred] {
		n := int64(len(object.Key) + len(object.ETag) + 16)
		cost.Bytes += n
		if i >= end-start {
			cost.DiscardedBytes += n
		}
	}
	return append([]storage.Object(nil), headers[start:end]...), cost
}

func (s *MeasuredStore) modeledListing(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	headers := s.ListingModel.Headers
	if headers == nil {
		var err error
		headers, err = s.base().List(ctx, prefix)
		if err != nil {
			return nil, err
		}
	} else {
		start := sort.Search(len(headers), func(i int) bool { return headers[i].Key >= prefix })
		end := start
		for end < len(headers) && strings.HasPrefix(headers[end].Key, prefix) {
			end++
		}
		headers = headers[start:end]
	}
	objects, cost := ModelListingRange(headers, after, through, *s.ListingModel)
	s.mu.Lock()
	s.metrics.ListPages += cost.Pages
	s.metrics.ListKeys += cost.Keys
	s.metrics.DiscardedListBytes += cost.DiscardedBytes
	s.metrics.LongestListPages = max(s.metrics.LongestListPages, cost.Pages)
	// end records returned bytes; add the transferred suffix separately.
	s.metrics.ListBytes += cost.DiscardedBytes
	s.mu.Unlock()
	delay := time.Duration(cost.Pages)*s.ListingModel.ListPageDelay + time.Duration(cost.Keys)*s.ListingModel.ListKeyDelay
	if err := waitMeasuredDelay(ctx, delay); err != nil {
		return nil, err
	}
	return objects, nil
}

// ListingSchedule deterministically dispatches range services in supplied order
// into a shared slot budget. Each entry is one sequential range's page count.
// It reports critical-path page waves, without converting them to wall time or
// assuming that separate prefixes each have their own concurrency budget.
func ListingSchedule(pages []int64, slots int) int64 {
	if slots <= 0 {
		panic("listing schedule requires positive slots")
	}
	ends := make([]int64, slots)
	for _, count := range pages {
		slot := 0
		for i := range ends {
			if ends[i] < ends[slot] {
				slot = i
			}
		}
		ends[slot] += count
	}
	var longest int64
	for _, end := range ends {
		longest = max(longest, end)
	}
	return longest
}
