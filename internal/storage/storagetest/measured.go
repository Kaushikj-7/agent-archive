package storagetest

import (
	"context"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// ReadMetrics contains content-free request costs. Bytes counts successful GET
// bodies; ListBytes estimates header payload as key/ETag bytes plus 16 bytes for
// size/time. It is not an estimate of provider XML, HTTP, or TLS overhead.
type ReadMetrics struct {
	Lists     int64
	Gets      int64
	Bytes     int64
	ListBytes int64
	PeakReads int64
}

// MeasuredStore preserves MemoryStore's reader extensions while measuring each
// LIST/GET once. Delay applies to both request kinds and honors cancellation.
type MeasuredStore struct {
	*MemoryStore
	Delay   time.Duration
	mu      sync.Mutex
	metrics ReadMetrics
	active  int64
}

// NewMeasuredStore wraps a synthetic store without recording fixture writes.
func NewMeasuredStore(store *MemoryStore, delay time.Duration) *MeasuredStore {
	return &MeasuredStore{MemoryStore: store, Delay: delay}
}

// Reset discards completed measurements. Call only after readers have joined.
func (s *MeasuredStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != 0 {
		panic("reset with active benchmark reads")
	}
	s.metrics = ReadMetrics{}
}

// Metrics returns a concurrency-safe snapshot.
func (s *MeasuredStore) Metrics() ReadMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metrics
}

func (s *MeasuredStore) begin(ctx context.Context, list bool) error {
	s.mu.Lock()
	if list {
		s.metrics.Lists++
	} else {
		s.metrics.Gets++
	}
	s.active++
	s.metrics.PeakReads = max(s.metrics.PeakReads, s.active)
	s.mu.Unlock()
	if s.Delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(s.Delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *MeasuredStore) end(data []byte, objects []storage.Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.metrics.Bytes += int64(len(data))
	for _, object := range objects {
		s.metrics.ListBytes += int64(len(object.Key) + len(object.ETag) + 16)
	}
}

// Get measures an ordinary body read.
func (s *MeasuredStore) Get(ctx context.Context, key string) (data []byte, err error) {
	defer func() { s.end(data, nil) }()
	if err = s.begin(ctx, false); err != nil {
		return
	}
	return s.MemoryStore.Get(ctx, key)
}

// GetVersioned measures one atomic body-and-validator read.
func (s *MeasuredStore) GetVersioned(ctx context.Context, key string) (data []byte, etag string, err error) {
	defer func() { s.end(data, nil) }()
	if err = s.begin(ctx, false); err != nil {
		return
	}
	return s.MemoryStore.GetVersioned(ctx, key)
}

// GetLimited measures a size-bounded body read.
func (s *MeasuredStore) GetLimited(ctx context.Context, key string, limit int64) (data []byte, err error) {
	defer func() { s.end(data, nil) }()
	if err = s.begin(ctx, false); err != nil {
		return
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

// List measures one complete listing.
func (s *MeasuredStore) List(ctx context.Context, prefix string) (objects []storage.Object, err error) {
	defer func() { s.end(nil, objects) }()
	if err = s.begin(ctx, true); err != nil {
		return
	}
	return s.MemoryStore.List(ctx, prefix)
}

// ListRange measures one disjoint range request.
func (s *MeasuredStore) ListRange(ctx context.Context, prefix, after, through string) (objects []storage.Object, err error) {
	defer func() { s.end(nil, objects) }()
	if err = s.begin(ctx, true); err != nil {
		return
	}
	return s.MemoryStore.ListRange(ctx, prefix, after, through)
}

// ListPage measures one page request, including its returned headers.
func (s *MeasuredStore) ListPage(ctx context.Context, prefix, continuation string, limit int32) (page storage.ObjectPage, err error) {
	defer func() { s.end(nil, page.Objects) }()
	if err = s.begin(ctx, true); err != nil {
		return
	}
	return s.MemoryStore.ListPage(ctx, prefix, continuation, limit)
}
