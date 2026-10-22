package state

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// compositionGenerationFixture is generationFixture on a home that requires
// selecting authority for every new pending publication.
func compositionGenerationFixture(t *testing.T) (*Store, archive.SessionRegistration, time.Time) {
	t.Helper()
	s, reg, at := generationFixture(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error {
		r.DestinationID = cfg.DestinationID()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, _, err = s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WithPublicationComposition(s.Home(), func(config.PublicationCompositionGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return s, reg, at
}

// selectableGenerationBuilder renders what the collector builder renders on a
// protected home: the fixtures' default skill policy and adapter facts in the
// metadata.
func selectableGenerationBuilder(at time.Time, clearStage bool) func(archive.SessionRegistration, string) (archive.SessionRegistration, PendingPublication, error) {
	build := generationBuilder(at)
	return func(reg archive.SessionRegistration, id string) (archive.SessionRegistration, PendingPublication, error) {
		if clearStage {
			reg.StagedImportSHA256 = ""
		}
		reg, pending, err := build(reg, id)
		if err != nil {
			return reg, pending, err
		}
		var metadata archive.Metadata
		if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
			return reg, pending, err
		}
		metadata.Adapter = archive.AdapterInfo{Name: pending.Bundle.Capture.AdapterName, Version: pending.Bundle.Capture.AdapterVersion}
		pending.MetadataBytes, err = json.Marshal(metadata)
		pending.SkillEvidence = string(config.SkillEvidenceBody)
		return reg, pending, err
	}
}

// assertSelectedSuccessor checks the completed transition: the successor is
// routed, owns an ordinary ready selection and the predecessor is frozen.
func assertSelectedSuccessor(t *testing.T, s *Store, original archive.SessionRegistration) PendingPublication {
	t.Helper()
	next, found, err := s.GenerationSuccessor(original.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("successor receipt: %v %v", found, err)
	}
	key, err := registrationKey(original)
	if err != nil {
		t.Fatal(err)
	}
	if active, found, err := s.ArchiveSessionID(key); err != nil || !found || active != next {
		t.Fatalf("successor not routed: %s %v %v", active, found, err)
	}
	old, found, err := s.LoadRegistration(original.ArchiveSessionID)
	if err != nil || !found || !old.CaptureFrozen || old.StagedImportSHA256 != original.StagedImportSHA256 {
		t.Fatalf("predecessor: %#v %v %v", old, found, err)
	}
	pending, found, err := s.LoadPending(next)
	if err != nil || !found || pending.Selection == nil || pending.Selection.Predecessor != ReadyPredecessorAbsent || pending.StagedProof() != nil {
		t.Fatalf("successor pending: %#v %v %v", pending.Selection, found, err)
	}
	if err := s.ValidateReadySelection(t.Context(), pending); err != nil {
		t.Fatalf("successor selection: %v", err)
	}
	return pending
}

func TestGenerationRecoverySelectsSuccessorOnCompositionHome(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); err != nil {
		t.Fatal(err)
	}
	pending := assertSelectedSuccessor(t, s, reg)
	published, err := s.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(pending, at); err != nil {
		t.Fatalf("successor publication: %v", err)
	}
	if err := s.RemovePending(pending.Bundle.ArchiveSessionID); err != nil {
		t.Fatalf("acknowledged successor cleanup: %v", err)
	}
}

// Replay derives the selection again; every interrupted step saves the same
// pending bytes the uninterrupted transition does.
func TestGenerationRecoverySuccessorSelectionReplaysIdentically(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"generation-journal", "generation-frozen", "generation-registration", "generation-pending", "generation-request", "generation-index"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			s, reg, at := compositionGenerationFixture(t)
			interrupted := errors.New("interrupted")
			var saved []byte
			fired := false
			s.onIndexStep = func(current string) error {
				if current == "generation-pending" && saved == nil {
					next, _, _ := s.GenerationSuccessor(reg.ArchiveSessionID)
					saved, _ = os.ReadFile(s.pendingPath(next))
				}
				if current == step && !fired {
					fired = true
					return interrupted
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); !errors.Is(err, interrupted) {
				t.Fatalf("did not interrupt %s: %v", step, err)
			}
			if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
				t.Fatalf("replay after %s: %v", step, err)
			}
			pending := assertSelectedSuccessor(t, s, reg)
			replayed, err := os.ReadFile(s.pendingPath(pending.Bundle.ArchiveSessionID))
			if err != nil || len(saved) == 0 || string(replayed) != string(saved) {
				t.Fatalf("replayed pending differs from the first save: %v", err)
			}
		})
	}
}

// A successor that cannot take selecting authority is refused while beginning,
// before a journal exists, a routing fence is written or the predecessor freezes.
func TestGenerationRecoveryRefusesUnselectableSuccessorBeforeJournal(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	before := stagedGenerationInventory(t, s.Home())
	// generationBuilder carries no current skill policy, so no ready selection binds it.
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, ErrGenerationSuccessorUnselectable) {
		t.Fatalf("unselectable successor: %v", err)
	}
	if after := stagedGenerationInventory(t, s.Home()); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused recovery changed durable state:\n%v\n%v", before, after)
	}
}

func setGenerationSkillEvidence(t *testing.T, s *Store, mode config.SkillEvidence) {
	t.Helper()
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SkillEvidence = mode
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
}

// A configuration change after the journal keeps recovery owed until its
// successor's publication is saved; from then on the saved pending completes
// the transition and ordinary publication owns the changed policy.
func TestGenerationRecoveryReplayAfterConfigurationChange(t *testing.T) {
	t.Parallel()
	for step, saved := range map[string]bool{"generation-journal": false, "generation-frozen": false, "generation-registration": false, "generation-pending": true, "generation-request": true, "generation-index": true} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			s, reg, at := compositionGenerationFixture(t)
			interrupted := errors.New("interrupted")
			s.onIndexStep = func(current string) error {
				if current == step {
					return interrupted
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); !errors.Is(err, interrupted) {
				t.Fatalf("did not interrupt %s: %v", step, err)
			}
			s.onIndexStep = nil
			next, _, err := s.GenerationSuccessor(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(s.pendingPath(next))
			setGenerationSkillEvidence(t, s, config.SkillEvidenceMetadata)
			err = s.ResumeGenerationRecoveries(t.Context())
			if saved {
				if err != nil {
					t.Fatalf("saved successor did not complete: %v", err)
				}
				if active, found, err := s.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}); err != nil || !found || active != next {
					t.Fatalf("saved successor not routed: %s %v %v", active, found, err)
				}
				// Ordinary publication holds the selected pending until its policy returns.
				if _, _, err := s.LoadPending(next); !errors.Is(err, ErrReadySelectionFrozen) {
					t.Fatalf("selected pending accepted under changed policy: %v", err)
				}
				setGenerationSkillEvidence(t, s, config.SkillEvidenceBody)
				pending := assertSelectedSuccessor(t, s, reg)
				after, err := os.ReadFile(s.pendingPath(pending.Bundle.ArchiveSessionID))
				if err != nil || len(before) == 0 || string(after) != string(before) {
					t.Fatalf("replay rewrote the saved successor pending: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrGenerationRecoveryConfigChanged) || !errors.Is(err, ErrReadySelectionFrozen) {
				t.Fatalf("replay with changed skill policy: %v", err)
			}
			if _, found, err := s.LoadPending(next); err != nil || found {
				t.Fatalf("refused replay saved a successor pending: %v %v", found, err)
			}
			if step == "generation-journal" {
				old, found, err := s.LoadRegistration(reg.ArchiveSessionID)
				if err != nil || !found || old.CaptureFrozen {
					t.Fatalf("refused replay froze the predecessor: %v %v", found, err)
				}
				key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
				if h, present, err := s.loadGenerationHead(key); err != nil || present {
					t.Fatalf("refused replay fenced routing: %#v %v", h, err)
				}
			}
			setGenerationSkillEvidence(t, s, config.SkillEvidenceBody)
			if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertSelectedSuccessor(t, s, reg)
		})
	}
}

func TestGenerationSuccessorWithHistoryHasNoOrdinarySelection(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	successor, pending, err := selectableGenerationBuilder(at, false)(reg, "synthetic-successor")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PendingPublication){
		"pending history": func(p *PendingPublication) { p.History = &PendingHistory{Version: 1} },
		"bundle history":  func(p *PendingPublication) { p.Bundle.History = &archive.SourceHistory{} },
		"history journal": func(p *PendingPublication) { p.JournalVersion = 5 },
	} {
		candidate := pending
		mutate(&candidate)
		if _, err := s.generationSuccessorPending(t.Context(), successor, candidate); !errors.Is(err, ErrGenerationHistorySuccessor) {
			t.Fatalf("%s successor selected: %v", name, err)
		}
	}
}

// The whole staged path: a typed staged selection released through readback,
// blocked as rewritten, then recovered into an ordinary selected successor.
func TestGenerationRecoveryCompletesOnStagedHome(t *testing.T) {
	t.Parallel()
	s, reg, at := settledStageGenerationFixture(t)
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, true)); err != nil {
		t.Fatal(err)
	}
	pending := assertSelectedSuccessor(t, s, reg)
	successor, found, err := s.LoadRegistration(pending.Bundle.ArchiveSessionID)
	if err != nil || !found || successor.StagedImportSHA256 != "" || successor.PreviousGenerationID != reg.ArchiveSessionID {
		t.Fatalf("successor registration: %#v %v %v", successor, found, err)
	}
	published, err := s.LoadPublishedState(successor.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(pending, at); err != nil {
		t.Fatalf("successor publication: %v", err)
	}
}

// require_skill_use is not bound by the selection, so enabling it after the
// journal keeps a successor without skill use from being saved.
func TestGenerationRecoveryRechecksSkillUseBeforeSavingSuccessor(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	setRequireSkillUse := func(on bool) {
		t.Helper()
		cfg, _, err := config.Load(s.Home())
		if err != nil {
			t.Fatal(err)
		}
		cfg.RequireSkillUse = on
		if err := config.Save(s.Home(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	setRequireSkillUse(true)
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); !errors.Is(err, ErrGenerationSuccessorUnselectable) {
		t.Fatalf("successor without skill use began: %v", err)
	}
	setRequireSkillUse(false)
	interrupted := errors.New("interrupted")
	s.onIndexStep = func(current string) error {
		if current == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); !errors.Is(err, interrupted) {
		t.Fatalf("did not interrupt at the journal: %v", err)
	}
	s.onIndexStep = nil
	setRequireSkillUse(true)
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrGenerationRecoveryConfigChanged) {
		t.Fatalf("replay saved a successor without skill use: %v", err)
	}
	next, _, err := s.GenerationSuccessor(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending(next); err != nil || found {
		t.Fatalf("refused replay saved a successor pending: %v %v", found, err)
	}
	setRequireSkillUse(false)
	if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSuccessor(t, s, reg)
}

// A saved successor proof is reused only when every configuration-independent
// field still binds the journaled successor; a self-consistent rewrite is not.
func TestGenerationRecoveryRefusesRewrittenSavedSuccessorProof(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	interrupted := errors.New("interrupted")
	s.onIndexStep = func(current string) error {
		if current == "generation-pending" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); !errors.Is(err, interrupted) {
		t.Fatalf("did not interrupt after saving the pending: %v", err)
	}
	s.onIndexStep = nil
	next, _, err := s.GenerationSuccessor(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := s.LoadPending(next)
	if err != nil || !found || pending.Selection == nil {
		t.Fatalf("saved pending: %v %v", found, err)
	}
	pending.Selection.Predecessor = ReadyPredecessorUnknown
	proof := *pending.Selection
	proof.SHA256 = ""
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	pending.Selection.SHA256 = readySHA(raw)
	if err := pending.Selection.validateProofDigest(); err != nil {
		t.Fatalf("rewrite is not self-consistent: %v", err)
	}
	if err := local.Write(s.pendingPath(next), pending); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("rewritten proof replayed: %v", err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
	if active, _, err := s.ArchiveSessionID(key); err == nil && active == next {
		t.Fatal("rewritten proof activated the successor")
	}
}

// A legacy owner records no destination, so the saved proof's destination is
// bound to the configured one before it is reused.
func TestGenerationRecoveryBindsLegacySavedProofToConfiguredDestination(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	reg.DestinationID = ""
	if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted")
	s.onIndexStep = func(current string) error {
		if current == "generation-pending" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(at, false)); !errors.Is(err, interrupted) {
		t.Fatalf("did not interrupt after saving the pending: %v", err)
	}
	s.onIndexStep = nil
	next, _, err := s.GenerationSuccessor(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := s.LoadPending(next)
	if err != nil || !found || pending.Selection == nil {
		t.Fatalf("saved pending: %v %v", found, err)
	}
	original := pending
	rewritten := *pending.Selection
	pending.Selection = &rewritten
	pending.Selection.DestinationID = "another-destination"
	proof := *pending.Selection
	proof.SHA256 = ""
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	pending.Selection.SHA256 = readySHA(raw)
	if err := local.Write(s.pendingPath(next), pending); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrGenerationRecoveryConfigChanged) {
		t.Fatalf("legacy proof for another destination replayed: %v", err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
	if active, _, err := s.ArchiveSessionID(key); err == nil && active == next {
		t.Fatal("legacy proof for another destination activated the successor")
	}
	if err := local.Write(s.pendingPath(next), original); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSuccessor(t, s, reg)
}
