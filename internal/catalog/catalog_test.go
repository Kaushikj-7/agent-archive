package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

type qualifiedStore struct{ *storagetest.MemoryStore }

func (*qualifiedStore) CatalogAtomicQualification() error { return nil }

func fixture(t *testing.T) (*Writer, *qualifiedStore) {
	t.Helper()
	s := &qualifiedStore{storagetest.NewMemoryStore()}
	w, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	return w, s
}

func mutation(t *testing.T, w *Writer, id string) CatalogMutation {
	t.Helper()
	ctx := t.Context()
	source := []byte("synthetic source " + id)
	sourceKey := "sessions/claude/" + id + "/source." + storage.SHA256Hex(source) + ".jsonl.gz"
	if err := w.store.Put(ctx, sourceKey, source); err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{SchemaVersion: 1, SessionID: id, Harness: archive.Harness{Name: "claude"}, ProjectID: "project", CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: storage.SHA256Hex(source), CompressedBytes: len(source)}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := w.PutImmutable(ctx, KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	return CatalogMutation{ID: "mutation-" + id, SessionKey: "sessions/claude/" + id + "/metadata.json", Next: &CatalogEntry{Metadata: ref, Summary: metadata}}
}

func TestProviderGate(t *testing.T) {
	if _, err := New(storagetest.NewMemoryStore()); !errors.Is(err, storage.ErrAtomicCatalogUnqualified) {
		t.Fatal(err)
	}
}

func TestConcurrentCommitsAndBoundedTree(t *testing.T) {
	w, s := fixture(t)
	const count = 110
	mutations := make([]CatalogMutation, count)
	for i := range mutations {
		mutations[i] = mutation(t, w, fmt.Sprintf("session-%04d", i))
	}
	var group sync.WaitGroup
	errs := make(chan error, count)
	// Bounded simultaneous writers exercise both root split and CAS rebasing.
	for start := 0; start < count; start += 4 {
		for i := start; i < min(start+4, count); i++ {
			group.Go(func() { _, err := w.Commit(t.Context(), mutations[i]); errs <- err })
		}
		group.Wait()
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range mutations {
		e, revision, err := w.Find(t.Context(), m.SessionKey)
		if err != nil || e == nil || revision == "" {
			t.Fatalf("lost entry: %s %v", m.SessionKey, err)
		}
	}
	objects, err := s.List(t.Context(), "catalog-v4/nodes/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objects {
		if o.Size > maxNodeBytes {
			t.Fatal("oversized node")
		}
	}
}

func TestConflictABAAndReceiptAfterNewerPublication(t *testing.T) {
	w, _ := fixture(t)
	a := mutation(t, w, "same")
	rev, err := w.Commit(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.ID = "successor"
	b.ExpectedRevision = rev
	newRev, err := w.Commit(t.Context(), b)
	if err != nil || newRev == rev {
		t.Fatal(err)
	}
	got, err := w.Commit(t.Context(), a)
	if err != nil || got != rev {
		t.Fatalf("receipt: %s %v", got, err)
	}
	_, current, err := w.Find(t.Context(), a.SessionKey)
	if err != nil || current != newRev {
		t.Fatal("retry replaced successor")
	}
	stale := b
	stale.ID = "stale"
	if _, err = w.Commit(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	changed := a
	changed.ExpectedRevision = rev
	if _, err = w.Commit(t.Context(), changed); !errors.Is(err, ErrMutationReuse) {
		t.Fatal(err)
	}
	del := CatalogMutation{ID: "delete", SessionKey: a.SessionKey, ExpectedRevision: newRev}
	deleted, err := w.Commit(t.Context(), del)
	if err != nil {
		t.Fatal(err)
	}
	stale.ID = "aba"
	stale.ExpectedRevision = ""
	if _, err = w.Commit(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted tombstone permitted ABA", err)
	}
	stale.ExpectedRevision = deleted
	if _, err = w.Commit(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
}

type lostStore struct {
	*qualifiedStore
	mu       sync.Mutex
	lost     bool
	failRead bool
}

func (s *lostStore) PutConditional(ctx context.Context, key string, b []byte, c storage.PutCondition) (string, error) {
	etag, err := s.qualifiedStore.PutConditional(ctx, key, b, c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == HeadKey && err == nil && !s.lost {
		s.lost = true
		return "", errors.New("lost response")
	}
	return etag, err
}

func (s *lostStore) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	s.mu.Lock()
	fail := s.failRead && s.lost
	s.mu.Unlock()
	if fail {
		return nil, storage.CatalogObjectVersion{}, errors.New("unavailable")
	}
	return s.qualifiedStore.GetCatalogVersion(ctx, key, limit)
}

func TestLostAcknowledgmentAndRestart(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(strconv.FormatBool(unknown), func(t *testing.T) {
			base := &qualifiedStore{storagetest.NewMemoryStore()}
			s := &lostStore{qualifiedStore: base, failRead: unknown}
			w, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			m := mutation(t, w, "lost")
			_, err = w.Commit(t.Context(), m)
			if unknown {
				if !errors.Is(err, ErrCommitUnknown) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.failRead = false
			s.mu.Unlock()
			restarted, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = restarted.Commit(t.Context(), m); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type heldBarrier struct {
	refs    []ObjectRef
	hold    func()
	release func()
}

func (b heldBarrier) Hold(context.Context) ([]ObjectRef, func(), error) {
	if b.hold != nil {
		b.hold()
	}
	return b.refs, func() {
		if b.release != nil {
			b.release()
		}
	}, nil
}

func TestDeletionSnapshotAndPendingFences(t *testing.T) {
	w, s := fixture(t)
	m := mutation(t, w, "retained")
	rev, err := w.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "delete", SessionKey: m.SessionKey, ExpectedRevision: rev}); err != nil {
		t.Fatal(err)
	}
	pending := []byte("pending private fixture")
	if err = s.Put(t.Context(), "sessions/claude/pending/source.json", pending); err != nil {
		t.Fatal(err)
	}
	barrier := heldBarrier{refs: []ObjectRef{{"sessions/claude/pending/source.json", storage.SHA256Hex(pending)}}}
	if err = w.Collect(t.Context(), barrier); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{m.Next.Metadata.Key, m.Next.Summary.SourceBundle.Key, "sessions/claude/pending/source.json"} {
		if _, err = s.Get(t.Context(), key); err != nil {
			t.Fatal("fence deleted", key, err)
		}
	}
	e, _, err := w.Find(t.Context(), m.SessionKey)
	if err != nil || e != nil {
		t.Fatal("deleted identity is live", err)
	}
}

func TestSourceImmutabilityAndMetadataAdapter(t *testing.T) {
	w, s := fixture(t)
	adapter, err := Wrap(s)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "adapter")
	raw, err := s.Get(t.Context(), m.Next.Metadata.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.Put(t.Context(), m.SessionKey, raw); err == nil {
		t.Fatal("unfrozen publication accepted")
	}
	if err = adapter.Publication(m.ID, m.SessionKey, "").Put(t.Context(), m.SessionKey, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(t.Context(), m.SessionKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("canonical metadata dual-written")
	}
	if got, err := adapter.Get(t.Context(), m.SessionKey); err != nil || string(got) != string(raw) {
		t.Fatal(err)
	}
	if err = adapter.Put(t.Context(), m.Next.Summary.SourceBundle.Key, []byte("changed")); !errors.Is(err, storage.ErrChecksumMismatch) && !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatal("source overwrite accepted", err)
	}
}

func TestGCLeaseCrashRecoveryAndNoHeadABA(t *testing.T) {
	w, s := fixture(t)
	m := mutation(t, w, "crash")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	_, before, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Invalid protected reference fails after lease acquisition and performs no
	// deletion. Recovery cannot infer permission merely from elapsed time.
	if err = w.Collect(t.Context(), heldBarrier{refs: []ObjectRef{{"missing", "bad"}}}); err == nil {
		t.Fatal("incomplete inventory accepted")
	}
	h, _, err := w.Head(t.Context())
	if err != nil || h.GCLease == "" {
		t.Fatal("failure released lease", err)
	}
	other := m
	other.ID = "blocked"
	if _, err = w.Commit(t.Context(), other); err == nil {
		t.Fatal("commit crossed GC lease")
	}
	if err = w.RecoverGC(t.Context(), heldBarrier{}, "wrong-owner"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = w.RecoverGC(t.Context(), heldBarrier{}, h.GCLease); err == nil {
		t.Fatal("recovery discarded incomplete protected inventory")
	}
	_, after, err := w.Head(t.Context())
	if err != nil || after == before {
		t.Fatal("GC recreated head ABA", err)
	}
	raw, err := s.Get(t.Context(), HeadKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutConditional(t.Context(), HeadKey, raw, storage.PutCondition{MatchETag: before}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatal("pre-GC CAS remained valid", err)
	}
}

func TestGCAgedRootsCollectsSourcesAndRejectsStagedPublication(t *testing.T) {
	rawStore := &clockFixture{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
	w, err := New(rawStore)
	if err != nil {
		t.Fatal(err)
	}
	s := rawStore.qualifiedStore
	m := mutation(t, w, "expired")
	rev, err := w.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "expired-delete", SessionKey: m.SessionKey, ExpectedRevision: rev}); err != nil {
		t.Fatal(err)
	}
	virtualNow := time.Now().Add(SnapshotLifetime + time.Minute)
	rawStore.override = true
	rawStore.clock = storage.CatalogTime{Earliest: virtualNow, Latest: virtualNow}
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(t.Context(), m.Next.Summary.SourceBundle.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("expired source retained", err)
	}
	// An old staged metadata pointer cannot become live after the barrier.
	m.ID = "staged-after-gc"
	_, m.ExpectedRevision, err = w.Find(t.Context(), m.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), m); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("staged publication references deleted bytes", err)
	}
}

func TestMissingCorruptAndOversizedNodesFailClosed(t *testing.T) {
	w, s := fixture(t)
	m := mutation(t, w, "corruption")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put(t.Context(), h.Identity.Key, []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.Find(t.Context(), m.SessionKey); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal(err)
	}
	if err = w.Collect(t.Context(), heldBarrier{}); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal("GC traversed corrupt authority", err)
	}
}

type pausedCASStore struct {
	*qualifiedStore
	mu      sync.Mutex
	armed   bool
	reached chan struct{}
	resume  chan struct{}
}

func (s *pausedCASStore) PutConditional(ctx context.Context, key string, b []byte, c storage.PutCondition) (string, error) {
	s.mu.Lock()
	pause := key == HeadKey && s.armed
	if pause {
		s.armed = false
	}
	s.mu.Unlock()
	if pause {
		close(s.reached)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.qualifiedStore.PutConditional(ctx, key, b, c)
}

func TestGCRacesDelayedCASAndStagedSource(t *testing.T) {
	s := &pausedCASStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}, reached: make(chan struct{}), resume: make(chan struct{})}
	w, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "delayed")
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := w.Commit(t.Context(), m); done <- err }()
	<-s.reached
	// A delayed CAS is still an admitted active owner. Collection must seal
	// new admissions but cannot obtain a held inventory or delete its source.
	if err = w.Collect(t.Context(), heldBarrier{}); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("collection crossed active writer admission", err)
	}
	if _, err = s.Get(t.Context(), m.Next.Summary.SourceBundle.Key); err != nil {
		t.Fatal("active writer source collected", err)
	}
	late := m
	late.ID = "late-admission"
	if _, err = w.Commit(t.Context(), late); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("post-seal writer admitted", err)
	}
	close(s.resume)
	if err = <-done; err != nil {
		t.Fatal("pre-admitted writer could not drain", err)
	}
	if e, _, err := w.Find(t.Context(), m.SessionKey); err != nil || e == nil {
		t.Fatal("settled publication missing", err)
	}
	state, _, err := w.Coordinator().read(t.Context())
	if err != nil || len(state.Owners) != 0 || state.Hold != "" || state.Seal == "" {
		t.Fatal("drain proof", state, err)
	}
	if err = w.Coordinator().Release(t.Context(), state.Seal); err != nil {
		t.Fatal(err)
	}
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(t.Context(), m.Next.Summary.SourceBundle.Key); err != nil {
		t.Fatal("settled source collected", err)
	}
}

func TestPreservedHistoryFenceSurvivesOldCaptureDates(t *testing.T) {
	w, s := fixture(t)
	source := []byte("active history")
	preserved := []byte("preserved history")
	ref := func(body []byte) archive.SourceReference {
		return archive.SourceReference{Key: "sessions/codex/history/source." + storage.SHA256Hex(body) + ".jsonl.gz", SHA256: storage.SHA256Hex(body), CompressedBytes: len(body)}
	}
	active, old := ref(source), ref(preserved)
	if err := s.Put(t.Context(), active.Key, source); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), old.Key, preserved); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	metadata := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: "history", NativeSessionID: "11111111-1111-4111-8111-111111111111", ProjectID: "project", Harness: archive.Harness{Name: "codex"}, CapturedAt: captured, SourceBundle: active, History: &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111", Preserved: []archive.RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: captured, Source: old}}}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	mr, err := w.PutImmutable(t.Context(), KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "history-publication", SessionKey: "sessions/codex/history/metadata.json", Next: &CatalogEntry{Metadata: mr, Summary: metadata}}); err != nil {
		t.Fatal(err)
	}
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(t.Context(), old.Key); err != nil {
		t.Fatal("history ref was not retained", err)
	}
}

func TestCatalogMutationReadsOnlyChangedPaths(t *testing.T) {
	raw := &qualifiedStore{storagetest.NewMemoryStore()}
	measured := &writerReadBreakdown{MeasuredStore: storagetest.NewMeasuredStore(raw, 0)}
	w, err := New(measured)
	if err != nil {
		t.Fatal("measured qualification lost", err)
	}
	for i := range 160 {
		m := mutation(t, w, fmt.Sprintf("bounded-%04d", i))
		if _, err = w.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	next := mutation(t, w, "bounded-new")
	measured.Reset()
	measured.coordinatorGets = 0
	measured.immutablePuts = 0
	if _, err = w.Commit(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if got := measured.Metrics(); got.Lists != 0 || got.Gets-measured.coordinatorGets > 40 || measured.coordinatorGets != measured.immutablePuts+4 {
		t.Fatal("mutation tree/protocol request breakdown", got, "coordinator", measured.coordinatorGets, "immutable writes", measured.immutablePuts)
	}
}

type partialNodeStore struct {
	*qualifiedStore
	remaining int
	fail      bool
}

func (s *partialNodeStore) PutConditional(ctx context.Context, key string, b []byte, c storage.PutCondition) (string, error) {
	if s.fail && strings.HasPrefix(key, "catalog-v4/nodes/") {
		if s.remaining == 0 {
			return "", errors.New("interrupted immutable node write")
		}
		s.remaining--
	}
	return s.qualifiedStore.PutConditional(ctx, key, b, c)
}

func TestPartialTreeWritesKeepOldHeadAndRetryMutation(t *testing.T) {
	raw := &partialNodeStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
	w, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	a := mutation(t, w, "stable")
	if _, err = w.Commit(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	_, oldTag, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b := mutation(t, w, "partial")
	raw.remaining = 2
	raw.fail = true
	if _, err = w.Commit(t.Context(), b); err == nil {
		t.Fatal("partial write acknowledged")
	}
	_, tag, err := w.Head(t.Context())
	if err != nil || tag != oldTag {
		t.Fatal("partial nodes changed head", err)
	}
	if e, _, err := w.Find(t.Context(), a.SessionKey); err != nil || e == nil {
		t.Fatal("old live entry disappeared", err)
	}
	if e, _, err := w.Find(t.Context(), b.SessionKey); err != nil || e != nil {
		t.Fatal("partial entry visible", err)
	}
	raw.fail = false
	if _, err = w.Commit(t.Context(), b); err != nil {
		t.Fatal("frozen retry did not recover", err)
	}
}

func TestInvalidHeadAndCancellationRefuseMutation(t *testing.T) {
	w, s := fixture(t)
	m := mutation(t, w, "schema")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	original, err := s.Get(t.Context(), HeadKey)
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err = json.Unmarshal(original, &h); err != nil {
		t.Fatal(err)
	}
	h["FutureAuthority"] = true
	future, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put(t.Context(), HeadKey, future); err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.Head(t.Context()); err == nil {
		t.Fatal("unknown head authority admitted")
	}
	if err = s.Put(t.Context(), HeadKey, original); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.ID = "cancelled"
	if _, err = w.Commit(ctx, m); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type clockFixture struct {
	*qualifiedStore
	clock            storage.CatalogTime
	override         bool
	missingPrecision bool
	regress          bool
}

func (s *clockFixture) CatalogServerClock(ctx context.Context) (storage.CatalogTime, error) {
	if s.override {
		return s.clock, nil
	}
	return s.qualifiedStore.CatalogServerClock(ctx)
}

func (s *clockFixture) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	b, v, err := s.qualifiedStore.GetCatalogVersion(ctx, key, limit)
	if s.override && err == nil {
		var h CatalogHead
		if json.Unmarshal(b, &h) == nil && h.PublicationWitness.ETag != "" {
			v.LastModified = s.clock.Earliest
		}
	}
	if s.missingPrecision {
		v.Precision = 0
	}
	if s.regress {
		v.LastModified = v.LastModified.Add(-time.Hour)
	}
	return b, v, err
}

func TestProviderClockAndLeasePreservePublicationWitness(t *testing.T) {
	raw := &clockFixture{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
	w, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "clock")
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	witness := h.PublicationWitness
	// Unbounded uncertainty cannot license deletion, regardless of machine time.
	raw.override = true
	raw.clock = storage.CatalogTime{Earliest: time.Now().Add(-time.Hour), Latest: time.Now().Add(time.Hour)}
	if err = w.Collect(t.Context(), heldBarrier{}); err == nil {
		t.Fatal("unqualified clock used")
	}
	raw.override = false
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal(err)
	}
	after, _, err := w.Head(t.Context())
	if err != nil || after.PublicationWitness != witness {
		t.Fatal("lease reset publication age", err)
	}
	raw.missingPrecision = true
	if _, _, err = w.Head(t.Context()); err == nil {
		t.Fatal("unknown precision admitted")
	}
	raw.missingPrecision = false
	raw.regress = true
	if _, _, err = w.Head(t.Context()); err == nil {
		t.Fatal("provider clock regression admitted")
	}
}

func TestCASStallUsesExactProviderCommitTimestamp(t *testing.T) {
	raw := &pausedCASStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}, reached: make(chan struct{}), resume: make(chan struct{})}
	w, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "stall")
	raw.mu.Lock()
	raw.armed = true
	raw.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := w.Commit(t.Context(), m); done <- err }()
	<-raw.reached
	// The immutable draft was prepared before this instant; the actual commit
	// timestamp must come from the successful provider version after resuming.
	earliestCommit := time.Now()
	close(raw.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Head(t.Context())
	if err != nil || h.PublicationWitness.LastModified.Before(earliestCommit) {
		t.Fatal("pre-CAS wall time used", err)
	}
	if h.CommittedAt != h.PublicationWitness.LastModified {
		t.Fatal("retirement timestamp differs from exact version")
	}
}

func TestFastCaptureClockCannotAgeCatalogSnapshot(t *testing.T) {
	w, s := fixture(t)
	m := mutation(t, w, "fast-clock")
	m.Next.Summary.CapturedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(m.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := w.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "fast-clock-delete", SessionKey: m.SessionKey, ExpectedRevision: rev}); err != nil {
		t.Fatal(err)
	}
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(t.Context(), m.Next.Summary.SourceBundle.Key); err != nil {
		t.Fatal("machine capture time expired a fresh catalog snapshot", err)
	}
}

func TestProtectedCurrentRootsStillTraverseLiveSources(t *testing.T) {
	w, s := fixture(t)
	m := mutation(t, w, "protected-roots")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	refs := []ObjectRef{h.Identity, h.Capture, h.Activity, h.Project, h.Receipts}
	if err = w.Collect(t.Context(), heldBarrier{refs: refs}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{m.Next.Metadata.Key, m.Next.Summary.SourceBundle.Key} {
		if _, err = s.Get(t.Context(), key); err != nil {
			t.Fatalf("protected root suppressed current reachability for %s: %v", key, err)
		}
	}
}

func TestIncompleteHeadReferenceFailsClosed(t *testing.T) {
	for _, missingKey := range []bool{false, true} {
		t.Run(strconv.FormatBool(missingKey), func(t *testing.T) {
			w, s := fixture(t)
			m := mutation(t, w, "incomplete-root")
			if _, err := w.Commit(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			h, _, err := w.Head(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if missingKey {
				h.Identity.Key = ""
			} else {
				h.Identity.SHA256 = ""
			}
			// Model a malformed provider head, rather than a stale carried witness.
			h.PublicationWitness = HeadWitness{}
			raw, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Put(t.Context(), HeadKey, raw); err != nil {
				t.Fatal(err)
			}
			if _, _, err = w.Head(t.Context()); err == nil {
				t.Fatal("incomplete root became valid authority")
			}
			if _, _, err = w.Find(t.Context(), m.SessionKey); err == nil {
				t.Fatal("incomplete root became absence authority")
			}
			if err = w.Collect(t.Context(), heldBarrier{}); err == nil {
				t.Fatal("incomplete root permitted collection")
			}
		})
	}
}

type checksumlessStore struct {
	*qualifiedStore
	sizeDelta int64
}

func (s *checksumlessStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.qualifiedStore.Stat(ctx, key)
	info.SHA256 = ""
	info.Size += s.sizeDelta
	return info, err
}

func TestChecksumlessSourceStillRequiresExactDeclaredSize(t *testing.T) {
	for _, delta := range []int64{0, 1} {
		t.Run(strconv.FormatInt(delta, 10), func(t *testing.T) {
			s := &checksumlessStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}, sizeDelta: delta}
			w, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			m := mutation(t, w, "wrong-source-size")
			m.Next.Summary.SourceBundle.CompressedBytes++
			raw, err := json.Marshal(m.Next.Summary)
			if err != nil {
				t.Fatal(err)
			}
			m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Commit(t.Context(), m); !errors.Is(err, storage.ErrChecksumMismatch) {
				t.Fatal("incorrect source length committed", err)
			}
		})
	}
}

type writerReadBreakdown struct {
	*storagetest.MeasuredStore
	coordinatorGets int64
	immutablePuts   int64
}

func (s *writerReadBreakdown) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	if key == CoordinatorKey {
		s.coordinatorGets++
	}
	return s.MeasuredStore.GetCatalogVersion(ctx, key, limit)
}

func (s *writerReadBreakdown) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	if strings.HasPrefix(key, "catalog-v4/nodes/") || strings.HasPrefix(key, "catalog-v4/heads/") || strings.HasPrefix(key, "catalog-v4/metadata/") {
		s.immutablePuts++
	}
	return s.MeasuredStore.PutConditional(ctx, key, raw, condition)
}

type sourceAuthorityStore struct {
	*qualifiedStore
	stats       int
	sourceReads int
	writes      int
}

type sourceReferenceCase string

const (
	sourceOtherSession    sourceReferenceCase = "other-session"
	sourceTitle           sourceReferenceCase = "title"
	sourceArbitrary       sourceReferenceCase = "arbitrary"
	sourceUppercaseDigest sourceReferenceCase = "uppercase-digest"
	sourceInvalidDigest   sourceReferenceCase = "invalid-digest"
	sourceValid           sourceReferenceCase = "valid"
)

func (s *sourceAuthorityStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.stats++
	return s.qualifiedStore.Stat(ctx, key)
}

func (s *sourceAuthorityStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(key, "catalog-v4/") {
		s.sourceReads++
	}
	return s.qualifiedStore.GetLimited(ctx, key, limit)
}

func (s *sourceAuthorityStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	s.writes++
	return s.qualifiedStore.PutConditional(ctx, key, raw, condition)
}

// Ordinary metadata's legacy reference validation does not establish source
// ownership. Catalog authority must reject foreign or unsafe references before
// consulting those sources or writing a new catalog revision.
func TestCatalogSourceReferencesRequireCanonicalSessionOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []sourceReferenceCase{sourceOtherSession, sourceTitle, sourceArbitrary, sourceUppercaseDigest, sourceInvalidDigest, sourceValid} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			s := &sourceAuthorityStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
			w, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			m := mutation(t, w, "source-owner")
			ref := &m.Next.Summary.SourceBundle
			body, err := s.Get(t.Context(), ref.Key)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case sourceOtherSession:
				ref.Key = strings.Replace(ref.Key, "/source-owner/", "/another-session/", 1)
			case sourceTitle:
				ref.Key = "sessions/claude/source-owner/private-session-title.jsonl.gz"
			case sourceArbitrary:
				ref.Key = "unrelated/private-source.jsonl.gz"
			case sourceUppercaseDigest:
				ref.SHA256 = strings.ToUpper(ref.SHA256)
				ref.Key = "sessions/claude/source-owner/source." + ref.SHA256 + ".jsonl.gz"
			case sourceInvalidDigest:
				ref.SHA256 = strings.Repeat("z", 64)
				ref.Key = "sessions/claude/source-owner/source." + ref.SHA256 + ".jsonl.gz"
			case sourceValid:
				// The ordinary generated source reference remains accepted.
			}
			if err = s.Put(t.Context(), ref.Key, body); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(m.Next.Summary)
			if err != nil {
				t.Fatal(err)
			}
			m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
			if err != nil {
				t.Fatal(err)
			}
			s.stats, s.sourceReads, s.writes = 0, 0, 0
			_, err = w.Commit(t.Context(), m)
			if kind == sourceValid {
				if err != nil || s.stats == 0 || s.writes == 0 {
					t.Fatal("canonical reference refused", err)
				}
				return
			}
			if err == nil || s.stats != 0 || s.sourceReads != 0 || s.writes != 0 {
				t.Fatalf("invalid source gained authority: error=%v stats=%d sourceReads=%d writes=%d", err, s.stats, s.sourceReads, s.writes)
			}
		})
	}
}

// Catalog object writes must remain behind the immutable writer and head CAS,
// including callers using the frozen publication adapter as an ObjectStore.
func TestCatalogAdapterRefusesDirectCatalogObjectWrites(t *testing.T) {
	t.Parallel()
	w, raw := fixture(t)
	adapter, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "direct-write")
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for phase := range 2 {
		if phase == 1 {
			if err = w.Collect(t.Context(), heldBarrier{refs: []ObjectRef{{Key: "missing", SHA256: strings.Repeat("0", 64)}}}); err == nil {
				t.Fatal("incomplete inventory unexpectedly released GC lease")
			}
			leased, _, err := w.Head(t.Context())
			if err != nil || leased.GCLease == "" {
				t.Fatal("expected durable GC lease", err)
			}
		}
		for _, remote := range []storage.ObjectStore{adapter, adapter.Publication("frozen", m.SessionKey, "")} {
			for _, key := range []string{HeadKey, h.Identity.Key, m.Next.Metadata.Key, "catalog-v4/nodes/new.json"} {
				before, readErr := raw.Get(t.Context(), key)
				if err = remote.Put(t.Context(), key, []byte("unconditional replacement")); err == nil {
					t.Fatalf("direct catalog PUT accepted for %s", key)
				}
				after, afterErr := raw.Get(t.Context(), key)
				if string(before) != string(after) || !errors.Is(afterErr, readErr) {
					t.Fatalf("refused PUT changed %s", key)
				}
			}
		}
	}
}

// A checksum of currently matching bytes cannot certify an immutable catalog
// metadata reference at an arbitrary mutable object key.
func TestCommitRequiresImmutableMetadataNamespace(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"mutable/private-title.json", "sessions/claude/metadata-ref/metadata.json", "catalog-v4/nodes/wrong-kind.json", "catalog-v4/metadata/wrong-digest.json"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			w, raw := fixture(t)
			m := mutation(t, w, "metadata-ref")
			body, err := raw.Get(t.Context(), m.Next.Metadata.Key)
			if err != nil {
				t.Fatal(err)
			}
			if err = raw.Put(t.Context(), key, body); err != nil {
				t.Fatal(err)
			}
			m.Next.Metadata.Key = key
			if _, err = w.Commit(t.Context(), m); err == nil {
				t.Fatal("mutable metadata reference committed")
			}
			if _, err = raw.Get(t.Context(), HeadKey); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("invalid reference changed head", err)
			}
		})
	}
}
