package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"testing"
	"time"
)

func TestInitialAdmissionUsesOriginalCwdAfterPhysicalRevision(t *testing.T) {
	root := t.TempDir()
	original, raw := historyFile(t, root, threadA, threadA, 0, nil, "original task")
	leaf, _ := historyFile(t, root, rolloutC, threadA, 2, map[string]any{"cwd": root + "/different", "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "revision task")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {original}}}
	pass := historyPass(t, root, lookup).(*relatedSourcePass)
	snapshot, err := pass.Read(t.Context(), original, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	admission := agentapi.SourceAdmission{NativeID: threadA, Cwd: root, NativeCreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), InitialProducerVersion: "0.160.0", InitialProducerOriginator: "codex_cli_rs", InitialProducerSource: "cli"}
	evidence, err := snapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), admission)
	if err != nil {
		t.Fatalf("original admission refused for revised cwd: %v", err)
	}
	if evidence.Binding.Cwd != root || evidence.Binding.SelectedCwd != root+"/different" || evidence.Binding.PhysicalRolloutID != rolloutC {
		t.Fatal("original and selected physical facts conflated")
	}
	admission.Cwd = root + "/wrong"
	if _, err := snapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), admission); !agentapi.HasFailure(err, agentapi.Unsafe) {
		t.Fatal("changed original cwd accepted", err)
	}
}
