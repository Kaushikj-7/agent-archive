package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func nativeAuthorityRegistration(t *testing.T) archive.SessionRegistration {
	t.Helper()
	reg := registration(t)
	reg.Harness.Name = "synthetic"
	reg.TranscriptPath = "/native/session.jsonl"
	reg.NativeBinding = &archive.NativeSourceBinding{Agent: "synthetic", Version: 1, NativeID: reg.NativeSessionID, NativeCreatedAt: reg.SessionStartedAt, Cwd: reg.ProjectRoot, Path: reg.TranscriptPath, RootRevision: "one", StreamID: "stream", RootID: "root", ParentID: "parent"}
	return reg
}
func protectNativeAuthority(t *testing.T, s *Store) {
	t.Helper()
	if err := config.Save(s.home, config.Config{RetentionDays: 90, NativeRoots: []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}}); err != nil {
		t.Fatal(err)
	}
}
func TestNativeBindingRequiresFenceAndCannotLoseAuthority(t *testing.T) {
	s := newTestStore(t)
	reg := nativeAuthorityRegistration(t)
	if err := s.SaveRegistration(reg); err == nil {
		t.Fatal("binding persisted before writer protection")
	}
	protectNativeAuthority(t, s)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*archive.SessionRegistration){
		func(r *archive.SessionRegistration) { r.NativeBinding = nil },
		func(r *archive.SessionRegistration) { r.NativeBinding.StreamID = "different" },
		func(r *archive.SessionRegistration) { r.NativeBinding.RootRevision = "different" },
		func(r *archive.SessionRegistration) {
			r.NativeBinding.NativeCreatedAt = r.NativeBinding.NativeCreatedAt.Add(1)
		},
		func(r *archive.SessionRegistration) { r.ProjectRoot = "/another" },
		func(r *archive.SessionRegistration) { r.DestinationID = "different" },
		func(r *archive.SessionRegistration) {
			r.NativeBinding.Path = "/native/moved"
			r.TranscriptPath = "/native/moved"
		},
	}
	for _, mutate := range mutations {
		if _, err := s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { mutate(r); return nil }); err == nil {
			t.Fatal("update changed native authority")
		}
		raw, _ := json.Marshal(reg)
		var replacement archive.SessionRegistration
		_ = json.Unmarshal(raw, &replacement)
		mutate(&replacement)
		if err := s.SaveRegistration(replacement); err == nil {
			t.Fatal("replacement changed native authority")
		}
	}
	if _, err := s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error {
		r.HookObservedAt = r.RegisteredAt
		r.CaptureFrozen = true
		return nil
	}); err != nil {
		t.Fatal("ordinary evidence/freeze refused", err)
	}
	got, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || !got.NativeBinding.PreservesFacts(reg.NativeBinding) {
		t.Fatal("authority lost", err)
	}
}
func TestMissingNativeBindingCannotBootstrapFromSurvivingLocator(t *testing.T) {
	s := newTestStore(t)
	protectNativeAuthority(t, s)
	reg := nativeAuthorityRegistration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	reg.NativeBinding = nil
	raw, _ := json.Marshal(reg)
	if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.HookObservedAt = r.RegisteredAt; return nil }); err == nil {
		t.Fatal("dropped authority silently reauthorized surviving locator")
	}
	changed := nativeAuthorityRegistration(t)
	changed.NativeBinding.StreamID = "replacement"
	if err := s.SaveRegistration(changed); err == nil {
		t.Fatal("current file supplied replacement authority after binding loss")
	}
}

func TestNativeBindingSurvivesGenerationJournalAndFrozenPending(t *testing.T) {
	s := newTestStore(t)
	protectNativeAuthority(t, s)
	reg := nativeAuthorityRegistration(t)
	reg.DestinationID = "original-destination"
	reg.AdmittedAt = reg.RegisteredAt
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("original-import")
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: reg.RegisteredAt}}
	p, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Save(bundle, reg.RegisteredAt, CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	if err = p.SaveBlocked(bundle, reg.RegisteredAt, BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("synthetic crash")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	at := reg.RegisteredAt.Add(time.Hour)
	if _, err = s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
		t.Fatal("did not stop after protected journal", err)
	}
	s.onIndexStep = nil
	if err = s.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	next, found, err := s.GenerationSuccessor(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	successor, found, err := s.LoadRegistration(next)
	if err != nil || !found || !sameNativeImportBinding(successor.NativeBinding, reg.NativeBinding) {
		t.Fatal("generation lost original authority", err)
	}
	if _, found, err := s.LoadPending(next); err != nil || !found {
		t.Fatal("frozen pending did not survive", err)
	}
}

func TestNativeImportOwnerCannotDropOrReplaceBinding(t *testing.T) {
	reg := nativeAuthorityRegistration(t)
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("one")
	stage := ImportStage{Registration: reg, Bundle: archive.SourceBundle{ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness}}}
	if err := stage.validateOwner(reg); err != nil {
		t.Fatal(err)
	}
	reg.NativeBinding = nil
	if err := stage.validateOwner(reg); err == nil {
		t.Fatal("import dropped native binding")
	}
}

func TestNativeBindingFirstCommitRefusesPendingSetup(t *testing.T) {
	s := newTestStore(t)
	protectNativeAuthority(t, s)
	bound := nativeAuthorityRegistration(t)
	unbound := bound
	unbound.NativeBinding = nil
	unbound.TranscriptPath = ""
	if err := s.SaveRegistration(unbound); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "setup-transaction.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRegistration(unbound.ArchiveSessionID, func(r *archive.SessionRegistration) error {
		r.NativeBinding = bound.NativeBinding
		r.TranscriptPath = bound.TranscriptPath
		return nil
	}); err == nil {
		t.Fatal("binding committed during pending setup")
	}
	got, _, err := s.LoadRegistration(unbound.ArchiveSessionID)
	if err != nil || got.NativeBinding != nil || got.TranscriptPath != "" {
		t.Fatal("failed commit changed pending owner", err)
	}
}
