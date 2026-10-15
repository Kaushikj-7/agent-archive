package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type discoveryArchive struct {
	*storagetest.MemoryStore
	sourceGets      int
	lists           int
	beforeRead      func(string)
	streamOversized string
	readError       func(context.Context, string) error
}

func (*discoveryArchive) CatalogAtomicQualification() error { return nil }

func (a *discoveryArchive) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if a.readError != nil {
		if err := a.readError(ctx, key); err != nil {
			return nil, err
		}
	}
	if key == a.streamOversized {
		return make([]byte, limit+1), storage.ErrObjectTooLarge
	}

	if a.beforeRead != nil {
		a.beforeRead(key)
	}
	if strings.HasPrefix(key, "sessions/") && !strings.HasSuffix(key, "/metadata.json") {
		a.sourceGets++
	}
	return a.MemoryStore.GetLimited(ctx, key, limit)
}

func (a *discoveryArchive) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	a.lists++
	return nil, errors.New("catalog must not physically enumerate sessions")
}

func (a *discoveryArchive) ListPage(ctx context.Context, prefix, token string, limit int32) (storage.ObjectPage, error) {
	a.lists++
	return storage.ObjectPage{}, errors.New("catalog must not physically enumerate sessions")
}

type searchArchiveFixture struct {
	raw    *discoveryArchive
	remote *catalog.Store
	serial int
}

func newSearchArchive(tb testing.TB) *searchArchiveFixture {
	tb.Helper()
	raw := &discoveryArchive{MemoryStore: storagetest.NewMemoryStore()}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		tb.Fatal(err)
	}
	return &searchArchiveFixture{raw: raw, remote: remote}
}

func (a *searchArchiveFixture) publish(tb testing.TB, m archive.Metadata) {
	tb.Helper()
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		tb.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		tb.Fatal(err)
	}
	ref, err := a.remote.Writer.PutImmutable(tb.Context(), catalog.KindMetadata, raw)
	if err != nil {
		tb.Fatal(err)
	}
	a.mutate(tb, key, &catalog.CatalogEntry{Metadata: ref, Summary: m})
}

func (a *searchArchiveFixture) mutate(tb testing.TB, key string, next *catalog.CatalogEntry) {
	tb.Helper()
	revision := ""
	if a.serial > 0 {
		snapshot, err := catalog.OpenSnapshot(tb.Context(), a.remote, nil)
		if err != nil {
			tb.Fatal(err)
		}
		old, err := snapshot.Find(tb.Context(), key)
		if err != nil {
			tb.Fatal(err)
		}
		if old != nil {
			revision = old.Revision
		}
	}
	a.serial++
	_, err := a.remote.Writer.Commit(tb.Context(), catalog.CatalogMutation{ID: fmt.Sprintf("synthetic-%d", a.serial), SessionKey: key, ExpectedRevision: revision, Next: next})
	if err != nil {
		tb.Fatal(err)
	}
	if a.serial == 1 {
		c := a.remote.Writer.Coordinator()
		owner, e := c.Seal(tb.Context())
		if e != nil {
			tb.Fatal(e)
		}
		if e = c.Activate(tb.Context(), owner, "synthetic search archive"); e != nil {
			tb.Fatal(e)
		}
		if e = c.Release(tb.Context(), owner); e != nil {
			tb.Fatal(e)
		}
	}
}

func (a *searchArchiveFixture) session(tb testing.TB, id string) archive.Metadata {
	tb.Helper()
	_, bundle, _ := verifiedFixture(tb, "claude", workerTranscript)
	bundle.ArchiveSessionID = id
	m, _, objects := publishFixture(tb, bundle, claude.Parser{})
	raw, err := objects.Get(tb.Context(), m.SourceBundle.Key)
	if err != nil {
		tb.Fatal(err)
	}
	if err = a.raw.Put(tb.Context(), m.SourceBundle.Key, raw); err != nil {
		tb.Fatal(err)
	}
	a.publish(tb, m)
	return m
}

func TestReconcileCrossMachineRestartMetadataPrivacyAndRemoval(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	first := a.session(t, "a")
	_ = a.session(t, "b")
	_ = a.session(t, "c")
	_ = a.session(t, "d")
	_ = a.session(t, "e")
	dir := t.TempDir()
	parsers := builtin.NewBuiltins()
	at := first.CapturedAt
	s, err := Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := s.Reconcile(t.Context(), a.remote, parsers, at, ReconcileBudget{Entries: 1, Checks: 1})
	if err != nil || discovery.CompletedWalk || discovery.Objects != 1 || a.raw.sourceGets != 0 || a.raw.lists != 0 {
		t.Fatalf("initial %+v source=%d lists=%d %v", discovery, a.raw.sourceGets, a.raw.lists, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for i := range 8 {
		discovery, err = s.Reconcile(t.Context(), a.remote, parsers, at.Add(time.Duration(i)*time.Second), ReconcileBudget{Entries: 1, Checks: 1})
		if err != nil {
			t.Fatal(err)
		}
		if discovery.CompletedWalk {
			break
		}
	}
	progress, err := s.Run(t.Context(), a.remote, parsers, at.Add(time.Minute), Budget{Sources: 8, VerifyCurrent: true})
	if err != nil || progress.Committed != 5 || a.raw.sourceGets != 5 {
		t.Fatalf("catchup %+v sources=%d %v", progress, a.raw.sourceGets, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 5 || !page.Observation.Complete {
		t.Fatalf("coverage %+v %v", page, err)
	}
	discovery, err = s.Reconcile(t.Context(), a.remote, parsers, at.Add(2*time.Minute), ReconcileBudget{})
	if err != nil || !discovery.Unchanged || discovery.Metadata != 0 || a.raw.sourceGets != 5 {
		t.Fatalf("unchanged %+v %v", discovery, err)
	}
	// A second producer changes the same shared archive without local hints.
	second, err := catalog.Wrap(a.raw)
	if err != nil {
		t.Fatal(err)
	}
	a.remote = second
	first.Name = "remote renamed title"
	a.publish(t, first)
	_, err = s.Reconcile(t.Context(), a.remote, parsers, at.Add(3*time.Minute), ReconcileBudget{})
	if err != nil {
		t.Fatal(err)
	}
	progress, err = s.Run(t.Context(), a.remote, parsers, at.Add(3*time.Minute), Budget{VerifyCurrent: true})
	if err != nil || progress.Loaded != 0 || progress.Committed != 1 {
		t.Fatalf("metadata %+v %v", progress, err)
	}
	first.FilterVersion = "unknown"
	a.publish(t, first)
	if _, err = s.Reconcile(t.Context(), a.remote, parsers, at.Add(4*time.Minute), ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 4 {
		t.Fatalf("privacy before replacement %+v %v", page, err)
	}
	key, _ := archive.MetadataObjectKey(first.Harness.Name, "b")
	a.mutate(t, key, nil)
	discovery, err = s.Reconcile(t.Context(), a.remote, parsers, at.Add(5*time.Minute), ReconcileBudget{})
	if err != nil || discovery.Removed != 1 {
		t.Fatalf("deletion %+v %v", discovery, err)
	}
	page, err = s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 3 {
		t.Fatalf("deleted %+v %v", page, err)
	}
	t.Logf("warm discovery %+v; source GETs=%d; last observed=%s", discovery, a.raw.sourceGets, page.Observation.LastObserved)
}

func TestReconcileMovingRootsNeverProveCompleteAndCatchupAdvances(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	first := a.session(t, "a")
	_ = a.session(t, "m")
	last := a.session(t, "z")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := first.CapturedAt
	for i := range 3 {
		first.Name = fmt.Sprintf("moving %d", i)
		a.publish(t, first)
		d, err := s.Reconcile(t.Context(), a.remote, parsers, at.Add(time.Duration(i)*time.Second), ReconcileBudget{Entries: 1, Checks: 1})
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && !d.CompletedWalk {
			t.Fatalf("catchup starved: %+v", d)
		}
	}
	var complete string
	if err := s.db.QueryRowContext(t.Context(), "SELECT complete_root FROM search_discovery").Scan(&complete); err != nil || complete != "" {
		t.Fatalf("moving roots claimed coverage %q %v", complete, err)
	}
	var count int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work WHERE id=?", last.Harness.Name+"/"+last.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old catchup missing %d %v", count, err)
	}
	// Inserting behind the checkpoint is discovered in the following sweep.
	_ = a.session(t, "0-behind")
	for range 5 {
		if _, err := s.Reconcile(t.Context(), a.remote, parsers, at, ReconcileBudget{Entries: 1, Checks: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&count); err != nil || count != 4 {
		t.Fatalf("behind checkpoint %d %v", count, err)
	}
}

func TestReconcileBudgetsCancellationAndTombstoneProgress(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "live")
	for i := range 6 {
		id := fmt.Sprintf("gone-%d", i)
		_ = a.session(t, id)
		key, _ := archive.MetadataObjectKey("claude", id)
		a.mutate(t, key, nil)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	d, err := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{Nodes: 1})
	if err == nil || d.Nodes > 1 || d.CompletedWalk {
		t.Fatalf("node bound %+v %v", d, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.Reconcile(ctx, a.remote, parsers, m.CapturedAt, ReconcileBudget{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i := range 8 {
		d, err = s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
		if err != nil {
			t.Fatal(err)
		}
		if d.Objects > 1 {
			t.Fatalf("physical identity cap %+v", d)
		}
		if d.CompletedWalk {
			if i < 6 {
				t.Fatalf("skipped tombstones %+v", d)
			}
			break
		}
	}
	if !d.CompletedWalk {
		t.Fatal("tombstones prevented progress")
	}
	d, err = s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{Bytes: 1})
	if err == nil || d.Bytes > 1 || d.CompletedWalk {
		t.Fatalf("byte bound %+v %v", d, err)
	}
}

func TestReconcileLegacyPhysicalPagesAndConfirmedAbsence(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for i := range 7 {
		if err := raw.Put(t.Context(), fmt.Sprintf("sessions/000-source-%d", i), []byte("synthetic")); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 12 {
		d, err := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
		if err != nil {
			t.Fatal(err)
		}
		if d.Objects > 1 {
			t.Fatalf("object page %+v", d)
		}
		if i < 7 && d.Metadata != 0 {
			t.Fatalf("sidecar-only bound %+v", d)
		}
		if d.CompletedWalk {
			break
		}
	}
	p, err := s.Run(t.Context(), raw, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Committed != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || page.Observation.Complete || page.Observation.DiscoveryComplete || page.Observation.LastReconciled.IsZero() {
		t.Fatalf("legacy coverage %+v %v", page, err)
	}
	key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err = raw.Put(t.Context(), key, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{}); err == nil {
		t.Fatal("corrupt body accepted")
	}
	page, err = s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("error became absence", page, err)
	}
	if err = raw.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	d, err := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{})
	if err == nil && d.Removed == 0 {
		d, err = s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{})
	}
	if err != nil || d.Removed != 1 {
		t.Fatalf("confirmed absent %+v %v", d, err)
	}
}

func TestReconciledWorkerRejectsDeletionDuringDownload(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if _, err := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	observed := &observingObjects{ObjectStore: raw, before: func() {
		key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		if err := raw.Delete(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}}
	p, err := s.Run(t.Context(), observed, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Committed != 0 || p.Pending != 0 {
		t.Fatalf("stale install %+v %v", p, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("deleted content installed %+v %v", page, err)
	}
	var staged int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("deleted staged data %d %v", staged, err)
	}
}

type upgradedSearchParser struct{ agentapi.TranscriptParser }

func (upgradedSearchParser) Version() string { return "synthetic-parser-upgrade" }

type upgradedSearchParsers struct{ agentapi.ParsersLookup }

func (p upgradedSearchParsers) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	parser, ok := p.ParsersLookup.LookupParser(name)
	if ok {
		return upgradedSearchParser{parser}, true
	}
	return parser, ok
}

func TestReconcileParserContractInvalidatesUnchangedRoot(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "session")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if _, err := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), a.remote, parsers, m.CapturedAt, Budget{VerifyCurrent: true}); err != nil {
		t.Fatal(err)
	}
	upgraded := upgradedSearchParsers{parsers}
	d, err := s.Reconcile(t.Context(), a.remote, upgraded, m.CapturedAt, ReconcileBudget{})
	if err != nil || d.Unchanged || d.Metadata != 0 {
		t.Fatalf("contract %+v %v", d, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 || page.Observation.Pending != 1 || page.Observation.Complete {
		t.Fatalf("old parser projection %+v %v", page, err)
	}
	if a.raw.sourceGets != 1 {
		t.Fatal("reconciliation parsed sources")
	}
}

func TestReconcileRetainedMembershipAndUnknownProvenance(t *testing.T) {
	t.Parallel()
	m, _, raw := workerHistory(t)
	putMetadata(t, raw, m)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if _, err := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Run(t.Context(), raw, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Loaded != 2 || p.Committed != 1 {
		t.Fatalf("history %+v %v", p, err)
	}
	m.History.Preserved[0].FilterVersion = ""
	m.History.Preserved[0].SourceSchemaVersion = 0
	putMetadata(t, raw, m)
	if _, err = s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(t.Context(), Query{Text: "old"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("unknown retained provenance %+v %v", page, err)
	}
	p, err = s.Run(t.Context(), raw, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Loaded != 2 || len(p.Gaps) != 0 {
		t.Fatalf("unknown provenance must be recovered from independently verified preserved bytes: %+v %v", p, err)
	}
	m.History.Preserved = nil
	putMetadata(t, raw, m)
	if _, err = s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	p, err = s.Run(t.Context(), raw, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Loaded != 0 || p.Committed != 1 {
		t.Fatalf("retention reuse %+v %v", p, err)
	}
}

func TestReconcileStaleSnapshotCannotInstallObservationOrBlockPendingWork(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "session")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	a.raw.beforeRead = func(key string) {
		if !strings.Contains(key, "metadata/") {
			return
		}
		a.raw.beforeRead = nil
		m.Name = "new remote title"
		a.publish(t, m)
	}
	d, err := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{})
	if !errors.Is(err, catalog.ErrStaleCursor) || d.CompletedWalk {
		t.Fatalf("stale %+v %v", d, err)
	}
	if _, err = s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	m.Name = "another root"
	a.publish(t, m)
	d, err = s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{Nodes: 1})
	if err == nil || d.CompletedWalk {
		t.Fatalf("expected bounded discovery failure %+v %v", d, err)
	}
	p, err := s.Run(t.Context(), a.remote, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Loaded != 1 || p.Committed != 1 {
		t.Fatalf("discovery failure starved known work %+v %v", p, err)
	}
}

func TestReconciledWorkerInvalidatesPrivacyChangedDuringDownload(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if _, err := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	observed := &observingObjects{ObjectStore: raw, before: func() { m.FilterVersion = "unknown"; putMetadata(t, raw, m) }}
	p, err := s.Run(t.Context(), observed, parsers, m.CapturedAt, Budget{VerifyCurrent: true, Sessions: 1})
	if err != nil || p.Committed != 0 || p.Pending != 1 {
		t.Fatalf("stale privacy %+v %v", p, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("stale privacy installed %+v %v", page, err)
	}
}

func BenchmarkReconcileColdAndWarm(b *testing.B) {
	a := newSearchArchive(b)
	var m archive.Metadata
	for i := range 40 {
		m = a.session(b, fmt.Sprintf("session-%03d", i))
	}
	parsers := builtin.NewBuiltins()
	b.Run("cold", func(b *testing.B) {
		var metadata, nodes, bytes int64
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			s, err := Open(b.Context(), b.TempDir(), "synthetic")
			if err != nil {
				b.Fatal(err)
			}
			d, err := s.Reconcile(b.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{Entries: 50})
			if err != nil || !d.CompletedWalk {
				b.Fatalf("%+v %v", d, err)
			}
			metadata += int64(d.Metadata)
			nodes += int64(d.Nodes)
			bytes += d.Bytes
			if err = s.Close(); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(metadata)/float64(b.N), "metadata-GET/op")
		b.ReportMetric(float64(nodes)/float64(b.N), "nodes/op")
		b.ReportMetric(float64(bytes)/float64(b.N), "discovery-bytes/op")
	})
	b.Run("warm", func(b *testing.B) {
		s, err := Open(b.Context(), b.TempDir(), "synthetic")
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if _, err = s.Reconcile(b.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{Entries: 50}); err != nil {
			b.Fatal(err)
		}
		var metadata, nodes, bytes int64
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			d, err := s.Reconcile(b.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{})
			if err != nil || !d.Unchanged {
				b.Fatalf("%+v %v", d, err)
			}
			metadata += int64(d.Metadata)
			nodes += int64(d.Nodes)
			bytes += d.Bytes
		}
		b.ReportMetric(float64(metadata)/float64(b.N), "metadata-GET/op")
		b.ReportMetric(float64(nodes)/float64(b.N), "nodes/op")
		b.ReportMetric(float64(bytes)/float64(b.N), "discovery-bytes/op")
	})
	if a.raw.sourceGets != 0 || a.raw.lists != 0 {
		b.Fatal("discovery performed source GETs or physical LIST")
	}
}

func TestReconcileRemovesNamedObservationWithoutPriorDiscovery(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if err := s.Observe(t.Context(), m, parsers, m.CapturedAt, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), raw, parsers, m.CapturedAt, Budget{VerifyCurrent: true}); err != nil {
		t.Fatal(err)
	}
	key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err := raw.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	d, err := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{})
	if err != nil || d.Removed != 1 {
		t.Fatalf("named removal %+v %v", d, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("stranded named result %+v %v", page, err)
	}
}

func TestReconcileOldSchemaIdentitiesResumeBoundedlyAfterUpgrade(t *testing.T) {
	t.Parallel()
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			t.Parallel()
			a := newSearchArchive(t)
			parsers := builtin.NewBuiltins()
			dir := t.TempDir()
			s, err := Open(t.Context(), dir, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			var last archive.Metadata
			for i := range 4 {
				last = a.session(t, fmt.Sprintf("session-%d", i))
				if err = s.Observe(t.Context(), last, parsers, last.CapturedAt, 0); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.Run(t.Context(), a.remote, parsers, last.CapturedAt, Budget{Sources: 8, VerifyCurrent: true}); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{"DROP TABLE search_discovery", "DROP TABLE search_observed", "DROP INDEX search_coverage_gaps"} {
				if _, err = s.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			if version == 1 {
				for _, statement := range []string{"DROP TABLE search_work", "DROP TABLE search_sources"} {
					if _, err = s.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err = s.db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			for i := range 4 {
				key, _ := archive.MetadataObjectKey("claude", fmt.Sprintf("session-%d", i))
				a.mutate(t, key, nil)
			}
			s, err = Open(t.Context(), dir, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.Reconcile(t.Context(), a.remote, parsers, last.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
			if err != nil || d.Removed > 1 {
				t.Fatalf("bounded upgrade %+v %v", d, err)
			}
			page, err := s.Search(t.Context(), Query{Text: "checksum"})
			if err != nil || page.Observation.Complete || page.Observation.IdentityChecksComplete {
				t.Fatalf("partial migration claimed coverage %+v %v", page, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(t.Context(), dir, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			for range 12 {
				if _, err = s.Reconcile(t.Context(), a.remote, parsers, last.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1}); err != nil {
					t.Fatal(err)
				}
			}
			page, err = s.Search(t.Context(), Query{Text: "checksum"})
			if err != nil || len(page.Hits) != 0 || !page.Observation.Complete || !page.Observation.IdentityChecksComplete {
				t.Fatalf("stranded upgraded rows %+v %v", page, err)
			}
			if a.raw.sourceGets != 4 {
				t.Fatalf("migration downloaded deleted sources %d", a.raw.sourceGets)
			}
		})
	}
}

func TestReconciledWorkerUsesFreshAuthorityBeyondCallerReadView(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "session")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if _, err := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	view := catalog.NewReadView(t.Context())
	if _, err := catalog.OpenSnapshot(view, a.remote, nil); err != nil {
		t.Fatal(err)
	}
	a.raw.beforeRead = func(key string) {
		if !strings.HasPrefix(key, "sessions/") {
			return
		}
		a.raw.beforeRead = nil
		metadataKey, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		a.mutate(t, metadataKey, nil)
	}
	p, err := s.Run(view, a.remote, parsers, m.CapturedAt, Budget{VerifyCurrent: true})
	if err != nil || p.Pending != 0 || p.Committed != 0 {
		t.Fatalf("reused stale caller authority %+v %v", p, err)
	}
	var staged int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("stale caller retained projections %d %v", staged, err)
	}
}

func TestReconcileDefaultPassCatchupOperations(t *testing.T) {
	a := newSearchArchive(t)
	var m archive.Metadata
	for i := range 40 {
		m = a.session(t, fmt.Sprintf("session-%03d", i))
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	started := time.Now()
	var total Discovery
	var sourceReads, sourceBytes, preinstallChecks, preinstallNodes int
	var preinstallBytes int64
	discoveredAt, completeAt := 0, 0
	for pass := 1; pass <= 12; pass++ {
		at := m.CapturedAt.Add(time.Duration(pass) * time.Second)
		d, err := s.Reconcile(t.Context(), a.remote, parsers, at, ReconcileBudget{})
		if err != nil {
			t.Fatal(err)
		}
		total.Pages += d.Pages
		total.Objects += d.Objects
		total.Metadata += d.Metadata
		total.Checks += d.Checks
		total.Nodes += d.Nodes
		total.Bytes += d.Bytes
		total.HeadReads += d.HeadReads
		total.CoordinatorReads += d.CoordinatorReads
		p, err := s.Run(t.Context(), a.remote, parsers, at, Budget{VerifyCurrent: true})
		if err != nil {
			t.Fatal(err)
		}
		if p.Loaded > 4 {
			t.Fatalf("source pass bound %+v", p)
		}
		sourceReads += p.Loaded
		sourceBytes += p.CompressedBytes
		preinstallChecks += p.MetadataChecks
		preinstallNodes += p.CatalogNodes
		preinstallBytes += p.MetadataBytes
		page, err := s.Search(t.Context(), Query{Text: "checksum"})
		if err != nil {
			t.Fatal(err)
		}
		if page.Observation.DiscoveryComplete && discoveredAt == 0 {
			discoveredAt = pass
		}
		if page.Observation.Complete {
			completeAt = pass
			break
		}
	}
	if discoveredAt != 2 || completeAt != 10 || sourceReads != 40 || total.Metadata != 40 || a.raw.sourceGets != 40 || a.raw.lists != 0 {
		t.Fatalf("default catchup discovered=%d complete=%d source=%d actual=%d discovery=%+v", discoveredAt, completeAt, sourceReads, a.raw.sourceGets, total)
	}
	t.Logf("40 sessions: discovered in %d default passes, indexed in %d; discovery=%+v; source GETs/parses=%d compressed=%d bytes; preinstall checks=%d nodes=%d bytes=%d; advisory elapsed=%s", discoveredAt, completeAt, total, sourceReads, sourceBytes, preinstallChecks, preinstallNodes, preinstallBytes, time.Since(started))
}

func TestMetadataPassBudgetExhaustionDoesNotBlockSourceWork(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if err := s.Observe(t.Context(), m, parsers, m.CapturedAt, 0); err != nil {
		t.Fatal(err)
	}
	key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	_, err := readBoundedMetadata(t.Context(), raw, key, 1)
	if !errors.Is(err, errMetadataBudget) || errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatalf("pass exhaustion classified as an impossible source: %v", err)
	}
	item, err := s.nextWork(t.Context(), m.CapturedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.failWork(t.Context(), item, m.CapturedAt, errMetadataBudget); err != nil {
		t.Fatal(err)
	}
	p, err := s.Run(t.Context(), raw, parsers, m.CapturedAt.Add(time.Minute), Budget{VerifyCurrent: true})
	if err != nil || p.Committed != 1 || p.Pending != 0 {
		t.Fatalf("budget exhaustion permanently blocked fitting work %+v %v", p, err)
	}
}

func TestReconciledWorkerRechecksAbsenceAfterIdentityRead(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "session")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if err := s.Observe(t.Context(), m, parsers, m.CapturedAt, 0); err != nil {
		t.Fatal(err)
	}
	key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	entry, _, err := a.remote.Writer.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	a.mutate(t, key, nil)
	restored := false
	a.raw.beforeRead = func(readKey string) {
		if !strings.Contains(readKey, "/nodes/") {
			return
		}
		a.raw.beforeRead = nil
		restored = true
		_, revision, e := a.remote.Writer.Find(t.Context(), key)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = a.remote.Writer.Commit(t.Context(), catalog.CatalogMutation{ID: "restored", SessionKey: key, ExpectedRevision: revision, Next: entry}); e != nil {
			t.Fatal(e)
		}
	}
	p, err := s.Run(t.Context(), a.remote, parsers, m.CapturedAt, Budget{VerifyCurrent: true, Sessions: 1})
	if err != nil || !restored || p.Committed != 0 || p.Pending != 1 {
		t.Fatalf("stale absence %+v restored=%v %v", p, restored, err)
	}
}

func TestReconcileUnreadableRecentMetadataDoesNotStarveCatchup(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	newest := a.session(t, "session-0")
	// Equal capture times use the catalog's stable identity tie order.
	for i := 1; i < 6; i++ {
		_ = a.session(t, fmt.Sprintf("session-%d", i))
	}
	key, _ := archive.MetadataObjectKey(newest.Harness.Name, newest.SessionID)
	snapshot, err := catalog.OpenSnapshot(t.Context(), a.remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := snapshot.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := a.raw.Get(t.Context(), entry.Metadata.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.raw.Delete(t.Context(), entry.Metadata.Key); err != nil {
		t.Fatal(err)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for range 12 {
		if _, err = s.Reconcile(t.Context(), a.remote, parsers, newest.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1}); err == nil {
			t.Fatal("unreadable recent metadata was not reported")
		}
	}
	var count int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&count); err != nil || count != 5 {
		t.Fatalf("unreadable recent metadata froze catchup: %d %v", count, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || page.Observation.DiscoveryComplete {
		t.Fatalf("unreadable sweep claimed complete: %+v %v", page.Observation, err)
	}
	if err = a.raw.Put(t.Context(), entry.Metadata.Key, body); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err = s.Reconcile(t.Context(), a.remote, parsers, newest.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Run(t.Context(), a.remote, parsers, newest.CapturedAt, Budget{Sources: 8, VerifyCurrent: true}); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 6 || !page.Observation.Complete {
		t.Fatalf("repaired metadata did not recover: %+v %v", page, err)
	}
}

func TestReconcileUnreadableKnownMetadataDoesNotStarveOtherIdentities(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	bad := a.session(t, "a-known")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if err := s.Observe(t.Context(), bad, parsers, bad.CapturedAt, 0); err != nil {
		t.Fatal(err)
	}
	bad.Name = "changed remote metadata"
	a.publish(t, bad)
	key, _ := archive.MetadataObjectKey(bad.Harness.Name, bad.SessionID)
	entry, _, err := a.remote.Writer.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.raw.Delete(t.Context(), entry.Metadata.Key); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		_ = a.session(t, fmt.Sprintf("new-%d", i))
	}
	for range 12 {
		_, _ = s.Reconcile(t.Context(), a.remote, parsers, bad.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
	}
	var count int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work WHERE id!=?", bad.Harness.Name+"/"+bad.SessionID).Scan(&count); err != nil || count != 6 {
		t.Fatalf("unreadable known metadata froze other identities: %d %v", count, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || page.Observation.Complete || page.Observation.IdentityChecksComplete {
		t.Fatalf("failed known validation claimed complete: %+v %v", page.Observation, err)
	}
}

func TestReconcileCorruptLegacyMetadataDoesNotStarveOtherObjects(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	if err := raw.Put(t.Context(), "sessions/000/metadata.json", []byte("broken metadata")); err != nil {
		t.Fatal(err)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for range 12 {
		_, _ = s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
	}
	var count int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&count); err != nil || count != 1 {
		t.Fatalf("corrupt legacy metadata froze other objects: %d %v", count, err)
	}
}

func TestReconcileWarmViewRechecksHeadAfterKnownIdentityRead(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "session")
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if _, err := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{}); err != nil {
		t.Fatal(err)
	}
	changed := false
	a.raw.beforeRead = func(key string) {
		if !strings.Contains(key, "/nodes/") {
			return
		}
		a.raw.beforeRead = nil
		changed = true
		m.Name = "new remote title"
		a.publish(t, m)
	}
	d, err := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{})
	if !changed || !errors.Is(err, catalog.ErrStaleCursor) || d.Unchanged || d.CompletedWalk {
		t.Fatalf("moving warm view accepted: %+v changed=%v %v", d, changed, err)
	}
}

type failingMetadataBody struct{ *storagetest.MemoryStore }

func (s failingMetadataBody) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if limit < 100 {
		return nil, "", storage.ErrObjectTooLarge
	}
	return []byte(strings.Repeat("x", 100)), "", errors.New("synthetic partial metadata read")
}

func TestReconcileChargesFailedLegacyMetadataReadBytes(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if err := s.Observe(t.Context(), m, parsers, m.CapturedAt, 0); err != nil {
		t.Fatal(err)
	}
	d, err := s.Reconcile(t.Context(), failingMetadataBody{raw}, parsers, m.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1, Bytes: 150})
	if !errors.Is(err, errMetadataBudget) || d.Bytes != 100 || d.Metadata != 2 {
		t.Fatalf("failed read escaped aggregate metadata budget: %+v %v", d, err)
	}
}

func TestReconcileOversizedMetadataDoesNotStarveOtherDiscovery(t *testing.T) {
	t.Parallel()
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known-%v", known), func(t *testing.T) {
			t.Parallel()
			a := newSearchArchive(t)
			bad := a.session(t, "a-bad")
			s := openTest(t)
			parsers := builtin.NewBuiltins()
			if known {
				if err := s.Observe(t.Context(), bad, parsers, bad.CapturedAt, 0); err != nil {
					t.Fatal(err)
				}
			}
			key, _ := archive.MetadataObjectKey(bad.Harness.Name, bad.SessionID)
			entry, _, err := a.remote.Writer.Find(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			a.raw.streamOversized = entry.Metadata.Key
			for i := range 6 {
				_ = a.session(t, fmt.Sprintf("good-%d", i))
			}
			for pass := range 32 {
				if pass%3 == 0 {
					_ = a.session(t, fmt.Sprintf("z-growing-%d", pass))
				}
				d, _ := s.Reconcile(t.Context(), a.remote, parsers, bad.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
				if d.Bytes > 16<<20 || d.Nodes > 512 {
					t.Fatalf("aggregate exceeded: %+v", d)
				}
			}
			var count int
			if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work WHERE id LIKE ?", bad.Harness.Name+"/good-%").Scan(&count); err != nil || count != 6 {
				t.Fatalf("oversized body froze unrelated discovery: %d %v", count, err)
			}
			page, err := s.Search(t.Context(), Query{Text: "checksum"})
			if err != nil || page.Observation.DiscoveryComplete || page.Observation.IdentityChecksComplete {
				t.Fatalf("oversized body claimed complete: %+v %v", page.Observation, err)
			}
		})
	}
}

func TestReconcileBorrowsUnusedCapacityForLargeMetadata(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "large")
	key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	entry, _, err := a.remote.Writer.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.raw.Get(t.Context(), entry.Metadata.Key)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(strings.Repeat(" ", 14<<20))...)
	ref, err := a.remote.Writer.PutImmutable(t.Context(), catalog.KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	entry.Metadata = ref
	a.mutate(t, key, entry)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for range 4 {
		d, _ := s.Reconcile(t.Context(), a.remote, parsers, m.CapturedAt, ReconcileBudget{})
		if d.Bytes > 16<<20 || d.Nodes > 512 {
			t.Fatalf("aggregate exceeded: %+v", d)
		}
	}
	var count int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&count); err != nil || count != 1 {
		t.Fatalf("fitting large metadata never hydrated: %d %v", count, err)
	}
}

func TestReconcileLegacyOversizedBodyResumesInsidePage(t *testing.T) {
	t.Parallel()
	m, _, raw := verifiedFixture(t, "claude", workerTranscript)
	if err := raw.Put(t.Context(), "sessions/000/metadata.json", []byte(strings.Repeat("x", 17<<20))); err != nil {
		t.Fatal(err)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for range 5 {
		d, _ := s.Reconcile(t.Context(), raw, parsers, m.CapturedAt, ReconcileBudget{})
		if d.Bytes > 16<<20 || d.Objects > 32 {
			t.Fatalf("aggregate exceeded: %+v", d)
		}
	}
	var count int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy oversized body froze later same-page metadata: %d %v", count, err)
	}
}

func TestReconcileRecentRotationAdvancesPastOversizedFirstBody(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	bad := a.session(t, "000-bad")
	key, _ := archive.MetadataObjectKey(bad.Harness.Name, bad.SessionID)
	entry, _, err := a.remote.Writer.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	a.raw.streamOversized = entry.Metadata.Key
	for i := range 32 {
		_ = a.session(t, fmt.Sprintf("good-%02d", i))
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for range 5 {
		d, _ := s.Reconcile(t.Context(), a.remote, parsers, bad.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
		if d.Bytes > 16<<20 {
			t.Fatalf("overflow escaped budget: %+v", d)
		}
	}
	for i := range 3 {
		var count int
		id := fmt.Sprintf("claude/good-%02d", i)
		if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work WHERE id=?", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("recent body %s starved behind oversized first body: %d %v", id, count, err)
		}
	}
}

func TestReconcileMetadataTimeoutAdvancesHistoryAndRecentWithLiveParent(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	bad := a.session(t, "000-bad")
	key, _ := archive.MetadataObjectKey(bad.Harness.Name, bad.SessionID)
	entry, _, err := a.remote.Writer.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	parent, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	failed := 0
	a.raw.readError = func(ctx context.Context, key string) error {
		if key != entry.Metadata.Key {
			return nil
		}
		childDeadline, ok := ctx.Deadline()
		// Reconcile caps the minute-long parent at20s, then reserves500ms.
		if !ok || !childDeadline.Before(deadline) || parent.Err() != nil {
			t.Fatal("metadata timeout lacks a live, longer parent")
		}
		failed++
		return context.DeadlineExceeded
	}
	for i := range 16 {
		_ = a.session(t, fmt.Sprintf("good-%02d", i))
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	for pass := range 56 {
		_, _ = s.Reconcile(parent, a.remote, parsers, bad.CapturedAt, ReconcileBudget{Entries: 1, Checks: 1})
		if pass == 4 {
			var count int
			if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work WHERE id IN ('claude/good-00','claude/good-01','claude/good-02')").Scan(&count); err != nil || count != 3 {
				t.Fatalf("recent timeout hid other recent rows: %d %v", count, err)
			}
		}
	}
	var count int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&count); err != nil || count != 16 || failed == 0 || parent.Err() != nil {
		t.Fatalf("timeout pinned historical discovery: rows=%d failures=%d parent=%v err=%v", count, failed, parent.Err(), err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || page.Observation.DiscoveryComplete {
		t.Fatalf("timeout certified complete: %+v %v", page.Observation, err)
	}
}

func TestDiscoveryMetadataDeadlineReservesCheckpointTime(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(10 * time.Second)
	parent, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	child, finish, err := discoveryMetadataContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	got, _ := child.Deadline()
	if !got.Equal(deadline.Add(-500 * time.Millisecond)) {
		t.Fatalf("metadata deadline=%v, parent=%v", got, deadline)
	}
	short, stop := context.WithDeadline(t.Context(), time.Now().Add(100*time.Millisecond))
	defer stop()
	if _, _, err = discoveryMetadataContext(short); !errors.Is(err, errMetadataBudget) {
		t.Fatalf("started metadata without checkpoint reserve: %v", err)
	}
}
