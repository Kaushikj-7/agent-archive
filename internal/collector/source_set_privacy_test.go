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
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func ordinaryPrivacyFixture(t *testing.T, initialMode ...config.SkillEvidence) (*state.Store, *metadataFailStore, archive.SessionRegistration, Options, state.PendingPublication, []byte) {
	t.Helper()
	mode := config.SkillEvidenceBody
	if len(initialMode) > 0 {
		mode = initialMode[0]
	}
	project := t.TempDir()
	skill := filepath.Join(project, ".agents", "skills", "sample", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: sample\n---\nprivate-ordinary-skill-body"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	reg.ProjectRoot = project
	local := newTestStore(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: mode, Now: func() time.Time { return at }, SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		return evidence.ObserveSkills(evidence.SkillOptions{Locations: skillPolicyRoots(project), Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
	}}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("fixture did not leave attempted pending work", result, err)
	}
	p, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || p.History != nil || p.Bundle.SchemaVersion != archive.SourceSchemaVersion || p.Commit == nil {
		t.Fatal("ordinary fixture", found, err)
	}
	raw, err := os.ReadFile(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return local, remote, reg, opts, p, raw
}

func TestOrdinaryPrivacySuccessorUsesRetainedSourceAndPreservesOriginal(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		for _, mode := range []config.SkillEvidence{config.SkillEvidenceBody, config.SkillEvidenceNone} {
			t.Run(fmt.Sprintf("old=%s/committed=%v", mode, committed), func(t *testing.T) {
				t.Parallel()
				checkOrdinaryPrivacySuccessor(t, mode, committed)
			})
		}
	}
}

func checkOrdinaryPrivacySuccessor(t *testing.T, mode config.SkillEvidence, committed bool) {
	t.Helper()
	local, remote, reg, opts, original, raw := ordinaryPrivacyFixture(t, mode)
	if committed {
		if err := remote.MemoryStore.Put(t.Context(), original.MetadataKey, original.MetadataBytes); err != nil {
			t.Fatal(err)
		}
	}
	oldEvidence, err := json.Marshal(original.Bundle.SupplementalEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(oldEvidence, []byte("private-ordinary-skill-body")) != (mode == config.SkillEvidenceBody) {
		t.Fatal("skill policy fixture did not retain its requested ceiling")
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	if mode == config.SkillEvidenceNone {
		opts.SkillEvidence = config.SkillEvidenceBody
	}
	opts.Now = func() time.Time { return original.Bundle.Capture.CapturedAt.Add(48 * time.Hour) }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("privacy resume read live skill state")
		return nil, nil
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] != nil {
		t.Fatal("prepare successor", result, err)
	}
	next, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit == nil || next.Commit.Purpose != state.PublicationPrivacyRewrite || next.History != nil || next.Bundle.History != nil || next.Bundle.SchemaVersion != archive.SourceSchemaVersion {
		t.Fatal("ordinary successor", found, err)
	}
	cleanupBefore, err := json.Marshal(next.Cleanup)
	if err != nil {
		t.Fatal(err)
	}
	foreign := original.SourceReference()
	foreign.Key += ".foreign"
	if err := next.RetireOrdinaryPrivacySources(t.Context(), nil, []archive.SourceReference{original.SourceReference(), foreign}, opts.Now()); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatal("foreign retirement accepted", err)
	}
	cleanupAfter, err := json.Marshal(next.Cleanup)
	if err != nil || !bytes.Equal(cleanupBefore, cleanupAfter) {
		t.Fatal("refused retirement mutated cleanup", err)
	}
	if !next.Bundle.Capture.CapturedAt.Equal(original.Bundle.Capture.CapturedAt) || next.RequestToken != original.RequestToken || next.ScanSignature != nil || bytes.Contains(next.SourceBytes, []byte("private-ordinary-skill-body")) {
		t.Fatal("maintenance changed capture provenance")
	}
	for _, item := range next.Bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindSkillSnapshot || item.Kind == archive.EvidenceKindSkillInventory {
			t.Fatal("stricter source retained skill evidence")
		}
	}
	evidenceRaw, err := os.ReadFile(filepath.Join(local.Home(), "publication-evidence", reg.ArchiveSessionID, "original.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frozen state.PublicationOriginalEvidence
	if err := json.Unmarshal(evidenceRaw, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.PrivacyOrigin == nil || !bytes.Equal(frozen.PrivacyOrigin.Raw, raw) {
		t.Fatal("exact old journal was not retained before selection")
	}
	beforeSelecting, err := remote.Get(t.Context(), original.MetadataKey)
	if committed {
		if err != nil || !bytes.Equal(beforeSelecting, original.MetadataBytes) {
			t.Fatal("preparation changed the actual committed predecessor", err)
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("broader metadata became authoritative", err)
	}
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] != nil {
		t.Fatal("publish stricter successor", result, err)
	}
	metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if metadata.History != nil || metadata.SourceBundle != next.SourceReference() || !metadata.CapturedAt.Equal(original.Bundle.Capture.CapturedAt) {
		t.Fatal("ordinary selecting identity or age changed")
	}
	if pending, err := local.HasPending(reg.ArchiveSessionID); err != nil || pending {
		t.Fatal("completed privacy work remains pending", pending, err)
	}
	if original.SourceReference() != next.SourceReference() {
		retired, err := local.LoadSuperseded(reg.ArchiveSessionID)
		if err != nil || len(retired) != 1 || retired[0].Key != original.SourceKey || !retired[0].PrivacySensitive {
			t.Fatal("broader immutable bytes lack privacy cleanup obligation", retired, err)
		}
	}
}

func requireFrozenPublicationOriginal(t *testing.T, local *state.Store, id string, raw []byte) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(local.Home(), "publication-evidence", id, "original.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frozen state.PublicationOriginalEvidence
	if err := json.Unmarshal(body, &frozen); err != nil {
		t.Fatal(err)
	}
	for _, role := range []*state.PublicationOriginalRole{frozen.MigrationOrigin, frozen.PrivacyOrigin} {
		if role != nil && bytes.Equal(role.Raw, raw) {
			return
		}
	}
	t.Fatal("the exact original pending journal was not preserved")
}

type privacyPositionStore struct {
	*metadataFailStore
	metadataKey string
	readError   error
	puts        int
}

func (s *privacyPositionStore) Put(ctx context.Context, key string, body []byte) error {
	s.puts++
	return s.metadataFailStore.Put(ctx, key, body)
}

func (s *privacyPositionStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if key == s.metadataKey && s.readError != nil {
		return nil, s.readError
	}
	return s.metadataFailStore.GetLimited(ctx, key, limit)
}

func TestOrdinaryPrivacyRefusesUnknownOrUnreadablePosition(t *testing.T) {
	t.Parallel()
	for _, unreadable := range []bool{false, true} {
		t.Run(fmt.Sprint(unreadable), func(t *testing.T) {
			t.Parallel()
			local, remote, reg, opts, original, raw := ordinaryPrivacyFixture(t)
			opts.SkillEvidence = config.SkillEvidenceNone
			winner := []byte(`{"unrelated":"committed winner"}`)
			if err := remote.MemoryStore.Put(t.Context(), original.MetadataKey, winner); err != nil {
				t.Fatal(err)
			}
			controlled := &privacyPositionStore{metadataFailStore: remote, metadataKey: original.MetadataKey}
			want := storage.ErrPublicationConflict
			if unreadable {
				want = errors.New("synthetic unreadable metadata")
				controlled.readError = want
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), local, controlled, reg, state.Request{}, published, opts.Now(), opts)
			defer scan.releaseRetained()
			_, handled, err := scan.resumeSourceSetPrivacy(original)
			if !handled || !errors.Is(err, want) || controlled.puts != 0 {
				t.Fatal("uncertain position authorized side effects", handled, err, controlled.puts)
			}
			after, err := os.ReadFile(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json"))
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("uncertain position replaced the original pending bytes", err)
			}
			actual, err := remote.MemoryStore.Get(t.Context(), original.MetadataKey)
			if err != nil || !bytes.Equal(actual, winner) {
				t.Fatal("uncertain winner changed", err)
			}
		})
	}
}

type privacyPositionFilter struct {
	agentapi.TranscriptFilter
	after func()
}

func (f privacyPositionFilter) RefilterLeased(ctx context.Context, bundle archive.SourceBundle, started time.Time, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	leased, ok := f.TranscriptFilter.(agentapi.LeasedTranscriptRefilter)
	if !ok {
		return archive.FilteredTranscript{}, nil, errors.New("fixture adapter has no leased retained filter")
	}
	filtered, release, err := leased.RefilterLeased(ctx, bundle, started, budget)
	if err == nil {
		f.after()
	}
	return filtered, release, err
}

func TestOrdinaryPrivacyRechecksPositionBeforeSelectingSuccessor(t *testing.T) {
	t.Parallel()
	local, remote, reg, opts, original, raw := ordinaryPrivacyFixture(t)
	opts.SkillEvidence = config.SkillEvidenceNone
	adapter, err := sourceAdapter(testSources, "codex")
	if err != nil {
		t.Fatal(err)
	}
	winner := []byte(`{"unrelated":"winner during retained filtering"}`)
	changed := false
	filter := privacyPositionFilter{TranscriptFilter: adapter, after: func() {
		changed = true
		if err := remote.MemoryStore.Put(t.Context(), original.MetadataKey, winner); err != nil {
			t.Fatal(err)
		}
	}}
	opts.Sources = retainedComparatorSources{SourcesLookup: testSources, filter: filter}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || !changed || !errors.Is(result.Errors[reg.ArchiveSessionID], storage.ErrPublicationConflict) {
		t.Fatal("changed position was selected", result, err, changed)
	}
	after, err := os.ReadFile(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json"))
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("stale predecessor replaced original evidence", err)
	}
	actual, err := remote.MemoryStore.Get(t.Context(), original.MetadataKey)
	if err != nil || !bytes.Equal(actual, winner) {
		t.Fatal("fresh winner was overwritten", err)
	}
}

func TestFrozenOrdinaryPrivacyRetryUsesRetainedSuccessor(t *testing.T) {
	t.Parallel()
	local, cloud, reg, opts, at := settledGenerationFixture(t)
	if !reg.CaptureFrozen {
		t.Fatal("fixture is not an actual frozen generation")
	}
	opts.AcceptSession = func(r archive.SessionRegistration) bool { return r.ArchiveSessionID == reg.ArchiveSessionID }
	if err := local.SaveRequest(reg.ArchiveSessionID, "feedback", at, archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-frozen-retry", ObservedAt: at, Payload: map[string]any{"text": "retained feedback"}}); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: cloud, failMetadata: true}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("maintenance upload did not fail", result, err)
	}
	old, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || old.Commit == nil || old.History != nil || old.Bundle.SchemaVersion != archive.SourceSchemaVersion {
		t.Fatal("missing sealed ordinary maintenance", found, err)
	}
	raw, err := os.ReadFile(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("frozen retry read live skill inventory")
		return nil, nil
	}
	remote.failMetadata = false
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] != nil {
		t.Fatal("prepare frozen successor", result, err)
	}
	next, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit == nil || next.Commit.Purpose != state.PublicationPrivacyRewrite || next.History != nil || next.Bundle.SchemaVersion != archive.SourceSchemaVersion || next.SkillEvidence != string(config.SkillEvidenceNone) || !next.Bundle.Capture.CapturedAt.Equal(old.Bundle.Capture.CapturedAt) || next.RequestToken != old.RequestToken {
		t.Fatal("invalid frozen successor", found, err)
	}
	var original state.PublicationOriginalEvidence
	body, err := os.ReadFile(filepath.Join(local.Home(), "publication-evidence", reg.ArchiveSessionID, "original.json"))
	if err != nil || json.Unmarshal(body, &original) != nil || original.PrivacyOrigin == nil || !bytes.Equal(original.PrivacyOrigin.Raw, raw) {
		t.Fatal("frozen original was not retained exactly", err)
	}
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] != nil {
		t.Fatal("complete frozen successor", result, err)
	}
	selected, err := cloud.Get(t.Context(), next.MetadataKey)
	if err != nil || !bytes.Equal(selected, next.MetadataBytes) {
		t.Fatal("frozen successor was not selected exactly", err)
	}
	if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("completed frozen successor remains owed", found, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("covered frozen request remains owed", found, err)
	}
	metadata := fetchMetadata(t, cloud, reg.Harness.Name, reg.ArchiveSessionID)
	if !metadata.CapturedAt.Equal(old.Bundle.Capture.CapturedAt) {
		t.Fatal("frozen successor manufactured activity")
	}
}
