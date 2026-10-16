package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestLegacyOrdinaryUnboundKeepsAnchoredSourceAndCurrentToken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ref := legacyTestFile(t, root, threadA, map[string]any{})
	ref.Key = threadA
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "one"}}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: lookup, LegacyUnboundRegistration: true, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if _, modern := snapshot.(agentapi.SourceAdmissionFacts); modern {
		t.Fatal("legacy evidence fabricated a modern binding")
	}
	validator, ok := snapshot.(agentapi.SourceAdmissionValidator)
	if !ok {
		t.Fatal("actual admission check missing")
	}
	if err := validator.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA, Cwd: root}); err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA, Cwd: t.TempDir()}); agentapi.Failure(err) != agentapi.Unsafe {
		t.Fatal("changed project accepted", err)
	}
	lookup.changed = true
	if err := snapshot.Input().File.Check(); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("current change ignored", err)
	}
}

// A legacy unbound registration keeps its legacy representation only for a
// plain local rollout. A bound, paginated, related or reverted rollout is
// never flattened into it: it is refused, or read through native selection.
func TestLegacyCompatibilityNeverFlattensBoundPaginatedRelatedOrRevert(t *testing.T) {
	t.Parallel()
	type scenarioVariant0 string
	const (
		scenarioBound0     scenarioVariant0 = "bound"
		scenarioPaginated0 scenarioVariant0 = "paginated"
		scenarioFork0      scenarioVariant0 = "fork"
		scenarioRevert0    scenarioVariant0 = "revert"
		scenarioBase0      scenarioVariant0 = "base"
	)
	for _, scenario := range []scenarioVariant0{scenarioBound0, scenarioPaginated0, scenarioFork0, scenarioRevert0, scenarioBase0} {
		t.Run(string(scenario), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			physical := threadA
			extra := map[string]any{}
			switch scenario {
			case scenarioBound0:
			case scenarioPaginated0:
				extra["history_mode"] = "paginated"
			case scenarioFork0:
				extra["forked_from_id"] = threadB
			case scenarioRevert0:
				physical = threadB
			case scenarioBase0:
				extra["history_base"] = map[string]any{"thread_id": threadB, "end_ordinal_exclusive": 1, "end_byte_offset": 1}
			}
			ref := legacyTestFile(t, root, physical, extra)
			ref.Key = threadA
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "one"}}
			pass, err := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{CodexRollouts: lookup, LegacyUnboundRegistration: scenario != scenarioBound0, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pass.Close() }()
			snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				return
			}
			_, flattened := snapshot.(legacyOrdinarySnapshot)
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}
			if flattened {
				t.Fatal("modern source flattened")
			}
		})
	}
}

func legacyTestFile(t *testing.T, root, physical string, extra map[string]any) agentapi.SourceRef {
	t.Helper()
	ref, raw := historyFile(t, root, physical, threadA, 0, extra, "legacy task")
	if _, present := extra["history_mode"]; !present {
		first, tail, _ := bytes.Cut(raw, []byte("\n"))
		var meta map[string]any
		if err := json.Unmarshal(first, &meta); err != nil {
			t.Fatal(err)
		}
		delete(meta["payload"].(map[string]any), "history_mode")
		first, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ref.Path, append(append(first, '\n'), tail...), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return ref
}

type registeredHistoryLookup struct{ *historyLookup }

func (l registeredHistoryLookup) RegisteredThread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	return l.Thread(ctx, id)
}

// A registered legacy read records the signature the next pass compares with
// Signature, so both must hash the same lookup revision.
func TestLegacyUnboundReadObservationMatchesSignature(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ref := legacyTestFile(t, root, threadA, map[string]any{})
	renamed := filepath.Join(root, "legacy-session.jsonl")
	if err := os.Rename(ref.Path, renamed); err != nil {
		t.Fatal(err)
	}
	ref.Path, ref.Key = renamed, "legacy-session"
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "revision"}}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: registeredHistoryLookup{lookup}, LegacyUnboundRegistration: true, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	observed, err := pass.Signature(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if snapshot.Observation().Signature != observed.Signature {
		t.Fatal("legacy read recorded a signature its next Signature cannot match")
	}
}
