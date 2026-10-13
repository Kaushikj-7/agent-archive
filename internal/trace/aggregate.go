package trace

import "io"

// Metric identifies one fixed, content-free aggregate. Unlike spans, aggregates
// retain constant space even when a command inspects many sessions or records.
type Metric uint8

const (
	// HeaderBytes counts bytes consumed by bounded native header reads.
	HeaderBytes Metric = iota
	// HashBytes counts bytes consumed while preparing native content digests.
	HashBytes
	// FilterBytes counts bytes consumed as native JSONL filter input.
	FilterBytes
	// VerificationBytes counts bytes consumed by independent prefix verification.
	VerificationBytes
	// AdapterInvocations counts complete planning adapter invocations.
	AdapterInvocations
	// WorkerMicroseconds counts occupied adapter-worker time, excluding budget waits.
	WorkerMicroseconds
	// BudgetWaitMicroseconds counts adapter-worker time waiting for byte admission.
	BudgetWaitMicroseconds
	// PeakWorkers records peak simultaneously admitted planning adapters.
	PeakWorkers
	// SelectedSessions counts final importable planning candidates.
	SelectedSessions
	// SkippedSessions counts final skipped planning candidates.
	SkippedSessions
	// CapacityDrainMicroseconds counts foreground collection needed for import admission.
	CapacityDrainMicroseconds
	// CapacityDrainPasses counts foreground capacity-drain collector calls.
	CapacityDrainPasses
	// InspectionCacheLookups counts bounded content-qualified cache lookups.
	InspectionCacheLookups
	// InspectionCacheHits counts reused successful intrinsic summaries.
	InspectionCacheHits
	// InspectionCacheQueued counts verified observations accepted by the command queue.
	InspectionCacheQueued
	// InspectionCacheDropped counts observations dropped by the bounded command queue.
	InspectionCacheDropped
	// InspectionCacheStored counts observations in successful persistence batches.
	InspectionCacheStored
	// InspectionCacheFailed counts observations in unavailable persistence batches.
	InspectionCacheFailed
	metricCount
)

var metricNames = [metricCount]string{"header_bytes", "hash_bytes", "filter_input_bytes", "verification_bytes", "adapter_invocations", "worker_us", "byte_budget_wait_us", "peak_workers", "selected_sessions", "skipped_sessions", "capacity_drain_us", "capacity_drain_passes", "inspection_cache_lookups", "inspection_cache_hits", "inspection_cache_queued", "inspection_cache_dropped", "inspection_cache_stored", "inspection_cache_failed"}

// Add records a local aggregate once its operation completes. With tracing off
// it performs only the recorder check and retains no measurements.
func Add(metric Metric, value int64) {
	if rec := active.Load(); rec != nil && metric < metricCount {
		rec.metrics[metric].Add(value)
	}
}

// Maximum records a peak aggregate without allocating a per-worker event.
func Maximum(metric Metric, value int64) {
	if rec := active.Load(); rec != nil && metric < metricCount {
		counter := &rec.metrics[metric]
		for old := counter.Load(); value > old; old = counter.Load() {
			if counter.CompareAndSwap(old, value) {
				break
			}
		}
	}
}

// CountReads counts actual reader bytes locally, then flushes one aggregate.
// The caller must invoke finish after the reader's last use, including errors.
// Disabled tracing returns the original reader without a wrapper allocation.
func CountReads(reader io.Reader, metric Metric) (io.Reader, func()) {
	rec := active.Load()
	if rec == nil || metric >= metricCount {
		return reader, func() {}
	}
	measured := &countedReader{Reader: reader}
	return measured, func() { rec.metrics[metric].Add(measured.bytes) }
}

type countedReader struct {
	io.Reader
	bytes int64
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += int64(n)
	return n, err
}
