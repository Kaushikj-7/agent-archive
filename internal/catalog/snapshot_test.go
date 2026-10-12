package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func activateFixture(t *testing.T, w *Writer) {
	t.Helper()
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), owner, "private synthetic proof"); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotBoundedRangeCountsCursorsAndBodyAuthority(t *testing.T) {
	w, store := fixture(t)
	for i := range 140 {
		m := mutation(t, w, fmt.Sprintf("session-%04d", i))
		if _, err := w.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	activateFixture(t, w)
	measured := storagetest.NewMeasuredStore(store, 0)
	cache := &NodeCache{}
	s, err := OpenSnapshot(t.Context(), measured, cache)
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Index: CaptureIndex, Lower: OrderPrefix(false) + "0", Upper: OrderPrefix(false) + ":", Reverse: true}
	count, err := s.Count(t.Context(), q)
	if err != nil || count != 140 {
		t.Fatalf("count %d %v", count, err)
	}
	p, err := s.Query(t.Context(), q, "", 50)
	if err != nil || len(p.Rows) != 50 || p.Next == "" {
		t.Fatalf("page %d %v", len(p.Rows), err)
	}
	for i, row := range p.Rows {
		if row.Entry.Summary.SessionID != fmt.Sprintf("session-%04d", i) {
			t.Fatalf("tie order %s", row.Entry.Summary.SessionID)
		}
	}
	next, err := s.Query(t.Context(), q, p.Next, 50)
	if err != nil || next.Rows[0].Entry.Summary.SessionID != "session-0050" {
		t.Fatalf("continuation %+v %v", next, err)
	}
	changed := q
	changed.Reverse = !changed.Reverse
	if _, err = s.Query(t.Context(), changed, p.Next, 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("query cursor accepted", err)
	}
	rawCursor, err := base64.RawURLEncoding.DecodeString(p.Next)
	if err != nil {
		t.Fatal(err)
	}
	var forged snapshotCursor
	if err = json.Unmarshal(rawCursor, &forged); err != nil {
		t.Fatal(err)
	}
	forged.After = "forged offset"
	rawCursor, err = json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Query(t.Context(), q, base64.RawURLEncoding.EncodeToString(rawCursor), 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("modified cursor accepted", err)
	}
	reopened, err := OpenSnapshot(t.Context(), measured, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Query(t.Context(), q, p.Next, 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("reopened cursor accepted", err)
	}
	before := measured.Metrics()
	if _, err = reopened.Query(t.Context(), q, "", 1); err != nil {
		t.Fatal(err)
	}
	cost := measured.Metrics()
	if cost.Lists != 0 || cost.Gets-before.Gets > 2 {
		t.Fatalf("warm node cost %+v before %+v", cost, before)
	}
	row := p.Rows[0]
	old, err := s.ReadMetadata(t.Context(), row.Entry)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "session-0000")
	m.ID = "successor"
	m.ExpectedRevision = row.Entry.Revision
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	pinned, err := s.ReadMetadata(t.Context(), row.Entry)
	if err != nil || string(old) != string(pinned) {
		t.Fatal("selected body switched roots", err)
	}
	if _, err = s.Query(t.Context(), q, p.Next, 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("changed head cursor accepted", err)
	}
	s.started = time.Now().Add(-SnapshotLifetime)
	if _, err = s.ReadMetadata(t.Context(), row.Entry); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("expired body accepted", err)
	}
}

func TestSnapshotMissingCorruptAndCandidateFailClosed(t *testing.T) {
	w, store := fixture(t)
	m := mutation(t, w, "one")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSnapshot(t.Context(), store, nil); err == nil {
		t.Fatal("candidate treated complete")
	}
	activateFixture(t, w)
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(t.Context(), head.Capture.Key, []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Query(t.Context(), Query{Index: CaptureIndex}, "", 1); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal(err)
	}
	if err = store.Delete(t.Context(), head.Identity.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Find(t.Context(), m.SessionKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCoordinatorDrainsGloballyAndNeverTimesOut(t *testing.T) {
	w, store := fixture(t)
	m := mutation(t, w, "blocked")
	c := w.Coordinator()
	if err := c.Admit(t.Context(), "remote machine/history", "digest", nil); err != nil {
		t.Fatal(err)
	}
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = restarted.Coordinator().HeldBarrier(owner).Hold(t.Context()); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("unfinished owner drained", err)
	}
	if err = restarted.Coordinator().Release(t.Context(), owner); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("unfinished owner recovered", err)
	}
	if err = c.Admit(t.Context(), "remote machine/history", "digest", nil); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("duplicate ownership accepted", err)
	}
	if err = c.Complete(t.Context(), "remote machine/history", "wrong"); !errors.Is(err, ErrMutationReuse) {
		t.Fatal(err)
	}
	if err = c.Complete(t.Context(), "remote machine/history", "digest"); err != nil {
		t.Fatal(err)
	}
	_, release, err := c.HeldBarrier(owner).Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), m); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("direct commit crossed global seal", err)
	}
	remote, err := Wrap(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.PutImmutable(t.Context(), KindMetadata, []byte("unbound")); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("immutable staging crossed seal", err)
	}
	if err = remote.Put(t.Context(), "sessions/claude/blocked/source", []byte("bytes")); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("source crossed seal", err)
	}
	release()
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
}

func TestOpeningAlreadyConsumedLifetimeFails(t *testing.T) {
	w, store := fixture(t)
	m := mutation(t, w, "one")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	if _, err := openSnapshotStarted(t.Context(), store, nil, true, time.Now().Add(-SnapshotLifetime)); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("opening extended consumed lifetime", err)
	}
}

func TestPendingInvocationExclusiveAndRestartClosed(t *testing.T) {
	w, raw := fixture(t)
	mutation := mutation(t, w, "pending")
	data, err := json.Marshal(mutation.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginPublication(t.Context(), "frozen", data); err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginPublication(t.Context(), "frozen", data); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("parallel duplicate admitted", err)
	}
	store.EndPublicationAttempt("frozen")
	admitted, err := store.BeginPublication(t.Context(), "frozen", data)
	if err != nil {
		t.Fatal("owned sequential retry refused", err)
	}
	restarted, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.BeginPublication(t.Context(), "frozen", data); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("unresolved restart ownership accepted", err)
	}
	if err = store.CompletePublication(admitted, "frozen", data); err != nil {
		t.Fatal(err)
	}
	store.EndPublicationAttempt("frozen")
}

func TestIndexCountsDeletionSplitsAndRootReplayDiscriminators(t *testing.T) {
	w, store := fixture(t)
	var mutations []CatalogMutation
	for i := range 100 {
		m := mutation(t, w, fmt.Sprintf("session-%04d", i))
		if i%3 == 0 {
			m.Next.Summary.ParentSessionID = "parent"
		}
		if i%5 == 0 {
			m.Next.Summary.Replay = &archive.Replay{}
		}
		raw, err := json.Marshal(m.Next.Summary)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := w.PutImmutable(t.Context(), KindMetadata, raw)
		if err != nil {
			t.Fatal(err)
		}
		m.Next.Metadata = ref
		revision, err := w.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		m.ExpectedRevision = revision
		mutations = append(mutations, m)
	}
	activateFixture(t, w)
	for step := range 2 {
		s, err := OpenSnapshot(t.Context(), store, nil)
		if err != nil {
			t.Fatal(err)
		}
		live, roots, children := 0, map[bool]uint64{}, map[bool]uint64{}
		for i, m := range mutations {
			if step == 1 && i%2 == 0 {
				continue
			}
			live++
			replay := m.Next.Summary.Replay != nil
			if m.Next.Summary.ParentSessionID == "" {
				roots[replay]++
			} else {
				children[replay]++
			}
		}
		for _, idx := range []Index{CaptureIndex, ActivityIndex, ProjectIndex} {
			total, err := s.Count(t.Context(), Query{Index: idx})
			if err != nil || total != uint64(live) {
				t.Fatalf("all-session aggregate %s %d want %d %v", idx, total, live, err)
			}
			for _, replay := range []bool{false, true} {
				p := OrderPrefix(replay)
				count, err := s.Count(t.Context(), Query{Index: idx, Lower: p, Upper: p + "~"})
				if err != nil || count != roots[replay] {
					t.Fatal("root count", idx, count, roots[replay], err)
				}
				kind := "ordinary/"
				if replay {
					kind = "replay/"
				}
				p = "!children/" + kind
				count, err = s.Count(t.Context(), Query{Index: idx, Lower: p, Upper: p + "~"})
				if err != nil || count != children[replay] {
					t.Fatal("child count", idx, count, children[replay], err)
				}
			}
		}
		if step == 0 {
			for i, m := range mutations {
				if i%2 == 0 {
					if _, err = w.Commit(t.Context(), CatalogMutation{ID: "delete/" + m.ID, SessionKey: m.SessionKey, ExpectedRevision: m.ExpectedRevision}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func replaceFixtureBody(t *testing.T, w *Writer, m CatalogMutation) CatalogMutation {
	t.Helper()
	raw, err := json.Marshal(m.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReadViewPinsBodiesDestinationAndRequestStart(t *testing.T) {
	w, raw := fixture(t)
	m := mutation(t, w, "view")
	revision, err := w.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithReadView(t.Context())
	initial, gotRevision, err := store.GetVersioned(ctx, m.SessionKey)
	if err != nil || gotRevision != revision {
		t.Fatal(gotRevision, err)
	}
	updated := mutation(t, w, "view")
	updated.ID = "update-view"
	updated.ExpectedRevision = revision
	updated.Next.Summary.ProjectID = "changed"
	updated = replaceFixtureBody(t, w, updated)
	if _, err = w.Commit(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	pinned, pinnedRevision, err := store.GetVersioned(ctx, m.SessionKey)
	if err != nil || pinnedRevision != revision || string(pinned) != string(initial) {
		t.Fatal("body switched roots", pinnedRevision, err)
	}
	fresh, _, err := store.GetVersioned(WithReadView(t.Context()), m.SessionKey)
	if err != nil || string(fresh) == string(initial) {
		t.Fatal("fresh view failed", err)
	}
	_, other := fixture(t)
	if _, err = OpenSnapshot(ctx, other, nil); err == nil {
		t.Fatal("view crossed destinations")
	}
	view := ctx.Value(readViewKey{}).(*readView)
	view.mu.Lock()
	view.started = time.Now().Add(-SnapshotLifetime)
	view.mu.Unlock()
	if _, err = store.Get(WithReadView(ctx), m.SessionKey); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("rewrapping extended view", err)
	}
	unopened := WithReadView(t.Context())
	unopened.Value(readViewKey{}).(*readView).started = time.Now().Add(-SnapshotLifetime)
	if _, err = OpenSnapshot(unopened, raw, nil); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("lazy capture extended lifetime", err)
	}
}

func TestDerivedChildCountersSurviveMovesDeletionAndParentAfterChildren(t *testing.T) {
	w, raw := fixture(t)
	var children []CatalogMutation
	var revisions []string
	for i := range 70 {
		m := mutation(t, w, fmt.Sprintf("child-%03d", i))
		m.Next.Summary.ParentSessionID = "parent"
		m = replaceFixtureBody(t, w, m)
		revision, err := w.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, m)
		revisions = append(revisions, revision)
	}
	parent := mutation(t, w, "parent")
	parentRevision, err := w.Commit(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	before, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := w.Find(t.Context(), parent.SessionKey)
	if err != nil || entry.OrdinaryChildren != 70 || entry.ReplayChildren != 0 {
		t.Fatal("parent after children", entry, err)
	}
	other := mutation(t, w, "other")
	if _, err = w.Commit(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	move := children[0]
	move.ID = "move-child"
	move.ExpectedRevision = revisions[0]
	move.Next.Summary.ParentSessionID = "other"
	move = replaceFixtureBody(t, w, move)
	if _, err = w.Commit(t.Context(), move); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "delete-child", SessionKey: children[1].SessionKey, ExpectedRevision: revisions[1]}); err != nil {
		t.Fatal(err)
	}
	replay := children[2]
	replay.ID = "replay-child"
	replay.ExpectedRevision = revisions[2]
	replay.Next.Summary.Replay = &archive.Replay{RunID: "private-test"}
	replay = replaceFixtureBody(t, w, replay)
	if _, err = w.Commit(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	entry, revision, err := w.Find(t.Context(), parent.SessionKey)
	if err != nil || entry.OrdinaryChildren != 67 || entry.ReplayChildren != 1 || revision != parentRevision || entry.Metadata != parent.Next.Metadata {
		t.Fatal("derived counts changed body authority", entry, revision, err)
	}
	otherEntry, _, err := w.Find(t.Context(), other.SessionKey)
	if err != nil || otherEntry.OrdinaryChildren != 1 {
		t.Fatal("new parent count", otherEntry, err)
	}
	after, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := after.Delta(t.Context(), before.Root())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range delta.Changed {
		if row.Key == parent.SessionKey {
			found = row.Entry.OrdinaryChildren == 67 && row.Entry.ReplayChildren == 1
		}
	}
	if !found {
		t.Fatal("count-only delta omitted parent")
	}
	prefix := ChildPrefix("claude", "parent", false)
	count, err := after.Count(t.Context(), Query{Index: ProjectIndex, Lower: prefix + "0", Upper: prefix + ":"})
	if err != nil || count != entry.OrdinaryChildren {
		t.Fatal("counter differs from tree oracle", count, err)
	}
}

func TestPendingClaimDrainsFrozenSourceAndHistoryAfterSeal(t *testing.T) {
	w, raw := fixture(t)
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	active, preserved := []byte("private active"), []byte("private preserved")
	id := "00000000-0000-0000-0000-000000000001"
	makeRef := func(body []byte) archive.SourceReference {
		return archive.SourceReference{Key: "sessions/codex/" + id + "/source." + storage.SHA256Hex(body) + ".jsonl.gz", SHA256: storage.SHA256Hex(body), CompressedBytes: len(body)}
	}
	metadata := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: id, NativeSessionID: id, ProjectID: "project", Harness: archive.Harness{Name: "codex"}, CapturedAt: time.Now().UTC(), SourceBundle: makeRef(active), History: &archive.RevisionHistory{CurrentRevision: id, Preserved: []archive.RevisionReference{{RevisionID: "00000000-0000-0000-0000-000000000002", CapturedAt: time.Now().UTC(), Source: makeRef(preserved)}}}}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.BeginPublication(t.Context(), "frozen-drain", data)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := w.Coordinator().Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.Coordinator().HeldBarrier(seal).Hold(t.Context()); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("active lifecycle frozen by refs alone", err)
	}
	if err = store.Put(ctx, metadata.SourceBundle.Key, active); err != nil {
		t.Fatal("active source could not drain", err)
	}
	if err = store.Put(ctx, metadata.History.Preserved[0].Source.Key, preserved); err != nil {
		t.Fatal("history source could not drain", err)
	}
	if err = store.Put(ctx, "sessions/codex/unprotected/source.jsonl.gz", []byte("unprotected")); err == nil {
		t.Fatal("unprotected source admitted")
	}
	_, other := fixture(t)
	otherStore, err := Wrap(other)
	if err != nil {
		t.Fatal(err)
	}
	if err = otherStore.Put(ctx, metadata.SourceBundle.Key, active); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("claim crossed destination", err)
	}
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Publication("frozen-drain", key, "").Put(ctx, key, data); err != nil {
		t.Fatal("publication could not drain", err)
	}
	if err = store.CompletePublication(ctx, "frozen-drain", data); err != nil {
		t.Fatal(err)
	}
	store.EndPublicationAttempt("frozen-drain")
	if err = store.Put(ctx, metadata.SourceBundle.Key, active); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("retired context accepted", err)
	}
	_, release, err := w.Coordinator().HeldBarrier(seal).Hold(t.Context())
	if err != nil {
		t.Fatal("drained lifecycle refused", err)
	}
	release()
}

type expireOnBodyStore struct {
	*qualifiedStore
	onBody func()
}

func (s *expireOnBodyStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	raw, err := s.qualifiedStore.GetLimited(ctx, key, limit)
	if strings.HasPrefix(key, "catalog-v4/metadata/") && s.onBody != nil {
		s.onBody()
	}
	return raw, err
}

func TestReadViewRejectsBodyThatCompletesAfterLifetime(t *testing.T) {
	w, raw := fixture(t)
	m := mutation(t, w, "expires-in-read")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	expiring := &expireOnBodyStore{qualifiedStore: raw}
	store, err := Wrap(expiring)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithReadView(t.Context())
	view := ctx.Value(readViewKey{}).(*readView)
	expiring.onBody = func() { view.mu.Lock(); view.started = time.Now().Add(-SnapshotLifetime); view.mu.Unlock() }
	if _, err = store.Get(ctx, m.SessionKey); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("expired in-flight body accepted", err)
	}
}

func TestPublicAdapterCASAndReadonlyMeasuredWriterRefuseMutation(t *testing.T) {
	w, raw := fixture(t)
	m := mutation(t, w, "readonly")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := New(store)
	if err != nil || exact != store.Writer {
		t.Fatal("adapter writer binding", err)
	}
	before, err := raw.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutConditional(t.Context(), CoordinatorKey, []byte("bypass"), storage.PutCondition{}); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("public CAS bypass", err)
	}
	measured := storagetest.NewMeasuredStore(store, 0)
	readonly, err := New(measured)
	if err != nil {
		t.Fatal(err)
	}
	if !readonly.readOnly || readonly.conditional != nil {
		t.Fatal("wrapper reconstructed writer authority")
	}
	if _, err = Wrap(measured); !errors.Is(err, ErrReadOnly) {
		t.Fatal("wrapper regained publication", err)
	}
	checks := []func() error{
		func() error { _, e := readonly.PutImmutable(t.Context(), KindMetadata, []byte("bypass")); return e },
		func() error { _, e := readonly.Commit(t.Context(), m); return e },
		func() error { return readonly.Collect(t.Context(), heldBarrier{}) },
		func() error { return readonly.RecoverGC(t.Context(), heldBarrier{}, "owner") },
		func() error { _, e := readonly.Coordinator().Seal(t.Context()); return e },
	}
	for _, check := range checks {
		if err = check(); !errors.Is(err, ErrReadOnly) {
			t.Fatal("read-only mutation result", err)
		}
	}
	after, err := raw.List(t.Context(), "")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("refused mutation wrote objects", err)
	}
	ctx := WithReadView(t.Context())
	snapshot, err := OpenSnapshot(ctx, measured, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := snapshot.Find(t.Context(), m.SessionKey)
	if err != nil || entry == nil {
		t.Fatal("measured snapshot lost reads", err)
	}
	if _, err = snapshot.ReadMetadata(t.Context(), *entry); err != nil {
		t.Fatal(err)
	}
	if _, err = measured.Get(ctx, m.SessionKey); err != nil {
		t.Fatal("measured view did not forward to same Store", err)
	}
	other, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Get(ctx, m.SessionKey); err == nil {
		t.Fatal("distinct authority shared read view")
	}
	if metrics := measured.Metrics(); metrics.Lists != 0 || metrics.Gets == 0 {
		t.Fatal("snapshot metrics", metrics)
	}
}

func TestNewReadViewRefreshesWrappedExpiredRequest(t *testing.T) {
	w, raw := fixture(t)
	m := mutation(t, w, "browser-refresh")
	revision, err := w.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	old := WithReadView(parent)
	before, _, err := store.GetVersioned(old, m.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	updated := mutation(t, w, "browser-refresh")
	updated.ID, updated.ExpectedRevision = "browser-new-root", revision
	updated.Next.Summary.ProjectID = "new-project"
	updated = replaceFixtureBody(t, w, updated)
	if _, err = w.Commit(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	view := old.Value(readViewKey{}).(*readView)
	view.mu.Lock()
	view.started = time.Now().Add(-SnapshotLifetime)
	view.mu.Unlock()
	fresh := NewReadView(old)
	after, _, err := store.GetVersioned(fresh, m.SessionKey)
	if err != nil || string(before) == string(after) {
		t.Fatal("new selection retained expired root", err)
	}
	if WithReadView(fresh) != fresh {
		t.Fatal("within-selection view replaced")
	}
	if _, err = store.Get(old, m.SessionKey); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("old view lifetime reset", err)
	}
	cancel()
	if _, err = store.Get(NewReadView(old), m.SessionKey); !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation lost", err)
	}
	deadline, stop := context.WithDeadline(t.Context(), time.Now().Add(time.Minute))
	defer stop()
	want, _ := deadline.Deadline()
	got, ok := NewReadView(WithReadView(deadline)).Deadline()
	if !ok || !got.Equal(want) {
		t.Fatal("parent deadline lost")
	}
}

func TestConcurrentChildCommitsRebaseParentCounters(t *testing.T) {
	w, raw := fixture(t)
	parent := mutation(t, w, "concurrent-parent")
	if _, err := w.Commit(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	children := make([]CatalogMutation, 2)
	for i := range children {
		child := mutation(t, w, fmt.Sprintf("concurrent-child-%d", i))
		child.Next.Summary.ParentSessionID = parent.Next.Summary.SessionID
		children[i] = replaceFixtureBody(t, w, child)
	}
	start := make(chan struct{})
	errors := make(chan error, len(children))
	for _, child := range children {
		go func(m CatalogMutation) { <-start; _, err := w.Commit(t.Context(), m); errors <- err }(child)
	}
	close(start)
	for range children {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	entry, revision, err := w.Find(t.Context(), parent.SessionKey)
	if err != nil || entry == nil || entry.OrdinaryChildren != 2 || entry.ReplayChildren != 0 {
		t.Fatalf("parent counters=%+v err=%v", entry, err)
	}
	for _, child := range children {
		if _, err = w.Commit(t.Context(), child); err != nil {
			t.Fatal("receipt retry", err)
		}
	}
	after, afterRevision, err := w.Find(t.Context(), parent.SessionKey)
	if err != nil || after.OrdinaryChildren != 2 || afterRevision != revision || after.Metadata != entry.Metadata {
		t.Fatal("receipt retry altered body/count authority", err)
	}
	activateFixture(t, w)
	s, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefix := ChildPrefix(parent.Next.Summary.Harness.Name, parent.Next.Summary.SessionID, false)
	count, err := s.Count(t.Context(), Query{Index: ProjectIndex, Lower: prefix + "0", Upper: prefix + ":"})
	if err != nil || count != after.OrdinaryChildren {
		t.Fatal("counter differs from complete indexed child oracle", count, err)
	}
}

func TestNodeCountRejectsOverflowBeforeAggregateReuse(t *testing.T) {
	ref := ObjectRef{Key: "catalog-v4/nodes/test", SHA256: strings.Repeat("a", 64)}
	raw, err := json.Marshal(node{Count: 0, Children: []branch{{Max: "a", Ref: ref, Count: ^uint64(0)}, {Max: "b", Ref: ref, Count: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeNode(raw); err == nil {
		t.Fatal("wrapped aggregate accepted")
	}
}

func TestSnapshotValidateReadExpiryCancellationDoesNotReadProvider(t *testing.T) {
	w, raw := fixture(t)
	if _, err := w.Commit(t.Context(), mutation(t, w, "validated-read")); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	measured := storagetest.NewMeasuredStore(raw, 0)
	snapshot, err := OpenSnapshot(t.Context(), measured, nil)
	if err != nil {
		t.Fatal(err)
	}
	measured.Reset()
	if err = snapshot.ValidateRead(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot.started = time.Now().Add(-SnapshotLifetime)
	if err = snapshot.ValidateRead(t.Context()); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("expired cache read accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = snapshot.ValidateRead(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if counts := measured.Metrics(); counts.Gets != 0 || counts.Lists != 0 {
		t.Fatal("validation refreshed provider", counts)
	}
}

func TestSnapshotValidateContinuationPreservesLifetimeAndRoot(t *testing.T) {
	w, raw := fixture(t)
	if _, err := w.Commit(t.Context(), mutation(t, w, "continuation")); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	measured := storagetest.NewMeasuredStore(raw, 0)
	snapshot, err := OpenSnapshot(t.Context(), measured, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, started := snapshot.Root(), snapshot.started
	measured.Reset()
	if err = snapshot.ValidateContinuation(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counts := measured.Metrics(); counts.Gets != 1 || counts.Lists != 0 {
		t.Fatal("continuation version observations", counts)
	}
	if snapshot.Root() != root || snapshot.started != started || snapshot.requests != 0 {
		t.Fatal("continuation renewed authority")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = snapshot.ValidateContinuation(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled continuation", err)
	}
	snapshot.started = time.Now().Add(-SnapshotLifetime)
	if err = snapshot.ValidateContinuation(t.Context()); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("expired continuation", err)
	}
	if counts := measured.Metrics(); counts.Gets != 1 {
		t.Fatal("invalid lifetime read provider", counts)
	}
	snapshot.started = started
	if _, err = w.Commit(t.Context(), mutation(t, w, "new-root")); err != nil {
		t.Fatal(err)
	}
	if err = snapshot.ValidateContinuation(t.Context()); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("changed head continuation", err)
	}
	if snapshot.Root() != root || snapshot.started != started {
		t.Fatal("head mismatch renewed view")
	}
}
