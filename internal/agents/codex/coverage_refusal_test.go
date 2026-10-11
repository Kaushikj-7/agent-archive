package codex

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// coverageCause reports the cause on err's outermost SourceError, walking
// joins as the collector's deferral classification does.
func coverageCause(err error) agentapi.SourceCause {
	if classified, ok := err.(agentapi.Classified); ok {
		_, cause := classified.Classification()
		return cause
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			if part != nil {
				return coverageCause(part)
			}
		}
	}
	return ""
}

// Every refusal the source gives only because the lookup has not proven the
// thread's rollout coverage names that cause itself, so the collector defers
// it without asking the lookup again; a refusal of the files it did select
// names none.
func TestCoverageRefusalsNameTheirCauseAtTheSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
	read := func(lookup *historyLookup) error {
		t.Helper()
		s, e := historyPass(t, dir, lookup).Read(t.Context(), base, agentapi.ReadLimits{})
		if e == nil {
			_ = s.Close()
		}
		return e
	}
	for name, lookup := range map[string]*historyLookup{
		"lineage incomplete":            {thread: agentapi.CodexRolloutSet{Candidates: []agentapi.SourceRef{leaf, base}}},
		"lineage from a retained proof": {thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: []agentapi.SourceRef{leaf, base}}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}},
	} {
		e := read(lookup)
		if agentapi.Failure(e) != agentapi.Unavailable || coverageCause(e) != agentapi.CauseHistoryCoverage {
			t.Errorf("%s: %v (cause %q)", name, e, coverageCause(e))
		}
	}
	// Complete coverage that names two tips is a refusal of the selected
	// files, not of coverage.
	other, _ := historyFile(t, dir, "44444444-4444-4444-8444-444444444444", threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "elsewhere")
	if e := read(&historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: []agentapi.SourceRef{leaf, base, other}}}); e == nil || coverageCause(e) != "" {
		t.Errorf("ambiguous tips named coverage: %v", e)
	}

	type revisions interface {
		RevisionCandidates(context.Context) ([]agentapi.SourceRef, error)
	}
	for name, ref := range map[string]agentapi.SourceRef{"history": leaf, "ordinary": base} {
		lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
		s, e := historyPass(t, dir, lookup).Read(t.Context(), ref, agentapi.ReadLimits{})
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		candidates, ok := s.(revisions)
		if !ok {
			t.Fatalf("%s snapshot lists no revision candidates", name)
		}
		_, e = candidates.RevisionCandidates(t.Context())
		if closeErr := s.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		var classified *agentapi.SourceError
		if !errors.As(e, &classified) || classified.Kind != agentapi.Unavailable || coverageCause(e) != agentapi.CauseHistoryCoverage {
			t.Errorf("%s revision candidates without coverage: %v", name, e)
		}
	}
}
