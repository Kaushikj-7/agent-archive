package codex

import (
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestRevisedHistoryAdmissionUsesOriginalProducerAndCwd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base, raw := historyFile(t, root, threadA, threadA, 0, map[string]any{"cwd": "/deleted/original", "cli_version": "0.150.0", "originator": "original-cli"}, "original")
	leaf, _ := historyFile(t, root, rolloutC, threadA, 2, map[string]any{"cwd": "/deleted/revision", "cli_version": "0.170.0", "originator": "revision-cli", "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "revision")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	pass := historyPass(t, root, lookup)
	snapshot, err := pass.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHeaderResource(t, snapshot)
	admission := agentapi.SourceAdmission{NativeID: threadA, Cwd: "/deleted/original", InitialProducerVersion: "0.150.0", InitialProducerOriginator: "original-cli"}
	if err := snapshot.(agentapi.SourceAdmissionValidator).ValidateAdmission(t.Context(), admission); err != nil {
		t.Fatalf("original facts rejected: %v", err)
	}
	if _, err := snapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), admission); err != nil {
		t.Fatalf("original evidence rejected: %v", err)
	}
	if err := pass.(agentapi.SourceAdmissionSignature).ValidateSourceAdmission(t.Context(), leaf, admission); err != nil {
		t.Fatalf("signature rejected: %v", err)
	}
	for _, bad := range []agentapi.SourceAdmission{{NativeID: threadA, Cwd: "/deleted/revision"}, {NativeID: threadA, InitialProducerVersion: "0.170.0"}, {NativeID: threadA, InitialProducerOriginator: "revision-cli"}} {
		if snapshot.(agentapi.SourceAdmissionValidator).ValidateAdmission(t.Context(), bad) == nil {
			t.Fatal("revision facts impersonated original")
		}
	}
}

func TestHistorySourcePolicyRefusesExcludedAncestorBeforeFiltering(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base, raw := historyFile(t, root, threadA, threadA, 0, map[string]any{"cwd": "/excluded/service"}, "synthetic excluded ancestor text")
	leaf, _ := historyFile(t, root, rolloutC, threadB, 2, map[string]any{"cwd": "/allowed/service", "forked_from_id": threadA, "forked_from_ordinal_exclusive": 2, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "allowed own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: lookup, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}, HistorySourceAllowed: func(cwd string) bool { return cwd != "/excluded/service" }})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHeaderResource(t, pass)
	snapshot, err := pass.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if !errors.Is(err, agentapi.ErrHistorySourceExcluded) || snapshot != nil {
		t.Fatalf("excluded prefix exposed: %v", err)
	}
}
