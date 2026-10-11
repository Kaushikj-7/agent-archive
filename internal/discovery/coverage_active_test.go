package discovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// changingCoverageAdapter changes native files each time sessions/ is
// enumerated from its start, so every change lands between an epoch's
// observation and its validation, as a running Codex session's appends do.
type changingCoverageAdapter struct {
	SourceAdapter
	change func(call int)
	calls  int
}

func (a *changingCoverageAdapter) Enumerate(ctx context.Context, root, path string, cookie int64) (SourceBatch, error) {
	if path == "sessions" && cookie == 0 {
		a.calls++
		a.change(a.calls)
	}
	return a.SourceAdapter.Enumerate(ctx, root, path, cookie)
}

func rolloutPath(root, folder, id string) string {
	return filepath.Join(root, folder, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
}

func appendRolloutLine(tb testing.TB, path string) {
	tb.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"event_msg","payload":{"type":"token_count"}}` + "\n"); err != nil {
		tb.Fatal(err)
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
}

type coverageFixture struct {
	store *state.Store
	cfg   config.Config
	at    time.Time
	root  string
}

// requestThread enrolls a thread's coverage the way a registered capture does.
func (f coverageFixture) requestThread(t *testing.T, id string) {
	t.Helper()
	lookup, err := NewCodexRolloutLookup(t.Context(), f.store, []string{f.root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.Thread(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
}

// pass runs one scheduled discovery pass and reads the thread's coverage.
func (f coverageFixture) pass(t *testing.T, n int, adapter SourceAdapter, id string) (bool, []string) {
	t.Helper()
	lookup, err := NewCodexRolloutLookup(t.Context(), f.store, []string{f.root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runWithAdapters(context.Background(), f.store, f.cfg, Options{Now: func() time.Time { return f.at.Add(time.Duration(n) * time.Minute) }, Rollouts: lookup}, []SourceAdapter{adapter}); err != nil {
		t.Fatal(err)
	}
	set, err := lookup.Thread(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, candidate := range set.Candidates {
		paths = append(paths, candidate.Path)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	return set.Complete, paths
}

// A running Codex session appends to its rollout continuously. That must not
// keep every other thread's history coverage from ever completing, whether the
// requested rollout was archived before it was requested or moved from
// sessions/ to archived_sessions/ afterwards.
func TestCoverageCompletesWhileAnotherRolloutIsAppended(t *testing.T) {
	t.Parallel()
	for _, movedAfterRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "archived before request", true: "archived after request"}[movedAfterRequest], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
			project := cfg.Archive.Projects[0].Root
			folder := "archived_sessions"
			if movedAfterRequest {
				folder = "sessions"
			}
			wanted := writeRollout(t, root, project, at.Add(-2*time.Hour), 1, folder)
			active := writeRollout(t, root, project, at.Add(-time.Hour), 2, "sessions")
			f.requestThread(t, wanted)
			archived := rolloutPath(root, "archived_sessions", wanted)
			if movedAfterRequest {
				if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(rolloutPath(root, "sessions", wanted), archived); err != nil {
					t.Fatal(err)
				}
			}
			adapter := &changingCoverageAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}, change: func(int) { appendRolloutLine(t, rolloutPath(root, "sessions", active)) }}
			for n := range 4 {
				complete, candidates := f.pass(t, n, adapter, wanted)
				if !complete {
					continue
				}
				if !slices.Equal(candidates, []string{archived}) {
					t.Fatalf("candidates %v, want only the archived rollout", candidates)
				}
				return
			}
			t.Fatal("coverage never completed while another rollout was being appended")
		})
	}
}

// Membership digests no longer include rollout sizes and times, so an
// unrelated rollout rewritten in place to claim the requested thread between
// observation and validation must still fail that epoch.
func TestCoverageRejectsRolloutRewrittenToRequestedThreadMidEpoch(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	project := cfg.Archive.Projects[0].Root
	wanted := writeRollout(t, root, project, at.Add(-2*time.Hour), 1, "archived_sessions")
	other := writeRollout(t, root, project, at.Add(-time.Hour), 2, "sessions")
	claim, err := os.ReadFile(rolloutPath(root, "archived_sessions", wanted))
	if err != nil {
		t.Fatal(err)
	}
	f.requestThread(t, wanted)
	otherPath := rolloutPath(root, "sessions", other)
	before, err := os.Stat(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := false
	adapter := &changingCoverageAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}, change: func(call int) {
		// The first enumeration observes; the second validates.
		if call == 2 {
			if err := os.WriteFile(otherPath, claim, 0600); err != nil {
				t.Fatal(err)
			}
			rewritten = true
		}
	}}
	complete, candidates := f.pass(t, 0, adapter, wanted)
	if !rewritten {
		t.Fatal("fixture did not rewrite during validation")
	}
	after, err := os.Stat(otherPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("fixture replaced the file instead of rewriting it in place")
	}
	if complete {
		t.Fatalf("certified a thread inventory missing a rewritten member: %v", candidates)
	}
	for n := 1; n < 4; n++ {
		complete, candidates = f.pass(t, n, adapter, wanted)
		if complete {
			want := []string{rolloutPath(root, "archived_sessions", wanted), otherPath}
			slices.Sort(want)
			if !slices.Equal(candidates, want) {
				t.Fatalf("candidates %v, want %v", candidates, want)
			}
			return
		}
	}
	t.Fatal("coverage did not converge after the rewrite")
}

// Codex archives a rollout by renaming it from sessions/ to archived_sessions/.
// The observation cache keeps the old path's identity, and a lookup restores
// cached facts into an epoch that is still observing. Complete coverage must
// list only rollouts validation found, never the vanished pre-move locator.
func TestCoverageDropsCachedCandidateMovedOutOfSessions(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-2*time.Hour), 1, "sessions")
	f.requestThread(t, wanted)
	adapter := codexAdapter{supported: syntheticSupport}
	n := 0
	for ; n < 4; n++ {
		if complete, _ := f.pass(t, n, adapter, wanted); complete {
			break
		}
	}
	if n == 4 {
		t.Fatal("baseline coverage never completed")
	}
	archived := rolloutPath(root, "archived_sessions", wanted)
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rolloutPath(root, "sessions", wanted), archived); err != nil {
		t.Fatal(err)
	}
	// A pass stopped before observing anything leaves the new epoch observing,
	// so the next lookup restores the cached pre-move fact into it.
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runWithAdapters(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(10 * time.Minute) }, Rollouts: lookup, Stop: func() bool { return true }}, []SourceAdapter{adapter}); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	for n = 11; n < 15; n++ {
		complete, candidates := f.pass(t, n, adapter, wanted)
		if !complete {
			continue
		}
		if !slices.Equal(candidates, []string{archived}) {
			t.Fatalf("candidates %v, want only the archived rollout", candidates)
		}
		return
	}
	t.Fatal("coverage never completed after the move")
}

// Re-reading a changed or uncached rollout's header spends the pass's probe
// budget. A validation batch holding more such rollouts than one pass can
// probe must still finish across passes instead of rereading the same prefix.
func TestCoverageMemberChecksProgressAcrossProbeLimitedPasses(t *testing.T) {
	t.Parallel()
	_, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	for n := 1; n <= 150; n++ {
		writeRollout(t, root, project, at.Add(-time.Hour), n, "archived_sessions")
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	c := newCoverage([]string{root})
	c.request("requested")
	c.restart()
	collectCoverageDirectory(t, c, root, "sessions", false)
	collectCoverageDirectory(t, c, root, "archived_sessions", false)
	c.beginValidation()
	// An empty observation cache stands for entries evicted or changed since
	// observation; each pass leaves room for only 100 header probes.
	cat := &catalog{Coverage: c, Cache: map[string]cached{}}
	for pass := range 4 {
		h := &Health{Probes: HeaderProbes - 100}
		advanceCoverageValidation(t.Context(), cat, h, codexAdapter{}, time.Now().Add(time.Minute), Options{})
		if c.Phase == coverageComplete {
			if c.Failed || pass == 0 {
				t.Fatalf("pass %d failed=%v", pass, c.Failed)
			}
			return
		}
	}
	t.Fatal("validation never finished under a per-pass probe limit")
}

// inspectingCoverageAdapter rewrites the observation of validation-time
// header reads, standing in for a racing append or an in-place rewrite.
type inspectingCoverageAdapter struct {
	SourceAdapter
	inspect func(call int, o Observation) Observation
	calls   int
}

func (a *inspectingCoverageAdapter) Inspect(ctx context.Context, source SourceDescriptor) Observation {
	a.calls++
	return a.inspect(a.calls, a.SourceAdapter.Inspect(ctx, source))
}

// validatingRequestedRollout observes one requested rollout and begins its
// epoch's validation with an empty observation cache, so validation rereads it.
func validatingRequestedRollout(t *testing.T) (*catalog, string) {
	t.Helper()
	_, cfg, at, root := fixture(t)
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	c := newCoverage([]string{root})
	c.request(wanted)
	c.restart()
	path := rolloutPath(root, "sessions", wanted)
	source := codexAdapter{}.Describe(root, "sessions", filepath.Base(path)).Source
	observed := codexAdapter{}.Inspect(t.Context(), source)
	if observed.Identity == nil || observed.Identity.ThreadID != wanted {
		t.Fatalf("fixture identity %#v", observed.Identity)
	}
	c.observe(source, Fingerprint{}, *observed.Identity)
	collectCoverageDirectory(t, c, root, "sessions", false)
	collectCoverageDirectory(t, c, root, "archived_sessions", false)
	c.beginValidation()
	return &catalog{Coverage: c, Cache: map[string]cached{}}, wanted
}

// A header read racing an append is deferred to a later pass in the same
// epoch; it is neither certified nor allowed to restart the epoch.
func TestCoverageMemberReadRacingAppendDefersWithoutFailingEpoch(t *testing.T) {
	t.Parallel()
	cat, wanted := validatingRequestedRollout(t)
	epoch := cat.Coverage.Epoch
	adapter := &inspectingCoverageAdapter{SourceAdapter: codexAdapter{}, inspect: func(call int, o Observation) Observation {
		if call == 1 {
			return Observation{Outcome: outcomeChanged}
		}
		return o
	}}
	advanceCoverageValidation(t.Context(), cat, &Health{}, adapter, time.Now().Add(time.Minute), Options{})
	if cat.Coverage.Phase != coverageValidate || cat.Coverage.Failed {
		t.Fatalf("raced read phase=%s failed=%v", cat.Coverage.Phase, cat.Coverage.Failed)
	}
	advanceCoverageValidation(t.Context(), cat, &Health{}, adapter, time.Now().Add(time.Minute), Options{})
	if cat.Coverage.Phase != coverageComplete || cat.Coverage.Failed || cat.Coverage.Epoch != epoch || len(cat.Coverage.Requests[wanted].Candidates) != 1 {
		t.Fatalf("deferred validation did not finish its epoch: %#v", cat.Coverage)
	}
}

// A requested rollout whose identity changed in place between observation and
// validation fails the epoch rather than certifying either identity.
func TestCoverageFailsWhenRequestedRolloutIdentityChangesMidEpoch(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*codexmeta.CodexIdentity){
		"other thread": func(id *codexmeta.CodexIdentity) { id.ThreadID = "00000000-0000-0000-0000-000000000099" },
		"new fork":     func(id *codexmeta.CodexIdentity) { id.ForkID = "00000000-0000-0000-0000-000000000098" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cat, _ := validatingRequestedRollout(t)
			adapter := &inspectingCoverageAdapter{SourceAdapter: codexAdapter{}, inspect: func(_ int, o Observation) Observation {
				if o.Identity != nil {
					id := *o.Identity
					change(&id)
					o.Identity = &id
				}
				return o
			}}
			advanceCoverageValidation(t.Context(), cat, &Health{}, adapter, time.Now().Add(time.Minute), Options{})
			if cat.Coverage.Phase != coverageComplete || !cat.Coverage.Failed {
				t.Fatalf("phase=%s failed=%v", cat.Coverage.Phase, cat.Coverage.Failed)
			}
		})
	}
}

// A validating checkpoint written before membership marks existed carries
// unmarked candidates for batches it already validated. Finishing that epoch
// must not drop them and certify the request with no candidates.
func TestCoverageFailsValidatingCheckpointWithoutMemberMarks(t *testing.T) {
	t.Parallel()
	cat, wanted := validatingRequestedRollout(t)
	c := cat.Coverage
	// Validate every batch but stop before the final directory stamp check.
	for range 8 {
		if len(c.Validation) == 0 {
			break
		}
		advanceCoverageValidation(t.Context(), cat, &Health{Entries: 2047}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	}
	if c.Phase != coverageValidate || len(c.Validation) != 0 || c.FinalOffset >= len(c.Directories) || c.Failed {
		t.Fatalf("setup phase=%s validation=%d final=%d failed=%v", c.Phase, len(c.Validation), c.FinalOffset, c.Failed)
	}
	// This writer's own checkpoint resumes and certifies its marked candidate.
	current := restoreCoverage(t, c)
	advanceCoverageValidation(t.Context(), &catalog{Coverage: current, Cache: map[string]cached{}}, &Health{}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	if current.Phase != coverageComplete || current.Failed || current.Requests[wanted].CompleteEpoch != current.Epoch || len(current.Requests[wanted].Candidates) != 1 {
		t.Fatalf("marked checkpoint phase=%s failed=%v request=%#v", current.Phase, current.Failed, current.Requests[wanted])
	}
	request := c.Requests[wanted]
	for locator, candidate := range request.Candidates {
		candidate.Member = false
		request.Candidates[locator] = candidate
	}
	c.MemberChecks = false
	legacy := restoreCoverage(t, c)
	advanceCoverageValidation(t.Context(), &catalog{Coverage: legacy, Cache: map[string]cached{}}, &Health{}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	if legacy.Phase != coverageComplete || !legacy.Failed || legacy.Requests[wanted].CompleteEpoch != 0 || len(legacy.Requests[wanted].Candidates) != 1 {
		t.Fatalf("unmarked checkpoint phase=%s failed=%v request=%#v", legacy.Phase, legacy.Failed, legacy.Requests[wanted])
	}
}

// restoreCoverage round-trips a checkpoint the way a later pass loads it.
func restoreCoverage(t *testing.T, c *coverageInventory) *coverageInventory {
	t.Helper()
	// The catalog is persisted through local's generic JSON writer and reader.
	var checkpoint any = catalog{Coverage: c}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var restored catalog
	var target any = &restored
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
	if restored.Coverage == nil {
		t.Fatal("checkpoint lost its coverage")
	}
	if err := restored.Coverage.validate(c.Roots); err != nil {
		t.Fatal(err)
	}
	return restored.Coverage
}

// An overflowed request is not member-checked, so finishing its epoch keeps
// the candidates it recorded rather than treating them all as unlisted.
func TestCoverageKeepsOverflowedRequestCandidates(t *testing.T) {
	t.Parallel()
	cat, wanted := validatingRequestedRollout(t)
	request := cat.Coverage.Requests[wanted]
	request.Overflow = true
	cat.Coverage.Requests[wanted] = request
	advanceCoverageValidation(t.Context(), cat, &Health{}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	if cat.Coverage.Phase != coverageComplete || cat.Coverage.Failed || len(cat.Coverage.Requests[wanted].Candidates) != 1 {
		t.Fatalf("overflowed request phase=%s failed=%v request=%#v", cat.Coverage.Phase, cat.Coverage.Failed, cat.Coverage.Requests[wanted])
	}
}
