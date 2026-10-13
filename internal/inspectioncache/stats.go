package inspectioncache

import "sync/atomic"

// Statistics are aggregate best-effort cache operation counts for diagnostics.
type Statistics struct { Lookups, Hits, Queued, Dropped, Stored, Failed uint64 }
var cacheLookups, cacheHits, cacheQueued, cacheDropped, cacheStored, cacheFailed atomic.Uint64

// OperationCounts returns process-wide counters, suitable for paired differences.
func OperationCounts() Statistics { return Statistics{cacheLookups.Load(),cacheHits.Load(),cacheQueued.Load(),cacheDropped.Load(),cacheStored.Load(),cacheFailed.Load()} }
