package storage_test

// Tests of the store helpers that run against the in-memory store, which
// lives in storagetest (test code only) and so can only be used from this
// external test package.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func responseError(status int, cause error) error {
	return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      cause,
	}}
}

// getOnlyStore hides MemoryStore's Stat, standing in for a store that can
// only verify an object by reading it.
type getOnlyStore struct {
	inner *storagetest.MemoryStore
	gets  int
}

func (s *getOnlyStore) Put(ctx context.Context, key string, data []byte) error {
	return s.inner.Put(ctx, key, data)
}

func (s *getOnlyStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.inner.Get(ctx, key)
}

func (s *getOnlyStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	return s.inner.List(ctx, prefix)
}

func (s *getOnlyStore) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, key)
}

// statCountingStore counts Gets on a store that can Stat.
type statCountingStore struct {
	*storagetest.MemoryStore
	gets int
}

func (s *statCountingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

func TestPutMetadataForSourceChecksTheRecordedSource(t *testing.T) {
	source := []byte("compressed source bytes")
	sum := storage.SHA256Hex(source)
	cases := []struct {
		name    string
		stored  []byte // nil: missing
		sha     string
		size    int
		wantErr error
	}{
		{"matching", source, sum, len(source), nil},
		{"size unknown", source, sum, 0, nil},
		{"missing", nil, sum, len(source), storage.ErrNotFound},
		{"different bytes", []byte("other bytes"), sum, len(source), storage.ErrChecksumMismatch},
		{"same size, different bytes", []byte(strings.Repeat("x", len(source))), sum, len(source), storage.ErrChecksumMismatch},
		{"different size", source, sum, len(source) + 1, storage.ErrChecksumMismatch},
	}
	for _, statter := range []bool{true, false} {
		for _, test := range cases {
			t.Run(map[bool]string{true: "stat ", false: "get "}[statter]+test.name, func(t *testing.T) {
				memory := storagetest.NewMemoryStore()
				var store storage.ObjectStore = &statCountingStore{MemoryStore: memory}
				if !statter {
					store = &getOnlyStore{inner: memory}
				}
				if test.stored != nil {
					if err := memory.Put(context.Background(), "source", test.stored); err != nil {
						t.Fatal(err)
					}
				}
				err := storage.PutMetadataForSourceIndexed(context.Background(), store, "source", test.sha, test.size, "metadata.json", []byte(`{}`), storage.RetryPolicy{MaxAttempts: 1}, nil)
				if !errors.Is(err, test.wantErr) || (test.wantErr == nil) != (err == nil) {
					t.Fatalf("err = %v, want %v", err, test.wantErr)
				}
				_, getErr := memory.Get(context.Background(), "metadata.json")
				if published := getErr == nil; published != (test.wantErr == nil) {
					t.Fatalf("metadata published = %v with err %v", published, err)
				}
				if counted, ok := store.(*statCountingStore); ok && counted.gets != 0 {
					t.Fatalf("a store that reports the digest was read %d times", counted.gets)
				}
			})
		}
	}
}

func TestIndexedPublicationAbortsBeforeMetadataWhenIndexWriteFails(t *testing.T) {
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	source := []byte("source bytes")
	metadata := []byte("metadata bytes")
	sum := storage.SHA256Hex(source)
	indexErr := errors.New("index unavailable")
	callback := func() error {
		if got, err := store.Get(ctx, "source"); err != nil || string(got) != string(source) {
			t.Fatalf("source not verified before index callback: %q, %v", got, err)
		}
		return indexErr
	}
	for _, publish := range []struct {
		name string
		run  func() error
	}{
		{"source and metadata", func() error {
			return storage.PutSourceThenMetadataIndexed(ctx, store, "source", "metadata", source, metadata, storage.RetryPolicy{MaxAttempts: 1}, callback)
		}},
		{"metadata only", func() error {
			return storage.PutMetadataForSourceIndexed(ctx, store, "source", sum, len(source), "metadata", metadata, storage.RetryPolicy{MaxAttempts: 1}, callback)
		}},
	} {
		t.Run(publish.name, func(t *testing.T) {
			if err := publish.run(); !errors.Is(err, indexErr) {
				t.Fatalf("error = %v, want index failure", err)
			}
			if _, err := store.Get(ctx, "metadata"); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("metadata published after index failure: %v", err)
			}
		})
	}
}

func TestVerifyAccessWithoutObjectKeyerNotesRelativeKey(t *testing.T) {
	store := failingDeleteStore{storagetest.NewMemoryStore()}
	err := storage.VerifyAccess(context.Background(), store)
	if err == nil {
		t.Fatal("expected cleanup failure")
	}
	if !strings.Contains(err.Error(), `.setup-test/`) || !strings.Contains(err.Error(), "relative to the configured prefix") {
		t.Fatalf("cleanup error should name the relative key and note the prefix: %v", err)
	}
}

type failingDeleteStore struct{ *storagetest.MemoryStore }

func (failingDeleteStore) Delete(context.Context, string) error {
	return errors.New("delete denied")
}

func TestVerifyAccessUsesUniqueObjectAndCleansUp(t *testing.T) {
	store := storagetest.NewMemoryStore()
	if err := storage.VerifyAccess(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("setup object was not removed: %#v", objects)
	}
}

func TestSourceFirstPublicationReusesVerifiedSource(t *testing.T) {
	store := storagetest.NewMemoryStore()
	source := []byte(`{"schema_version":1}`)
	metadata := []byte(`{"source":"source.abc"}`)
	key := "sessions/codex/id/source." + storage.SHA256Hex(source) + ".jsonl.gz"
	if err := storage.PutSourceThenMetadataIndexed(context.Background(), store, key, "sessions/codex/id/metadata.json", source, metadata, storage.RetryPolicy{MaxAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "sessions/codex/id/metadata.json", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSourceThenMetadataIndexed(context.Background(), store, key, "sessions/codex/id/metadata.json", source, metadata, storage.RetryPolicy{MaxAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "sessions/codex/id/metadata.json")
	if err != nil || string(got) != string(metadata) {
		t.Fatalf("metadata = %q, err = %v", got, err)
	}
}

type flakyStore struct {
	*storagetest.MemoryStore
	mu       sync.Mutex
	failPuts int
}

func (s *flakyStore) Put(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPuts > 0 {
		s.failPuts--
		return io.ErrUnexpectedEOF
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestSourcePublicationRetriesAndPublishesMetadataLast(t *testing.T) {
	store := &flakyStore{MemoryStore: storagetest.NewMemoryStore(), failPuts: 2}
	err := storage.PutSourceThenMetadataIndexed(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), storage.RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil || len(objects) != 2 {
		t.Fatalf("objects = %#v, err = %v", objects, err)
	}
}

// Setup fails, naming the permissions page, when a missing object does not
// read as missing: the first publication of every session would fail the
// same way.
func TestVerifyAccessRequiresAMissingObjectToReadAsNotFound(t *testing.T) {
	for name, store := range map[string]storage.ObjectStore{
		"denied":        deniedAfterDeleteStore{storagetest.NewMemoryStore()},
		"still present": keepingDeleteStore{storagetest.NewMemoryStore()},
	} {
		err := storage.VerifyAccess(context.Background(), store)
		if err == nil {
			t.Fatalf("%s: storage.VerifyAccess succeeded", name)
		}
		if !strings.Contains(err.Error(), "read after delete") {
			t.Errorf("%s: error = %v, want the read-after-delete check named", name, err)
		}
	}
	err := storage.VerifyAccess(context.Background(), deniedAfterDeleteStore{storagetest.NewMemoryStore()})
	if !strings.Contains(err.Error(), "bucket-permissions.md") {
		t.Errorf("error = %v, want it to point at the permissions page", err)
	}
}

// deniedAfterDeleteStore answers a read of a deleted key with a permission
// error instead of storage.ErrNotFound.
type deniedAfterDeleteStore struct{ *storagetest.MemoryStore }

func (s deniedAfterDeleteStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := s.MemoryStore.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, responseError(http.StatusForbidden, errors.New("AccessDenied"))
	}
	return data, err
}

// keepingDeleteStore reports success from Delete without deleting.
type keepingDeleteStore struct{ *storagetest.MemoryStore }

func (keepingDeleteStore) Delete(context.Context, string) error { return nil }

type countingGetStore struct {
	*storagetest.MemoryStore
	gets  int
	stats int
	puts  int
}

func (s *countingGetStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

// GetLimited counts the same source read when verification uses its bound.
func (s *countingGetStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.gets++
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func (s *countingGetStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.stats++
	return s.MemoryStore.Stat(ctx, key)
}

func (s *countingGetStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts++
	return s.MemoryStore.Put(ctx, key, data)
}

// An existing source whose bytes differ will differ on every attempt.
func TestSourcePublicationDoesNotRetryChecksumMismatch(t *testing.T) {
	store := &countingGetStore{MemoryStore: storagetest.NewMemoryStore()}
	if err := store.Put(context.Background(), "source.hash", []byte("other bytes")); err != nil {
		t.Fatal(err)
	}
	err := storage.PutSourceThenMetadataIndexed(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), storage.RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond}, nil)
	if !errors.Is(err, storage.ErrChecksumMismatch) || store.gets+store.stats != 1 {
		t.Fatalf("gets = %d, stats = %d, err = %v", store.gets, store.stats, err)
	}
}

// A store that reports each object's SHA-256 verifies a publication with
// Stat alone: publishing a large source costs its upload, not a download of
// it as well, and republishing metadata over a source already stored costs
// neither.
func TestSourcePublicationVerifiesWithoutDownloading(t *testing.T) {
	store := &countingGetStore{MemoryStore: storagetest.NewMemoryStore()}
	source := []byte("source bytes")
	key := "sessions/codex/id/source." + storage.SHA256Hex(source) + ".jsonl.gz"
	for range 2 {
		if err := storage.PutSourceThenMetadataIndexed(context.Background(), store, key, "sessions/codex/id/metadata.json", source, []byte("metadata"), storage.RetryPolicy{MaxAttempts: 1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if store.gets != 0 {
		t.Errorf("gets = %d, want none: Stat reports the checksum", store.gets)
	}
	// One source upload, then two metadata uploads.
	if store.puts != 3 {
		t.Errorf("puts = %d, want the source uploaded once", store.puts)
	}
	// Before and after the first upload, and before the second.
	if store.stats != 3 {
		t.Errorf("stats = %d, want 3", store.stats)
	}
}

// noChecksumStore describes objects without their SHA-256, as a store does
// for an object uploaded without a checksum or in parts.
type noChecksumStore struct{ *countingGetStore }

func (s noChecksumStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.countingGetStore.Stat(ctx, key)
	info.SHA256 = ""
	return info, err
}

// corruptingStore stores other bytes than it was given, with a checksum of
// what it stored (or none, when hideChecksum is set).
type corruptingStore struct {
	*storagetest.MemoryStore
	hideChecksum bool
}

func (s corruptingStore) Put(ctx context.Context, key string, data []byte) error {
	return s.MemoryStore.Put(ctx, key, append([]byte("x"), data...))
}

func (s corruptingStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	if s.hideChecksum {
		info.SHA256 = ""
	}
	return info, err
}

// Without a checksum from Stat, the source is read back and hashed, as
// before; either way a source stored with other bytes than were uploaded
// fails the publication, and no metadata points at it.
func TestSourcePublicationReadsBackWhenStoreReportsNoChecksum(t *testing.T) {
	store := noChecksumStore{&countingGetStore{MemoryStore: storagetest.NewMemoryStore()}}
	if err := storage.PutSourceThenMetadataIndexed(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), storage.RetryPolicy{MaxAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if store.gets != 1 {
		t.Errorf("gets = %d, want the uploaded source read back once", store.gets)
	}
	for _, hide := range []bool{false, true} {
		store := corruptingStore{MemoryStore: storagetest.NewMemoryStore(), hideChecksum: hide}
		err := storage.PutSourceThenMetadataIndexed(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), storage.RetryPolicy{MaxAttempts: 1}, nil)
		if !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Errorf("hide checksum %t: err = %v, want a checksum mismatch", hide, err)
		}
		if _, err := store.Get(context.Background(), "metadata.json"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("hide checksum %t: metadata was published over a bad source (err = %v)", hide, err)
		}
	}
}
