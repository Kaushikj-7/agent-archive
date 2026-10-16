package storagetest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ReadMetrics contains content-free request costs. Bytes counts successful GET
// bodies; ListBytes estimates header payload as key/ETag bytes plus 16 bytes for
// size/time. It is not an estimate of provider XML, HTTP, or TLS overhead.
type ReadMetrics struct {
	Lists              int64
	Gets               int64
	Bytes              int64
	ListBytes          int64
	PeakReads          int64
	ListPages          int64
	ListKeys           int64
	DiscardedListBytes int64
	LongestListPages   int64
}

// MeasuredStore preserves MemoryStore's reader extensions while measuring each
// LIST/GET once. Delay applies to both request kinds and honors cancellation.
type MeasuredStore struct {
	*MemoryStore
	Store storage.ObjectStore
	Delay time.Duration
	// ListingModel optionally models provider pagination instead of per-call LIST delay.
	ListingModel *ListingLatencyModel
	// GetDelay overrides Delay for body requests when nonzero.
	GetDelay time.Duration
	mu       sync.Mutex
	metrics  ReadMetrics
	active   int64
}

// NewMeasuredStore wraps a synthetic store without recording fixture writes.
func NewMeasuredStore(store storage.ObjectStore, delay time.Duration) *MeasuredStore {
	if memory, ok := store.(*MemoryStore); ok {
		return &MeasuredStore{MemoryStore: memory, Delay: delay}
	}
	return &MeasuredStore{Store: store, Delay: delay}
}

func (s *MeasuredStore) base() storage.ObjectStore {
	if s.Store != nil {
		return s.Store
	}
	return s.MemoryStore
}

// Put preserves fixture writes without counting them as reads.
func (s *MeasuredStore) Put(ctx context.Context, key string, data []byte) error {
	return s.base().Put(ctx, key, data)
}

// Delete forwards fixture removal without read metrics.
func (s *MeasuredStore) Delete(ctx context.Context, key string) error {
	return s.base().Delete(ctx, key)
}

// Stat forwards checksum capability without counting it as a GET.
func (s *MeasuredStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if p, ok := s.base().(storage.ObjectStatter); ok {
		return p.Stat(ctx, key)
	}
	return storage.ObjectInfo{}, errors.New("underlying store has no stat capability")
}

// PutConditional forwards the underlying atomic write capability.
func (s *MeasuredStore) PutConditional(ctx context.Context, key string, data []byte, c storage.PutCondition) (string, error) {
	if p, ok := s.base().(storage.ConditionalPutter); ok {
		return p.PutConditional(ctx, key, data, c)
	}
	return "", storage.ErrAtomicCatalogUnqualified
}

// CatalogAtomicQualification forwards evidence without manufacturing support.
func (s *MeasuredStore) CatalogAtomicQualification() error {
	if p, ok := s.base().(storage.AtomicCatalogProvider); ok {
		return p.CatalogAtomicQualification()
	}
	return storage.ErrAtomicCatalogUnqualified
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
	delay := s.Delay
	if !list && s.GetDelay != 0 {
		delay = s.GetDelay
	}
	if list && s.ListingModel != nil {
		delay = 0
	}
	return waitMeasuredDelay(ctx, delay)
}

func waitMeasuredDelay(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
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
	return s.base().Get(ctx, key)
}

// GetVersioned measures one atomic body-and-validator read.
func (s *MeasuredStore) GetVersioned(ctx context.Context, key string) (data []byte, etag string, err error) {
	defer func() { s.end(data, nil) }()
	if err = s.begin(ctx, false); err != nil {
		return
	}
	if p, ok := s.base().(storage.VersionedGetter); ok {
		return p.GetVersioned(ctx, key)
	}
	return nil, "", errors.New("underlying store has no versioned read capability")
}

// GetLimited measures a size-bounded body read.
func (s *MeasuredStore) GetLimited(ctx context.Context, key string, limit int64) (data []byte, err error) {
	defer func() { s.end(data, nil) }()
	if err = s.begin(ctx, false); err != nil {
		return
	}
	if p, ok := s.base().(storage.LimitedGetter); ok {
		return p.GetLimited(ctx, key, limit)
	}
	return nil, errors.New("underlying store has no bounded read capability")
}

// List measures one complete listing.
func (s *MeasuredStore) List(ctx context.Context, prefix string) (objects []storage.Object, err error) {
	defer func() { s.end(nil, objects) }()
	if err = s.begin(ctx, true); err != nil {
		return
	}
	if s.ListingModel != nil {
		return s.modeledListing(ctx, prefix, "", "")
	}
	return s.base().List(ctx, prefix)
}

// ListRange measures one disjoint range request.
func (s *MeasuredStore) ListRange(ctx context.Context, prefix, after, through string) (objects []storage.Object, err error) {
	defer func() { s.end(nil, objects) }()
	if err = s.begin(ctx, true); err != nil {
		return
	}
	if s.ListingModel != nil {
		return s.modeledListing(ctx, prefix, after, through)
	}
	if p, ok := s.base().(storage.RangeLister); ok {
		return p.ListRange(ctx, prefix, after, through)
	}
	return nil, errors.New("underlying store has no range listing capability")
}

// ListPage measures one page request, including its returned headers.
func (s *MeasuredStore) ListPage(ctx context.Context, prefix, continuation string, limit int32) (page storage.ObjectPage, err error) {
	defer func() { s.end(nil, page.Objects) }()
	if err = s.begin(ctx, true); err != nil {
		return
	}
	if p, ok := s.base().(storage.PageLister); ok {
		page, err = p.ListPage(ctx, prefix, continuation, limit)
		if err == nil && s.ListingModel != nil {
			s.mu.Lock()
			s.metrics.ListPages++
			s.metrics.ListKeys += int64(len(page.Objects))
			s.metrics.LongestListPages = max(s.metrics.LongestListPages, 1)
			s.mu.Unlock()
			err = waitMeasuredDelay(ctx, s.ListingModel.ListPageDelay+time.Duration(len(page.Objects))*s.ListingModel.ListKeyDelay)
		}
		return page, err
	}
	return storage.ObjectPage{}, errors.New("underlying store has no page listing capability")
}

// ObjectKey forwards namespace composition when the underlying store has it.
func (s *MeasuredStore) ObjectKey(key string) string {
	if p, ok := s.base().(storage.ObjectKeyer); ok {
		return p.ObjectKey(key)
	}
	return key
}

// CatalogMetadataAuthority truthfully forwards the narrow catalog adapter flag.
func (s *MeasuredStore) CatalogMetadataAuthority() bool {
	if p, ok := s.base().(storage.CatalogPublisher); ok {
		return p.CatalogMetadataAuthority()
	}
	return false
}

// FreezeCatalogMutation forwards catalog revision observation for journaling.
func (s *MeasuredStore) FreezeCatalogMutation(ctx context.Context, key string) (string, string, error) {
	if p, ok := s.base().(storage.CatalogPublisher); ok && p.CatalogMetadataAuthority() {
		return p.FreezeCatalogMutation(ctx, key)
	}
	return "", "", storage.ErrAtomicCatalogUnqualified
}

// Publication preserves the frozen metadata-write adapter through measurement.
func (s *MeasuredStore) Publication(id, key, expected string) storage.ObjectStore {
	if p, ok := s.base().(storage.CatalogPublisher); ok && p.CatalogMetadataAuthority() {
		return p.Publication(id, key, expected)
	}
	return s
}

// DeleteSession forwards transactional deletion only for catalog authority.
func (s *MeasuredStore) DeleteSession(ctx context.Context, key string) error {
	if p, ok := s.base().(interface {
		DeleteSession(context.Context, string) error
		CatalogMetadataAuthority() bool
	}); ok && p.CatalogMetadataAuthority() {
		return p.DeleteSession(ctx, key)
	}
	return storage.ErrAtomicCatalogUnqualified
}

// BeginPublication forwards the global durable pending lifecycle admission.
func (s *MeasuredStore) BeginPublication(ctx context.Context, id string, raw []byte) (context.Context, error) {
	if lifecycle, ok := s.base().(storage.CatalogLifecycle); ok {
		return lifecycle.BeginPublication(ctx, id, raw)
	}
	return ctx, nil
}

// CompletePublication forwards acknowledged history and pending settlement.
func (s *MeasuredStore) CompletePublication(ctx context.Context, id string, raw []byte) error {
	if lifecycle, ok := s.base().(storage.CatalogLifecycle); ok {
		return lifecycle.CompletePublication(ctx, id, raw)
	}
	return nil
}

// EndPublicationAttempt releases the admitted invocation execution claim.
func (s *MeasuredStore) EndPublicationAttempt(id string) {
	if lifecycle, ok := s.base().(storage.CatalogLifecycle); ok {
		lifecycle.EndPublicationAttempt(id)
	}
}

// BeginJournalPublication forwards actual originating-journal authority.
func (s *MeasuredStore) BeginJournalPublication(ctx context.Context, id string, raw []byte, journal local.CatalogJournal, guard *local.CollectorGuard) (context.Context, bool, error) {
	if lifecycle, ok := s.base().(storage.CatalogJournalLifecycle); ok {
		return lifecycle.BeginJournalPublication(ctx, id, raw, journal, guard)
	}
	return nil, false, storage.ErrAtomicCatalogUnqualified
}

// AcknowledgeJournalRemoval forwards positive durable unlink evidence.
func (s *MeasuredStore) AcknowledgeJournalRemoval(ctx context.Context, proof *local.JournalRemoval, guard *local.CollectorGuard) error {
	if lifecycle, ok := s.base().(storage.CatalogJournalLifecycle); ok {
		return lifecycle.AcknowledgeJournalRemoval(ctx, proof, guard)
	}
	return storage.ErrAtomicCatalogUnqualified
}

// CatalogReadScope forwards an actual opaque catalog authority key. An absent
// capability returns empty so callers conservatively bind this wrapper itself.
func (s *MeasuredStore) CatalogReadScope() string {
	if authority, ok := s.base().(interface{ CatalogReadScope() string }); ok {
		return authority.CatalogReadScope()
	}
	return ""
}
