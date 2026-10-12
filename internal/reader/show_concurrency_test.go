package reader

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type gatedMetadataStore struct {
	storage.ObjectStore
	entered chan string
	release chan struct{}
	mu      sync.Mutex
	active  int
	peak    int
	calls   int
}

func (s *gatedMetadataStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.active++
	s.calls++
	s.peak = max(s.peak, s.active)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	s.entered <- key
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return s.ObjectStore.Get(ctx, key)
	}
}

func awaitMetadataReads(t *testing.T, entered <-chan string, count int) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for range count {
		select {
		case <-entered:
		case <-timer.C:
			t.Fatal("metadata workers did not overlap")
		}
	}
}

func TestLinkedMetadataReadsOverlapAndRetainEveryState(t *testing.T) {
	t.Parallel()
	parent, _, mem := fixture(t)
	for i := range 50 {
		parent.LinkedSessions = append(parent.LinkedSessions, archive.LinkedSessionReference{SessionID: "missing-" + strconv.Itoa(i), Relationship: "subagent", Status: archive.LinkedSessionPending})
	}
	parent.LinkedSessions = append(parent.LinkedSessions,
		archive.LinkedSessionReference{SessionID: "unavailable", Relationship: "subagent", Status: archive.LinkedSessionUnavailable},
		archive.LinkedSessionReference{SessionID: parent.SessionID, Relationship: "subagent", Status: archive.LinkedSessionPublished},
		archive.LinkedSessionReference{SessionID: "expired", Relationship: "subagent", Status: archive.LinkedSessionPublished},
	)
	want := make([]LinkedAvailability, len(parent.LinkedSessions))
	for i, link := range parent.LinkedSessions {
		want[i] = resolveLinkedSession(context.Background(), mem, parent, link)
	}
	store := &gatedMetadataStore{ObjectStore: mem, entered: make(chan string, 100), release: make(chan struct{})}
	done := make(chan []LinkedAvailability, 1)
	go func() { done <- ResolveLinkedSessions(context.Background(), store, parent) }()
	awaitMetadataReads(t, store.entered, 8)
	close(store.release)
	got := <-done
	if !reflect.DeepEqual(got, want) || store.peak != 8 || store.active != 0 || store.calls != 51 {
		t.Fatalf("results=%v want=%v peak=%d active=%d calls=%d", got, want, store.peak, store.active, store.calls)
	}
}

func TestLinkedMetadataCancellationJoinsAndStopsReads(t *testing.T) {
	t.Parallel()
	parent, _, mem := fixture(t)
	for i := range 50 {
		parent.LinkedSessions = append(parent.LinkedSessions, archive.LinkedSessionReference{SessionID: "child-" + strconv.Itoa(i), Relationship: "subagent", Status: archive.LinkedSessionPublished})
	}
	store := &gatedMetadataStore{ObjectStore: mem, entered: make(chan string, 100), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []LinkedAvailability, 1)
	go func() { done <- ResolveLinkedSessions(ctx, store, parent) }()
	awaitMetadataReads(t, store.entered, 8)
	cancel()
	got := <-done
	if store.active != 0 || store.calls != 8 || len(got) != 50 {
		t.Fatalf("active=%d calls=%d results=%d", store.active, store.calls, len(got))
	}
	for i, item := range got {
		if item.SessionID != parent.LinkedSessions[i].SessionID || item.State != LinkedStateLookupFailed {
			t.Fatalf("cancelled link %d: %+v", i, item)
		}
	}
}

func TestMetadataFinderOverlapsProbesAndRetainsBodies(t *testing.T) {
	t.Parallel()
	mem := newCountingStore()
	key := putSession(t, mem, "codex", "known", baseTime)
	store := &gatedMetadataStore{ObjectStore: mem, entered: make(chan string, 100), release: make(chan struct{})}
	type lookupResult struct {
		lookups []MetadataLookup
		err     error
	}
	done := make(chan lookupResult, 1)
	go func() {
		lookups, err := defaultFinder.FindMetadata(context.Background(), store, "sessions", "known")
		done <- lookupResult{lookups: lookups, err: err}
	}()
	awaitMetadataReads(t, store.entered, len(defaultFinder.harnesses))
	close(store.release)
	got := <-done
	if got.err != nil || len(got.lookups) != 1 || got.lookups[0].Key != key || got.lookups[0].Metadata.SessionID != "known" || store.calls != len(defaultFinder.harnesses) || store.peak != len(defaultFinder.harnesses) || store.active != 0 {
		t.Fatalf("result=%+v calls=%d peak=%d active=%d", got, store.calls, store.peak, store.active)
	}
}

func TestMetadataFinderErrorsKeepHarnessOrderAndKeyCompatibility(t *testing.T) {
	t.Parallel()
	store := newCountingStore()
	first := errors.New("first harness error")
	for i, harness := range defaultFinder.harnesses {
		key := "sessions/" + harness + "/failed/metadata.json"
		store.failGet[key] = errors.New("later harness error")
		if i == 0 {
			store.failGet[key] = first
		}
	}
	if _, err := defaultFinder.FindMetadata(context.Background(), store, "sessions", "failed"); !errors.Is(err, first) {
		t.Fatalf("error order changed: %v", err)
	}
	key := "sessions/codex/malformed/metadata.json"
	if err := store.Put(context.Background(), key, nil); err != nil {
		t.Fatal(err)
	}
	keys, err := defaultFinder.FindMetadataKeys(context.Background(), store, "sessions", "malformed")
	if err != nil || !reflect.DeepEqual(keys, []string{key}) {
		t.Fatalf("existence-only compatibility: %v %v", keys, err)
	}
	store.reset()
	lookups, err := defaultFinder.FindMetadata(context.Background(), store, "sessions", "malformed")
	_, gets := store.counts()
	if !errors.Is(err, ErrInvalidMetadata) || len(lookups) != 1 || len(gets) != len(defaultFinder.harnesses) {
		t.Fatalf("malformed body reread: %v %v %v", lookups, err, gets)
	}
}

func TestMetadataFinderCancellationBoundsAndJoinsProbes(t *testing.T) {
	t.Parallel()
	_, _, mem := fixture(t)
	finder := &MetadataFinder{}
	for i := range 20 {
		finder.harnesses = append(finder.harnesses, "harness-"+strconv.Itoa(i))
	}
	store := &gatedMetadataStore{ObjectStore: mem, entered: make(chan string, 100), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := finder.FindMetadata(ctx, store, "sessions", "known")
		done <- err
	}()
	awaitMetadataReads(t, store.entered, 8)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || store.calls != 8 || store.peak != 8 || store.active != 0 {
		t.Fatalf("error=%v calls=%d peak=%d active=%d", err, store.calls, store.peak, store.active)
	}
}

func TestSelectedMetadataStillRefreshesMissingSourceAndVerifiesOwnership(t *testing.T) {
	t.Parallel()
	metadata, wantBundle, mem := fixture(t)
	stale := metadata
	stale.SourceBundle.Key = "sessions/codex/session-1/source." + fakeSourceSHA + ".jsonl.gz"
	stale.SourceBundle.SHA256 = fakeSourceSHA
	store := newCountingStore()
	store.MemoryStore = mem
	got, bundle, err := RefreshAndLoadMetadata(context.Background(), store, "sessions/codex/session-1/metadata.json", stale, Limits{})
	gotMetadataJSON, _ := json.Marshal(got)
	wantMetadataJSON, _ := json.Marshal(metadata)
	gotBundleJSON, _ := json.Marshal(bundle)
	wantBundleJSON, _ := json.Marshal(wantBundle)
	if err != nil || string(gotMetadataJSON) != string(wantMetadataJSON) || string(gotBundleJSON) != string(wantBundleJSON) {
		t.Fatalf("source refresh changed: metadata=%+v bundle=%+v err=%v", got, bundle, err)
	}
	_, gets := store.counts()
	parentReads := 0
	for _, key := range gets {
		if key == "sessions/codex/session-1/metadata.json" {
			parentReads++
		}
	}
	if parentReads != 1 {
		t.Fatalf("refresh parent reads=%d gets=%v", parentReads, gets)
	}
	metadata.ParentSessionID = "wrong-parent"
	if _, _, err := RefreshAndLoadMetadata(context.Background(), store, "sessions/codex/session-1/metadata.json", metadata, Limits{}); err == nil {
		t.Fatal("selected metadata bypassed source ownership verification")
	}
}

func TestMetadataLookupUnknownFallbackAndAmbiguityKeepKeyOrder(t *testing.T) {
	t.Parallel()
	store := newCountingStore()
	unknown := putSession(t, store, "future-agent", "unknown", baseTime)
	lookups, err := defaultFinder.FindMetadata(context.Background(), store, "sessions", "unknown")
	if err != nil || len(lookups) != 1 || lookups[0].Key != unknown || lookups[0].Metadata.SessionID != "unknown" {
		t.Fatalf("unknown metadata lookup: %+v %v", lookups, err)
	}
	lists, gets := store.counts()
	if len(lists) != 1 || len(gets) != len(defaultFinder.harnesses)+1 {
		t.Fatalf("unknown fallback costs: lists=%v gets=%v", lists, gets)
	}
	putSession(t, store, "codex", "shared", baseTime)
	putSession(t, store, "claude", "shared", baseTime)
	store.reset()
	lookups, err = defaultFinder.FindMetadata(context.Background(), store, "sessions", "shared")
	if err != nil || len(lookups) != 2 || lookups[0].Key != "sessions/claude/shared/metadata.json" || lookups[1].Key != "sessions/codex/shared/metadata.json" {
		t.Fatalf("ambiguous key order changed: %+v %v", lookups, err)
	}
	lists, gets = store.counts()
	if len(lists) != 0 || len(gets) != len(defaultFinder.harnesses) {
		t.Fatalf("ambiguous probe costs: lists=%v gets=%v", lists, gets)
	}
}
