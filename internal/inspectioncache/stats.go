package inspectioncache

import "sync/atomic"

// Statistics are aggregate best-effort cache operation counts for diagnostics.
type Statistics struct {
	Lookups uint64
	Hits    uint64
	Queued  uint64
	Dropped uint64
	Stored  uint64
	Failed  uint64
}

var cacheLookups, cacheHits, cacheQueued, cacheDropped, cacheStored, cacheFailed atomic.Uint64

// OperationCounts returns process-wide counters, suitable for paired differences.
func OperationCounts() Statistics {
	return Statistics{cacheLookups.Load(), cacheHits.Load(), cacheQueued.Load(), cacheDropped.Load(), cacheStored.Load(), cacheFailed.Load()}
}
