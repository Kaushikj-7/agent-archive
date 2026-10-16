package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestSkillPolicyLimitsPendingAndUploadedBytes(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceMetadata} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			skill := filepath.Join(project, ".agents", "skills", "secret", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skill, []byte("---\nname: secret\n---\nunique-private-skill-body"), 0600); err != nil {
				t.Fatal(err)
			}
			transcript := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
			reg := registration(t, transcript)
			reg.ProjectRoot = project
			local := newTestStore(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			options := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, SkillEvidence: mode,
				SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
					return evidence.ObserveSkills(evidence.SkillOptions{Locations: skillPolicyRoots(project), Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
				},
			}
			_, _ = Run(context.Background(), local, remote, options)
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatalf("pending: %v %v", found, err)
			}
			check := func(label string, source []byte) {
				t.Helper()
				bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range bundle.SupplementalEvidence {
					if item.Kind == archive.EvidenceKindSkillSnapshot || mode == config.SkillEvidenceNone && item.Kind == archive.EvidenceKindSkillInventory {
						t.Fatalf("%s carried forbidden %s", label, item.Kind)
					}
				}
				if strings.Contains(string(source), "unique-private-skill-body") {
					t.Fatalf("%s leaked body", label)
				}
			}
			check("pending", pending.SourceBytes)
			remote.failMetadata = false
			if _, err := Run(context.Background(), local, remote, options); err != nil {
				t.Fatal(err)
			}
			meta := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			uploaded, err := remote.Get(context.Background(), meta.SourceBundle.Key)
			if err != nil {
				t.Fatal(err)
			}
			check("uploaded", uploaded)
		})
	}
}

func TestStricterPolicyRetainsFrozenPendingEvidence(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	skill := filepath.Join(project, ".agents", "skills", "sample", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: sample\n---\nprivate-skill-body"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	reg.ProjectRoot = project
	local := newTestStore(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	mode := config.SkillEvidenceBody
	options := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC) }, SkillEvidence: mode,
		SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
			return evidence.ObserveSkills(evidence.SkillOptions{Locations: skillPolicyRoots(project), Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
		},
	}
	_, _ = Run(context.Background(), local, remote, options)
	old, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("old pending: %v %v", found, err)
	}
	if !strings.Contains(old.Bundle.SupplementalEvidence[len(old.Bundle.SupplementalEvidence)-1].Payload["snapshot"].(string), "private-skill-body") {
		t.Fatal("body setup failed")
	}
	before, err := os.ReadFile(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	mode = config.SkillEvidenceNone
	options.SkillEvidence = mode
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", options.Now()); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, options)
	if err != nil || result.Errors[reg.ArchiveSessionID] != nil || len(result.Published) != 0 {
		t.Fatal("changed policy did not prepare retained pending work", result, err)
	}
	next, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit == nil || next.Commit.Purpose != state.PublicationPrivacyRewrite || next.Preparation == nil || len(next.Preparation.Inputs) != 1 || next.Preparation.Inputs[0].Reference != old.SourceReference() {
		t.Fatal("changed policy lost exact retained input authority", found, err)
	}
	requireFrozenPublicationOriginal(t, local, reg.ArchiveSessionID, before)
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("obsolete-policy request completed prematurely", found, err)
	}
	work, err := local.Outstanding(reg, false)
	if err != nil || !work.Upload || !work.Pending() || !work.DefersExpiry() {
		t.Fatal("obsolete-policy evidence disappeared from work status", work, err)
	}
	if _, err := remote.Get(t.Context(), old.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("obsolete source became authoritative", err)
	}

}

func TestStricterPolicyReplacesPublishedSourceWithoutTranscriptChange(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	skill := filepath.Join(project, ".agents", "skills", "sample", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: sample\n---\nprivate-skill-body"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	reg.ProjectRoot = project
	local := newTestStore(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mode := config.SkillEvidenceBody
	options := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, SkillEvidence: mode,
		SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
			return evidence.ObserveSkills(evidence.SkillOptions{Locations: skillPolicyRoots(project), Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
		},
	}
	if _, err := Run(context.Background(), local, remote, options); err != nil {
		t.Fatal(err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	mode = config.SkillEvidenceNone
	options.SkillEvidence = mode
	now = now.Add(time.Hour)
	if _, err := Run(context.Background(), local, remote, options); err != nil {
		t.Fatal(err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.SourceBundle.Key == old.SourceBundle.Key {
		t.Fatal("policy downgrade retained old source")
	}
	source, err := remote.Get(context.Background(), next.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindSkillInventory || item.Kind == archive.EvidenceKindSkillSnapshot {
			t.Fatalf("downgraded source carried %s", item.Kind)
		}
	}
}

func skillPolicyRoots(project string) []agentapi.SkillRoot {
	provider, _ := testSources.LookupSkills("codex")
	return provider.EvidenceRoots(agentapi.SkillLocations{ProjectRoot: project})
}
