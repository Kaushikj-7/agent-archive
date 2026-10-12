package codex

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"testing"
	"time"
)

// These are actual history admission entrypoint controls. The selected physical
// producer may differ, while the initial producer must still match the original.
func TestHistoryInitialProducerAndKnownBindingControls(t *testing.T) {
	root := t.TempDir()
	original, _ := historyFile(t, root, threadA, threadA, 0, nil, "original")
	leaf, _ := historyFile(t, root, rolloutC, threadA, 0, map[string]any{"cwd": root + "/revised", "cli_version": "0.161.0", "originator": "codex_revision_test"}, "revision")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {original}}}
	pass := historyPass(t, root, lookup)
	snapshot, err := pass.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	admission := agentapi.SourceAdmission{NativeID: threadA, Cwd: root, NativeCreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), InitialProducerVersion: "0.160.0", InitialProducerOriginator: "codex_cli_rs", InitialProducerSource: "cli"}
	reader := snapshot.(agentapi.SourceAdmissionEvidence)
	evidence, err := reader.AdmissionEvidence(t.Context(), admission)
	if err != nil {
		t.Fatalf("original producer rejected after revision: %v", err)
	}
	if evidence.Binding.PhysicalProducerVersion != "0.161.0" || evidence.Binding.PhysicalProducerOriginator != "codex_revision_test" {
		t.Fatal("selected producer evidence lost")
	}
	wrong := admission
	wrong.InitialProducerVersion = "0.159.0"
	if _, err := reader.AdmissionEvidence(t.Context(), wrong); !agentapi.HasFailure(err, agentapi.Unsafe) {
		t.Fatal("wrong original version accepted", err)
	}
	wrong = admission
	wrong.InitialProducerOriginator = "wrong-original"
	if _, err := reader.AdmissionEvidence(t.Context(), wrong); !agentapi.HasFailure(err, agentapi.Unsafe) {
		t.Fatal("wrong original originator accepted", err)
	}
	wrong = admission
	wrong.InitialProducerSource = "wrong-source"
	if _, err := reader.AdmissionEvidence(t.Context(), wrong); !agentapi.HasFailure(err, agentapi.Unsafe) {
		t.Fatal("wrong original source accepted", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.AdmissionEvidence(canceled, admission); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation became immutable admission mismatch", err)
	}
	// A fresh pass has no original locator/proof. Immutable known binding may
	// supply it, but an initial unbound admission must not invent that proof.
	missing := historyPass(t, root, &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}})
	missingSnapshot, err := missing.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := missingSnapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	missingReader := missingSnapshot.(agentapi.SourceAdmissionEvidence)
	if _, err := missingReader.AdmissionEvidence(t.Context(), admission); !agentapi.HasFailure(err, agentapi.Unavailable) {
		t.Fatal("unknown original admitted", err)
	}
	known := agentapi.SourceAdmission{NativeID: threadA, Binding: &evidence.Binding}
	if _, err := missingReader.AdmissionEvidence(t.Context(), known); err != nil {
		t.Fatal("known immutable binding refused without original", err)
	}
	modifiedBinding := evidence.Binding
	modifiedBinding.PhysicalProducerVersion = "wrong-current-version"
	known.Binding = &modifiedBinding
	if _, err := missingReader.AdmissionEvidence(t.Context(), known); !agentapi.HasFailure(err, agentapi.Unsafe) {
		t.Fatal("same physical producer mutation accepted", err)
	}
}
