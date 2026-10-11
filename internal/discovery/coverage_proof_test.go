package discovery

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

// passThread runs one discovery pass, stopping it once stop reports true, and
// returns the thread's rollout set as a capture in that pass would see it.
func (f coverageFixture) passThread(t *testing.T, n int, id string, stop func(*CodexRolloutLookup) bool, inspect func(*CodexRolloutLookup)) agentapi.CodexRolloutSet {
	t.Helper()
	lookup, err := NewCodexRolloutLookup(t.Context(), f.store, []string{f.root})
	if err != nil {
		t.Fatal(err)
	}
	var halt func() bool
	if stop != nil {
		halt = func() bool { return stop(lookup) }
	}
	o := Options{Now: func() time.Time { return f.at.Add(time.Duration(n) * time.Minute) }, Rollouts: lookup, Stop: halt}
	if _, err := runWithAdapters(context.Background(), f.store, f.cfg, o, []SourceAdapter{codexAdapter{supported: syntheticSupport}}); err != nil {
		t.Fatal(err)
	}
	set, err := lookup.Thread(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inspect != nil {
		inspect(lookup)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	return set
}

func candidatePaths(set agentapi.CodexRolloutSet) []string {
	var paths []string
	for _, candidate := range set.Candidates {
		paths = append(paths, candidate.Path)
	}
	return paths
}

// completedCoverage requests a thread and runs passes until its coverage
// completes, returning the complete set.
func completedCoverage(t *testing.T, f coverageFixture, id string) agentapi.CodexRolloutSet {
	t.Helper()
	f.requestThread(t, id)
	for n := range 4 {
		if set := f.passThread(t, n, id, nil, nil); set.Complete {
			return set
		}
	}
	t.Fatal("baseline coverage never completed")
	return agentapi.CodexRolloutSet{}
}

func stopNow(*CodexRolloutLookup) bool { return true }

// A completed epoch restarts at the next pass's load. Its proof must stay
// usable, with the same revision, while the following epoch is in progress,
// and in every later pass, so an unchanged lineage never reads as incomplete
// or changed just because coverage rotated.
func TestCoverageProofStaysUsableWhileNextEpochRuns(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	complete := completedCoverage(t, f, wanted)
	want := []string{rolloutPath(root, "sessions", wanted)}
	if !slices.Equal(candidatePaths(complete), want) || complete.LineageToken() != complete.Revision {
		t.Fatalf("baseline %#v", complete)
	}
	for n := 10; n < 16; n++ {
		stop := stopNow
		if n%2 == 1 {
			stop = nil // a whole pass, which completes the next epoch itself
		}
		var epoch, proven uint64
		set := f.passThread(t, n, wanted, stop, func(l *CodexRolloutLookup) {
			epoch, proven = l.coverage.Epoch, l.coverage.Requests[wanted].ProvenEpoch
		})
		if stop != nil && (proven == 0 || proven >= epoch) {
			t.Fatalf("pass %d did not run on a retained proof: epoch %d proven %d", n, epoch, proven)
		}
		if !set.Complete || !slices.Equal(candidatePaths(set), want) {
			t.Fatalf("pass %d complete=%v candidates %v", n, set.Complete, candidatePaths(set))
		}
		if set.Revision != complete.Revision || set.LineageToken() != complete.Revision {
			t.Fatalf("pass %d changed an unchanged lineage's revision", n)
		}
	}
}

// Without any proof the same evidence is incomplete, so its revision differs,
// but its lineage token stays what the complete set carried: source
// signatures built on it do not change when coverage alone does.
func TestCoverageLineageTokenIgnoresCompleteness(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	complete := completedCoverage(t, f, wanted)
	f.passThread(t, 10, wanted, stopNow, func(l *CodexRolloutLookup) {
		request := l.coverage.Requests[wanted]
		request.ProvenEpoch, request.Proven = 0, nil
		l.coverage.Requests[wanted] = request
		set, err := l.Thread(t.Context(), wanted)
		if err != nil {
			t.Fatal(err)
		}
		if set.Complete || set.Revision == complete.Revision || !slices.Equal(candidatePaths(set), candidatePaths(complete)) {
			t.Fatalf("incomplete set %#v", set)
		}
		if set.LineageToken() != complete.LineageToken() {
			t.Fatal("coverage completeness changed the lineage token")
		}
		if err := l.Check(t.Context(), wanted, complete.Revision); err == nil {
			t.Fatal("in-pass revision check ignored a completeness change")
		}
	})
}

// A listing that finds a locator the retained proof does not hold (here, the
// rollout renamed into archived_sessions/) retires that proof at once: the
// thread reads incomplete until a newer epoch proves the new set.
func TestCoverageProofRetiredWhenListingFindsNewMember(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	completedCoverage(t, f, wanted)
	archived := rolloutPath(root, "archived_sessions", wanted)
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rolloutPath(root, "sessions", wanted), archived); err != nil {
		t.Fatal(err)
	}
	// Observe every directory, but stop before validation begins.
	observed := func(l *CodexRolloutLookup) bool { return l.coverage.Phase == coverageValidate }
	set := f.passThread(t, 10, wanted, observed, nil)
	if set.Complete {
		t.Fatalf("served a proof the listing contradicts: %v", candidatePaths(set))
	}
	set = f.passThread(t, 11, wanted, nil, nil)
	if !set.Complete || !slices.Equal(candidatePaths(set), []string{archived}) {
		t.Fatalf("new epoch complete=%v candidates %v", set.Complete, candidatePaths(set))
	}
}

// Restored observation-cache facts may name a locator that no longer exists,
// so they never retire a retained proof.
func TestCoverageProofSurvivesRestoredStaleCacheFacts(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	complete := completedCoverage(t, f, wanted)
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	var c catalog
	if err := local.Read(path, &c); err != nil {
		t.Fatal(err)
	}
	entry, found := c.Cache[rolloutPath(root, "sessions", wanted)]
	if !found {
		t.Fatal("fixture cache lacks the rollout")
	}
	c.Cache[rolloutPath(root, "archived_sessions", wanted)] = entry
	if err := local.WriteCompact(path, c); err != nil {
		t.Fatal(err)
	}
	set := f.passThread(t, 10, wanted, stopNow, nil)
	if !set.Complete || !slices.Equal(candidatePaths(set), candidatePaths(complete)) {
		t.Fatalf("stale cache fact retired the proof: complete=%v candidates %v", set.Complete, candidatePaths(set))
	}
}

// restart keeps a retained proof across a failed epoch, replaces it with an
// unfailed epoch's result, and drops it when that epoch did not cover the
// request. Checkpoints round-trip it and refuse inconsistent proofs.
func TestCoverageRetainedProofRotationAndCheckpoint(t *testing.T) {
	t.Parallel()
	cat, wanted := validatingRequestedRollout(t)
	c := cat.Coverage
	advanceCoverageValidation(t.Context(), cat, &Health{}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	if c.Phase != coverageComplete || c.Failed {
		t.Fatalf("setup phase=%s failed=%v", c.Phase, c.Failed)
	}
	proved := c.Epoch
	c.restart()
	if r := c.Requests[wanted]; r.ProvenEpoch != proved || len(r.Proven) != 1 || len(r.Candidates) != 0 {
		t.Fatalf("restart did not retain the proof: %#v", r)
	}
	if _, epoch, ok := c.proven(wanted); !ok || epoch != proved {
		t.Fatal("retained proof unusable while the next epoch observes")
	}
	restored := restoreCoverage(t, c)
	if _, epoch, ok := restored.proven(wanted); !ok || epoch != proved {
		t.Fatal("checkpoint lost the retained proof")
	}

	// A failed epoch keeps the proof.
	c.Phase, c.Failed = coverageComplete, true
	c.restart()
	if c.Requests[wanted].ProvenEpoch != proved {
		t.Fatal("failed epoch discarded the retained proof")
	}

	// Inconsistent retained proofs are refused on load.
	for name, tamper := range map[string]func(*coverageRequest){
		"unmarked member": func(r *coverageRequest) {
			for locator, candidate := range r.Proven {
				candidate.Member = false
				r.Proven[locator] = candidate
			}
		},
		"future epoch":   func(r *coverageRequest) { r.ProvenEpoch = c.Epoch },
		"epochless":      func(r *coverageRequest) { r.ProvenEpoch = 0 },
		"other identity": func(r *coverageRequest) { r.Proven = map[string]coverageCandidate{"/elsewhere": {}} },
		"requested after the proof": func(r *coverageRequest) {
			r.TargetEpoch = r.ProvenEpoch + 1
		},
		"too many candidates": func(r *coverageRequest) {
			var member coverageCandidate
			for _, candidate := range r.Proven {
				member = candidate
			}
			for n := range 65 {
				copied := member
				copied.Source.Locator = filepath.Join(member.Source.Root, "sessions", "extra-"+strconv.Itoa(n)+".jsonl")
				r.Proven[copied.Source.Locator] = copied
			}
		},
	} {
		copied := restoreCoverage(t, c)
		r := copied.Requests[wanted]
		tamper(&r)
		copied.Requests[wanted] = r
		if err := copied.validate(copied.Roots); err == nil {
			t.Fatalf("%s: accepted an inconsistent retained proof", name)
		}
	}

	// An unfailed epoch that did not cover the request (it overflowed) drops it.
	c.Phase, c.proofEpoch = coverageComplete, c.Epoch
	r := c.Requests[wanted]
	r.Overflow = true
	c.Requests[wanted] = r
	c.restart()
	if r := c.Requests[wanted]; r.ProvenEpoch != 0 || r.Proven != nil {
		t.Fatalf("uncovered epoch kept an older proof: %#v", r)
	}
}

// A pass whose catalog checkpoint misses its deadline, possibly right after
// coverage completed, must not lose that epoch: the lookup's close writes it,
// so the next pass still serves the proof while its own epoch runs.
func TestCoverageCompletionSurvivesFailedCheckpoint(t *testing.T) {
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	f.requestThread(t, wanted)
	for n := range 4 {
		lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		persistCatalog = func(context.Context, *CodexRolloutLookup, string, catalog) error {
			return context.DeadlineExceeded
		}
		_, err = runWithAdapters(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(time.Duration(n) * time.Minute) }, Rollouts: lookup}, []SourceAdapter{codexAdapter{supported: syntheticSupport}})
		persistCatalog = persistNativeCatalog
		if err == nil {
			t.Fatal("checkpoint failure not reported")
		}
		if err := lookup.Close(); err != nil {
			t.Fatal(err)
		}
		if set := f.passThread(t, 10+n, wanted, stopNow, nil); set.Complete {
			if !slices.Equal(candidatePaths(set), []string{rolloutPath(root, "sessions", wanted)}) {
				t.Fatalf("candidates %v", candidatePaths(set))
			}
			return
		}
	}
	t.Fatal("coverage completed only in memory; failed checkpoints lost it")
}

// A retained proof is a past observation: a same-thread segment Codex creates
// after it, in a directory no listing has reached since, is missing from it.
// The set must say its proof is retained, so a consumer that would select a
// lineage tip from the candidates alone (no current locator) can refuse.
func TestCoverageRetainedProofMarksSetRetained(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	complete := completedCoverage(t, f, wanted)
	if complete.Retained {
		t.Fatal("the completing epoch's own proof reported as retained")
	}
	raw, err := os.ReadFile(rolloutPath(root, "sessions", wanted))
	if err != nil {
		t.Fatal(err)
	}
	const segment = "abcdefab-abcd-4abc-8abc-abcdefabcdea"
	header := string(raw[:strings.IndexByte(string(raw), '\n')])
	header = strings.Replace(header, `"source":"cli"`, `"source":"cli","history_mode":"paginated","history_base":{"thread_id":"`+wanted+`","end_ordinal_exclusive":1,"end_byte_offset":`+strconv.Itoa(len(header)+1)+`}`, 1)
	newer := filepath.Join(root, "sessions", "2026", "10", "02", "rollout-2026-10-02T12-00-00-"+segment+".jsonl")
	if err := os.MkdirAll(filepath.Dir(newer), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte(header+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	set := f.passThread(t, 10, wanted, stopNow, nil)
	if !set.Complete || slices.Contains(candidatePaths(set), newer) {
		t.Fatalf("fixture: expected the retained proof without the new segment, got complete=%v %v", set.Complete, candidatePaths(set))
	}
	if !set.Retained {
		t.Fatal("a retained proof missing a newer segment was served as this epoch's")
	}
	// A whole pass lists the segment, which retires the proof.
	set = f.passThread(t, 11, wanted, nil, nil)
	if set.Complete && !slices.Contains(candidatePaths(set), newer) {
		t.Fatalf("listing the new segment left a proof without it: %v", candidatePaths(set))
	}
}

// Live evidence retires the current epoch's own proof too, once it is
// complete, and that retired proof is not retained by the next epoch. Restored
// cache facts do neither. A request overflowing in a later epoch, which has
// listed more candidates than any proof can hold, is served no retained proof.
func TestCoverageCurrentProofRetiredByLiveEvidence(t *testing.T) {
	t.Parallel()
	cat, wanted := validatingRequestedRollout(t)
	c := cat.Coverage
	advanceCoverageValidation(t.Context(), cat, &Health{}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	if _, epoch, ok := c.proven(wanted); !ok || epoch != c.Epoch {
		t.Fatalf("setup phase=%s failed=%v", c.Phase, c.Failed)
	}
	var held coverageCandidate
	for _, candidate := range c.Requests[wanted].Candidates {
		held = candidate
	}
	other := held.Source
	other.Locator = filepath.Join(other.Root, "archived_sessions", filepath.Base(held.Source.Locator))
	c.observe(other, Fingerprint{}, held.Identity, false)
	if _, _, ok := c.proven(wanted); !ok {
		t.Fatal("a restored cache fact retired the current proof")
	}
	c.observe(other, Fingerprint{}, held.Identity, true)
	if _, _, ok := c.proven(wanted); ok {
		t.Fatal("a listed locator the current proof lacks left it in use")
	}
	c.restart()
	if _, _, ok := c.proven(wanted); ok {
		t.Fatal("the next epoch retained a retired proof")
	}

	cat, wanted = validatingRequestedRollout(t)
	c = cat.Coverage
	advanceCoverageValidation(t.Context(), cat, &Health{}, codexAdapter{}, time.Now().Add(time.Minute), Options{})
	c.restart()
	if _, _, ok := c.proven(wanted); !ok {
		t.Fatal("setup: no retained proof")
	}
	r := c.Requests[wanted]
	r.Overflow = true
	c.Requests[wanted] = r
	if _, _, ok := c.proven(wanted); ok {
		t.Fatal("an overflowing epoch still served the retained proof")
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
		if (l.catalog == &c && l.coverageDirty) != tc.deferred {
			t.Fatalf("%v: deferred=%v dirty=%v", tc.err, l.catalog == &c, l.coverageDirty)
		}
	}
	var none *CodexRolloutLookup
	none.deferCheckpoint(&catalog{}, context.DeadlineExceeded)
}
