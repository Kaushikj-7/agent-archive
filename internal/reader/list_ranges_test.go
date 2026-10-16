package reader

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func keysOf(objects []storage.Object) []string {
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = object.Key
	}
	return keys
}

func TestPlanRangesSplitsEveryRangeSidecarsKnownKeys(t *testing.T) {
	known := func(n int) []string {
		keys := make([]string, n)
		for i := range keys {
			// Reverse order: planning must not depend on the order the
			// cache directory lists in.
			keys[i] = fmt.Sprintf("sessions/claude/%04d/metadata.json", n-i)
		}
		return keys
	}
	if bounds := planRanges(known(2*rangeSidecars - 1)); bounds != nil {
		t.Fatalf("under two ranges' worth planned %d boundaries, want none", len(bounds))
	}
	bounds := planRanges(known(2 * rangeSidecars))
	if want := []string{fmt.Sprintf("sessions/claude/%04d/metadata.json", rangeSidecars)}; !slices.Equal(bounds, want) {
		t.Fatalf("planRanges(%d keys) = %v, want %v", 2*rangeSidecars, bounds, want)
	}
	// 662 sessions, the archive this was measured on: four ranges of at most
	// rangeSidecars known sidecars.
	bounds = planRanges(known(662))
	if len(bounds) != 3 || !sort.StringsAreSorted(bounds) {
		t.Fatalf("planRanges(662 keys) = %v, want 3 sorted boundaries", bounds)
	}
}

// randomKey draws keys shaped like the archive's and unlike it: sidecars and
// sources of hex IDs, other harnesses, IDs that are not hex, and names that
// sort before and after every "sessions/<harness>/" key.
func randomKey(r *rand.Rand) string {
	harnesses := []string{"claude", "codex", "cursor", "aider", "Zed", "a"}
	id := fmt.Sprintf("%032x", r.Uint64())[:4+r.IntN(28)]
	if r.IntN(5) == 0 {
		id = []string{"file-abc", "-dash", "_under", "Z", "0", "~tilde"}[r.IntN(6)]
	}
	harness := harnesses[r.IntN(len(harnesses))]
	switch r.IntN(4) {
	case 0:
		return "sessions/" + harness + "/" + id + "/source." + id + ".jsonl.gz"
	case 1:
		return "sessions/" + harness
	default:
		return "sessions/" + harness + "/" + id + "/metadata.json"
	}
}

// Whatever the keys and wherever the boundaries fall (between keys, on a
// key, before or after all of them), the joined ranges are exactly the plain
// listing, in the same order.
func TestListRangesEqualsListForAnyKeysAndBoundaries(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	ctx := context.Background()
	for trial := range 300 {
		store := storagetest.NewMemoryStore()
		var keys []string
		for range r.IntN(60) {
			key := randomKey(r)
			keys = append(keys, key)
			if err := store.Put(ctx, key, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		// Unrelated objects outside the listing prefix never appear.
		if err := store.Put(ctx, "listing/v1/zzz", []byte("x")); err != nil {
			t.Fatal(err)
		}
		candidates := append(slices.Clone(keys), "", "a", "sessions/", "sessions/claude/", "sessions/claude/8", "sessions/zzzz", "~")
		var bounds []string
		for range 1 + r.IntN(8) {
			if b := candidates[r.IntN(len(candidates))]; b != "" {
				bounds = append(bounds, b)
			}
		}
		sort.Strings(bounds)
		bounds = slices.Compact(bounds)
		if len(bounds) == 0 {
			continue
		}
		want, err := store.List(ctx, "sessions")
		if err != nil {
			t.Fatal(err)
		}
		got, err := listRanges(ctx, store, "sessions", bounds, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("trial %d, bounds %q:\nranges %q\nlist   %q", trial, bounds, keysOf(got), keysOf(want))
		}
	}
}

// rangeRecorder is a RangeLister that records the ranges asked for and how
// many ran at once, and can hold every range until released or fail one.
type rangeRecorder struct {
	*storagetest.MemoryStore
	hold      chan struct{}
	ignoreCtx bool   // held ranges wait for hold alone, like a store that never observes cancellation
	echoErr   error  // what a held range returns when cancelled, instead of ctx.Err()
	failAt    string // the through bound of a range that fails at once
	failErr   error

	mu          sync.Mutex
	ranges      [][2]string
	lists       int
	inFlight    int
	maxInFlight int
}

func (s *rangeRecorder) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.MemoryStore.List(ctx, prefix)
}

func (s *rangeRecorder) ListRange(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	s.mu.Lock()
	s.ranges = append(s.ranges, [2]string{after, through})
	s.inFlight++
	s.maxInFlight = max(s.maxInFlight, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if s.failErr != nil && through == s.failAt {
		return nil, s.failErr
	}
	switch {
	case s.hold != nil && s.ignoreCtx:
		<-s.hold
		// Succeed whatever happened to ctx meanwhile.
		return s.MemoryStore.ListRange(context.WithoutCancel(ctx), prefix, after, through)
	case s.hold != nil:
		select {
		case <-s.hold:
		case <-ctx.Done():
			if s.echoErr != nil {
				return nil, s.echoErr
			}
			return nil, ctx.Err()
		}
	}
	return s.MemoryStore.ListRange(ctx, prefix, after, through)
}

func boundsN(n int) []string {
	bounds := make([]string, n)
	for i := range bounds {
		bounds[i] = fmt.Sprintf("sessions/claude/%04d/metadata.json", i)
	}
	return bounds
}

func TestListRangesBoundsConcurrency(t *testing.T) {
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore(), hold: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := listRanges(context.Background(), store, "sessions", boundsN(3*rangeConcurrency), nil)
		done <- err
	}()
	// Wait for the first wave to fill every slot, then release them all.
	waitInFlight(t, store, rangeConcurrency)
	close(store.hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if store.maxInFlight != rangeConcurrency || len(store.ranges) != 3*rangeConcurrency+1 {
		t.Fatalf("max in flight %d (want %d), ranges %d (want %d)", store.maxInFlight, rangeConcurrency, len(store.ranges), 3*rangeConcurrency+1)
	}
}

// One failing range cancels the others, and its error, not the cancellation
// it caused, is what the listing returns. Nothing is still running after.
func TestListRangesReturnsTheFailureNotTheCancellationItCaused(t *testing.T) {
	failure := errors.New("403 forbidden")
	bounds := boundsN(5)
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore(), hold: make(chan struct{}), failAt: bounds[3], failErr: failure}
	_, err := listRanges(context.Background(), store, "sessions", bounds, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("listRanges error = %v, want the range's own failure", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.inFlight != 0 {
		t.Fatalf("%d ranges still running after listRanges returned", store.inFlight)
	}
}

// A cancelled range can fail with an error that doesn't wrap
// context.Canceled (a body read cut off mid-response). Arriving after the
// real failure, it must not replace it, even from a range lower in key order.
func TestListRangesReturnsTheFirstFailureNotALaterEcho(t *testing.T) {
	failure := errors.New("403 forbidden")
	bounds := boundsN(5)
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore(), hold: make(chan struct{}),
		echoErr: errors.New("connection reset"), failAt: bounds[4], failErr: failure}
	if _, err := listRanges(context.Background(), store, "sessions", bounds, nil); !errors.Is(err, failure) {
		t.Fatalf("listRanges error = %v, want the first failure", err)
	}
}

// waitInFlight waits until n ranges are running.
func waitInFlight(t *testing.T, store *rangeRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		inFlight := store.inFlight
		store.mu.Unlock()
		if inFlight == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d ranges in flight, want %d", inFlight, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A store that ignores cancellation lets every range already started
// succeed; the ranges never started still make the listing incomplete, so
// it must fail rather than return part of the archive (which would also let
// cache eviction forget sessions that still exist).
func TestListRangesFailsWhenCancelledBeforeEveryRangeStarted(t *testing.T) {
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore(), hold: make(chan struct{}), ignoreCtx: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := listRanges(ctx, store, "sessions", boundsN(3*rangeConcurrency), nil)
		done <- err
	}()
	waitInFlight(t, store, rangeConcurrency)
	cancel()
	close(store.hold)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("listRanges error = %v, want context.Canceled", err)
	}
}

func TestListRangesReportsTheCallersCancellation(t *testing.T) {
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore(), hold: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := listRanges(ctx, store, "sessions", boundsN(3*rangeConcurrency), nil)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("listRanges error = %v, want context.Canceled", err)
	}
}

// End to end: once the metadata cache knows enough sidecars, a listing is
// made of ranges, and it returns exactly what the single listing does,
// including sessions published since the cache was filled.
func TestListMetadataUsesCachePlannedRanges(t *testing.T) {
	ctx := context.Background()
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore()}
	for i := range 1100 {
		putSession(t, store, "claude", fmt.Sprintf("%032x", i*7919), baseTime.Add(time.Duration(i)*time.Minute))
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := ListMetadataWithOptions(ctx, store, "sessions", Filter{}, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if store.lists != 1 || len(store.ranges) != 0 {
		t.Fatalf("cold cache: %d lists and %d ranges, want one plain listing", store.lists, len(store.ranges))
	}
	// Sessions the cache has never seen, sorting before, between and after
	// the known ones, and under another harness.
	for i, id := range []string{"0000", "5555", "ffffffffffffffffffffffffffffffff"} {
		putSession(t, store, "claude", id, baseTime.Add(time.Duration(1000+i)*time.Minute))
	}
	putSession(t, store, "codex", "abcd", baseTime.Add(2000*time.Minute))
	second, err := ListMetadataWithOptions(ctx, store, "sessions", Filter{}, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if store.lists != 1 || len(store.ranges) != 3 {
		t.Fatalf("warm cache: %d lists and %d ranges, want 3 key-balanced ranges and no plain listing", store.lists, len(store.ranges))
	}
	plain, err := ListMetadata(ctx, store, "sessions", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first)+4 || len(second) != len(plain) {
		t.Fatalf("ranged listing found %d sessions, plain %d, cold %d", len(second), len(plain), len(first))
	}
	for i := range plain {
		if second[i].SessionID != plain[i].SessionID {
			t.Fatalf("session %d: ranged %s, plain %s", i, second[i].SessionID, plain[i].SessionID)
		}
	}
}

// Without a cache (list --no-cache) nothing is known to plan from, so the
// listing is the single one it always was.
func TestListMetadataWithoutCacheListsOnce(t *testing.T) {
	store := &rangeRecorder{MemoryStore: storagetest.NewMemoryStore()}
	for i := range 2*rangeSidecars + 1 {
		putSession(t, store, "claude", fmt.Sprintf("%032x", i), baseTime)
	}
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if store.lists != 1 || len(store.ranges) != 0 {
		t.Fatalf("%d lists and %d ranges, want one plain listing", store.lists, len(store.ranges))
	}
}
