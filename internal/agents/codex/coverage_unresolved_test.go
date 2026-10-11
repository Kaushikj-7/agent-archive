package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

const missingDependency = "history dependency missing or ambiguous"

const rolloutE = "55555555-5555-4555-8555-555555555555"

// refusal is the native message of err's first SourceError.
func refusal(err error) string {
	var e *agentapi.SourceError
	if !errors.As(err, &e) || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// copyDir is a directory below dir for a second locator of the same rollout.
func copyDir(t *testing.T, dir string) string {
	t.Helper()
	copied := filepath.Join(dir, "copy")
	if err := os.MkdirAll(copied, 0700); err != nil {
		t.Fatal(err)
	}
	return copied
}

// A history the lookup's rollouts do not resolve (a missing dependency, no
// single tip, no original segment) is a coverage refusal while coverage is
// unproven: incomplete, or a retained proof with no current locator. The
// same refusal on a proof of this pass, or on a retained proof beside a
// current locator, is a failure.
func TestUnresolvedHistoryNamesCoverageOnlyWhileUnproven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	position := codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": position}, "after")
	other, _ := historyFile(t, dir, rolloutD, threadA, 2, map[string]any{"history_base": position}, "elsewhere")
	// The same physical rollout at a second locator.
	copied, _ := historyFile(t, copyDir(t, dir), rolloutC, threadA, 2, map[string]any{"history_base": position, "cwd": dir}, "after")
	// Two segments based on each other: neither is a tip.
	cycleDir := t.TempDir()
	cycleD, _ := historyFile(t, cycleDir, rolloutD, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutE, EndOrdinal: 1, EndByteOffset: 1}})
	cycleE, _ := historyFile(t, cycleDir, rolloutE, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutD, EndOrdinal: 1, EndByteOffset: 1}})
	read := func(root string, lookup *historyLookup, ref agentapi.SourceRef) error {
		t.Helper()
		s, e := historyPass(t, root, lookup).Read(t.Context(), ref, agentapi.ReadLimits{})
		if e == nil {
			_ = s.Close()
		}
		return e
	}
	tips := []agentapi.SourceRef{leaf, other, base}
	bases := map[string][]agentapi.SourceRef{threadA: {base}}
	for _, tc := range []struct {
		name     string
		root     string
		lookup   *historyLookup
		ref      agentapi.SourceRef
		refusal  string
		coverage bool
	}{
		// Rollout(base) finds nothing before coverage has seen the base.
		{"dependency unseen while incomplete", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}, leaf, missingDependency, true},
		{"dependency missing on a complete proof", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Complete: true}}, leaf, missingDependency, false},
		{"dependency missing on a retained proof", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: []agentapi.SourceRef{leaf}}}, base, missingDependency, true},
		{"dependency missing on a retained proof with a current locator", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Complete: true, Retained: true}}, leaf, missingDependency, false},
		{"tips ambiguous on a retained proof", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: tips}, rollouts: bases}, base, "ambiguous rollout tips", true},
		{"tips ambiguous on this pass's proof", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: tips}, rollouts: bases}, base, "ambiguous rollout tips", false},
		{"physical rollout ambiguous on a retained proof", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: []agentapi.SourceRef{leaf, copied, base}}, rollouts: bases}, base, "ambiguous physical rollout", true},
		{"physical rollout ambiguous on this pass's proof", dir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: []agentapi.SourceRef{leaf, copied, base}}, rollouts: bases}, base, "ambiguous physical rollout", false},
		{"no tip on a retained proof", cycleDir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: []agentapi.SourceRef{cycleD, cycleE}}}, cycleD, "no complete rollout tip", true},
		{"no tip on this pass's proof", cycleDir, &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: []agentapi.SourceRef{cycleD, cycleE}}}, cycleD, "no complete rollout tip", false},
	} {
		e := read(tc.root, tc.lookup, tc.ref)
		if e == nil || agentapi.Failure(e) != agentapi.Unavailable || refusal(e) != tc.refusal {
			t.Errorf("%s: %v (refusal %q, want %q)", tc.name, e, refusal(e), tc.refusal)
			continue
		}
		if named := coverageCause(e) == agentapi.CauseHistoryCoverage; named != tc.coverage {
			t.Errorf("%s: %v names coverage %v, want %v", tc.name, e, named, tc.coverage)
		}
	}
}

// A candidate the selected tip's history never reaches leaves the lineage
// disconnected: a coverage refusal on a retained proof only.
func TestDisconnectedLineageNamesCoverageOnlyWhileUnproven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
	// D and E are based on each other, so neither is a tip, and the tip C
	// never reaches them.
	d, _ := historyFile(t, dir, rolloutD, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutE, EndOrdinal: 1, EndByteOffset: 1}})
	e, _ := historyFile(t, dir, rolloutE, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutD, EndOrdinal: 1, EndByteOffset: 1}})
	candidates := []agentapi.SourceRef{leaf, base, d, e}
	for _, retained := range []bool{true, false} {
		lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: retained, Candidates: candidates}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
		s, err := historyPass(t, dir, lookup).Read(t.Context(), base, agentapi.ReadLimits{})
		if err == nil {
			_ = s.Close()
		}
		named := coverageCause(err) == agentapi.CauseHistoryCoverage
		if refusal(err) != "disconnected rollout lineage" || named != retained {
			t.Errorf("retained=%v: %v (cause %q)", retained, err, coverageCause(err))
		}
	}
}

// Without a rollout lookup there is no coverage to wait for: the same
// refusal is never a coverage refusal.
func TestCoverageRefusalWithoutLookupIsAFailure(t *testing.T) {
	t.Parallel()
	p := &relatedSourcePass{}
	err := p.coverageRefusal(agentapi.CodexRolloutSet{}, missingDependency)
	if agentapi.Failure(err) != agentapi.Unavailable || coverageCause(err) != "" {
		t.Fatalf("%v (cause %q)", err, coverageCause(err))
	}
}

// A revision whose spans hold no segment of the thread's own, with no known
// binding, has no original creation facts until coverage finds that
// segment, and two such segments are ambiguous until coverage settles.
func TestMissingOriginalFactsNameCoverageOnlyWhileUnproven(t *testing.T) {
	t.Parallel()
	for _, ambiguous := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			dir := t.TempDir()
			base, raw := historyFile(t, dir, threadB, threadB, 0, nil, "before")
			leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadB, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
			rollouts := map[string][]agentapi.SourceRef{threadB: {base}}
			want := "original native creation facts unavailable"
			if ambiguous {
				first, _ := historyFile(t, dir, threadA, threadA, 0, nil, "own")
				second, _ := historyFile(t, copyDir(t, dir), threadA, threadA, 0, map[string]any{"cwd": dir}, "own")
				rollouts[threadA] = []agentapi.SourceRef{first, second}
				want = "original native identity ambiguous"
			}
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Complete: complete}, rollouts: rollouts}
			s, e := historyPass(t, dir, lookup).Read(t.Context(), leaf, agentapi.ReadLimits{})
			if e != nil {
				t.Fatalf("ambiguous=%v complete=%v: %v", ambiguous, complete, e)
			}
			evidence, ok := s.(agentapi.SourceAdmissionEvidence)
			if !ok {
				t.Fatal("history snapshot gathers no admission evidence")
			}
			_, e = evidence.AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: threadA})
			if closeErr := s.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			named := coverageCause(e) == agentapi.CauseHistoryCoverage
			if agentapi.Failure(e) != agentapi.Unavailable || refusal(e) != want || named == complete {
				t.Errorf("ambiguous=%v complete=%v: %v (cause %q)", ambiguous, complete, e, coverageCause(e))
			}
		}
	}
}

// A registered rollout that is no longer at its locator, with no current
// locator or candidate to follow, is missing only once coverage proves it:
// Codex may have renamed it into archived_sessions/ (or back). Until then a
// read defers instead of recording a transcript_missing gap.
func TestMissingRegisteredRolloutDefersWhileCoverageIsUnproven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gone := agentapi.SourceRef{Path: filepath.Join(dir, "rollout-2026-10-01T12-00-00-"+threadA+".jsonl"), Key: threadA}
	for _, tc := range []struct {
		name     string
		set      agentapi.CodexRolloutSet
		coverage bool
	}{
		{"coverage incomplete", agentapi.CodexRolloutSet{}, true},
		{"retained proof without candidates", agentapi.CodexRolloutSet{Complete: true, Retained: true}, true},
		{"this pass's proof without candidates", agentapi.CodexRolloutSet{Complete: true}, false},
	} {
		s, e := historyPass(t, dir, &historyLookup{thread: tc.set}).Read(t.Context(), gone, agentapi.ReadLimits{})
		if e == nil {
			_ = s.Close()
			t.Fatalf("%s: a missing rollout was read", tc.name)
		}
		named := coverageCause(e) == agentapi.CauseHistoryCoverage
		if named != tc.coverage || errors.Is(e, os.ErrNotExist) == tc.coverage {
			t.Errorf("%s: %v (cause %q)", tc.name, e, coverageCause(e))
		}
	}
}
