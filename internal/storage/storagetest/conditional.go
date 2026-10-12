package storagetest

import (
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// PutConditional compares and writes under the same in-memory lock. This is
// protocol test behavior and never evidence of any provider's semantics.
func (s *MemoryStore) PutConditional(ctx context.Context, key string, data []byte, c storage.PutCondition) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.CreateOnly == (c.MatchETag != "") {
		return "", errors.New("invalid put condition")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.objects[key]
	if c.CreateOnly && exists || !c.CreateOnly && (!exists || old.etag != c.MatchETag) {
		return "", storage.ErrPreconditionFailed
	}
	b := append([]byte(nil), data...)
	etag := md5Hex(b)
	s.objects[key] = memoryObject{data: b, etag: etag, when: time.Now().UTC()}
	return etag, nil
}

// CatalogAtomicQualification refuses unqualified fixtures by default.
func (s *MemoryStore) CatalogAtomicQualification() error { return storage.ErrAtomicCatalogUnqualified }

// GetLimitedVersioned returns a bounded body and validator from one lock scope.
func (s *MemoryStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrNotFound
	}
	if limit < 1 || int64(len(obj.data)) > limit {
		return nil, "", storage.ErrObjectTooLarge
	}
	return append([]byte(nil), obj.data...), obj.etag, nil
}

// GetLimitedVersioned measures one bounded body-and-validator read.
func (s *MeasuredStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) (data []byte, etag string, err error) {
	err = s.begin(ctx, false)
	defer func() { s.end(data, nil) }()
	if err != nil {
		return nil, "", err
	}
	if p, ok := s.base().(storage.LimitedVersionedGetter); ok {
		return p.GetLimitedVersioned(ctx, key, limit)
	}
	return nil, "", errors.New("underlying store has no bounded versioned read capability")
}

// GetCatalogVersion exposes the exact fixture version and its write timestamp.
// This is synthetic protocol data, never provider precision evidence.
func (s *MemoryStore) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.CatalogObjectVersion{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, storage.CatalogObjectVersion{}, storage.ErrNotFound
	}
	if limit < 1 || int64(len(obj.data)) > limit {
		return nil, storage.CatalogObjectVersion{}, storage.ErrObjectTooLarge
	}
	return append([]byte(nil), obj.data...), storage.CatalogObjectVersion{ETag: obj.etag, LastModified: obj.when, Precision: time.Nanosecond}, nil
}

// CatalogServerClock returns exact synthetic fixture time; qualification still refuses.
func (s *MemoryStore) CatalogServerClock(ctx context.Context) (storage.CatalogTime, error) {
	if err := ctx.Err(); err != nil {
		return storage.CatalogTime{}, err
	}
	now := time.Now().UTC()
	return storage.CatalogTime{Earliest: now, Latest: now}, nil
}

// GetCatalogVersion counts one exact version read including provider witness.
func (s *MeasuredStore) GetCatalogVersion(ctx context.Context, key string, limit int64) (data []byte, v storage.CatalogObjectVersion, err error) {
	err = s.begin(ctx, false)
	defer func() { s.end(data, nil) }()
	if err != nil {
		return nil, v, err
	}
	if p, ok := s.base().(storage.CatalogVersionedGetter); ok {
		return p.GetCatalogVersion(ctx, key, limit)
	}
	return nil, v, storage.ErrAtomicCatalogUnqualified
}

// CatalogServerClock forwards qualified clock evidence without using local time.
func (s *MeasuredStore) CatalogServerClock(ctx context.Context) (storage.CatalogTime, error) {
	if p, ok := s.base().(storage.CatalogClock); ok {
		return p.CatalogServerClock(ctx)
	}
	return storage.CatalogTime{}, storage.ErrAtomicCatalogUnqualified
}
