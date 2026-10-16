package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func ordinaryPrivacyFixture(t *testing.T) (*state.Store, *metadataFailStore, archive.SessionRegistration, Options, state.PendingPublication, []byte) {
	t.Helper()
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
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceBody, Now: func() time.Time { return at }, SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		return evidence.ObserveSkills(evidence.SkillOptions{Locations: skillPolicyRoots(project), Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: config.SkillEvidenceBody})
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
	local, remote, reg, opts, original, raw := ordinaryPrivacyFixture(t)
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	opts.Now = func() time.Time { return original.Bundle.Capture.CapturedAt.Add(48 * time.Hour) }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("privacy resume read live skill state")
		return nil, nil
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], archive.ErrHistoryMutationPending) {
		t.Fatal("prepare successor", result, err)
	}
	next, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit == nil || next.Commit.Purpose != state.PublicationPrivacyRewrite || next.History != nil || next.Bundle.History != nil || next.Bundle.SchemaVersion != archive.SourceSchemaVersion {
		t.Fatal("ordinary successor", found, err)
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
	if _, err := remote.Get(t.Context(), original.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("broader metadata became authoritative", err)
	}
	t.Logf("guard=%v", local.CheckPublicationSessionRead(reg, opts.MachineID, (&sessionScan{reg: reg}).publicationAdmission()))
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
}
