package backfill

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestConfirmedGuardedPolicyRefusalCleansUnlinkedChildren(t *testing.T) {
	for _, changeAt := range []int{0, 3, 4} {
		t.Run(map[int]string{0: "unchanged", 3: "second child", 4: "parent"}[changeAt], func(t *testing.T) {
			r, c, cfg, previous := guardedHistoricalFixture(t)
			before, err := os.ReadFile(filepath.Join(r.Home, "sessions", previous.ArchiveSessionID, "import-stage", "stage.json"))
			if err != nil {
				t.Fatal(err)
			}
			policyCalls, currentChecks := 0, 0
			r.HistoricalSourcePolicy = func(current config.Config) func(string) bool {
				policyCalls++
				if policyCalls == changeAt {
					if !current.AcceptSession(r.registration(c, "check", "")) {
						t.Fatal("renewal did not observe accepted current policy")
					}
					currentChecks++
					// This callback runs after renewStageProof's current-policy
					// check, before the actual state save takes its durable guard.
					latest, _, err := config.Load(r.Home)
					if err != nil {
						t.Fatal(err)
					}
					latest.HistoricalProjects[0].Permissions[0].Enabled = false
					if err := config.Save(r.Home, latest); err != nil {
						t.Fatal(err)
					}
				}
				return func(string) bool { return true }
			}
			result, err := r.Run([]Candidate{c})
			if changeAt == 0 {
				if err != nil || len(result.Sessions) != 1 || len(result.Subagents) != 2 || !cfg.AcceptSession(r.registration(c, "check", "")) {
					t.Fatal("unchanged complete parent/child preparation failed", result, err)
				}
			} else if currentChecks != 1 || !errors.Is(err, state.ErrDurableStorageRecovery) || !strings.Contains(err.Error(), "import policy changed before durable staging") || len(result.Sessions)+len(result.Subagents) != 0 {
				t.Fatal("actual renewed-policy then guarded-refusal boundary was not reached", currentChecks, result, err)
			}
			checkGuardedPolicyStages(t, r, c, previous, before, changeAt != 0)
		})
	}
}

func guardedHistoricalFixture(t *testing.T) (Registration, Candidate, config.Config, archive.SessionRegistration) {
	t.Helper()
	r, c, cfg := stagingFixture(t)
	c.Harness = archive.HarnessClaude
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatal("earlier completed unrelated group failed", result, err)
	}
	previous, found, err := r.Store.LoadRegistration(result.Sessions[0])
	if err != nil || !found || previous.StagedImportSHA256 == "" {
		t.Fatal("earlier admitted stage missing", found, err)
	}
	project := c.ProjectRoot
	c.NativeSessionID, c.TranscriptPath = "synthetic-guarded-parent", filepath.Join(project, "guarded-parent.jsonl")
	if err := os.WriteFile(c.TranscriptPath, []byte(claudeTranscript(c.NativeSessionID, project, c.StartedAt)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, child := range []string{"one", "two"} {
		path := filepath.Join(project, "agent-"+child+".jsonl")
		if err := os.WriteFile(path, []byte(subagentTranscript(c.NativeSessionID, child, fixedNow)), 0600); err != nil {
			t.Fatal(err)
		}
		c.Subagents = append(c.Subagents, Subagent{Path: path, AgentID: child})
	}
	r.Batch = "2026-09-23-2"
	if err := SaveBatch(r.Home, Batch{ID: r.Batch, StartedAt: fixedNow, DestinationID: r.DestinationID}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = ""
	c.HistoricalProject = &archive.HistoricalProjectProof{Version: 1, ProjectID: strings.Repeat("a", 32), DestinationID: r.DestinationID, OriginalCwd: project, CanonicalCwd: project, Label: "Synthetic retired project"}
	cfg.Harnesses, cfg.HistoricalProjectProtection = []string{c.Harness}, true
	cfg.HistoricalProjects = []config.HistoricalProject{{ID: c.HistoricalProject.ProjectID, Label: c.HistoricalProject.Label, Permissions: []config.HistoricalProjectPermission{{DestinationID: r.DestinationID, Enabled: true}}}}
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	return r, c, cfg, previous
}

func checkGuardedPolicyStages(t *testing.T, r Registration, c Candidate, previous archive.SessionRegistration, before []byte, refused bool) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(r.Home, "sessions", previous.ArchiveSessionID, "import-stage", "stage.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("guarded refusal changed earlier admitted unrelated evidence", err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range regs {
		if refused && reg.InBatch(r.Batch) {
			t.Fatal("guarded policy refusal admitted fresh parent or child")
		}
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	unlinked := 0
	for _, stage := range stages {
		if stage.Registration.InBatch(r.Batch) {
			unlinked++
			if stage.Registration.ParentSessionID == "" && refused || stage.Registration.HistoricalProject == nil || stage.Registration.HistoricalProject.ProjectID != c.ProjectIdentity() {
				t.Fatal("unexpected guarded-stage owner")
			}
			full, found, err := r.Store.LoadImportStage(t.Context(), stage.Registration)
			if err != nil || !found || full.SHA256 != stage.SHA256 || full.Bundle.NativeSessionID != stage.Registration.NativeSessionID {
				t.Fatal("guarded selection lost exact validated retained evidence", found, err)
			}
		}
	}
	if refused {
		t.Logf("actual guarded policy refusal after successful renewal; no fresh admission; earlier admitted unrelated stage byte-exact; unlinked retained stages=%d", unlinked)
		if unlinked != 0 {
			t.Error("guarded policy refusal retained unlinked child staging", unlinked)
		}
	} else if unlinked != 3 {
		t.Fatal("unchanged group lost selected parent/children", unlinked)
	}
}

func TestConfirmedGuardedPolicyRefusalPreservesTypedChildStage(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	c.Harness = archive.HarnessClaude
	cfg.DurableStorageProtection, cfg.PublicationCompositionProtection = true, true
	cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	parentID, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: c.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	parent := r.registration(c, parentID, "")
	work := &parentWork{c: c, id: parentID, stagedChildren: map[string]state.SubagentCandidate{}}
	var children []state.SubagentCandidate
	for _, name := range []string{"one", "two"} {
		native := c.NativeSessionID + ":subagent:" + name
		id, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: native})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(c.ProjectRoot, "agent-"+name+".jsonl")
		if err := os.WriteFile(path, []byte(subagentTranscript(c.NativeSessionID, name, fixedNow)), 0600); err != nil {
			t.Fatal(err)
		}
		children = append(children, state.SubagentCandidate{ArchiveSessionID: id, NativeSessionID: native, ParentArchiveSessionID: parentID, ParentNativeSessionID: c.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: name, TranscriptPath: path, ObservedAt: fixedNow, Origin: archive.SessionOriginImport})
	}
	renew := func() (bool, error) { return r.renewStageProof(t.Context(), work, nil, cfg.EffectiveSkillEvidence()) }
	digest, current, err := r.stageSelectedChild(t.Context(), parent, children[0], cfg.EffectiveSkillEvidence(), renew)
	if err != nil || !current || digest == "" {
		t.Fatal("typed first child preparation failed", current, err)
	}
	children[0].StagedImportSHA256 = digest
	work.stagedChildren[children[0].AgentID] = children[0]
	path := filepath.Join(r.Home, "sessions", children[0].ArchiveSessionID, "import-stage", "stage.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = r.stageSelectedChild(t.Context(), parent, children[1], cfg.EffectiveSkillEvidence(), func() (bool, error) {
		current, err := renew()
		if err != nil || !current {
			t.Fatal("typed child policy renewal failed before guarded mutation", current, err)
		}
		latest, _, err := config.Load(r.Home)
		if err != nil {
			t.Fatal(err)
		}
		latest.Paused = true
		if err := config.Save(r.Home, latest); err != nil {
			t.Fatal(err)
		}
		return current, nil
	})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || !strings.Contains(err.Error(), "import policy changed before durable staging") {
		t.Fatal("typed actual guarded policy refusal missing", err)
	}
	if err := r.cancelUnlinkedChildren(t.Context(), work); !errors.Is(err, state.ErrReadySelectionFrozen) {
		t.Fatal("typed cancellation writer protection changed", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("typed policy refusal changed exact prepared evidence", err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatal("typed refused child gained admission", len(regs), err)
	}
	stages, _, closeStages, err := r.Store.ImportStagesOwned(t.Context(), "", 50)
	defer closeStages()
	if err != nil || len(stages) != 1 || !stages[0].TypedSelection || stages[0].SHA256 != digest {
		t.Fatal("typed guarded refusal did not preserve exactly the original child stage", len(stages), err)
	}
	t.Log("actual typed guard refusal after valid renewal; exact first child bytes/digest retained; second child absent; no admission; cancellation remains frozen")
}
