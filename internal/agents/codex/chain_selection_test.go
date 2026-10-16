package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// sourceCause is the cause of err's first SourceError.
func sourceCause(err error) agentapi.SourceCause {
	var e *agentapi.SourceError
	if !errors.As(err, &e) {
		return ""
	}
	return e.Cause
}

// chainFixture is thread A's original segment and a later segment C that
// continues from it.
func chainFixture(t *testing.T, dir string) (base, leaf agentapi.SourceRef, position codexmeta.CodexHistoryPosition) {
	t.Helper()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	position = codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}
	leaf, _ = historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": position}, "after")
	return base, leaf, position
}

// A rollout the current segment continues from that cannot be found is a
// named failure the next pass retries, never a wait on some other proof.
func TestMissingHistoryBaseIsANamedRetryableFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, _ := chainFixture(t, dir)
	// A base the lookup found that is gone by the time it is opened.
	vanished := base
	vanished.Path = filepath.Join(filepath.Dir(base.Path), "gone-"+filepath.Base(base.Path))
	for name, rollouts := range map[string]map[string][]agentapi.SourceRef{
		"missing":   nil,
		"ambiguous": {threadA: {base, leaf}},
		"vanished":  {threadA: {vanished}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: rollouts}
			s, err := historyPass(t, dir, l).Read(t.Context(), leaf, agentapi.ReadLimits{})
			if err == nil {
				_ = s.Close()
				t.Fatal("read a lineage whose base is unresolved")
			}
			if agentapi.Failure(err) != agentapi.Unavailable || sourceCause(err) != agentapi.CauseHistoryBaseUnavailable || agentapi.Deterministic(err) {
				t.Fatalf("unresolved base: kind=%s cause=%q deterministic=%v: %v", agentapi.Failure(err), sourceCause(err), agentapi.Deterministic(err), err)
			}
		})
	}
}

// With a current locator, an observed same-thread rollout off its chain
// (another branch from the same base) neither blocks the read nor chooses
// the tip; it is offered as a best-effort historical revision after the
// chain, and an observation whose file is gone is passed over.
func TestOffChainSameThreadRolloutIsNoBlockerBesideCurrentLocator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, position := chainFixture(t, dir)
	other, _ := historyFile(t, dir, rolloutD, threadA, 2, map[string]any{"history_base": position}, "elsewhere")
	gone := agentapi.SourceRef{Path: filepath.Join(dir, "rollout-2026-10-01T12-00-00-55555555-5555-4555-8555-555555555555.jsonl"), Key: threadA}
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Candidates: []agentapi.SourceRef{gone, other}, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p := historyPass(t, dir, l)
	s, err := p.Read(t.Context(), base, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	candidates, ok := s.(agentapi.SourceRevisionCandidates)
	if !ok {
		t.Fatal("history snapshot lists no revision candidates")
	}
	set, err := candidates.RevisionCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	paths := func(refs []agentapi.SourceRef) []string {
		var out []string
		for _, ref := range refs {
			out = append(out, ref.Path)
		}
		return out
	}
	if got, want := paths(set.Chain), []string{base.Path, leaf.Path}; !slices.Equal(got, want) {
		t.Fatalf("chain candidates = %v, want %v", got, want)
	}
	if got, want := paths(set.BestEffort), []string{other.Path}; !slices.Equal(got, want) {
		t.Fatalf("best-effort candidates = %v, want the off-chain branch %v", got, want)
	}
}

// Without a current locator, the seed alone is its own tip; its chain is
// found through the base lookup.
func TestNoCurrentLocatorReadsTheSeedAsItsOwnTip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, _ := chainFixture(t, dir)
	l := &historyLookup{rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	if b := historyRead(t, historyPass(t, dir, l), leaf); b.History.ActiveRolloutID != rolloutC || len(b.NativeRecords) != 4 {
		t.Fatalf("seed tip: %+v", b.History)
	}
}

// Without a current locator, observed rollouts that end in more than one tip
// are a named failure, not a wait.
func TestNoCurrentLocatorAmbiguousTipsIsANamedFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, position := chainFixture(t, dir)
	other, _ := historyFile(t, dir, rolloutD, threadA, 2, map[string]any{"history_base": position}, "elsewhere")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Candidates: []agentapi.SourceRef{base, leaf, other}}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	s, err := historyPass(t, dir, l).Read(t.Context(), base, agentapi.ReadLimits{})
	if err == nil {
		_ = s.Close()
		t.Fatal("chose one of two tips")
	}
	if agentapi.Failure(err) != agentapi.Unavailable || sourceCause(err) != agentapi.CauseRolloutTipAmbiguous {
		t.Fatalf("ambiguous tips: kind=%s cause=%q: %v", agentapi.Failure(err), sourceCause(err), err)
	}
}

// A registered rollout Codex renamed is found through the thread's observed
// rollouts when Codex's index names none; an observation of a file that is
// gone since is passed over.
func TestMissingRegisteredRolloutFallsBackToObservedCandidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, _ := chainFixture(t, dir)
	registered := leaf
	moved := filepath.Join(dir, "archived")
	if err := os.MkdirAll(moved, 0700); err != nil {
		t.Fatal(err)
	}
	leaf.Path = filepath.Join(moved, filepath.Base(registered.Path))
	if err := os.Rename(registered.Path, leaf.Path); err != nil {
		t.Fatal(err)
	}
	registered.Key = threadA
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Candidates: []agentapi.SourceRef{registered, leaf}}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	if b := historyRead(t, historyPass(t, dir, l), registered); b.History.ActiveRolloutID != rolloutC {
		t.Fatalf("renamed rollout: %+v", b.History)
	}
}

// Without a current locator, an observed root and an observed tip whose
// middle segment was never observed are one chain, not two tips: the tip's
// walk back by rollout ID reaches the root.
func TestNoCurrentLocatorUnobservedMiddleSegmentIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, middle, position := chainFixture(t, dir)
	middleRaw, err := os.ReadFile(middle.Path)
	if err != nil {
		t.Fatal(err)
	}
	next := codexmeta.CodexHistoryPosition{RolloutID: rolloutC, EndOrdinal: position.EndOrdinal + 2, EndByteOffset: uint64(len(middleRaw))}
	tip, _ := historyFile(t, dir, rolloutD, threadA, next.EndOrdinal, map[string]any{"history_base": next}, "latest")
	l := &historyLookup{
		thread:   agentapi.CodexRolloutSet{Candidates: []agentapi.SourceRef{base, tip}},
		rollouts: map[string][]agentapi.SourceRef{threadA: {base}, rolloutC: {middle}},
	}
	if b := historyRead(t, historyPass(t, dir, l), base); b.History.ActiveRolloutID != rolloutD {
		t.Fatalf("tip behind an unobserved segment: %+v", b.History)
	}
}

// Observed off-chain rollouts past the candidate limit are left out of the
// historical revisions; they never refuse the chain's own capture.
func TestOffChainCandidatesPastTheLimitDoNotRefuseCapture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, _ := chainFixture(t, dir)
	var offChain []agentapi.SourceRef
	for n := range archive.MaxHistorySpans {
		ref := agentapi.SourceRef{Path: filepath.Join(dir, fmt.Sprintf("rollout-2026-10-01T12-00-00-%08x-0000-4000-8000-000000000000.jsonl", 0xa0000000+n)), Key: threadA}
		if err := os.WriteFile(ref.Path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		offChain = append(offChain, ref)
	}
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Candidates: offChain[:archive.MaxHistorySpans-1], Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	s, err := historyPass(t, dir, l).Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	set, err := s.(agentapi.SourceRevisionCandidates).RevisionCandidates(t.Context())
	if err != nil {
		t.Fatal("off-chain rollouts refused the chain's capture: ", err)
	}
	if len(set.Chain) != 2 || set.Chain[0].Path != base.Path || set.Chain[1].Path != leaf.Path || len(set.Chain)+len(set.BestEffort) != archive.MaxHistorySpans {
		t.Fatalf("revision candidates: chain %v, %d best effort", set.Chain, len(set.BestEffort))
	}
}

// The revision candidates name the chain's tip, the rollout the snapshot
// read as the thread's current revision, so a reconciliation that holds the
// snapshot's records need not read it again; the tip is still in the chain.
func TestRevisionCandidatesNameTheTipTheSnapshotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, leaf, _ := chainFixture(t, dir)
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	s, err := historyPass(t, dir, l).Read(t.Context(), base, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	set, err := s.(agentapi.SourceRevisionCandidates).RevisionCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if set.Active.Path != leaf.Path {
		t.Fatalf("active candidate = %q, want the tip %q", set.Active.Path, leaf.Path)
	}
	if !slices.ContainsFunc(set.Chain, func(ref agentapi.SourceRef) bool { return ref.Path == leaf.Path }) {
		t.Fatal("the tip left the chain")
	}
}
