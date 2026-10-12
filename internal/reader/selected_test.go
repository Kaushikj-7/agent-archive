package reader

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type selectedGetterFunc func(context.Context, string) ([]byte, string, error)

func (f selectedGetterFunc) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	return f(ctx, key)
}

func selectedFixture(t *testing.T, count int) (*storagetest.MemoryStore, []listingindex.Revision) {
	t.Helper()
	store := storagetest.NewMemoryStore()
	revisions := make([]listingindex.Revision, count)
	for i := range count {
		key := putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*time.Minute))
		data, etag, err := store.GetVersioned(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		revisions[i], err = listingindex.NewRevision(key, data, etag)
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, revisions
}

func TestSelectedReadsOverlapAndWarmCacheAvoidsGETs(t *testing.T) {
	store, revisions := selectedFixture(t, 50)
	measured := storagetest.NewMeasuredStore(store, time.Millisecond)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	opts := ListOptions{Cache: cache, BodyRead: func(key string, _ bool) { observed = append(observed, key) }}
	reads := readSelected(context.Background(), measured, revisions, opts)
	want := make([]string, len(revisions))
	for i, r := range revisions {
		want[i] = r.MetadataKey
		if reads[i].Err != nil || reads[i].Cached {
			t.Fatalf("cold slot %d: %+v", i, reads[i])
		}
	}
	if !reflect.DeepEqual(observed, want) {
		t.Fatal("observer order differs from selection")
	}
	metrics := measured.Metrics()
	if metrics.Gets != 50 || metrics.PeakReads < 2 || metrics.PeakReads > 8 {
		t.Fatalf("cold reads: %+v", metrics)
	}
	reads = readSelected(context.Background(), measured, revisions, opts)
	for i, read := range reads {
		if read.Err != nil || !read.Cached {
			t.Fatalf("warm slot %d: %+v", i, read)
		}
	}
	if measured.Metrics().Gets != 50 {
		t.Fatal("warm cache made remote GETs")
	}
}

func TestSelectedReadsReportProgressBeforeJoinAndFromCache(t *testing.T) {
	store, revisions := selectedFixture(t, 2)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, warm := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			var mu sync.Mutex
			var seen []int
			opts := ListOptions{Cache: cache, Progress: func(done, total int) {
				mu.Lock()
				defer mu.Unlock()
				if total != len(revisions) {
					t.Errorf("progress total=%d want=%d", total, len(revisions))
				}
				seen = append(seen, done)
			}}
			getter := selectedGetterFunc(func(ctx context.Context, key string) ([]byte, string, error) {
				if key == revisions[1].MetadataKey {
					<-release
				}
				return store.GetVersioned(ctx, key)
			})
			done := make(chan []selectedRead)
			go func() { done <- readSelected(t.Context(), getter, revisions, opts) }()
			synctest.Wait()
			mu.Lock()
			count := len(seen)
			mu.Unlock()
			if !warm && count != 1 {
				t.Errorf("progress before blocked read finishes=%d want=1", count)
			}
			close(release)
			reads := <-done
			for _, read := range reads {
				if read.Err != nil || read.Cached != warm {
					t.Fatalf("warm=%v read=%+v", warm, read)
				}
			}
			slices.Sort(seen)
			if !reflect.DeepEqual(seen, []int{1, 2}) {
				t.Fatalf("progress=%v want=[1 2]", seen)
			}
		})
	}
}

func TestSelectedProgressAndBodyObserversAreSerial(t *testing.T) {
	store, revisions := selectedFixture(t, 20)
	synctest.Test(t, func(t *testing.T) {
		var progress []int
		var bodies []string
		reads := readSelected(t.Context(), store, revisions, ListOptions{
			Progress: func(done, total int) {
				if total != len(revisions) {
					t.Errorf("progress total=%d want=%d", total, len(revisions))
				}
				// Let another callback start if observers wrongly run on workers.
				if done == 1 {
					time.Sleep(time.Millisecond)
				}
				progress = append(progress, done)
			},
			BodyRead: func(key string, _ bool) {
				if len(progress) != len(revisions) {
					t.Error("body observer ran before progress callbacks finished")
				}
				bodies = append(bodies, key)
			},
		})
		for i, read := range reads {
			if read.Err != nil || progress[i] != i+1 || bodies[i] != revisions[i].MetadataKey {
				t.Fatalf("observer order at %d: progress=%v bodies=%v read=%+v", i, progress, bodies, read)
			}
		}
	})
}

func TestSelectedFailureStopsDispatchAndJoinsEarlierErrors(t *testing.T) {
	_, revisions := selectedFixture(t, 50)
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int64
		earlier, later := errors.New("earlier revision failure"), errors.New("later revision failure")
		getter := selectedGetterFunc(func(_ context.Context, key string) ([]byte, string, error) {
			calls.Add(1)
			if key == revisions[1].MetadataKey {
				return nil, "", later
			}
			<-release
			return nil, "", earlier
		})
		done := make(chan []selectedRead)
		go func() { done <- readSelected(context.Background(), getter, revisions, ListOptions{}) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("returned before earlier in-flight reads joined")
		default:
		}
		if calls.Load() > 8 {
			t.Fatalf("dispatched after failure: %d", calls.Load())
		}
		close(release)
		reads := <-done
		if len(reads) < 2 || !errors.Is(reads[0].Err, earlier) || !errors.Is(reads[1].Err, later) {
			t.Fatalf("errors lost selection order: %+v", reads)
		}
	})
}

func TestSelectedCancellationStopsDispatchAndJoins(t *testing.T) {
	_, revisions := selectedFixture(t, 50)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int64
		getter := selectedGetterFunc(func(ctx context.Context, _ string) ([]byte, string, error) {
			calls.Add(1)
			<-ctx.Done()
			return nil, "", ctx.Err()
		})
		done := make(chan []selectedRead)
		go func() { done <- readSelected(ctx, getter, revisions, ListOptions{}) }()
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("active reads=%d", calls.Load())
		}
		cancel()
		reads := <-done
		if len(reads) != 8 || calls.Load() != 8 {
			t.Fatalf("dispatched after cancellation: slots=%d calls=%d", len(reads), calls.Load())
		}
		for _, read := range reads {
			if !errors.Is(read.Err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", read.Err)
			}
		}
		if reads := readSelected(ctx, getter, revisions, ListOptions{}); len(reads) != 0 {
			t.Fatal("canceled query dispatched reads")
		}
	})
}

type invalidRevisionEvidence string

const (
	missingRevisionEvidence   invalidRevisionEvidence = "missing"
	validatorRevisionEvidence invalidRevisionEvidence = "validator"
	hashRevisionEvidence      invalidRevisionEvidence = "hash"
	summaryRevisionEvidence   invalidRevisionEvidence = "summary"
	schemaRevisionEvidence    invalidRevisionEvidence = "schema"
)

func TestSelectedReadsRejectInvalidRevisionEvidence(t *testing.T) {
	store, original := selectedFixture(t, 1)
	data, etag, err := store.GetVersioned(context.Background(), original[0].MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []invalidRevisionEvidence{missingRevisionEvidence, validatorRevisionEvidence, hashRevisionEvidence, summaryRevisionEvidence, schemaRevisionEvidence} {
		t.Run(string(kind), func(t *testing.T) {
			revisions := append([]listingindex.Revision(nil), original...)
			response, validator := data, etag
			var responseErr error
			switch kind {
			case missingRevisionEvidence:
				responseErr = errors.New("missing selected body")
			case validatorRevisionEvidence:
				validator = "changed"
			case hashRevisionEvidence:
				response = []byte("corrupt")
			case summaryRevisionEvidence:
				revisions[0].ProjectID = "wrong-project"
			case schemaRevisionEvidence:
				response = []byte(`{"schema_version":999}`)
				revisions[0].Hash = storage.SHA256Hex(response)
			}
			getter := selectedGetterFunc(func(context.Context, string) ([]byte, string, error) { return response, validator, responseErr })
			reads := readSelected(context.Background(), getter, revisions, ListOptions{})
			if len(reads) != 1 || reads[0].Err == nil {
				t.Fatalf("accepted %s evidence: %+v", kind, reads)
			}
		})
	}
}
