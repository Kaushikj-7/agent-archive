package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// chainEpoch is well away from a local midnight, so a rollout's day
// directory is the one its UUIDv7 time names in any time zone offset tests
// run under.
var chainEpoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)

// v7ID is a synthetic UUIDv7 created at, distinguished by n.
func v7ID(at time.Time, n int) string {
	ms := uint64(at.UnixMilli())
	return fmt.Sprintf("%08x-%04x-7000-8000-%012x", ms>>16, ms&0xffff, n)
}

// dayFolder is where Codex writes a rollout created at: its local date.
func dayFolder(id string) string {
	created, ok := uuidV7Time(id)
	if !ok {
		panic("not a UUIDv7: " + id)
	}
	return filepath.Join("sessions", created.Local().Format("2006/01/02"))
}

// chainRollout writes a paginated rollout of thread whose own records start
// after start, continuing from base when it is not nil.
func chainRollout(tb testing.TB, root, folder, rollout, thread string, start uint64, base *codexmeta.CodexHistoryPosition, texts ...string) (string, []byte) {
	tb.Helper()
	meta := map[string]any{"id": thread, "cwd": root, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
	if base != nil {
		meta["history_base"] = *base
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{"type": "session_meta", "ordinal": start, "payload": meta}); err != nil {
		tb.Fatal(err)
	}
	for i, text := range texts {
		if err := enc.Encode(map[string]any{"type": "response_item", "ordinal": start + uint64(i) + 1, "timestamp": "2026-10-01T12:01:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}); err != nil {
			tb.Fatal(err)
		}
	}
	path := filepath.Join(root, folder, "rollout-2026-10-01T12-00-00-"+rollout+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
		tb.Fatal(err)
	}
	return path, out.Bytes()
}

// twoSegmentThread writes a thread's original segment and a later segment,
// created a day later, that continues from it.
func twoSegmentThread(tb testing.TB, root string, n int, baseFolder string) (thread, basePath, tip, tipPath string) {
	tb.Helper()
	thread = v7ID(chainEpoch, n)
	if baseFolder == "" {
		baseFolder = dayFolder(thread)
	}
	basePath, raw := chainRollout(tb, root, baseFolder, thread, thread, 0, nil, "before")
	tip = v7ID(chainEpoch.Add(25*time.Hour), n)
	position := codexmeta.CodexHistoryPosition{RolloutID: thread, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}
	tipPath, _ = chainRollout(tb, root, dayFolder(tip), tip, thread, 2, &position, "after")
	return thread, basePath, tip, tipPath
}

// unrelatedRollout is a single-segment thread nobody asked about, the kind a
// running Codex keeps creating.
func unrelatedRollout(tb testing.TB, root string, n int) string {
	tb.Helper()
	id := v7ID(chainEpoch.Add(time.Duration(n)*time.Minute), 1<<20+n)
	path, _ := chainRollout(tb, root, dayFolder(id), id, id, 0, nil, "unrelated")
	return path
}

// chainRoot is a native home spelled as the lookup approves it.
func chainRoot(tb testing.TB) string {
	tb.Helper()
	root, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	return root
}

func chainPass(tb testing.TB, root string, lookup *CodexRolloutLookup) agentapi.SourcePass {
	tb.Helper()
	pass, err := (codex.SourceProvider{}).OpenPass(tb.Context(), agentapi.SourceEnvironment{CodexRollouts: lookup, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := pass.Close(); err != nil {
			tb.Error(err)
		}
	})
	return pass
}

func chainLookup(tb testing.TB, root string) *CodexRolloutLookup {
	tb.Helper()
	store, _, _, _ := fixture(tb)
	lookup, err := NewCodexRolloutLookup(tb.Context(), store, []string{root})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = lookup.Close() })
	return lookup
}

func revisionPaths(tb testing.TB, s agentapi.SourceSnapshot) []string {
	tb.Helper()
	candidates, ok := s.(agentapi.SourceRevisionCandidates)
	if !ok {
		tb.Fatal("snapshot lists no revision candidates")
	}
	set, err := candidates.RevisionCandidates(tb.Context())
	if err != nil {
		tb.Fatal(err)
	}
	var paths []string
	for _, ref := range slices.Concat(set.Chain, set.BestEffort) {
		paths = append(paths, ref.Path)
	}
	slices.Sort(paths)
	return paths
}

// Regression: Codex rollouts kept appearing while Codex ran, so the global
// coverage epoch a thread's capture waited for never validated, and once 256
// threads had requested it no new thread could even ask. A thread whose
// current rollout and history base are readable is captured in one pass,
// with any number of other threads and unrelated rollouts appearing around
// it, and the same lineage keeps its signature on the next pass.
func TestThreadChainCapturesWhileUnrelatedRolloutsAppear(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	thread, basePath, _, tipPath := twoSegmentThread(t, root, 1, "")
	db := hintDatabase(t, root, false)
	addHint(t, db, thread, tipPath, chainEpoch)
	lookup := chainLookup(t, root)
	// More threads than the old coverage request table held.
	for n := range 300 {
		path := unrelatedRollout(t, root, n)
		if _, err := lookup.Thread(t.Context(), codexmeta.RolloutID(path)); err != nil {
			t.Fatal(err)
		}
	}
	pass := chainPass(t, root, lookup)
	ref := agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tipPath, Key: thread}
	observed, err := pass.Signature(t.Context(), ref)
	if err != nil {
		t.Fatalf("signature waited on unrelated rollouts: %v", err)
	}
	unrelatedRollout(t, root, 1000)
	s, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatalf("read waited on unrelated rollouts: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.Observation().Signature != observed.Signature {
		t.Fatal("read and signature disagree on an unchanged lineage")
	}
	if got, want := revisionPaths(t, s), []string{basePath, tipPath}; !slices.Equal(got, want) {
		t.Fatalf("revision candidates = %v, want %v", got, want)
	}

	// The next pass, after more unrelated rollouts, sees the same lineage.
	unrelatedRollout(t, root, 1001)
	next := chainLookup(t, root)
	again, err := chainPass(t, root, next).Signature(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if again.Signature != observed.Signature {
		t.Fatal("unrelated rollouts changed an unchanged lineage's signature")
	}
}

// Codex moves archived rollouts into archived_sessions/; a base found there
// by its ID serves the chain as well as one in its day directory.
func TestThreadChainFindsBaseInArchivedSessions(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	thread, basePath, _, tipPath := twoSegmentThread(t, root, 2, "archived_sessions")
	db := hintDatabase(t, root, false)
	addHint(t, db, thread, tipPath, chainEpoch)
	pass := chainPass(t, root, chainLookup(t, root))
	s, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tipPath, Key: thread}, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if got, want := revisionPaths(t, s), []string{basePath, tipPath}; !slices.Equal(got, want) {
		t.Fatalf("revision candidates = %v, want %v", got, want)
	}
}

// A base observed in its day directory and since moved into
// archived_sessions/ is found where it is now: the stale observation neither
// hides it nor counts as a second locator.
func TestThreadChainFindsObservedBaseMovedToArchivedSessions(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	thread, oldPath, _, tipPath := twoSegmentThread(t, root, 4, "")
	db := hintDatabase(t, root, false)
	addHint(t, db, thread, tipPath, chainEpoch)
	lookup := chainLookup(t, root)
	info, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	lookup.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: oldPath, StableKey: thread}, Fingerprint{Size: info.Size(), Mtime: info.ModTime().UnixNano()}, &codexmeta.CodexIdentity{ThreadID: thread, RolloutID: thread})
	basePath := filepath.Join(root, "archived_sessions", filepath.Base(oldPath))
	if err := os.MkdirAll(filepath.Dir(basePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, basePath); err != nil {
		t.Fatal(err)
	}
	refs, err := lookup.Rollout(t.Context(), thread)
	if err != nil || len(refs) != 1 || refs[0].Path != basePath {
		t.Fatal("moved base: ", refs, err)
	}
	s, err := chainPass(t, root, lookup).Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tipPath, Key: thread}, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}

// A segment the chain continues from that is nowhere to be found is a named
// failure a later pass retries.
func TestThreadChainMissingBaseIsANamedFailure(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	thread, basePath, _, tipPath := twoSegmentThread(t, root, 3, "")
	if err := os.Remove(basePath); err != nil {
		t.Fatal(err)
	}
	db := hintDatabase(t, root, false)
	addHint(t, db, thread, tipPath, chainEpoch)
	pass := chainPass(t, root, chainLookup(t, root))
	s, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tipPath, Key: thread}, agentapi.ReadLimits{})
	if err == nil {
		_ = s.Close()
		t.Fatal("read a chain whose base is gone")
	}
	var e *agentapi.SourceError
	if !errors.As(err, &e) || e.Kind != agentapi.Unavailable || e.Cause != agentapi.CauseHistoryBaseUnavailable || agentapi.Deterministic(err) {
		t.Fatalf("missing base: %v", err)
	}
}

// Without a row in Codex's index, the thread's observed rollouts are its
// candidates, and a segment observed later changes the revision, so the
// source looks changed and is captured again.
func TestThreadWithoutCurrentLocatorFollowsObservedSegments(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	thread, basePath, tip, tipPath := twoSegmentThread(t, root, 4, "")
	lookup := chainLookup(t, root)
	observe := func(l *CodexRolloutLookup, path, rollout string) {
		t.Helper()
		header := sourcefacts.ReadHeader(t.Context(), root, path)
		if header.Identity == nil {
			t.Fatal("synthetic header unreadable", header.Outcome)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		l.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: rollout}, Fingerprint{Size: info.Size(), Mtime: info.ModTime().UnixNano()}, header.Identity)
	}
	observe(lookup, basePath, thread)
	before, err := lookup.Thread(t.Context(), thread)
	if err != nil || before.Current != nil || len(before.Candidates) != 1 {
		t.Fatal(before, err)
	}
	observe(lookup, tipPath, tip)
	if err := lookup.Check(t.Context(), thread, before.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("a newly observed segment kept the revision", err)
	}
	next := chainLookup(t, root)
	observe(next, basePath, thread)
	observe(next, tipPath, tip)
	pass := chainPass(t, root, next)
	s, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: basePath, Key: thread}, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if got, want := revisionPaths(t, s), []string{basePath, tipPath}; !slices.Equal(got, want) {
		t.Fatalf("tip not chosen from observations: %v", got)
	}
}

// A catalog an earlier version wrote still carries its coverage epoch, here
// the failed, full one a running Codex left behind. It loads, its observation
// cache still serves, and the next checkpoint drops the epoch.
func TestCatalogWithLegacyCoverageEpochLoadsAndDropsIt(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	requests := map[string]any{}
	for n := range 256 {
		requests[fmt.Sprintf("00000000-0000-4000-8000-%012d", n)] = map[string]any{"order": n + 1, "target_epoch": 956, "candidates": map[string]any{}}
	}
	legacy := map[string]any{
		"version": catalogVersion, "roots": []string{root}, "queue": []any{}, "cache": map[string]any{},
		"native_coverage": map[string]any{"version": 1, "epoch": 956, "phase": "validate", "failed": true, "roots": []string{root}, "requests": requests, "directories": map[string]any{}},
	}
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	if err := local.WriteCompact(path, legacy); err != nil {
		t.Fatal(err)
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal("legacy catalog refused:", err)
	}
	if lookup.catalog == nil {
		t.Fatal("legacy catalog was rebuilt rather than read")
	}
	if _, err := lookup.Thread(t.Context(), v7ID(chainEpoch, 9)); err != nil {
		t.Fatal(err)
	}
	if _, err := runWithAdapters(t.Context(), store, cfg, Options{Rollouts: lookup, Now: func() time.Time { return at.Add(time.Minute) }}, []SourceAdapter{codexAdapter{supported: syntheticSupport}}); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "native_coverage") {
		t.Fatal("checkpoint kept the legacy coverage epoch")
	}
}

func TestUUIDv7TimeSelectsDayDirectories(t *testing.T) {
	t.Parallel()
	id := v7ID(chainEpoch, 1)
	created, ok := uuidV7Time(id)
	if !ok || !created.Equal(chainEpoch) {
		t.Fatalf("uuidV7Time(%s) = %v %v", id, created, ok)
	}
	for _, other := range []string{"11111111-1111-4111-8111-111111111111", "not-a-uuid", ""} {
		if _, ok := uuidV7Time(other); ok {
			t.Fatalf("%q read as UUIDv7", other)
		}
	}
	dirs := rolloutDirectories(id)
	want := []string{
		filepath.Join("sessions", chainEpoch.AddDate(0, 0, -1).Format("2006/01/02")),
		filepath.Join("sessions", chainEpoch.Format("2006/01/02")),
		filepath.Join("sessions", chainEpoch.AddDate(0, 0, 1).Format("2006/01/02")),
		"archived_sessions",
	}
	if !slices.Equal(dirs, want) {
		t.Fatalf("directories = %v, want %v", dirs, want)
	}
	if got := rolloutDirectories("11111111-1111-4111-8111-111111111111"); !slices.Equal(got, []string{"archived_sessions"}) {
		t.Fatalf("non-v7 directories = %v", got)
	}
}

// Each directory a base search reads is listed once per pass, however many
// lineages it serves, and its charge is returned at close.
func TestRolloutSearchListsEachDirectoryOncePerPass(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	var ids []string
	for n := range 3 {
		path := unrelatedRollout(t, root, n)
		ids = append(ids, codexmeta.RolloutID(path))
	}
	lookup := chainLookup(t, root)
	for _, id := range ids {
		refs, err := lookup.Rollout(t.Context(), id)
		if err != nil || len(refs) != 1 || codexmeta.RolloutID(refs[0].Path) != id {
			t.Fatal(refs, err)
		}
	}
	if len(lookup.listings) != len(rolloutDirectories(ids[0])) || lookup.listingBytes == 0 {
		t.Fatalf("listings = %d, bytes = %d", len(lookup.listings), lookup.listingBytes)
	}
	if refs, err := lookup.Rollout(t.Context(), v7ID(chainEpoch, 77)); err != nil || len(refs) != 0 {
		t.Fatal("found a rollout that does not exist", refs, err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("listing charge leaked", used)
	}
}

// Only a checkpoint that ran out of time or was cancelled is handed to the
// lookup's close; other write failures stay the pass's own.
func TestDeferCheckpointOnlyForDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err      error
		deferred bool
	}{
		{context.DeadlineExceeded, true},
		{context.Canceled, true},
		{errCatalogBudget, false},
	} {
		l := &CodexRolloutLookup{}
		c := catalog{Version: catalogVersion}
		l.deferCheckpoint(&c, tc.err)
		if (l.catalog == &c && l.dirty) != tc.deferred {
			t.Fatalf("%v: deferred=%v dirty=%v", tc.err, l.catalog == &c, l.dirty)
		}
	}
	var none *CodexRolloutLookup
	none.deferCheckpoint(&catalog{}, context.DeadlineExceeded)
}

func TestLookupUnchangedCloseDoesNotRewriteCatalog(t *testing.T) {
	t.Parallel()
	store, cfg, _, root := fixture(t)
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t.Context(), store, cfg, Options{Rollouts: l}, syntheticSupport); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.dirty {
		t.Fatal("saved checkpoint still dirty")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged close wrote catalog")
	}
}

func TestHistoricalPolicySkipsOnlyUnsolicitedDisabledDiscovery(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name       string
		historical bool
		enabled    bool
		observed   bool
	}{
		{name: "historical disabled", historical: true},
		{name: "historical explicitly enabled", historical: true, enabled: true, observed: true},
		{name: "legacy disabled", observed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			cfg.HistoricalProjectProtection = scenario.historical
			cfg.Discovery.Enabled = scenario.enabled
			l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := l.Close(); err != nil {
					t.Error(err)
				}
			}()
			health, err := Run(t.Context(), store, cfg, Options{Rollouts: l, Now: func() time.Time { return at.Add(time.Minute) }})
			if err != nil {
				t.Fatal(err)
			}
			if (health.Entries > 0) != scenario.observed || (len(l.observations) > 0) != scenario.observed {
				t.Fatalf("native inventory eligibility changed: %+v", health)
			}
		})
	}
}

// A directory whose listing the pass can no longer keep (a large
// archived_sessions/, say) is still searched for the one rollout asked for,
// rather than failing every base search through it on every pass.
func TestRolloutSearchPastListingAllowanceStillFindsTheRollout(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	id := v7ID(chainEpoch, 5)
	path, _ := chainRollout(t, root, "archived_sessions", id, id, 0, nil, "archived")
	lookup := chainLookup(t, root)
	// Nothing was charged for this allowance; Close must not release it.
	lookup.listingBytes = maxListingBytes
	t.Cleanup(func() { lookup.listingBytes = 0 })
	refs, err := lookup.Rollout(t.Context(), id)
	if err != nil || len(refs) != 1 || refs[0].Path != path {
		t.Fatal(refs, err)
	}
	if _, kept := lookup.listings[filepath.Join(root, "archived_sessions")]; kept {
		t.Fatal("kept a listing past the allowance")
	}
}

// A directory a rollout search cannot read (archived_sessions/ too large to
// list, say) does not hide the rollout from the directories it can.
func TestRolloutSearchPastAnUnreadableDirectoryStillFindsTheRollout(t *testing.T) {
	t.Parallel()
	root := chainRoot(t)
	id := v7ID(chainEpoch, 6)
	path, _ := chainRollout(t, root, rolloutDirectories(id)[1], id, id, 0, nil, "day")
	if err := os.WriteFile(filepath.Join(root, "archived_sessions"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := chainLookup(t, root)
	refs, err := lookup.Rollout(t.Context(), id)
	if err != nil || len(refs) != 1 || refs[0].Path != path {
		t.Fatal(refs, err)
	}
	// With nothing found, the unreadable directory is still the failure.
	if refs, err := lookup.Rollout(t.Context(), v7ID(chainEpoch, 7)); err == nil {
		t.Fatal("an unreadable directory hid nothing: ", refs)
	}
}
