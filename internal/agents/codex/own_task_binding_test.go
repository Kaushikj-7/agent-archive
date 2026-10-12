package codex

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestRevisionOwnTaskUsesRetainedBoundaryAfterAdmissionValidation(t *testing.T) {
	zero, owned := uint64(0), uint64(2)
	for _, scenario := range []struct {
		name        string
		retained    *uint64
		native      *uint64
		wantOwned   bool
		wantRefusal bool
	}{
		{name: "retained", retained: &owned, wantOwned: true},
		{name: "absent"},
		{name: "zero", retained: &zero},
		{name: "changed", retained: &owned, native: &zero, wantRefusal: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			metadata := map[string]any{"parent_thread_id": threadA, "session_id": threadA}
			if scenario.native != nil {
				metadata["subagent_history_start_ordinal"] = *scenario.native
			}
			ref, _ := historyFile(t, root, threadB, threadB, 0, metadata)
			task := func(id string) map[string]any {
				return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": threadA, "started_at": "2026-10-01T12:02:00Z"}}
			}
			appendHistoryRows(t, ref, 1, task("copied-imported-turn"), task(threadB))
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
			pass := historyPass(t, root, lookup)
			initial, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := initial.(agentapi.SourceAdmissionFacts).AdmissionFacts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			// Previously admitted ownership survives an omitted physical boundary.
			// Absence/zero cannot skip an invalid first task; contradictory current
			// boundary evidence must refuse instead of overriding retained facts.
			binding.OwnStart = scenario.retained
			if scenario.wantOwned {
				binding.FirstNativeTaskAt = binding.NativeCreatedAt.Add(2 * time.Minute)
				binding.FirstNativeTaskID = threadB
			}
			admission := agentapi.SourceAdmission{NativeID: threadB, Binding: &binding}
			revision, err := pass.(agentapi.SourceRevisions).ReadRevision(t.Context(), ref, admission, agentapi.ReadLimits{})
			if scenario.wantRefusal {
				if !agentapi.HasFailure(err, agentapi.Unsafe) || revision != nil {
					t.Fatal("changed native boundary accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = revision.Close() }()
			evidence, err := revision.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), admission)
			if err != nil {
				t.Fatal(err)
			}
			wantTask := "copied-imported-turn"
			if scenario.wantOwned {
				wantTask = threadB
			}
			if !evidence.Task.Seen || !evidence.Task.LocalExecution || evidence.Task.Native != scenario.wantOwned || evidence.Task.TurnID != wantTask {
				t.Fatalf("retained own boundary returned wrong first task: %+v", evidence.Task)
			}
			if (evidence.Binding.OwnStart == nil) != (scenario.retained == nil) || scenario.retained != nil && *evidence.Binding.OwnStart != *scenario.retained {
				t.Fatal("absent/zero retained boundary changed", evidence.Binding.OwnStart)
			}
		})
	}
}
