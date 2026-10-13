package codex

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
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
	for name, rollouts := range map[string]map[string][]agentapi.SourceRef{
		"missing":   nil,
		"ambiguous": {threadA: {base, leaf}},
	} {
		t.Run(name, func(t *testing.T) {
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
	refs, err := candidates.RevisionCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, ref := range refs {
		paths = append(paths, ref.Path)
	}
	chain := []string{leaf.Path, base.Path}
	slices.Sort(paths[:min(len(paths), 2)])
	slices.Sort(chain)
	if want := append(chain, other.Path); !slices.Equal(paths, want) {
		t.Fatalf("revision candidates = %v, want the chain then the off-chain branch %v", paths, want)
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
