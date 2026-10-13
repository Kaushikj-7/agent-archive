package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

type metadataScenario string

type metadataInterruptScenario string

const (
	metadataScenarioMalformed    metadataScenario          = "malformed"
	metadataScenarioUnknown      metadataScenario          = "unknown"
	metadataScenarioUnsafeStore  metadataScenario          = "unsafe-store"
	metadataScenarioMembership   metadataScenario          = "membership"
	metadataScenarioCancelled    metadataScenario          = "cancelled"
	metadataScenarioRootReplaced metadataScenario          = "root-replaced"
	metadataScenarioRewrite      metadataInterruptScenario = "rewrite"
	metadataScenarioReplace      metadataInterruptScenario = "replace"
	metadataScenarioCancel       metadataInterruptScenario = "cancel"
)

func metadataFixture(t *testing.T, count int) (*CodexRolloutLookup, []string, string) {
	t.Helper()
	store, cfg, at, root := fixture(t)
	ids := make([]string, count)
	for i := range count {
		ids[i] = writeRollout(t, root, cfg.Archive.Projects[0].Root, at, i+1, "sessions/nested")
		path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[i]+".jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), `"cli_version":"test"`, `"cli_version":"0.160.0"`), `"originator":"synthetic"`, `"originator":"codex-tui"`))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Error(err)
		}
	})
	return lookup, ids, root
}

func TestMetadataInventorySharesFiveSweepsAtPhysicalScale(t *testing.T) {
	if testing.Short() {
		t.Skip("full physical sweep scale; weekly and exact candidates")
	}
	testMetadataInventoryScale(t, false, 1100, 256)
}

func TestMetadataInventorySharesFiveSweepsWithIndexedWAL(t *testing.T) {
	if testing.Short() {
		t.Skip("full indexed physical sweep scale; weekly and exact candidates")
	}
	testMetadataInventoryScale(t, true, 1100, 256)
}

func testMetadataInventoryScale(t *testing.T, indexed bool, logical, pairs int) {
	t.Helper()
	lookup, ids, root := metadataFixture(t, logical)
	// A second copy for every logical ID retains all physical candidates in
	// the requested epoch; the full fixture also exceeds the observation cache.
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
		raw, err := os.ReadFile(filepath.Join(root, "sessions", "nested", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "archived_sessions", "nested", name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if indexed {
		db := hintDatabase(t, root, true)
		for _, id := range ids {
			addHint(t, db, id, filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+id+".jsonl"), time.Now())
		}
	}

	if _, ok := any(lookup).(agentapi.CodexRolloutSliceProvider); ok {
		t.Fatal("base lookup became full mode")
	}
	view := lookup.MetadataInventory().(*metadataInventory)
	if view.started || view.charge != 0 || view != lookup.MetadataInventory() {
		t.Fatal("request performed I/O or created second view")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	var slice agentapi.CodexRolloutSlice
	for i, id := range ids {
		if i%pairs == 0 {
			if slice != nil {
				if err := slice.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			phaseStart := time.Now()
			slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			t.Logf("slice=%d acquisition/validation=%s elapsed=%s fullRemaining=%s counts=%+v queries=%d", i/pairs+1, time.Since(phaseStart), time.Since(started), view.remaining, view.counts, lookup.queries)
			if err != nil {
				t.Fatal(err)
			}
			if err := slice.Valid(t.Context()); err != nil {
				t.Fatalf("scale convergence gap: %v counts=%+v elapsed=%v remaining=%v", err, view.counts, time.Since(started), lookup.remaining)
			}
		}
		set, err := slice.Thread(t.Context(), id)
		if err != nil {
			metadataScaleFailure(t, "thread", err, started, lookup, view)
		}
		if !set.Complete || len(set.Candidates) != 2 {
			t.Fatalf("incomplete physical set: %+v", set)
		}
		if err := slice.Check(t.Context(), id, set.Revision); err != nil {
			metadataScaleFailure(t, "check", err, started, lookup, view)
		}
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	_, peak := lookup.readBudget.Charged()
	if indexed && lookup.queries != logical*4 {
		t.Fatalf("live current SQL=%d", lookup.queries)
	}
	if view.counts.sweeps != (logical+pairs-1)/pairs || view.counts.headers != logical*2 || len(view.facts) != logical*2 {
		t.Fatalf("unshared epoch: %+v", view.counts)
	}
	t.Logf("physical=%d logical=%d sweeps=%d currentSQL=%d counts=%+v chargedPeak=%d allocBytes=%d allocations=%d heap=%d elapsed=%s ordinaryRemaining=%s", logical*2, logical, view.counts.sweeps, lookup.queries, view.counts, peak, after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.HeapAlloc, time.Since(started), lookup.remaining)
	t.Logf("fullRemaining=%s retained=%d headerwork=%d", view.remaining, view.charge, view.headerBytes)
	for _, index := range lookup.indexes {
		if index.snapshot != nil {
			t.Logf("indexCopy=%+v", index.snapshot.metrics)
		}
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	used, _ := lookup.readBudget.Charged()
	if used != 0 {
		t.Fatalf("leaked shared charge %d", used)
	}
}

func TestMetadataSliceChecksCurrentDespiteSharedFilesystemProof(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 2)
	db := hintDatabase(t, root, true)
	path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	addHint(t, db, ids[0], path, time.Now())
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || set.Current == nil {
		t.Fatal(set, err)
	}
	if err := slice.Check(t.Context(), ids[0], set.Revision); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[1]+".jsonl")
	if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", other, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := slice.Check(t.Context(), ids[0], set.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatalf("stale current accepted: %v", err)
	}
	if view.counts.sweeps != 1 || lookup.queries != 6 {
		t.Fatalf("filesystem reuse lost live queries: sweeps=%d SQL=%d", view.counts.sweeps, lookup.queries)
	}
}

type metadataReadRanges struct {
	reader  io.ReaderAt
	offsets []int64
	lengths []int
}

func (r *metadataReadRanges) ReadAt(p []byte, off int64) (int, error) {
	r.offsets = append(r.offsets, off)
	r.lengths = append(r.lengths, len(p))
	return r.reader.ReadAt(p, off)
}

func TestMetadataHeaderExactRangesAndResumableMaximum(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(raw[:strings.IndexByte(string(raw), '\n')])
	line += strings.Repeat(" ", metadataHeaderBytes-len(line)-1) + "\n"
	if err := os.WriteFile(path, []byte(line+"SENSITIVE_BODY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: ids[0]}
	f, err := view.open(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !view.reserve(metadataScratch) {
		t.Fatal("scratch declined")
	}
	ranges := &metadataReadRanges{reader: f}
	view.cursor = &metadataCursor{file: f, reader: ranges, source: source, info: info, line: make([]byte, 0, metadataHeaderBytes), scratch: metadataScratch}
	view.queue = nil
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	quanta := 0
	var maxStep time.Duration
	for view.cursor != nil {
		n := quanta
		quanta++
		if quanta > 65536 {
			t.Fatal("header made no bounded progress")
		}
		before := view.counts.operations
		started := time.Now()
		if err := view.step(ctx); err != nil {
			t.Fatal(err)
		}
		maxStep = max(maxStep, time.Since(started))
		if view.counts.operations-before > 1024 {
			t.Fatal("operation quantum exceeded")
		}
		if n < 63 && view.cursor == nil {
			t.Fatal("premature cursor close")
		}
	}
	if view.cursor != nil || len(ranges.offsets) != metadataHeaderBytes || view.counts.requested != metadataHeaderBytes || view.counts.returned != metadataHeaderBytes {
		t.Fatalf("maximum header failed: %+v", view.counts)
	}
	for i, off := range ranges.offsets {
		if off != int64(i) || ranges.lengths[i] != 1 {
			t.Fatalf("body/range read at %d: offset=%d length=%d", i, off, ranges.lengths[i])
		}
	}
	t.Logf("64KiB header quanta=%d operations=%d requested=%d returned=%d maxStep=%s callerLiveness=30s", quanta, view.counts.operations, view.counts.requested, view.counts.returned, maxStep)
}

func TestMetadataInventoryGapsAndFailedSliceRenewal(t *testing.T) {
	for _, mode := range []metadataScenario{metadataScenarioMalformed, metadataScenarioUnknown, metadataScenarioUnsafeStore, metadataScenarioMembership, metadataScenarioCancelled, metadataScenarioRootReplaced} {
		t.Run(string(mode), func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			view := lookup.MetadataInventory().(*metadataInventory)
			path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			ctx := t.Context()
			switch mode {
			case metadataScenarioMalformed:
				if err := os.WriteFile(path, []byte("{}\nSECRET\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case metadataScenarioUnknown:
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), `"source":"cli"`, `"source":"future"`)), 0600); err != nil {
					t.Fatal(err)
				}
			case metadataScenarioUnsafeStore:
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "archived_sessions")); err != nil {
					t.Fatal(err)
				}
			case metadataScenarioCancelled:
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case metadataScenarioMembership, metadataScenarioRootReplaced:
				first, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
				if err != nil || first.Valid(ctx) != nil {
					t.Fatal(err, first.Valid(ctx))
				}
				_ = first.Close()
				if mode == metadataScenarioMembership {
					if err := os.WriteFile(filepath.Join(root, "sessions", "new"), []byte("fixture"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(root, root+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			slice, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
			if mode == metadataScenarioCancelled {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = slice.Close() }()
			if err := slice.Valid(ctx); err == nil {
				t.Fatal("gap marked complete")
			}
			sweeps := view.counts.sweeps
			for range 20 {
				if _, err := slice.Thread(ctx, ids[0]); err == nil {
					t.Fatal("failed slice accepted")
				}
			}
			if view.counts.sweeps != sweeps {
				t.Fatal("failed slice reswept")
			}
			if mode == metadataScenarioMembership {
				fresh, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
				if err != nil {
					t.Fatal(err)
				}
				_ = fresh.Close()
				if view.counts.sweeps != sweeps+1 {
					t.Fatal("renewal did not attempt one sweep")
				}
			}
			if mode == metadataScenarioMalformed && view.counts.physical != 1 {
				t.Fatal("invalid header evaded physical acquisition count")
			}
			if view.cursor != nil {
				t.Fatal("failed operation retained partial header")
			}
		})
	}
}

func TestMetadataInventoryRetainsMorePhysicalCopiesThanGraphLimit(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
	raw, err := os.ReadFile(filepath.Join(root, "sessions", "nested", name))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 70 {
		dir := filepath.Join(root, "archived_sessions", strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || !set.Complete || len(set.Candidates) != 71 {
		t.Fatal(len(set.Candidates), set.Complete, err)
	}
	if archive.MaxHistorySpans != 64 {
		t.Fatal("graph cap silently changed")
	}
}

func TestMetadataFullEpochKeepsOrdinaryDeadlineIndependent(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	db := hintDatabase(t, root, true)
	path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	addHint(t, db, ids[0], path, time.Now())
	// Build the shared projection on the ordinary lane first, then expire that
	// lane. The full view still uses its caller context and the same projection.
	if _, err := lookup.Thread(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	private := lookup.indexes[root].snapshot.path
	lookup.remaining = 0
	lookup.deadline = time.Now().Add(-time.Second)
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || set.Current == nil {
		t.Fatal(set, err)
	}
	if lookup.indexes[root].snapshot.path != private || lookup.remaining != 0 {
		t.Fatal("full lane changed ordinary allowance or projection")
	}
	remaining := view.remaining
	_ = slice.Close()
	slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{Duration: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_ = slice.Close()
	if view.remaining >= remaining || view.remaining > 30*time.Second {
		t.Fatal("slice renewed epoch allowance")
	}
	if _, err := lookup.Thread(t.Context(), ids[0]); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal("full projection extended ordinary lane", err)
	}
}

func TestMetadataInventoryMaximumCopiesAndJointCapacity(t *testing.T) {
	if testing.Short() {
		t.Skip("full 16384 physical metadata copies; weekly and exact candidates")
	}
	testMetadataInventoryCopiesAndJointCapacity(t, 16384)
}

func TestMetadataInventorySmallCopiesAndJointCapacity(t *testing.T) {
	testMetadataInventoryCopiesAndJointCapacity(t, 64)
}

func testMetadataInventoryCopiesAndJointCapacity(t *testing.T, copies int) {
	t.Helper()
	store, _, _, root := fixture(t)
	id := "00000000-0000-0000-0000-000000000001"
	dir := filepath.Join(root, "sessions", "nested")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"session_meta","timestamp":"2026-10-01T12:00:00Z","payload":{"id":"` + id + `","cwd":"/synthetic","source":"cli","cli_version":"0.160.0","originator":"codex-tui"}}` + "\n"
	for i := range copies {
		name := fmt.Sprintf("rollout-%05d-%s.jsonl", i, id)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"SECRET_BODY\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.CloseReadOnly() }()
	view := lookup.MetadataInventory().(*metadataInventory)
	started := time.Now()
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	if err := slice.Valid(t.Context()); err != nil {
		if copies != 16384 || !raceEnabled || agentapi.Failure(err) != agentapi.Limit || view.remaining > 0 || t.Context().Err() != nil {
			t.Fatalf("maximum fixture gap: %v physical=%d retained=%d header=%d remaining=%s", err, len(view.facts), view.charge, view.headerBytes, view.remaining)
		}
		set, refusal := slice.Thread(t.Context(), id)
		if agentapi.Failure(refusal) != agentapi.Limit || set.Complete || len(set.Candidates) != 0 || view.complete || view.cursor != nil || view.counts.physical > copies || view.counts.requested > int64(len(line)*view.counts.physical) {
			t.Fatalf("race refusal retained partial authority: set=%+v refusal=%v complete=%t cursor=%t counts=%+v headerLen=%d remaining=%s", set, refusal, view.complete, view.cursor != nil, view.counts, len(line), view.remaining)
		}
		t.Logf("instrumented maximum owned refusal elapsed=%s remaining=%s retained=%d headerwork=%d counts=%+v", time.Since(started), view.remaining, view.charge, view.headerBytes, view.counts)
		if err := slice.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lookup.CloseReadOnly(); err != nil {
			t.Fatal(err)
		}
		if used, _ := lookup.readBudget.Charged(); used != 0 {
			t.Fatal("maximum refusal leaked shared charge", used)
		}
		return
	}
	if len(view.facts) != copies || view.counts.physical != copies || view.counts.entries != copies+1 || !view.complete {
		t.Fatal("maximum physical metadata truncated")
	}
	used, peak := lookup.readBudget.Charged()
	t.Logf("copies=%d headerLen=%d retained=%d headerwork=%d charged=%d peak=%d elapsed=%s remaining=%s counts=%+v", copies, len(line), view.charge, view.headerBytes, used, peak, time.Since(started), view.remaining, view.counts)
	// Joint capacity refusal precedes allocating a candidate result whose source
	// metadata already consumes the aggregate cap; it does not discard facts.
	if !view.reserve(metadataBytes - view.charge - view.headerBytes) {
		t.Fatal("boundary charge failed")
	}
	set, err := slice.Thread(t.Context(), id)
	if agentapi.Failure(err) != agentapi.Limit || set.Complete || len(set.Candidates) != 0 {
		t.Fatalf("joint cap retained partial result authority: set=%+v err=%v", set, err)
	}
	if len(view.facts) != copies || view.counts.requested != int64(len(line)*copies) {
		t.Fatal("capacity refusal lost physical facts or metadata read body")
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("maximum success leaked charge", used)
	}
}

func TestMetadataInventoryLateRequestDoesNotPromoteRequestedCache(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 3)
	// Request coverage after a round already started. A disposable hint map is
	// deliberately incomplete and must not supply full epoch completeness.
	lookup.coverage.Directories["midround"] = coverageDirectory{}
	lookup.coverage.request(ids[0])
	view := lookup.MetadataInventory().(*metadataInventory)
	if view.started {
		t.Fatal("late request performed inventory I/O")
	}
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	for _, id := range ids {
		set, err := slice.Thread(t.Context(), id)
		if err != nil || !set.Complete || len(set.Candidates) != 1 {
			t.Fatal(set, err)
		}
	}
	if len(view.facts) != 3 || lookup.coverage.Phase == coverageComplete {
		t.Fatal("full view used requested proof or cache")
	}
	if err := os.WriteFile(filepath.Join(root, "archived_sessions"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if fresh.Valid(t.Context()) == nil {
		t.Fatal("missing store proof accepted replacement")
	}
}

type metadataInterruptRead struct {
	reader io.ReaderAt
	once   func()
}

func (r *metadataInterruptRead) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.reader.ReadAt(p, off)
	if r.once != nil {
		callback := r.once
		r.once = nil
		callback()
	}
	return n, err
}

func TestMetadataPartialHeaderRejectsRewriteReplacementAndCancellation(t *testing.T) {
	for _, mode := range []metadataInterruptScenario{metadataScenarioRewrite, metadataScenarioReplace, metadataScenarioCancel} {
		t.Run(string(mode), func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			view := lookup.MetadataInventory().(*metadataInventory)
			path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := view.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			view.queue = nil
			source := SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: ids[0]}
			f, err := view.open(source)
			if err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if !view.reserve(metadataScratch) {
				t.Fatal("scratch declined")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &metadataInterruptRead{reader: f, once: func() {
				switch mode {
				case metadataScenarioCancel:
					cancel()
				case metadataScenarioReplace:
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				case metadataScenarioRewrite:
					if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), "codex-tui", "codex-xyz")), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}}
			view.cursor = &metadataCursor{file: f, reader: reader, source: source, info: info, line: make([]byte, 0, metadataHeaderBytes), scratch: metadataScratch}
			if err := view.ensure(ctx); err == nil {
				t.Fatal("interrupted header accepted")
			}
			if view.cursor != nil || view.complete || len(view.facts) != 0 {
				t.Fatal("partial authority retained")
			}
			newline := strings.IndexByte(string(raw), '\n')
			if view.counts.requested > int64(newline+1) {
				t.Fatal("metadata read requested body")
			}
		})
	}
}

func TestMetadataApprovedAliasAndComponentRetargetRefuse(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lookup.homes = []string{alias}
	view := lookup.MetadataInventory().(*metadataInventory)
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	if err := view.ensure(t.Context()); err == nil {
		t.Fatal("approved spelling retarget accepted")
	}
	other, otherIDs, otherRoot := metadataFixture(t, 1)
	otherView := other.MetadataInventory().(*metadataInventory)
	if err := otherView.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(otherRoot, "sessions", "nested")
	if err := os.Rename(nested, nested+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested+".old", nested); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "rollout-2026-10-01T12-00-00-"+otherIDs[0]+".jsonl")
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: otherRoot, Locator: path, StableKey: ids[0]}
	if file, err := otherView.open(source); err == nil {
		_ = file.Close()
		t.Fatal("raced intermediate symlink accepted")
	}
}

func TestMetadataConflictingBodiesRemainPhysicalFacts(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
	raw, err := os.ReadFile(filepath.Join(root, "sessions", "nested", name))
	if err != nil {
		t.Fatal(err)
	}
	header := raw[:strings.IndexByte(string(raw), '\n')+1]
	dir := filepath.Join(root, "archived_sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), append(append([]byte{}, header...), []byte("DIFFERENT_BODY\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || !set.Complete || len(set.Candidates) != 2 {
		t.Fatal(set, err)
	}
	if view.counts.returned != 2*int64(len(header)) {
		t.Fatal("body read or copy discarded")
	}
}

func TestMetadataFullProjectionDoesNotExtendOrdinaryLane(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	db := hintDatabase(t, root, true)
	path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	addHint(t, db, ids[0], path, time.Now())
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = slice.Thread(t.Context(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = slice.Close()
	private := lookup.indexes[root].snapshot.path
	if lookup.remaining != Budget {
		t.Fatal("full work consumed ordinary allowance")
	}
	if _, err := lookup.Thread(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	if lookup.indexes[root].snapshot.path != private || lookup.remaining >= Budget {
		t.Fatal("ordinary lane did not reuse projection within own allowance")
	}
	lookup.remaining = 0
	if _, err := lookup.Thread(t.Context(), ids[0]); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal("full view extended ordinary time", err)
	}
}

func TestMetadataStableFactsConflictAndMultipleHomes(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			other := t.TempDir()
			name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
			raw, err := os.ReadFile(filepath.Join(root, "sessions", "nested", name))
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				raw = []byte(strings.ReplaceAll(string(raw), `"originator":"codex-tui"`, `"originator":"codex_cli_rs"`))
			}
			if err := os.MkdirAll(filepath.Join(other, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "sessions", name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			store := lookup.store
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			lookup, err = NewCodexRolloutLookup(t.Context(), store, []string{root, other})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lookup.CloseReadOnly(); err != nil {
					t.Error(err)
				}
				if used, _ := lookup.readBudget.Charged(); used != 0 {
					t.Error("multiple-home cleanup charge", used)
				}
			})
			view := lookup.MetadataInventory().(*metadataInventory)
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = slice.Close() }()
			set, err := slice.Thread(t.Context(), ids[0])
			if conflict {
				if err == nil || set.Complete {
					t.Fatal("conflicting stable metadata accepted", set, err)
				}
			} else if err != nil || !set.Complete || len(set.Candidates) != 2 {
				t.Fatal(set, err)
			}
		})
	}
}

func TestMetadataPhysicalCeilingRefusesBeforeExtraOpen(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	view.counts.physical = 16384
	view.batch = []SourceEntry{{Source: SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")}}}
	before := view.counts.fileOpens
	if err := view.step(t.Context()); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal(err)
	}
	if view.counts.fileOpens != before || view.cursor != nil || view.counts.physical != 16384 {
		t.Fatal("extra physical file opened beyond cap")
	}
}

func TestMetadataInventorySmallerPhysicalScale(t *testing.T) {
	testMetadataInventoryScale(t, false, 64, 16)
}

func TestMetadataInventorySmallerIndexedScale(t *testing.T) {
	testMetadataInventoryScale(t, true, 64, 16)
}

func metadataScaleFailure(t *testing.T, phase string, err error, started time.Time, lookup *CodexRolloutLookup, view *metadataInventory) {
	t.Helper()
	var sourceErr *agentapi.SourceError
	var cause error
	if errors.As(err, &sourceErr) {
		cause = sourceErr.Err
	}
	for _, index := range lookup.indexes {
		if index.snapshot != nil {
			t.Logf("failureIndexCopy=%+v", index.snapshot.metrics)
		}
	}
	used, peak := lookup.readBudget.Charged()
	t.Fatalf("phase=%s failure=%v kind=%s cause=%v elapsed=%s fullRemaining=%s counts=%+v SQL=%d retained=%d headerwork=%d charged=%d peak=%d", phase, err, agentapi.Failure(err), cause, time.Since(started), view.remaining, view.counts, lookup.queries, view.charge, view.headerBytes, used, peak)
}

func TestMetadataValidationOwnedDeadlineInvalidatesCompleteInventory(t *testing.T) {
	lookup, ids, _ := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	first, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Valid(t.Context()); err != nil || !view.complete {
		t.Fatal("fixture inventory incomplete", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Cancellation belongs to the caller and must not poison the complete
	// inventory or prevent a different caller from renewing validation.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{}); !errors.Is(err, context.Canceled) || !view.complete || view.failure != nil {
		t.Fatal("caller cancellation invalidated inventory", err)
	}
	renewed, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := renewed.Valid(t.Context()); err != nil {
		t.Fatal("caller cancellation prevented renewal", err)
	}
	if err := renewed.Close(); err != nil {
		t.Fatal(err)
	}

	// Acquisition is already complete, so an exhausted owned allowance now
	// fails in validation, rather than through ensure's failure cleanup.
	view.remaining = time.Nanosecond
	sweeps := view.counts.sweeps
	refused, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = refused.Close() }()
	if err := refused.Valid(t.Context()); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal("owned validation deadline lost", err)
	}
	set, err := refused.Thread(t.Context(), ids[0])
	if agentapi.Failure(err) != agentapi.Limit || set.Current != nil || set.Complete || len(set.Candidates) != 0 {
		t.Fatal("owned validation refusal retained authority", set, err)
	}
	if view.counts.sweeps != sweeps+1 || view.remaining > 0 || view.complete || view.cursor != nil || agentapi.Failure(view.failure) != agentapi.Limit {
		t.Fatalf("owned validation refusal retained inventory: sweeps=%d remaining=%s complete=%v cursor=%v failure=%v", view.counts.sweeps-sweeps, view.remaining, view.complete, view.cursor != nil, view.failure)
	}
	if t.Context().Err() != nil {
		t.Fatal("owned deadline canceled caller", t.Context().Err())
	}
}

func TestMetadataOwnedDeadlineRefusesAndPreservesCaller(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	db := hintDatabase(t, root, true)
	addHint(t, db, ids[0], filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl"), time.Now())
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	view.remaining = time.Nanosecond
	set, err := slice.Thread(t.Context(), ids[0])
	if agentapi.Failure(err) != agentapi.Limit || set.Complete {
		t.Fatal("expired work did not report typed limit", set, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := slice.Thread(ctx, ids[0]); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation lost", err)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := slice.Thread(ctx, ids[0]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline lost", err)
	}
	var failure *agentapi.SourceError
	var cause error
	if errors.As(err, &failure) {
		cause = failure.Err
	}
	t.Logf("owned expiration kind=%s cause=%v remaining=%s queries=%d complete=%v", agentapi.Failure(err), cause, view.remaining, lookup.queries, set.Complete)
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("expired work leaked charge", used)
	}
}

func TestMetadataAliasedHomesSharePhysicalFactsButFenceEverySpelling(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lookup.homes = []string{root, alias}
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || !set.Complete || len(set.Candidates) != 1 || view.counts.physical != 1 || view.counts.headers != 1 {
		t.Fatal("alias duplicated physical authority", set, err, view.counts)
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := slice.Valid(t.Context()); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("secondary approved alias fence lost", err)
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataInventoryAccountsAdversarialParserRepresentations(t *testing.T) {
	dense := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(`,"k` + strconv.Itoa(i) + `":0`)
		}
		return b.String()
	}
	parent := "00000000-0000-4000-8000-000000000099"
	cases := []struct {
		name     string
		source   string
		extra    string
		producer string
		version  string
	}{
		{"ordinary-html", `"cli"`, `,"git":{"repository_url":"` + strings.Repeat("<", 4096) + `"}`, strings.Repeat("<", 256), "0.160.0"},
		{"child-html-raw-source", `{"subagent":{"other":"ok"},"unknown":"` + strings.Repeat("<", 17000) + `"}`, "", strings.Repeat("<", 20000), strings.Repeat("<", 20000)},
		{"fork-html", `"cli"`, `,"forked_from_id":"` + parent + `"`, strings.Repeat("<", 20000), strings.Repeat("<", 20000)},
		{"dense-git", `"cli"`, `,"git":{"repository_url":"ok"` + dense(4000) + `}`, "codex-tui", "0.160.0"},
		{"dense-source-subagent", `{"subagent":{"other":"ok"` + dense(2000) + `}` + dense(2000) + `}`, "", "codex-tui", "0.160.0"},
		{"nested-invalid-utf8", `"cli"`, `,"git":{"repository_url":"` + string([]byte{0xff}) + `","unknown":` + strings.Repeat("[", 2000) + `0` + strings.Repeat("]", 2000) + `}`, "codex-tui", "0.160.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			line := `{"type":"session_meta","timestamp":"2026-10-01T12:00:00Z","payload":{"id":"` + ids[0] + `","cwd":"/` + strings.Repeat("<", 4095) + `","source":` + tc.source + `,"originator":"` + tc.producer + `","cli_version":"` + tc.version + `"` + tc.extra + `}}`
			if len(line) >= metadataHeaderBytes {
				t.Fatal("fixture exceeds header bound", len(line))
			}
			line += strings.Repeat(" ", metadataHeaderBytes-len(line)-1) + "\n"
			path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			if err := os.WriteFile(path, []byte(line+"SENSITIVE_BODY\n"), 0600); err != nil {
				t.Fatal(err)
			}
			view := lookup.MetadataInventory().(*metadataInventory)
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			set, err := slice.Thread(t.Context(), ids[0])
			if err != nil || !set.Complete || len(view.facts) != 1 {
				t.Fatalf("accepted parser shape failed: %v %+v", err, set)
			}
			if view.counts.requested != metadataHeaderBytes || view.cursor != nil {
				t.Fatal("body read or scratch retained", view.counts)
			}
			_, peak := lookup.readBudget.Charged()
			if peak < 8454144 {
				t.Fatal("proportional scratch not charged", peak)
			}
			t.Logf("shape=%s header=%d chargedPeak=%d retained=%d", tc.name, len(line), peak, view.charge)
			if err := slice.Close(); err != nil {
				t.Fatal(err)
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != 0 {
				t.Fatal("scratch leaked", used)
			}
		})
	}
}

func TestMetadataInventoryDeclinesParserBeforeDecodeAndReleasesCursor(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: ids[0]}
	f, err := view.open(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !view.reserve(metadataScratch) {
		t.Fatal("initial scratch refused")
	}
	// Deliberately invalid JSON distinguishes parser failure from prior charge refusal.
	view.cursor = &metadataCursor{file: f, reader: f, source: source, info: info, line: []byte(strings.Repeat("x", 4096) + "\n"), scratch: metadataScratch}
	if !view.reserve(metadataBytes - view.charge - view.headerBytes - 1) {
		t.Fatal("pressure reserve failed")
	}
	err = view.fail(view.finishHeader(t.Context()))
	if agentapi.Failure(err) != agentapi.Limit || view.cursor != nil || view.complete || len(view.facts) != 0 {
		t.Fatalf("not truthful pre-parser refusal: %v", err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("refusal leaked", used)
	}
}

func TestMetadataEnumerationPressurePreservesLimitAndCleanup(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(strconv.FormatBool(shared), func(t *testing.T) {
			lookup, _, _ := metadataFixture(t, 1)
			reservation := lookup.readBudget.Reservation()
			view := lookup.MetadataInventory().(*metadataInventory)
			if err := view.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			var external int64
			if shared {
				external = lookup.readBudget.Available()
				if !reservation.Reserve(external) {
					t.Fatal("shared pressure reserve failed")
				}
			} else if !view.reserve(metadataBytes - view.charge - view.headerBytes) {
				t.Fatal("metadata pressure reserve failed")
			}
			err := view.ensure(t.Context())
			if agentapi.Failure(err) != agentapi.Limit || agentapi.Failure(view.failure) != agentapi.Limit {
				t.Fatalf("reservation lost typed Limit: %v", err)
			}
			if view.complete || view.cursor != nil || len(view.facts) != 0 || view.counts.requested != 0 || view.counts.physical != 0 {
				t.Fatal("reservation refusal retained partial authority", view.counts)
			}
			before := view.counts
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			for range 20 {
				if _, err := slice.Thread(t.Context(), "00000000-0000-4000-8000-000000000001"); agentapi.Failure(err) != agentapi.Limit {
					t.Fatal("cached refusal changed", err)
				}
			}
			if view.counts != before {
				t.Fatal("failure repeated enumeration", before, view.counts)
			}
			if err := slice.Close(); err != nil {
				t.Fatal(err)
			}
			reservation.Release(external)
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != 0 {
				t.Fatal("reservation refusal leaked charge", used)
			}
		})
	}
}

// Expiry belongs to the fixed epoch even while a permitted longer slice is live.
func TestMetadataSliceRolloutSharesPubliclyExhaustedEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lookup, ids, root := metadataFixture(t, 1)
		native := hintDatabase(t, root, true)
		addHint(t, native, ids[0], filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl"), time.Now())
		view := lookup.MetadataInventory().(*metadataInventory)
		slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{Duration: 60 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := slice.Thread(t.Context(), ids[0]); err != nil {
			t.Fatal(err)
		}
		held, err := lookup.indexes[root].db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Occupy the real projection's sole connection. Public Thread calls then
		// consume the epoch via the targeted current query's nested five-second cap.
		for i := range 6 {
			results := make(chan error, 1)
			go func() { _, err := slice.Thread(t.Context(), ids[0]); results <- err }()
			synctest.Wait()
			time.Sleep(5 * time.Second)
			synctest.Wait()
			err := <-results
			want := agentapi.Unavailable
			if i == 5 {
				want = agentapi.Limit
			}
			if agentapi.Failure(err) != want || t.Context().Err() != nil {
				t.Fatal("query did not preserve local versus epoch refusal", i, err)
			}
		}
		if view.remaining > 0 || slice.Valid(t.Context()) != nil {
			t.Fatal("fixture did not exhaust only the owned epoch", view.remaining)
		}
		before := view.counts
		charge, _ := lookup.readBudget.Charged()
		refs, err := slice.Rollout(t.Context(), ids[0])
		if err == nil || agentapi.Failure(err) != agentapi.Limit || len(refs) != 0 {
			t.Fatal("exhausted epoch returned cached metadata refs", len(refs), err)
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if refs, err := slice.Rollout(cancelled, ids[0]); !errors.Is(err, context.Canceled) || len(refs) != 0 {
			t.Fatal("caller cancellation lost", err)
		}
		expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer stop()
		if refs, err := slice.Rollout(expired, ids[0]); !errors.Is(err, context.DeadlineExceeded) || len(refs) != 0 {
			t.Fatal("caller deadline lost", err)
		}
		after, _ := lookup.readBudget.Charged()
		if before != view.counts || charge != after {
			t.Fatal("exhausted projection performed work or retained refs")
		}
		if err := held.Close(); err != nil {
			t.Fatal(err)
		}
		if err := slice.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lookup.CloseReadOnly(); err != nil {
			t.Fatal(err)
		}
		if used, _ := lookup.readBudget.Charged(); used != 0 {
			t.Fatal("cleanup charge", used)
		}
	})
}

func TestMetadataSliceRolloutDebitsProjectionWork(t *testing.T) {
	lookup, ids, _ := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	before, counts := view.remaining, view.counts
	refs, err := slice.Rollout(t.Context(), ids[0])
	if err != nil || len(refs) != 1 {
		t.Fatal("physical projection failed", refs, err)
	}
	if view.remaining >= before || view.counts != counts {
		t.Fatal("projection did not consume active work independently of native I/O", before, view.remaining)
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("projection cleanup charge", used)
	}
}

// A query can finish assembling a revision just as its owned allowance expires.
// The error must not carry a complete selection, even for an empty native census.
func TestMetadataThreadExpiryReturnsNoCompleteSelection(t *testing.T) {
	lookup, _, _ := metadataFixture(t, 0)
	lookup.homes = nil
	lookup.roots = nil
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil || slice.Valid(t.Context()) != nil {
		t.Fatal("empty census failed", err)
	}
	defer func() { _ = slice.Close() }()
	before, _ := lookup.readBudget.Charged()
	view.remaining = time.Nanosecond
	set, err := slice.Thread(t.Context(), "11111111-1111-4111-8111-111111111111")
	if agentapi.Failure(err) != agentapi.Limit || set.Complete || set.Current != nil || set.Revision != "" || len(set.Candidates) != 0 {
		t.Fatalf("expired query returned selection authority: %+v %v", set, err)
	}
	if after, _ := lookup.readBudget.Charged(); after != before {
		t.Fatal("failed query retained a result charge", before, after)
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		} else {
			cancel()
		}
		want := context.Canceled
		if deadline {
			want = context.DeadlineExceeded
		}
		set, err = slice.Thread(ctx, "11111111-1111-4111-8111-111111111111")
		cancel()
		if !errors.Is(err, want) || set.Complete || set.Current != nil || set.Revision != "" || len(set.Candidates) != 0 {
			t.Fatalf("caller refusal returned selection authority: %+v %v", set, err)
		}
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("failed query cleanup retained shared charge", used)
	}
}

// Close releases acquired representations even when a caller retains the view.
func TestMetadataInventoryCloseClearsAcquiredRepresentations(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(readOnly), func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			path := filepath.Join(root, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			view := lookup.MetadataInventory().(*metadataInventory)
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if err := slice.Valid(t.Context()); err != nil {
				t.Fatal(err)
			}
			set, err := slice.Thread(t.Context(), ids[0])
			if err != nil || !set.Complete || len(set.Candidates) != 1 {
				t.Fatal(set, err)
			}
			if len(view.directoryIndex) == 0 || len(view.roots) == 0 || view.next.Root == "" {
				t.Fatal("fixture lacks acquired representations")
			}
			counts, failure, headerWork := view.counts, view.failure, view.headerBytes
			closeOwner := lookup.Close
			if readOnly {
				closeOwner = lookup.CloseReadOnly
			}
			// The live result lease and slice deliberately outlive the owner's close.
			for range 2 {
				if err := closeOwner(); err != nil {
					t.Fatal(err)
				}
				if view.facts != nil || view.batch != nil || view.queue != nil || view.threads != nil || view.interned != nil || view.rollouts != nil || view.directories != nil || view.directoryIndex != nil || view.roots != nil || view.next != (directory{}) || view.cursor != nil || view.batchCharge != 0 || view.charge != 0 {
					t.Fatal("closed inventory retains acquired representations")
				}
				if view.owner != lookup || view.counts != counts || !errors.Is(view.failure, failure) || view.headerBytes != headerWork {
					t.Fatal("close discarded borrowed owner or diagnostic evidence")
				}
				if used, _ := lookup.readBudget.Charged(); used != 0 {
					t.Fatal("closed inventory retained charge", used)
				}
			}
			if _, err := view.Thread(t.Context(), ids[0]); !errors.Is(err, agentapi.ErrClosed) {
				t.Fatal("closed view accepted query", err)
			}
			if _, err := slice.Thread(t.Context(), ids[0]); !errors.Is(err, agentapi.ErrClosed) {
				t.Fatal("closed owner accepted slice query", err)
			}
			if err := slice.Close(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != 0 {
				t.Fatal("late slice close changed charge", used)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("close changed native source", err)
			}
			if _, err := os.Stat(filepath.Join(lookup.store.Home(), "discovery-catalog.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("metadata close checkpointed capture catalog", err)
			}
		})
	}
}

// One physical rollout cannot claim conflicting thread or pagination identities.
func TestMetadataPhysicalIdentityConflictsLeaveIncompleteInventory(t *testing.T) {
	const otherThread = "22222222-2222-4222-8222-222222222222"
	const otherRollout = "33333333-3333-4333-8333-333333333333"
	const baseRollout = "abcdefab-abcd-4abc-8abc-abcdefabcdef"
	const physicalRollout = "abcdefab-abcd-4abc-8abc-abcdefabcdea"
	base := `,"history_base":{"thread_id":"` + baseRollout + `","end_ordinal_exclusive":1,"end_byte_offset":2}`
	for _, tc := range []struct {
		name          string
		thread        string
		rollout       string
		extra         string
		complete      bool
		originalExtra string
	}{
		{"conflicting-thread", otherThread, "", "", false, ""},
		{"case-variant-conflicting-thread", otherThread, strings.ToUpper(physicalRollout), "", false, ""},
		{"case-variant-copy", "", strings.ToUpper(physicalRollout), "", true, ""},
		{"case-variant-base-copy", "", "", strings.Replace(base, baseRollout, strings.ToUpper(baseRollout), 1), true, base},
		{"conflicting-mode", "", "", `,"history_mode":"paginated"`, false, ""},
		{"conflicting-base", "", "", base, false, ""},
		{"conflicting-base-rollout", "", "", strings.Replace(base, baseRollout, otherRollout, 1), false, base},
		{"conflicting-base-ordinal", "", "", strings.Replace(base, `"end_ordinal_exclusive":1`, `"end_ordinal_exclusive":2`, 1), false, base},
		{"conflicting-base-offset", "", "", strings.Replace(base, `"end_byte_offset":2`, `"end_byte_offset":3`, 1), false, base},
		{"identical-base-copy", "", "", base, true, base},
		{"identical-copy", "", "", "", true, ""},
		{"explicit-legacy-copy", "", "", `,"history_mode":"legacy"`, true, ""},
		{"distinct-paginated-revision", "", otherRollout, `,"history_mode":"paginated"` + base, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			id := ids[0]
			name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
			raw, err := os.ReadFile(filepath.Join(root, "sessions", "nested", name))
			if err != nil {
				t.Fatal(err)
			}
			physicalName := "rollout-" + physicalRollout + ".jsonl"
			if err := os.Rename(filepath.Join(root, "sessions", "nested", name), filepath.Join(root, "sessions", "nested", physicalName)); err != nil {
				t.Fatal(err)
			}
			name = physicalName
			header := string(raw[:strings.IndexByte(string(raw), '\n')])
			if tc.originalExtra != "" {
				header = strings.Replace(header, `"source":"cli"`, `"source":"cli"`+tc.originalExtra, 1)
				if err := os.WriteFile(filepath.Join(root, "sessions", "nested", name), []byte(header+"\nPRIVATE_BODY\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			copyHeader := string(raw[:strings.IndexByte(string(raw), '\n')])
			if tc.thread != "" {
				copyHeader = strings.Replace(copyHeader, `"id":"`+id+`"`, `"id":"`+tc.thread+`"`, 1)
			}
			copyHeader = strings.Replace(copyHeader, `"source":"cli"`, `"source":"cli"`+tc.extra, 1) + "\n"
			if !json.Valid([]byte(copyHeader)) {
				t.Fatal("invalid fixture header", copyHeader)
			}
			copyName := name
			if tc.rollout != "" {
				copyName = "rollout-" + tc.rollout + ".jsonl"
			}
			dir := filepath.Join(root, "archived_sessions")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, copyName), []byte(copyHeader+"PRIVATE_DIFFERENT_BODY\n"), 0600); err != nil {
				t.Fatal(err)
			}
			meta, created, found, parseErr := codexmeta.ParseCodexMeta([]byte(copyHeader))
			_, identityOutcome := meta.Identity(filepath.Join(dir, copyName))
			if parseErr != nil || !found || created.IsZero() || identityOutcome != "" {
				t.Fatal("fixture identity invalid", parseErr, identityOutcome)
			}
			t.Cleanup(func() {
				if err := lookup.CloseReadOnly(); err != nil {
					t.Error(err)
				}
				if used, _ := lookup.readBudget.Charged(); used != 0 {
					t.Error("cleanup charge", used)
				}
			})
			view := lookup.MetadataInventory().(*metadataInventory)
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			set, err := slice.Thread(t.Context(), id)
			if tc.complete {
				if err != nil || !set.Complete || len(set.Candidates) != 2 {
					t.Fatalf("valid physical copies/revisions rejected: %+v %v", set, err)
				}
				if refs, err := slice.Rollout(t.Context(), strings.Repeat("A", 4096)); err != nil || len(refs) != 0 {
					t.Fatal("unknown physical lookup changed semantics", refs, err)
				}
				if tc.rollout == "" || strings.EqualFold(tc.rollout, physicalRollout) {
					for _, query := range []string{physicalRollout, strings.ToUpper(physicalRollout)} {
						if refs, err := slice.Rollout(t.Context(), query); err != nil || len(refs) != 2 {
							t.Fatal("physical UUID query spelling lost copies", query, refs, err)
						}
					}
				}
			} else {
				if agentapi.Failure(err) != agentapi.Unavailable || set.Complete || set.Current != nil || set.Revision != "" || len(set.Candidates) != 0 || view.complete || view.cursor != nil {
					t.Fatalf("conflicting physical identity returned authority: %+v %v", set, err)
				}
				before := view.counts
				for range 2 {
					if refs, err := slice.Rollout(t.Context(), physicalRollout); agentapi.Failure(err) != agentapi.Unavailable || len(refs) != 0 {
						t.Fatal("conflicting physical refs accepted", refs, err)
					}
				}
				if before != view.counts {
					t.Fatal("conflict repeated acquisition")
				}
				cancelled, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err := slice.Thread(cancelled, id); !errors.Is(err, context.Canceled) {
					t.Fatal("conflict hid caller cancellation", err)
				}
			}
			if view.counts.physical != 2 || view.counts.requested != int64(len(header)+1+len(copyHeader)) {
				t.Fatal("physical census or header-only ranges lost", view.counts)
			}
			if err := slice.Close(); err != nil {
				t.Fatal(err)
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != 0 {
				t.Fatal("physical identity cleanup charge", used)
			}
		})
	}
}

type metadataRootScenario string

const (
	metadataRootRetarget       metadataRootScenario = "retarget"
	metadataRootCollapse       metadataRootScenario = "collapse"
	metadataRootUnchangedAlias metadataRootScenario = "unchanged-alias"
	metadataRootDuplicateAlias metadataRootScenario = "duplicate-alias"
)

func TestMetadataConstructorRootsRemainCoherentBeforeLazyAcquisition(t *testing.T) {
	for _, scenario := range []metadataRootScenario{metadataRootRetarget, metadataRootCollapse, metadataRootUnchangedAlias, metadataRootDuplicateAlias} {
		t.Run(string(scenario), func(t *testing.T) {
			fixtureLookup, ids, rootB := metadataFixture(t, 1)
			store := fixtureLookup.store
			if err := fixtureLookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(rootB, "sessions", "nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			db := hintDatabase(t, rootB, true)
			addHint(t, db, ids[0], path, time.Now())
			rootA := t.TempDir()
			alias := filepath.Join(t.TempDir(), "home")
			initial := rootB
			changed := scenario == metadataRootRetarget || scenario == metadataRootCollapse
			if changed {
				initial = rootA
			}
			if err := os.Symlink(initial, alias); err != nil {
				t.Fatal(err)
			}
			homes := []string{alias}
			if scenario == metadataRootCollapse || scenario == metadataRootDuplicateAlias {
				homes = append(homes, rootB)
			}
			lookup, err := NewCodexRolloutLookup(t.Context(), store, homes)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lookup.CloseReadOnly(); err != nil {
					t.Error(err)
				}
				if used, _ := lookup.readBudget.Charged(); used != 0 {
					t.Error("root coherence cleanup charge", used)
				}
			})
			if lookup.metadata != nil || lookup.queries != 0 {
				t.Fatal("constructor acquired metadata or SQL")
			}
			if changed {
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(rootB, alias); err != nil {
					t.Fatal(err)
				}
			}
			view := lookup.MetadataInventory().(*metadataInventory)
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = slice.Close() }()
			set, err := slice.Thread(t.Context(), ids[0])
			if changed {
				if agentapi.Failure(err) != agentapi.Changed || set.Complete || set.Current != nil || set.Revision != "" || len(set.Candidates) != 0 || view.complete || view.cursor != nil {
					t.Fatalf("constructor/acquisition root split returned authority: %+v %v", set, err)
				}
				if view.counts.physical != 0 || view.counts.requested != 0 || lookup.queries != 0 {
					t.Fatal("changed root acquired native evidence", view.counts, lookup.queries)
				}
				before := view.counts
				if refs, err := slice.Rollout(t.Context(), ids[0]); agentapi.Failure(err) != agentapi.Changed || len(refs) != 0 || view.counts != before {
					t.Fatal("changed root failure reacquired refs", refs, err)
				}
				cancelled, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err := slice.Thread(cancelled, ids[0]); !errors.Is(err, context.Canceled) {
					t.Fatal("changed root hid cancellation", err)
				}
			} else {
				if err != nil || !set.Complete || set.Current == nil || set.Current.Path != path || len(set.Candidates) != 1 || view.counts.physical != 1 {
					t.Fatal("coherent alias lost current selection", set, err, view.counts)
				}
				if err := slice.Check(t.Context(), ids[0], set.Revision); err != nil {
					t.Fatal("coherent current revision rejected", err)
				}
			}
			if err := slice.Close(); err != nil {
				t.Fatal(err)
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != 0 {
				t.Fatal("root coherence leaked charge", used)
			}
		})
	}
}

// Enumeration can finish before the fixed epoch expires during validation.
// A refused validation must not leave the inventory marked complete.
func TestMetadataValidationExpiryClearsCompleteInventory(t *testing.T) {
	lookup, _, _ := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil || slice.Valid(t.Context()) != nil || !view.complete {
		t.Fatal("initial census failed", err)
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	view.remaining = time.Nanosecond
	slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, refusal := slice.Thread(t.Context(), "11111111-1111-4111-8111-111111111111")
	if agentapi.Failure(slice.Valid(t.Context())) != agentapi.Limit || agentapi.Failure(refusal) != agentapi.Limit || set.Complete || set.Current != nil || len(set.Candidates) != 0 || view.complete || view.cursor != nil || agentapi.Failure(view.failure) != agentapi.Limit {
		t.Fatalf("expired validation retained authority: set=%+v refusal=%v complete=%t cursor=%t failure=%v remaining=%s", set, refusal, view.complete, view.cursor != nil, view.failure, view.remaining)
	}
}

// BeginValidationSlice and slice queries check the caller before entry and
// after completed work. The third check is the completed-work boundary.
type metadataCompletionContext struct {
	context.Context
	checks   int
	complete func()
}

func (c *metadataCompletionContext) Err() error {
	c.checks++
	if c.checks == 3 {
		c.complete()
	}
	return c.Context.Err()
}

type metadataExpiryOperation string

const (
	metadataExpiryRenewal metadataExpiryOperation = "renewal"
	metadataExpiryThread  metadataExpiryOperation = "thread"
	metadataExpiryRollout metadataExpiryOperation = "rollout"
	metadataExpiryCheck   metadataExpiryOperation = "check"
	metadataExpiryEntry   metadataExpiryOperation = "entry"
)

func TestMetadataOwnedExpiryInvalidatesCompletedEpoch(t *testing.T) {
	for _, operation := range []metadataExpiryOperation{metadataExpiryRenewal, metadataExpiryThread, metadataExpiryRollout, metadataExpiryCheck, metadataExpiryEntry} {
		t.Run(string(operation), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lookup, _, _ := metadataFixture(t, 0)
				lookup.homes, lookup.roots = nil, nil
				defer func() { _ = lookup.CloseReadOnly() }()
				view := lookup.MetadataInventory().(*metadataInventory)
				held, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
				if err != nil || held.Valid(t.Context()) != nil || !view.complete {
					t.Fatal("initial census", err)
				}
				defer func() { _ = held.Close() }()
				ctx := &metadataCompletionContext{Context: t.Context(), complete: func() { time.Sleep(31 * time.Second) }}
				var failure error
				switch operation {
				case metadataExpiryRenewal, metadataExpiryEntry:
					if operation == metadataExpiryEntry {
						view.remaining = 0
					}
					renewed, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = renewed.Close() }()
					failure = renewed.Valid(t.Context())
				case metadataExpiryThread:
					set, err := held.Thread(ctx, "00000000-0000-0000-0000-000000000001")
					failure = err
					if set.Complete || set.Current != nil || set.Revision != "" || len(set.Candidates) != 0 {
						t.Fatal("expired query retained selection", set)
					}
				case metadataExpiryRollout:
					refs, err := held.Rollout(ctx, "00000000-0000-0000-0000-000000000001")
					failure = err
					if len(refs) != 0 {
						t.Fatal("expired query retained candidates", refs)
					}
				case metadataExpiryCheck:
					failure = held.Check(ctx, "00000000-0000-0000-0000-000000000001", "revision")
				}
				if agentapi.Failure(failure) != agentapi.Limit || view.complete || view.failure == nil || view.cursor != nil || t.Context().Err() != nil {
					t.Fatalf("owned expiry retained epoch: operation=%s failure=%v complete=%v recorded=%v cursor=%v checks=%d remaining=%s", operation, failure, view.complete, view.failure, view.cursor, ctx.checks, view.remaining)
				}
				if operation != metadataExpiryEntry && ctx.checks < 3 {
					t.Fatal("did not reach completed-work boundary", ctx.checks)
				}
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
				if err := lookup.CloseReadOnly(); err != nil {
					t.Fatal(err)
				}
				if used, _ := lookup.readBudget.Charged(); used != 0 {
					t.Fatal("expiry leaked charge", used)
				}
			})
		})
	}
}

func TestMetadataCanceledValidationRenewalPreservesRetryableEpoch(t *testing.T) {
	lookup, _, _ := metadataFixture(t, 0)
	lookup.homes, lookup.roots = nil, nil
	defer func() { _ = lookup.CloseReadOnly() }()
	view := lookup.MetadataInventory().(*metadataInventory)
	initial, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil || initial.Valid(t.Context()) != nil || !view.complete {
		t.Fatal("initial census", err)
	}
	defer func() { _ = initial.Close() }()
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &metadataCompletionContext{Context: parent, complete: cancel}
	canceled, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = canceled.Close() }()
	if err := canceled.Valid(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation lost", err)
	}
	if !view.complete || view.failure != nil || view.cursor != nil {
		t.Fatal("caller cancellation poisoned completed epoch")
	}
	retry, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil || retry.Valid(t.Context()) != nil || !view.complete {
		t.Fatal("validation renewal did not retry", err)
	}
	defer func() { _ = retry.Close() }()
}
