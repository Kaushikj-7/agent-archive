package reader

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type gatedHeaderStore struct {
	*storagetest.MemoryStore
	started    chan string
	release    chan struct{}
	failPrefix string
	failure    error
	finished   chan string
}

func (s *gatedHeaderStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.started <- prefix
	defer func() { s.finished <- prefix }()
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.failPrefix != "" {
		if prefix == s.failPrefix {
			return nil, s.failure
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.MemoryStore.List(ctx, prefix)
}

func TestHeaderDiscoveryOverlapsAndNarrowsHarness(t *testing.T) {
	t.Parallel()
	store := &gatedHeaderStore{MemoryStore: storagetest.NewMemoryStore(), started: make(chan string, 3), release: make(chan struct{}), finished: make(chan string, 3)}
	done := make(chan error, 1)
	go func() {
		_, err := discoverHeaders(t.Context(), store, "sessions", Filter{Harness: "codex"}, nil)
		done <- err
	}()
	prefixes := make([]string, 3)
	for i := range prefixes {
		prefixes[i] = <-store.started
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sort.Strings(prefixes)
	want := []string{listingindex.V2Prefix, listingindex.V3Prefix + "codex/", "sessions/codex/"}
	sort.Strings(want)
	if !reflect.DeepEqual(prefixes, want) || len(store.finished) != 3 {
		t.Fatalf("prefixes=%v finished=%d", prefixes, len(store.finished))
	}
}

func TestHeaderDiscoveryCancelsAndJoinsSiblings(t *testing.T) {
	t.Parallel()
	for _, providerFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "provider"}[providerFailure], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("provider unavailable")
			store := &gatedHeaderStore{MemoryStore: storagetest.NewMemoryStore(), started: make(chan string, 3), release: make(chan struct{}), finished: make(chan string, 3), failPrefix: listingindex.V2Prefix, failure: failure}
			done := make(chan error, 1)
			go func() {
				_, err := discoverHeaders(ctx, store, "sessions", Filter{}, nil)
				done <- err
			}()
			for range 3 {
				<-store.started
			}
			want := error(context.Canceled)
			if providerFailure {
				want = failure
				close(store.release)
			} else {
				cancel()
			}
			if err := <-done; !errors.Is(err, want) || len(store.finished) != 3 {
				t.Fatalf("error=%v finished=%d", err, len(store.finished))
			}
		})
	}
}

func TestIndexFallbackReusesCanonicalHeadersAndMatchesExhaustive(t *testing.T) {
	t.Parallel()
	for _, damaged := range []bool{false, true} {
		t.Run(map[bool]string{false: "coverage-gap", true: "damaged-hint"}[damaged], func(t *testing.T) {
			t.Parallel()
			store := newCountingStore()
			putSession(t, store, "codex", "first", baseTime)
			putSession(t, store, "claude-code", "second", baseTime.Add(1))
			if damaged {
				if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
					t.Fatal(err)
				}
				if err := store.Put(t.Context(), listingindex.V3Prefix+"damaged", nil); err != nil {
					t.Fatal(err)
				}
			}
			oracle, err := ListRecent(t.Context(), store, "sessions", Filter{}, 0, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			store.reset()
			scans := 0
			got, err := ListRecent(t.Context(), store, "sessions", Filter{}, 50, ListOptions{CompatibilityScan: func(string) { scans++ }})
			if err != nil || !reflect.DeepEqual(got, oracle) || scans != 1 {
				t.Fatalf("got=%+v oracle=%+v scans=%d err=%v", got, oracle, scans, err)
			}
			lists, _ := store.counts()
			canonical := 0
			for _, prefix := range lists {
				if prefix == "sessions" {
					canonical++
				}
			}
			if canonical != 1 || len(lists) != 3 {
				t.Fatalf("lists=%v", lists)
			}
		})
	}
}

func TestHarnessHeaderDiscoveryIgnoresOtherV3Harnesses(t *testing.T) {
	t.Parallel()
	store := newCountingStore()
	putSession(t, store, "codex", "selected", baseTime)
	if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), listingindex.V3Prefix+"claude-code/damaged", nil); err != nil {
		t.Fatal(err)
	}
	scanned := false
	got, err := ListRecent(t.Context(), store, "sessions", Filter{Harness: "codex"}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }})
	if err != nil || scanned || len(got.Sessions) != 1 || got.Sessions[0].SessionID != "selected" {
		t.Fatalf("sessions=%v fallback=%v error=%v", got.Sessions, scanned, err)
	}
}

func TestHeaderDiscoveryUsesCompleteCachePlannedRanges(t *testing.T) {
	t.Parallel()
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore()}
	for i := range 2 * rangeSidecars {
		putSession(t, store, "codex", strconv.Itoa(i), baseTime)
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListMetadataWithOptions(t.Context(), store, "sessions", Filter{}, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "claude-code", "new", baseTime)
	store.lists = 0
	headers, err := discoverHeaders(t.Context(), store, "sessions", Filter{}, cache)
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := store.MemoryStore.List(t.Context(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(headers.Canonical, oracle) || store.lists != 2 || len(store.ranges) != 2 {
		t.Fatalf("canonical=%d oracle=%d lists=%d ranges=%d", len(headers.Canonical), len(oracle), store.lists, len(store.ranges))
	}
}

type failingAuxiliaryHeaderStore struct {
	*storagetest.MemoryStore
	canonicalDone chan struct{}
	canonicalErr  error
	cancelCaller  context.CancelFunc
	failure       error
}

func (s *failingAuxiliaryHeaderStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if prefix == "sessions" {
		objects, err := s.MemoryStore.List(ctx, prefix)
		close(s.canonicalDone)
		if err != nil {
			return nil, err
		}
		return objects, s.canonicalErr
	}
	<-s.canonicalDone
	if prefix == listingindex.V2Prefix && s.canonicalErr == nil {
		if s.cancelCaller == nil {
			return nil, s.failure
		}
		s.cancelCaller()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type headerFailureMode string

const (
	headerAuxiliaryFailure      headerFailureMode = "auxiliary-failure"
	headerEmptyAuxiliaryFailure headerFailureMode = "empty-auxiliary-failure"
	headerCallerCancellation    headerFailureMode = "caller-cancellation"
	headerCanonicalPartial      headerFailureMode = "canonical-partial"
)

// Complete canonical discovery proves deletion even if unrelated auxiliary
// discovery fails. A partial canonical response cannot prove absence.
func TestHeaderDiscoveryEvictsProvenDeletionDespiteAuxiliaryFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []headerFailureMode{headerAuxiliaryFailure, headerEmptyAuxiliaryFailure, headerCallerCancellation, headerCanonicalPartial} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			mem := storagetest.NewMemoryStore()
			deleted := putSession(t, mem, "codex", "deleted", baseTime)
			present := putSession(t, mem, "codex", "present", baseTime)
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ListMetadataWithOptions(t.Context(), mem, "sessions", Filter{}, ListOptions{Cache: cache}); err != nil {
				t.Fatal(err)
			}
			if err := mem.Delete(t.Context(), deleted); err != nil {
				t.Fatal(err)
			}
			if mode == headerEmptyAuxiliaryFailure {
				if err := mem.Delete(t.Context(), present); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("header provider unavailable")
			store := &failingAuxiliaryHeaderStore{MemoryStore: mem, canonicalDone: make(chan struct{}), failure: failure}
			want := error(failure)
			switch mode {
			case headerAuxiliaryFailure, headerEmptyAuxiliaryFailure:
				// Complete canonical discovery precedes the provider failure.
			case headerCallerCancellation:
				store.cancelCaller, want = cancel, context.Canceled
			case headerCanonicalPartial:
				store.canonicalErr = failure
			}
			if _, err := discoverHeaders(ctx, store, "sessions", Filter{}, cache); !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
			retained := false
			for _, key := range cache.keys("sessions") {
				retained = retained || key == deleted
			}
			if retained != (mode == headerCanonicalPartial) {
				t.Fatalf("deleted cache entry retained=%v in %s", retained, mode)
			}
		})
	}
}
