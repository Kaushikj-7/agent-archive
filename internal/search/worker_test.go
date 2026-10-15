package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const workerTranscript = `{"type":"user","uuid":"u1","message":{"role":"user","content":"checksum verification"}}` + "\n"

func TestWorkerReusesMetadataAndResumesAfterRestart(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	dir := t.TempDir()
	s, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	at := metadata.CapturedAt
	parsers := builtin.NewBuiltins()
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || progress.Loaded != 1 || progress.Committed != 1 || progress.Pending != 0 || counted.gets != 1 {
		t.Fatalf("%+v gets=%d %v", progress, counted.gets, err)
	}
	metadata.Branch = "renamed"
	metadata.RepoKey = "renamed-repository"
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || progress.Loaded != 0 || progress.Reused != 0 || progress.Writes.Inserted != 0 || progress.Committed != 1 || counted.gets != 1 {
		t.Fatalf("metadata-only %+v gets=%d %v", progress, counted.gets, err)
	}
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || progress.Committed != 0 || progress.Pending != 0 {
		t.Fatalf("duplicate %+v %v", progress, err)
	}
}

type observingObjects struct {
	storage.ObjectStore
	before func()
}

func (o *observingObjects) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if o.before != nil {
		callback := o.before
		o.before = nil
		callback()
	}
	return o.ObjectStore.(storage.LimitedGetter).GetLimited(ctx, key, limit)
}

func TestWorkerRejectsSupersededGenerationOutsideTransactions(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	observed := &observingObjects{ObjectStore: objects, before: func() {
		metadata.Name = "new desired title"
		if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
			t.Fatal(err)
		}
	}}
	progress, err := s.Run(t.Context(), observed, parsers, at, Budget{Sessions: 1})
	if err != nil || progress.Committed != 0 || progress.Pending != 1 {
		t.Fatalf("stale %+v %v", progress, err)
	}
	var sources int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("stale source staged: %d %v", sources, err)
	}
	progress, err = s.Run(t.Context(), objects, parsers, at, Budget{})
	if err != nil || progress.Committed != 1 {
		var due, gen int64
		var desired, indexed string
		_ = s.db.QueryRowContext(t.Context(), "SELECT due,generation,desired,indexed FROM search_work").Scan(&due, &gen, &desired, &indexed)
		t.Fatalf("new desired %+v %v due=%d at=%d gen=%d desired=%s indexed=%s", progress, err, due, at.UnixNano(), gen, desired, indexed)
	}
	page, err := s.Search(t.Context(), Query{Text: "desired"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
}

type failingObjects struct{ storage.ObjectStore }

func (o failingObjects) GetLimited(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("synthetic secret must not be persisted")
}

func TestWorkerBackoffCoalescingAndCancellation(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	progress, err := s.Run(t.Context(), failingObjects{objects}, parsers, at, Budget{})
	if err != nil || progress.Failed != 1 || progress.Pending != 1 {
		t.Fatalf("%+v %v", progress, err)
	}
	progress, err = s.Run(t.Context(), objects, parsers, at, Budget{})
	if err != nil || progress.Loaded != 0 {
		t.Fatalf("backoff bypassed %+v %v", progress, err)
	}
	progress, err = s.Run(t.Context(), objects, parsers, at.Add(time.Second), Budget{})
	if err != nil || progress.Committed != 1 {
		t.Fatalf("retry %+v %v", progress, err)
	}
	metadata.Name = "latest title"
	if err = s.Observe(t.Context(), metadata, parsers, at.Add(2*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	metadata.Name = "coalesced title"
	if err = s.Observe(t.Context(), metadata, parsers, at.Add(3*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	progress, err = s.Run(t.Context(), objects, parsers, at.Add(59*time.Second), Budget{})
	if err != nil || progress.Committed != 0 || progress.Pending != 1 {
		t.Fatalf("interval %+v %v", progress, err)
	}
	progress, err = s.Run(t.Context(), objects, parsers, at.Add(62*time.Second), Budget{})
	if err != nil || progress.Committed != 1 || progress.Loaded != 0 {
		t.Fatalf("coalesced %+v %v", progress, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.Run(ctx, objects, parsers, at, Budget{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWorkerPrivacyInvalidatesBeforeUnavailableReplacement(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
		t.Fatal(err)
	}
	metadata.FilterVersion = ""
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("old privacy text served %+v %v", page, err)
	}
}

// Restoring a previous snapshot must not acknowledge rows an observation removed.
func TestWorkerRestoredSnapshotRemainsPendingAfterInvalidation(t *testing.T) {
	t.Parallel()
	for _, invalidation := range []string{"membership", "privacy"} {
		t.Run(invalidation, func(t *testing.T) {
			t.Parallel()
			metadata, _, objects := workerHistory(t)
			s := openTest(t)
			parsers := builtin.NewBuiltins()
			at := metadata.CapturedAt
			if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
				t.Fatal(err)
			}
			removed := metadata
			if invalidation == "membership" {
				history := *metadata.History
				removed.History = &history
				removed.History.Preserved = nil
			} else {
				removed.FilterVersion = ""
			}
			if err := s.Observe(t.Context(), removed, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			progress, err := s.Run(t.Context(), objects, parsers, at, Budget{})
			if err != nil || progress.Committed != 1 || progress.Pending != 0 {
				t.Fatalf("restored snapshot was mistaken for installed state: %+v %v", progress, err)
			}
			page, err := s.Search(t.Context(), Query{Text: "old"})
			if err != nil || len(page.Hits) != 1 {
				t.Fatalf("restored session remained absent: %+v %v", page, err)
			}
		})
	}
}

func TestWorkerOversizeHasFiniteBlockedState(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{CompressedBytes: 1})
	if err != nil || progress.Failed != 1 || progress.Pending != 1 || counted.gets != 0 {
		t.Fatalf("%+v %v", progress, err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at.Add(time.Hour), Budget{})
	if err != nil || progress.Loaded != 0 {
		t.Fatalf("blocked source retried %+v %v", progress, err)
	}
	// A new authoritative revision clears the blocked state.
	metadata.Name = "changed"
	if err = s.Observe(t.Context(), metadata, parsers, at.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at.Add(time.Hour), Budget{})
	if err != nil || progress.Committed != 1 {
		t.Fatalf("unblocked %+v %v", progress, err)
	}
}

func BenchmarkWorkerMetadataRefresh(b *testing.B) {
	metadata, _, objects := verifiedFixture(b, "claude", workerTranscript)
	s := openTest(b)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(b.Context(), metadata, parsers, at, 0); err != nil {
		b.Fatal(err)
	}
	if _, err := s.Run(b.Context(), objects, parsers, at, Budget{}); err != nil {
		b.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	b.ResetTimer()
	for i := range b.N {
		metadata.RepoKey = strconv.Itoa(i + 1)
		if err := s.Observe(b.Context(), metadata, parsers, at, 0); err != nil {
			b.Fatal(err)
		}
		progress, err := s.Run(b.Context(), counted, parsers, at, Budget{})
		if err != nil || progress.Loaded != 0 || progress.Writes.Inserted != 0 {
			b.Fatalf("%+v %v", progress, err)
		}
	}
	b.ReportMetric(float64(counted.gets)/float64(b.N), "source-GET/op")
}

func workerHistory(tb testing.TB) (archive.Metadata, archive.SourceBundle, *storagetest.MemoryStore) {
	tb.Helper()
	const thread = "11111111-1111-4111-8111-111111111111"
	const previous = "22222222-2222-4222-8222-222222222222"
	const current = "33333333-3333-4333-8333-333333333333"
	_, old, _ := verifiedFixture(tb, "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"old checksum"}}`)
	old.SchemaVersion = archive.HistorySourceSchemaVersion
	old.NativeSessionID = thread
	old.Ordinals = []uint64{0}
	old.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: previous, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: previous, FirstRecord: 0, EndRecord: 1, StartOrdinal: 0, EndOrdinal: 1}}}
	oldMetadata, _, oldObjects := publishFixture(tb, old, codex.Parser{})
	active := old
	active.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: current, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: current, FirstRecord: 0, EndRecord: 1, StartOrdinal: 0, EndOrdinal: 1}}}
	active.NativeRecords = []map[string]any{{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": "active checksum"}}}
	metadata, _, objects := publishFixture(tb, active, codex.Parser{})
	metadata.History.Preserved = []archive.RevisionReference{{RevisionID: previous, CapturedAt: old.Capture.CapturedAt, Source: oldMetadata.SourceBundle, SourceSchemaVersion: old.SchemaVersion, FilterVersion: old.Capture.FilterVersion}}
	data, err := oldObjects.Get(context.Background(), oldMetadata.SourceBundle.Key)
	if err != nil {
		tb.Fatal(err)
	}
	if err = objects.Put(context.Background(), oldMetadata.SourceBundle.Key, data); err != nil {
		tb.Fatal(err)
	}
	return metadata, active, objects
}

func TestWorkerStagesOneSourceAndReusesPreservedHistory(t *testing.T) {
	t.Parallel()
	metadata, active, objects := workerHistory(t)
	dir := t.TempDir()
	s, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	at := metadata.CapturedAt
	parsers := builtin.NewBuiltins()
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{Sources: 1})
	if err != nil || progress.Loaded != 1 || progress.Committed != 0 || progress.Pending != 1 {
		t.Fatalf("partial %+v %v", progress, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("partial membership published %+v %v", page, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	progress, err = s.Run(t.Context(), counted, parsers, at, Budget{Sources: 1})
	if err != nil || progress.Loaded != 1 || progress.Reused != 1 || progress.Committed != 1 || counted.gets != 2 {
		t.Fatalf("restart %+v gets=%d %v", progress, counted.gets, err)
	}
	active.NativeRecords[0]["payload"] = map[string]any{"type": "message", "role": "user", "content": "changed checksum"}
	changed, _, changedObjects := publishFixture(t, active, codex.Parser{})
	changed.History.Preserved = metadata.History.Preserved
	refs, _ := metadata.SourceReferences()
	for _, ref := range refs {
		data, e := objects.Get(t.Context(), ref.Key)
		if e != nil {
			t.Fatal(e)
		}
		if e = changedObjects.Put(t.Context(), ref.Key, data); e != nil {
			t.Fatal(e)
		}
	}
	if err = s.Observe(t.Context(), changed, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted = &countedObjects{ObjectStore: changedObjects}
	progress, err = s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || progress.Loaded != 1 || progress.Reused != 1 || progress.Committed != 1 || counted.gets != 1 {
		t.Fatalf("preserved reprocessed %+v %v", progress, err)
	}
	page, err = s.Search(t.Context(), Query{Text: "old"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("preserved lost %+v %v", page, err)
	}
}

func TestWorkerSupersededSessionDoesNotStarveOtherSessions(t *testing.T) {
	t.Parallel()
	metadata, seed, objects := verifiedFixture(t, "claude", workerTranscript)
	seed.ArchiveSessionID = "another"
	other, _, otherObjects := publishFixture(t, seed, claude.Parser{})
	data, err := otherObjects.Get(t.Context(), other.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.Put(t.Context(), other.SourceBundle.Key, data); err != nil {
		t.Fatal(err)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	// "another" sorts before "session"; supersede it while its source is read.
	for _, m := range []archive.Metadata{other, metadata} {
		if err = s.Observe(t.Context(), m, parsers, at, 0); err != nil {
			t.Fatal(err)
		}
	}
	observed := &observingObjects{ObjectStore: objects, before: func() {
		other.Name = "changed during read"
		if e := s.Observe(t.Context(), other, parsers, at, 0); e != nil {
			t.Fatal(e)
		}
	}}
	progress, err := s.Run(t.Context(), observed, parsers, at, Budget{Sources: 2, Sessions: 2})
	if err != nil || progress.Committed != 1 || progress.Pending != 1 {
		t.Fatalf("fairness %+v %v", progress, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].SessionID != metadata.SessionID {
		t.Fatalf("other session starved %+v %v", page, err)
	}
}

func TestWorkerChangedSourceWithSameMessagesAvoidsFTSRewrite(t *testing.T) {
	t.Parallel()
	metadata, bundle, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	at := metadata.CapturedAt
	parsers := builtin.NewBuiltins()
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
		t.Fatal(err)
	}
	bundle.Capture.CapturedAt = at.Add(time.Second)
	changed, _, changedObjects := publishFixture(t, bundle, claude.Parser{})
	if err := s.Observe(t.Context(), changed, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	progress, err := s.Run(t.Context(), changedObjects, parsers, at, Budget{})
	if err != nil || progress.Loaded != 1 || progress.Writes.Inserted != 0 || progress.Writes.Deleted != 0 {
		t.Fatalf("unchanged text rewritten %+v %v", progress, err)
	}
	var data []byte
	if err = s.db.QueryRowContext(t.Context(), "SELECT reference FROM search_passages WHERE title_hit=0 LIMIT 1").Scan(&data); err != nil {
		t.Fatal(err)
	}
	var ref Reference
	if err = json.Unmarshal(data, &ref); err != nil || ref.Source.Reference != changed.SourceBundle {
		t.Fatalf("reference not replaced %+v %v", ref, err)
	}

}

func BenchmarkWorkerGrowingSession(b *testing.B) {
	for _, coalesce := range []bool{false, true} {
		name := "every-publication"
		if coalesce {
			name = "coalesced"
		}
		b.Run(name, func(b *testing.B) {
			parsers := builtin.NewBuiltins()
			var snapshots []archive.Metadata
			objects := storagetest.NewMemoryStore()
			raw := ""
			for i := range 10 {
				raw += fmt.Sprintf(`{"type":"user","uuid":"u%d","message":{"role":"user","content":"checksum record %d"}}`+"\n", i, i)
				_, bundle, _ := verifiedFixture(b, "claude", raw)
				bundle.Capture.CapturedAt = bundle.Capture.CapturedAt.Add(time.Duration(i) * time.Second)
				metadata, _, sourceObjects := publishFixture(b, bundle, claude.Parser{})
				data, err := sourceObjects.Get(b.Context(), metadata.SourceBundle.Key)
				if err != nil {
					b.Fatal(err)
				}
				if err = objects.Put(b.Context(), metadata.SourceBundle.Key, data); err != nil {
					b.Fatal(err)
				}
				snapshots = append(snapshots, metadata)
			}
			totalGets := 0
			totalBytes := 0
			totalWrites := 0
			resources := map[string]float64{}
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				dir := b.TempDir()
				s, err := Open(b.Context(), dir, "test/context")
				if err != nil {
					b.Fatal(err)
				}
				resources["initial-disk_B"] += float64(benchmarkDiskBytes(b, dir))
				counted := &countedObjects{ObjectStore: objects}
				at := snapshots[0].CapturedAt
				b.StartTimer()
				for i, metadata := range snapshots {
					if err := s.Observe(b.Context(), metadata, parsers, at, 0); err != nil {
						b.Fatal(err)
					}
					// Every new source revision prunes the previous projection before
					// replacement. These inspection reads are outside the work timer.
					b.StopTimer()
					var cacheRows int
					if err := s.db.QueryRowContext(b.Context(), "SELECT count(*) FROM search_sources").Scan(&cacheRows); err != nil || cacheRows != 0 {
						b.Fatalf("old source cache retained after revision observation: rows%d err%v", cacheRows, err)
					}
					if !coalesce && i > 0 {
						resources["pruned-revisions/lifetime"]++
					}
					b.StartTimer()
					if !coalesce {
						progress, err := s.Run(b.Context(), counted, parsers, at, Budget{})
						if err != nil {
							b.Fatal(err)
						}
						totalWrites += progress.Writes.Inserted + progress.Writes.Deleted
					}
				}
				if coalesce {
					progress, err := s.Run(b.Context(), counted, parsers, at, Budget{})
					if err != nil || progress.Failed != 0 {
						b.Fatalf("%+v %v", progress, err)
					}
					totalWrites += progress.Writes.Inserted + progress.Writes.Deleted
				}
				b.StopTimer()
				var cached, payload, staged, members, sessions, work, pending int
				if err := s.db.QueryRowContext(b.Context(), `SELECT
 (SELECT count(*) FROM search_sources),
 (SELECT coalesce(sum(length(projection)),0) FROM search_sources),
 (SELECT count(*) FROM search_sources c JOIN search_work w ON w.id=c.session WHERE w.desired!=w.indexed),
 (SELECT count(*) FROM search_passages WHERE title_hit=0),
 (SELECT count(*) FROM search_sessions),
 (SELECT count(*) FROM search_work),
 (SELECT count(*) FROM search_work WHERE desired!=indexed)`).Scan(&cached, &payload, &staged, &members, &sessions, &work, &pending); err != nil {
					b.Fatal(err)
				}
				if cached != 1 || staged != 0 || members != 10 || sessions != 1 || work != 1 || pending != 0 {
					b.Fatalf("settled cache/membership/work differ: cache%d staged%d members%d sessions%d work%d pending%d", cached, staged, members, sessions, work, pending)
				}
				resources["final-disk_B"] += float64(benchmarkDiskBytes(b, dir))
				resources["projection-cache_B"] += float64(payload)
				resources["projection-cache-rows"] += float64(cached)
				resources["uninstalled-cache-rows"] += float64(staged)
				resources["body-memberships"] += float64(members)
				resources["indexed-sessions"] += float64(sessions)
				resources["work-rows"] += float64(work)
				resources["pending-work-rows"] += float64(pending)
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
				totalGets += counted.gets
				totalBytes += counted.bytes
				b.StartTimer()
			}
			for name, total := range resources {
				b.ReportMetric(total/float64(b.N), name)
			}
			b.ReportMetric(float64(totalGets)/float64(b.N), "source-GET/lifetime")
			b.ReportMetric(float64(totalBytes)/float64(b.N), "compressed-B/lifetime")
			b.ReportMetric(float64(totalWrites)/float64(b.N), "FTS-writes/lifetime")
		})
	}
}

func TestWorkerFailedCommitKeepsStagedSourcesAndOldView(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash/failure at the indexed-state write after passage writes.
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER stop_commit BEFORE UPDATE OF indexed ON search_work BEGIN SELECT RAISE(ABORT,'synthetic interrupted commit'); END`); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || progress.Failed != 1 || progress.Committed != 0 || progress.Pending != 1 {
		t.Fatalf("%+v %v", progress, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("partial transaction visible %+v %v", page, err)
	}
	if _, err = s.db.ExecContext(t.Context(), "DROP TRIGGER stop_commit"); err != nil {
		t.Fatal(err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at.Add(time.Second), Budget{})
	if err != nil || progress.Committed != 1 || progress.Loaded != 0 || counted.gets != 1 {
		t.Fatalf("retry reparsed source %+v %v", progress, err)
	}
}

func TestWorkerBusyWriterLeavesRecoverableDesiredState(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	dir := t.TempDir()
	s, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	other, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	tx, err := other.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(t.Context(), "UPDATE search_state SET generation=generation+1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Run(t.Context(), objects, parsers, at, Budget{}); err == nil {
		t.Fatal("busy writer accepted work")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	progress, err := s.Run(t.Context(), objects, parsers, at, Budget{})
	if err != nil || progress.Committed != 1 || progress.Pending != 0 {
		t.Fatalf("busy state not recoverable %+v %v", progress, err)
	}
}

func TestWorkerCompletedArrivalsDoNotStarveOlderActiveWork(t *testing.T) {
	t.Parallel()
	metadata, bundle, objects := verifiedFixture(t, "claude", workerTranscript)
	metadata.State = archive.MetadataStateActive
	at := metadata.CapturedAt
	bundle.ArchiveSessionID = "new-completed"
	bundle.Capture.CapturedAt = at.Add(time.Second)
	newer, _, newObjects := publishFixture(t, bundle, claude.Parser{})
	newer.State = archive.MetadataStateClosed
	data, err := newObjects.Get(t.Context(), newer.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.Put(t.Context(), newer.SourceBundle.Key, data); err != nil {
		t.Fatal(err)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(t.Context(), newer, parsers, at.Add(time.Second), 0); err != nil {
		t.Fatal(err)
	}
	progress, err := s.Run(t.Context(), objects, parsers, at.Add(time.Second), Budget{Sources: 1, Sessions: 1})
	if err != nil || progress.Committed != 1 || progress.Pending != 1 {
		t.Fatalf("%+v %v", progress, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].SessionID != metadata.SessionID {
		t.Fatalf("older active work starved %+v %v", page, err)
	}
}

func TestWorkLockSerializesStoreHandlesWithoutBlockingQueries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(t.Context(), dir, "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	release, err := first.LockWork(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = second.LockWork(t.Context()); !errors.Is(err, ErrWorkBusy) {
		t.Fatalf("overlap lock: %v", err)
	}
	if _, err = second.Search(t.Context(), Query{Text: "checksum"}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerDoesNotReuseSourcesAcrossReaderIdentityChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name   string
		mutate func(*archive.Metadata)
	}{
		{"native-session", func(m *archive.Metadata) { m.NativeSessionID = "different-native-session" }},
		{"project", func(m *archive.Metadata) { m.ProjectID = "different-project" }},
		{"parent", func(m *archive.Metadata) { m.ParentSessionID = "different-parent" }},
		{"capture-time", func(m *archive.Metadata) { m.CapturedAt = m.CapturedAt.Add(time.Second) }},
		{"producer", func(m *archive.Metadata) { m.Harness.Version = "different-producer" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			t.Parallel()
			metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
			s := openTest(t)
			parsers := builtin.NewBuiltins()
			at := metadata.CapturedAt
			if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
				t.Fatal(err)
			}
			change.mutate(&metadata)
			if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			counted := &countedObjects{ObjectStore: objects}
			progress, err := s.Run(t.Context(), counted, parsers, at, Budget{})
			if err != nil || progress.Loaded != 1 || progress.Committed != 0 || progress.Failed != 1 || counted.gets != 1 {
				t.Fatalf("reader identity mismatch bypassed %+v %v gets=%d", progress, err, counted.gets)
			}
		})
	}
}

// The active history source must retain reader's full producer identity checks.
func TestWorkerHistoryActiveProducerChangeRequiresVerification(t *testing.T) {
	t.Parallel()
	metadata, _, objects := workerHistory(t)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
		t.Fatal(err)
	}
	metadata.Harness.Version = "different-producer"
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{})
	if err != nil || progress.Loaded != 1 || progress.Committed != 0 || progress.Failed != 1 || counted.gets != 1 {
		t.Fatalf("active history identity bypassed verification: %+v gets=%d %v", progress, counted.gets, err)
	}
}

// A new active revision can be the same publication that tightens privacy.
func TestWorkerPrivacyChangeInvalidatesAcrossActiveRevisionRotation(t *testing.T) {
	t.Parallel()
	metadata, _, objects := workerHistory(t)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
		t.Fatal(err)
	}
	metadata.History.CurrentRevision = "44444444-4444-4444-8444-444444444444"
	metadata.FilterVersion = "6"
	if metadata.FilterVersion == archive.FilterVersion {
		metadata.FilterVersion = "7"
	}
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("old privacy text served across rotation: %+v %v", page, err)
	}
}

func TestWorkerDecompressedOversizeStaysBlocked(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{Limits: reader.Limits{MaxUncompressedBytes: 1}})
	if err != nil || progress.Failed != 1 || counted.gets != 1 {
		t.Fatalf("initial limit: %+v %v", progress, err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at.Add(time.Hour), Budget{Limits: reader.Limits{MaxUncompressedBytes: 1}})
	if err != nil || progress.Loaded != 0 || counted.gets != 1 {
		t.Fatalf("impossible unit retried: %+v gets=%d %v", progress, counted.gets, err)
	}
}

// Keep this memory-heavy real-reader/parser fixture sequential.
func TestWorkerCumulativeOversizeDiscardsStagingAndStaysBlocked(t *testing.T) {
	metadata, active, originalObjects := workerHistory(t)
	previous := metadata.History.Preserved[0]
	old, err := reader.LoadRevision(t.Context(), originalObjects, metadata, previous.RevisionID, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	active.NativeRecords[0]["payload"] = map[string]any{"type": "message", "role": "user", "content": strings.Repeat("x", 15<<20)}
	old.NativeRecords[0]["payload"] = map[string]any{"type": "message", "role": "user", "content": strings.Repeat("y", 15<<20)}
	metadata, _, objects := publishFixture(t, active, codex.Parser{})
	oldMetadata, _, oldObjects := publishFixture(t, old, codex.Parser{})
	previous.Source = oldMetadata.SourceBundle
	metadata.History.Preserved = []archive.RevisionReference{previous}
	data, err := oldObjects.Get(t.Context(), previous.Source.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.Put(t.Context(), previous.Source.Key, data); err != nil {
		t.Fatal(err)
	}
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	counted := &countedObjects{ObjectStore: objects}
	progress, err := s.Run(t.Context(), counted, parsers, at, Budget{Sources: 1})
	if err != nil || progress.Loaded != 1 || progress.Committed != 0 || progress.Failed != 0 {
		t.Fatalf("first source did not stage: %+v %v", progress, err)
	}
	var staged int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&staged); err != nil || staged != 1 {
		t.Fatalf("first staging: %d %v", staged, err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at, Budget{Sources: 1})
	if err != nil || progress.Loaded != 1 || progress.Reused != 1 || progress.Failed != 1 || progress.Committed != 0 {
		t.Fatalf("cumulative limit not enforced: %+v %v", progress, err)
	}
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("oversized staging retained: %d %v", staged, err)
	}
	progress, err = s.Run(t.Context(), counted, parsers, at.Add(time.Hour), Budget{})
	if err != nil || progress.Loaded != 0 || progress.Failed != 1 || counted.gets != 2 {
		t.Fatalf("impossible projection retried: %+v gets=%d %v", progress, counted.gets, err)
	}
}

func TestWorkerOversizeCleanupIsAtomicAndGenerationFenced(t *testing.T) {
	t.Parallel()
	metadata, _, objects := verifiedFixture(t, "claude", workerTranscript)
	s := openTest(t)
	parsers := builtin.NewBuiltins()
	at := metadata.CapturedAt
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil {
		t.Fatal(err)
	}
	metadata.Name = "pending title"
	if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	stale, err := s.nextWork(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Name = "newer pending title"
	if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.failWork(t.Context(), stale, at, storage.ErrObjectTooLarge); err != nil {
		t.Fatal(err)
	}
	current, err := s.nextWork(t.Context(), at)
	if err != nil {
		t.Fatalf("stale failure blocked newer work: %v", err)
	}
	var staged int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&staged); err != nil || staged != 1 {
		t.Fatalf("stale failure deleted newer cache: %d %v", staged, err)
	}
	if _, err = s.db.ExecContext(t.Context(), `CREATE TRIGGER stop_cleanup BEFORE DELETE ON search_sources BEGIN SELECT RAISE(ABORT,'synthetic interrupted cleanup'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.failWork(t.Context(), current, at, storage.ErrObjectTooLarge); err == nil {
		t.Fatal("interrupted cache cleanup succeeded")
	}
	if _, err = s.nextWork(t.Context(), at); err != nil {
		t.Fatalf("failed cleanup committed blocked state: %v", err)
	}
	if _, err = s.db.ExecContext(t.Context(), "DROP TRIGGER stop_cleanup"); err != nil {
		t.Fatal(err)
	}
	if err = s.failWork(t.Context(), current, at, storage.ErrObjectTooLarge); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sources").Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("blocked cache retained: %d %v", staged, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("oversize cleanup removed installed view: %+v %v", page, err)
	}
}
