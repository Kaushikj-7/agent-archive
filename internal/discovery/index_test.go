package discovery

import (
	"context"
	"database/sql"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func hintDatabase(tb testing.TB, root string, wal bool) *sql.DB {
	tb.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	tb.Cleanup(func() { _ = db.Close() })
	if wal {
		if _, err := db.ExecContext(tb.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0"); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := db.ExecContext(tb.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT,created_at_ms INTEGER,updated_at_ms INTEGER,preview TEXT); CREATE INDEX idx_threads_created_at_ms ON threads(created_at_ms DESC,id DESC); CREATE INDEX idx_threads_updated_at_ms ON threads(updated_at_ms DESC,id DESC)"); err != nil {
		tb.Fatal(err)
	}
	return db
}

func addHint(tb testing.TB, db *sql.DB, id, path string, at time.Time) {
	tb.Helper()
	if _, err := db.ExecContext(tb.Context(), "INSERT INTO threads(id,rollout_path,created_at_ms,updated_at_ms) VALUES(?,?,?,?)", id, path, at.UnixMilli(), at.UnixMilli()); err != nil {
		tb.Fatal(err)
	}
}

func TestIndexedHintFindsFreshTaskAheadOfLargeFlatOrSameDayHistory(t *testing.T) {
	t.Parallel()
	for _, folder := range []string{"sessions", "sessions/2026/10/01"} {
		t.Run(folder, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			project := cfg.Archive.Projects[0].Root
			for n := 1; n <= 1024; n++ {
				writeRollout(t, root, project, at.Add(-time.Hour), n, folder)
			}
			native := writeRollout(t, root, project, at.Add(time.Minute), 9000, folder)
			path := filepath.Join(root, folder, "rollout-2026-10-01T12-00-00-"+native+".jsonl")
			db := hintDatabase(t, root, false)
			addHint(t, db, native, path, at.Add(time.Minute))
			h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
			if err != nil || h.Registered != 1 || h.IndexQueries != 4 || h.IndexLocators != 1 || h.Probes > HeaderProbes || !h.Pending {
				t.Fatalf("hint or fair backlog lost: %#v %v", h, err)
			}
		})
	}
}

func TestIndexHintsNeverSupplyAuthorizationOrSourceSupport(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	id := writeRollout(t, root, project, at.Add(-time.Hour), 1, "sessions")
	db := hintDatabase(t, root, false)
	addHint(t, db, id, filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl"), at.Add(time.Minute))
	h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["start_not_authorized"] == 0 {
		t.Fatalf("index date authorized native start: %#v %v", h, err)
	}
	writeRollout(t, root, project, at.Add(time.Minute), 2, "sessions")
	h, err = runWithAdapters(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, []SourceAdapter{codexAdapter{supported: func(sourcefacts.CodexMeta) bool { return false }}})
	if err != nil || h.Supported || h.Registered != 0 || sourcefacts.SupportedCodexProducer(sourcefacts.CodexMeta{}) {
		t.Fatal("SQLite bypassed source compatibility checks", err)
	}
}

func TestIndexHintsRejectOutsideRootAndEscapingSources(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	outside := t.TempDir()
	native := writeRollout(t, outside, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	external := filepath.Join(outside, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	link := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	db := hintDatabase(t, root, false)
	addHint(t, db, "outside", external, at)
	addHint(t, db, "link", link, at)
	var refusals []error
	adapter := &diagnosticHintAdapter{codexAdapter: codexAdapter{supported: syntheticSupport}, refusals: &refusals}
	h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, []SourceAdapter{adapter})
	if err != nil || h.Registered != 0 || h.Probes != 0 || h.Outcomes["index_locator_rejected"] == 0 {
		t.Fatalf("unsafe source read: %#v %v; original hint refusals: %v", h, err, refusals)
	}
}

type indexFailureCase string

const (
	indexFailureMissing     indexFailureCase = "missing"
	indexFailureCorrupt     indexFailureCase = "corrupt"
	indexFailureNoIndex     indexFailureCase = "no_index"
	indexFailureWrongIndex  indexFailureCase = "wrong_index"
	indexFailureFifo        indexFailureCase = "fifo"
	indexFailureSideSymlink indexFailureCase = "side_symlink"
)

func TestIndexHintFallbackForMissingCorruptAndUnorderedIndexes(t *testing.T) {
	t.Parallel()
	for _, failure := range []indexFailureCase{indexFailureMissing, indexFailureCorrupt, indexFailureNoIndex, indexFailureWrongIndex, indexFailureFifo, indexFailureSideSymlink} {
		t.Run(string(failure), func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
			path := filepath.Join(root, "state_5.sqlite")
			switch failure {
			case indexFailureMissing:
				// A missing optional DB deliberately has no fixture.
			case indexFailureCorrupt:
				if err := os.WriteFile(path, []byte("not a database"), 0600); err != nil {
					t.Fatal(err)
				}
			case indexFailureFifo:
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case indexFailureNoIndex, indexFailureWrongIndex, indexFailureSideSymlink:
				db := hintDatabase(t, root, false)
				if failure == indexFailureSideSymlink {
					if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path+"-wal"); err != nil {
						t.Fatal(err)
					}
				} else {
					statement := "DROP INDEX idx_threads_created_at_ms"
					if failure == indexFailureWrongIndex {
						statement += "; CREATE INDEX idx_threads_created_at_ms ON threads(rollout_path)"
					}
					if _, err := db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
			if err != nil || h.Registered != 1 {
				t.Fatalf("optional index stopped enumeration: %#v %v", h, err)
			}
			if h.Outcomes["index_hints_missing"]+h.Outcomes["index_hints_unavailable"] == 0 {
				t.Fatal("hint failure not visible")
			}
		})
	}
}

func TestLiveWALHintsFallBackWithoutNativeWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	expected := filepath.Join(root, "sessions", "rollout-00000000-0000-0000-0000-000000000001.jsonl")
	addHint(t, db, "one", expected, time.Now())
	before := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		raw, err := os.ReadFile(filepath.Join(root, "state_5.sqlite") + suffix)
		if err != nil {
			t.Fatal(err)
		}
		before[suffix] = string(raw)
	}
	hints, queries, _, outcome := indexHints(context.Background(), root)
	if outcome != "index_hints_unavailable" || queries != 0 || len(hints) != 0 {
		t.Fatalf("live WAL did not fall back: %v %d %s", hints, queries, outcome)
	}
	for suffix, want := range before {
		raw, err := os.ReadFile(filepath.Join(root, "state_5.sqlite") + suffix)
		if err != nil || string(raw) != want {
			t.Fatalf("SQLite hint changed native %q: %v", suffix, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hints, _, _, outcome := indexHints(ctx, root); len(hints) != 0 || outcome != "index_hints_unavailable" {
		t.Fatal("cancelled query returned hints")
	}
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM threads").Scan(&count); err != nil || count != 1 {
		t.Fatal("hint reader damaged writer", err)
	}
}

func TestIndexHintsBoundReturnedLocators(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, false)
	addHint(t, db, "huge", strings.Repeat("x", 10000), time.Now())
	hints, _, _, outcome := indexHints(context.Background(), root)
	if outcome != "" || len(hints) != 0 {
		t.Fatal("oversized locator retained")
	}
}

func TestSettledIndexCancellationFallsBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_ = hintDatabase(t, root, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hints, _, _, outcome := indexHints(ctx, root); len(hints) != 0 || outcome != "index_hints_unavailable" {
		t.Fatal("cancelled query returned hints")
	}
}

func TestSettledIndexHintsLeaveDatabaseAndDirectoryUnchanged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, false)
	addHint(t, db, "one", filepath.Join(root, "sessions", "rollout-00000000-0000-0000-0000-000000000001.jsonl"), time.Now())
	path := filepath.Join(root, "state_5.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	namesBefore, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	hints, queries, bytes, outcome, refusal := indexHintsDetailed(context.Background(), root)
	if outcome != "" || len(hints) != 1 || queries != 4 || bytes == 0 || bytes > 4<<20 {
		t.Fatalf("bounded hint failed: %v %d %d %s; original refusal: %v", hints, queries, bytes, outcome, refusal)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("hint read changed settled DB", err)
	}
	namesAfter, err := os.ReadDir(root)
	if err != nil || len(namesBefore) != len(namesAfter) {
		t.Fatal("hint read created side files", err)
	}
	for i := range namesBefore {
		if namesBefore[i].Name() != namesAfter[i].Name() {
			t.Fatal("hint read changed source directory")
		}
	}
	addHint(t, db, "two", "outside-source", time.Now())
}

// diagnosticHintAdapter observes the actual hint attempt; it does not repeat
// the SQLite query, change its deadline, or alter the returned batch.
type diagnosticHintAdapter struct {
	codexAdapter
	refusals *[]error
}

func (a *diagnosticHintAdapter) Hints(ctx context.Context, root string) HintBatch {
	batch := a.codexAdapter.Hints(ctx, root)
	if batch.refusal != nil {
		*a.refusals = append(*a.refusals, batch.refusal)
	}
	return batch
}

// indexHints keeps the four-result synthetic test seam without an unused
// production wrapper. It delegates to the same original-attempt reader.
func indexHints(ctx context.Context, root string) ([]string, int, int64, string) {
	hints, queries, bytes, outcome, _ := indexHintsDetailed(ctx, root)
	return hints, queries, bytes, outcome
}
