package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const ordinaryPrivateSkill = "private-ordinary-skill-body"

func ordinaryBundleHasSkillBody(t *testing.T, p state.PendingPublication) bool {
	t.Helper()
	encoded, err := json.Marshal(p.Bundle.SupplementalEvidence)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(encoded, []byte(ordinaryPrivateSkill))
}

// runOrdinaryPolicy prepares one retained policy successor without native or
// live skill input and returns the installed uncommitted journal.
func runOrdinaryPolicy(t *testing.T, local *state.Store, remote *metadataFailStore, reg archive.SessionRegistration, opts Options, mode config.SkillEvidence) state.PendingPublication {
	t.Helper()
	opts.SkillEvidence = mode
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("privacy successor read live skill state")
		return nil, nil
	}
	if _, err := Run(t.Context(), local, remote, opts); err != nil {
		t.Fatal(err)
	}
	p, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || p.SkillEvidence != string(mode) || p.Commit == nil || p.Commit.Purpose != state.PublicationPrivacyRewrite {
		t.Fatal("policy successor was not installed", mode, found, err)
	}
	return p
}

func TestOrdinaryPrivacyIncreaseRestoresOldestUncommittedContent(t *testing.T) {
	t.Parallel()
	for _, remoteOriginal := range []bool{true, false} {
		t.Run(fmt.Sprintf("remote-original=%t", remoteOriginal), func(t *testing.T) {
			t.Parallel()
			local, remote, reg, opts, original, _ := ordinaryPrivacyFixture(t)
			if !ordinaryBundleHasSkillBody(t, original) {
				t.Fatal("fixture lacks body-authorized skill evidence")
			}
			if !remoteOriginal {
				if err := remote.Delete(t.Context(), original.SourceKey); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(reg.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			reduced := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceNone)
			if ordinaryBundleHasSkillBody(t, reduced) {
				t.Fatal("reduction retained skill body")
			}
			restored := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceBody)
			if !ordinaryBundleHasSkillBody(t, restored) {
				t.Fatal("policy increase did not restore the oldest uncommitted content")
			}
			if restored.RequestToken != original.RequestToken || !restored.Bundle.Capture.CapturedAt.Equal(original.Bundle.Capture.CapturedAt) || restored.Preparation.Inputs[0].Reference != reduced.SourceReference() {
				t.Fatal("restoration changed request, age or immediate correspondence")
			}
			// A later reduction still limits the restored oldest content.
			again := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceMetadata)
			if ordinaryBundleHasSkillBody(t, again) {
				t.Fatal("metadata policy kept skill body")
			}
			restored = runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceBody)
			if !ordinaryBundleHasSkillBody(t, restored) {
				t.Fatal("repeated policy increase lost the oldest uncommitted content")
			}
			remote.failMetadata = false
			opts.SkillEvidence = config.SkillEvidenceBody
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] != nil {
				t.Fatal("publish restored successor", result, err)
			}
			metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if metadata.SourceBundle != restored.SourceReference() {
				t.Fatal("restored successor was not selected")
			}
			if pending, err := local.HasPending(reg.ArchiveSessionID); err != nil || pending {
				t.Fatal("restored successor remains pending", pending, err)
			}
		})
	}
}

func TestOrdinaryPrivacyIncreaseKeepsCommittedReduction(t *testing.T) {
	t.Parallel()
	local, remote, reg, opts, _, _ := ordinaryPrivacyFixture(t)
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	reduced := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceNone)
	// The reduction reached the remote before the process stopped, so it is the
	// committed baseline even though local settlement did not run.
	if err := remote.MemoryStore.Put(t.Context(), reduced.SourceKey, reduced.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if err := remote.MemoryStore.Put(t.Context(), reduced.MetadataKey, reduced.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceBody
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("privacy successor read live skill state")
		return nil, nil
	}
	for range 3 {
		if _, err := Run(t.Context(), local, remote, opts); err != nil {
			t.Fatal(err)
		}
	}
	if p, found, err := local.LoadPublicationPending(reg.ArchiveSessionID); err != nil || found && ordinaryBundleHasSkillBody(t, p) {
		t.Fatal("committed reduction was resurrected in pending work", found, err)
	}
	metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	raw, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil || bytes.Contains(encoded, []byte(ordinaryPrivateSkill)) {
		t.Fatal("committed reduction was resurrected remotely", err)
	}
}

const historyPrivateSkill = "private-history-skill-body"

func historyHasSkillBody(t *testing.T, scan *sessionScan, p state.PendingPublication) bool {
	t.Helper()
	found := false
	for _, stage := range p.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(bundle.SupplementalEvidence)
		if err != nil {
			t.Fatal(err)
		}
		found = found || bytes.Contains(encoded, []byte(historyPrivateSkill))
	}
	return found
}

// historyPolicyJournal leaves an attempted, body-authorized complete
// current/preserved history journal whose native rollouts are then removed.
func historyPolicyJournal(t *testing.T) (*sessionScan, state.PendingPublication) {
	t.Helper()
	scan, _ := reconciliationFixture(t)
	scan.opts.SkillEvidence = config.SkillEvidenceBody
	scan.opts.SupplementalEvidence = func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: at, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "snapshot": historyPrivateSkill}}}, nil
	}
	if _, err := scan.run(scan.ctx); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	p, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found {
		t.Fatal(found, err)
	}
	for p.History.Preparing {
		if err := scan.advanceHistoryPreparation(&p); err != nil {
			t.Fatal(err)
		}
	}
	p.Attempted = true
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	if !historyHasSkillBody(t, scan, p) {
		t.Fatal("fixture lacks body-authorized skill evidence")
	}
	metadata, err := scan.frozenHistoryMetadata(p)
	if err != nil || len(metadata.History.Preserved) == 0 {
		t.Fatal("fixture is not a complete current/preserved set", err)
	}
	if err := os.RemoveAll(filepath.Dir(scan.reg.TranscriptPath)); err != nil {
		t.Fatal(err)
	}
	scan.opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("history privacy successor read live skill state")
		return nil, nil
	}
	return scan, p
}

func stricterHistoryPolicy(t *testing.T, scan *sessionScan, p state.PendingPublication, mode config.SkillEvidence) state.PendingPublication {
	t.Helper()
	scan.opts.SkillEvidence = mode
	if _, err := scan.resumeStricterHistory(p); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(mode, err)
	}
	next, found, err := scan.local.LoadPublicationPending(scan.id())
	if err != nil || !found || next.History == nil || next.History.Preparing || next.SkillEvidence != string(mode) {
		t.Fatal("history policy successor was not installed", mode, found, err)
	}
	return next
}

func TestHistoryPrivacyIncreaseRestoresOldestCompleteSourceSet(t *testing.T) {
	t.Parallel()
	scan, original := historyPolicyJournal(t)
	reduced := stricterHistoryPolicy(t, scan, original, config.SkillEvidenceNone)
	if historyHasSkillBody(t, scan, reduced) {
		t.Fatal("reduction retained skill body")
	}
	restored := stricterHistoryPolicy(t, scan, reduced, config.SkillEvidenceBody)
	if !historyHasSkillBody(t, scan, restored) {
		t.Fatal("policy increase did not restore the oldest uncommitted source set")
	}
	if restored.RequestToken != original.RequestToken || len(restored.History.Sources) != len(reduced.History.Sources) {
		t.Fatal("restoration changed request or source-set shape")
	}
	again := stricterHistoryPolicy(t, scan, restored, config.SkillEvidenceMetadata)
	if historyHasSkillBody(t, scan, again) {
		t.Fatal("metadata policy kept skill body")
	}
	restored = stricterHistoryPolicy(t, scan, again, config.SkillEvidenceBody)
	if !historyHasSkillBody(t, scan, restored) {
		t.Fatal("repeated policy increase lost the oldest uncommitted source set")
	}
	if outcome, err := scan.publishPending(scan.ctx, restored); err != nil || outcome != outcomePublished {
		t.Fatal("restored successor failed", err)
	}
	assertCompleteHistory(t, scan, restored, scan.remote)
}

func TestHistoryPrivacyIncreaseKeepsCommittedReduction(t *testing.T) {
	t.Parallel()
	scan, original := historyPolicyJournal(t)
	reduced := stricterHistoryPolicy(t, scan, original, config.SkillEvidenceNone)
	for _, stage := range reduced.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := scan.remote.Put(t.Context(), stage.Reference.Key, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.remote.Put(t.Context(), reduced.MetadataKey, reduced.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	reduced.Attempted = true
	if err := scan.local.SavePending(scan.id(), reduced); err != nil {
		t.Fatal(err)
	}
	next := stricterHistoryPolicy(t, scan, reduced, config.SkillEvidenceBody)
	if historyHasSkillBody(t, scan, next) {
		t.Fatal("committed reduction was resurrected")
	}
}

// frozenSkillGenerationFixture settles a frozen generation whose retained
// capture holds body-authorized skill evidence.
func frozenSkillGenerationFixture(t *testing.T) (*state.Store, *storagetest.MemoryStore, archive.SessionRegistration, Options, time.Time) {
	t.Helper()
	s, cloud := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	if err := s.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", SkillEvidence: config.SkillEvidenceBody, Now: func() time.Time { return at }, RepoKey: func(string) string { return "" }, SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: at, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "snapshot": ordinaryPrivateSkill}}}, nil
	}}
	if result, err := Run(t.Context(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("publish: %#v %v", result, err)
	}
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	opts.Now = func() time.Time { return at.Add(time.Hour) }
	if result, err := Run(t.Context(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("block: %#v %v", result, err)
	}
	reg, _, err := s.LoadRegistration("session-1")
	if err != nil {
		t.Fatal(err)
	}
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), reg, at.Add(2*time.Hour), opts)
	defer closePreview()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at.Add(2*time.Hour), build); err != nil {
		t.Fatal(err)
	}
	opts.Now = func() time.Time { return at.Add(3 * time.Hour) }
	for range 2 {
		if result, err := Run(t.Context(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
			t.Fatalf("settle: %#v %v", result, err)
		}
	}
	reg, _, err = s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s, cloud, reg, opts, at
}

func TestFrozenOrdinaryPrivacyIncreaseRestoresOldestUncommittedContent(t *testing.T) {
	t.Parallel()
	local, cloud, reg, opts, at := frozenSkillGenerationFixture(t)
	if !reg.CaptureFrozen {
		t.Fatal("fixture is not an actual frozen generation")
	}
	opts.AcceptSession = func(r archive.SessionRegistration) bool { return r.ArchiveSessionID == reg.ArchiveSessionID }
	if err := local.SaveRequest(reg.ArchiveSessionID, "feedback", at, archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-frozen-oldest", ObservedAt: at, Payload: map[string]any{"text": "retained feedback"}}); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: cloud, failMetadata: true}
	if _, err := Run(t.Context(), local, remote, opts); err != nil {
		t.Fatal(err)
	}
	original, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || original.Commit == nil || original.History != nil {
		t.Fatal("missing sealed frozen maintenance", found, err)
	}
	if !ordinaryBundleHasSkillBody(t, original) {
		t.Fatal("frozen maintenance lacks body-authorized skill evidence")
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	reduced := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceNone)
	if ordinaryBundleHasSkillBody(t, reduced) {
		t.Fatal("frozen reduction retained skill body")
	}
	restored := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceBody)
	if !ordinaryBundleHasSkillBody(t, restored) || restored.RequestToken != original.RequestToken || !restored.Bundle.Capture.CapturedAt.Equal(original.Bundle.Capture.CapturedAt) {
		t.Fatal("frozen policy increase did not restore the oldest uncommitted content")
	}
}

type failingOnceRefilter struct {
	agentapi.TranscriptFilter
	failed *bool
}

func (f failingOnceRefilter) RefilterLeased(ctx context.Context, bundle archive.SourceBundle, started time.Time, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	if !*f.failed {
		*f.failed = true
		return archive.FilteredTranscript{}, nil, errors.New("synthetic interruption during retained filtering")
	}
	return f.TranscriptFilter.(agentapi.LeasedTranscriptRefilter).RefilterLeased(ctx, bundle, started, budget)
}

func TestOrdinaryPrivacyIncreaseSurvivesInterruptionAndLeavesNewerRequestOwed(t *testing.T) {
	t.Parallel()
	local, remote, reg, opts, original, _ := ordinaryPrivacyFixture(t)
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	reduced := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceNone)
	later := opts.Now().Add(time.Minute)
	if err := local.SaveRequest(reg.ArchiveSessionID, "feedback", later, archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-newer", ObservedAt: later, Payload: map[string]any{"text": "newer feedback"}}); err != nil {
		t.Fatal(err)
	}
	newer, found, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || newer.Token == "" {
		t.Fatal("newer request was not recorded", found, err)

	}
	path := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := sourceAdapter(testSources, "codex")
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	interrupted := opts
	interrupted.Sources = retainedComparatorSources{SourcesLookup: testSources, filter: failingOnceRefilter{TranscriptFilter: adapter, failed: &failed}}
	interrupted.SkillEvidence = config.SkillEvidenceBody
	result, err := Run(t.Context(), local, remote, interrupted)
	if err != nil || !failed || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("interruption did not stop the successor", result, err, failed)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatal("interrupted successor replaced the reduced journal", err)
	}
	restored := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceBody)
	if !ordinaryBundleHasSkillBody(t, restored) || restored.Preparation.Inputs[0].Reference != reduced.SourceReference() || restored.RequestToken != original.RequestToken || restored.RequestToken == newer.Token {
		t.Fatal("restart did not restore the oldest uncommitted content")
	}
	if req, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found || req.Token != newer.Token {
		t.Fatal("restoration discharged or replaced the newer request", found, err)
	}
}

func TestPublicationPrivacyOriginChargesStoreBudget(t *testing.T) {
	t.Parallel()
	local, remote, reg, opts, _, _ := ordinaryPrivacyFixture(t)
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	reduced := runOrdinaryPolicy(t, local, remote, reg, opts, config.SkillEvidenceNone)
	absent := state.PublicationPredecessor{State: state.PredecessorAbsent}
	budget := agentapi.NewNativeReadBudget(1 << 30)
	scoped, closeScope := local.WithReadBudget(t.Context(), budget)
	origin, found, err := scoped.PublicationPrivacyOrigin(reg.ArchiveSessionID, reduced, absent)
	if err != nil || !found || origin.Policy != config.SkillEvidenceBody || len(origin.Sources) != 1 {
		t.Fatal("linked oldest original was not returned", found, err)
	}
	if budget.Available() >= 1<<30 {
		t.Fatal("retained original was not charged")
	}
	closeScope()
	if budget.Available() != 1<<30 {
		t.Fatal("retained original charge leaked", budget.Available())
	}
	tiny := agentapi.NewNativeReadBudget(1 << 10)
	scoped, closeScope = local.WithReadBudget(t.Context(), tiny)
	if _, _, err := scoped.PublicationPrivacyOrigin(reg.ArchiveSessionID, reduced, absent); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("over-budget original was admitted", err)
	}
	closeScope()
	if tiny.Available() != 1<<10 {
		t.Fatal("refused original leaked its charge", tiny.Available())
	}
	if _, found, err := local.PublicationPrivacyOrigin(reg.ArchiveSessionID, reduced, state.PublicationPredecessor{State: state.PredecessorPresent, Body: reduced.MetadataBytes}); err != nil || found {
		t.Fatal("original authorized across a committed position", found, err)
	}
}
