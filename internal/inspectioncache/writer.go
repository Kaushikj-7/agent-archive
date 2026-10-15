package inspectioncache

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/trace"
	"sync"
	"sync/atomic"
)

// QueueCapacity bounds outstanding observations owned by a command writer.
const QueueCapacity = 256

// WriterStats reports best-effort persistence coverage without changing decisions.
type WriterStats struct {
	Queued  uint64
	Dropped uint64
	Stored  uint64
	Failed  uint64
}

// Writer owns one bounded queue and one goroutine outside the planner.
// Close joins the worker; no worker survives the command that owns it.
type Writer struct {
	cache   *Cache
	ctx     context.Context
	queue   chan Observation
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	queued  atomic.Uint64
	dropped atomic.Uint64
	stored  atomic.Uint64
	failed  atomic.Uint64
}

// NewWriter starts the command-owned cache worker. The context bounds its work.
func NewWriter(ctx context.Context, cache *Cache) *Writer {
	writer := &Writer{cache: cache, ctx: ctx, queue: make(chan Observation, QueueCapacity), done: make(chan struct{})}
	go writer.run()
	return writer
}

// Observe enqueues a verified fact without waiting for cache I/O.
func (w *Writer) Observe(observed Observation) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctx.Err() != nil {
		w.dropped.Add(1)
		trace.Add(trace.InspectionCacheDropped, 1)
		return
	}
	select {
	case w.queue <- observed:
		w.queued.Add(1)
		trace.Add(trace.InspectionCacheQueued, 1)
	default:
		w.dropped.Add(1)
		trace.Add(trace.InspectionCacheDropped, 1)
	}
}

func (w *Writer) run() {
	defer close(w.done)
	// Each persistence operation verifies the bounded directory inventory under
	// its process-shared lock. Waiting for a bounded batch avoids rescanning it
	// once per source when serial inspections arrive farther apart than writes.
	// Close flushes the final partial batch before the command returns.
	batch := make([]Observation, 0, 32)
	for observed := range w.queue {
		batch = append(batch, observed)
		if len(batch) == cap(batch) {
			w.persist(batch)
			clear(batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		w.persist(batch)
	}
}

func (w *Writer) persist(batch []Observation) {
	if w.cache.StoreBatch(w.ctx, batch) != nil {
		w.failed.Add(uint64(len(batch)))
		trace.Add(trace.InspectionCacheFailed, int64(len(batch)))
		return
	}
	w.stored.Add(uint64(len(batch)))
	trace.Add(trace.InspectionCacheStored, int64(len(batch)))
}

// Close drains on success and quickly discards writes after cancellation.
func (w *Writer) Close() WriterStats {
	if w == nil {
		return WriterStats{}
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
	return w.Stats()
}

// Stats exposes aggregate queue and persistence failures to the command owner.
func (w *Writer) Stats() WriterStats {
	return WriterStats{w.queued.Load(), w.dropped.Load(), w.stored.Load(), w.failed.Load()}
}
