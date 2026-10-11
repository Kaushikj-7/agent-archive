package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func stagingFixture(t *testing.T) (Registration, Candidate, config.Config) {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{RetentionDays: 90, MachineID: "synthetic", Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: fixedNow.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "parent.jsonl")
	if err := os.WriteFile(path, []byte(claudeTranscript("synthetic-parent", project, fixedNow.Add(-time.Minute))), 0600); err != nil {
		t.Fatal(err)
	}
	r := Registration{Sources: testSources, DurableSources: true, Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: fixedNow, MaxHoldSteps: 1, DestinationID: cfg.DestinationID()}
	if err := SaveBatch(home, Batch{ID: r.Batch, StartedAt: fixedNow, DestinationID: r.DestinationID}); err != nil {
		t.Fatal(err)
	}
	c := Candidate{Harness: "claude-code", NativeSessionID: "synthetic-parent", TranscriptPath: path, ProjectRoot: project, StartedAt: fixedNow.Add(-time.Minute), StartedAtSource: archive.StartedAtSourceTranscript, SourceKind: archive.SourceKindFile}
	return r, c, cfg
}

func TestStagedImportedChildrenSurviveDeletionBeforeAdmission(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	child := filepath.Join(c.ProjectRoot, "agent-selected-child.jsonl")
	if err := os.WriteFile(child, []byte(subagentTranscript(c.NativeSessionID, "selected-child", fixedNow.Add(-30*time.Second))), 0600); err != nil {
		t.Fatal(err)
	}
	c.Subagents = []Subagent{{Path: child, AgentID: "selected-child"}}
	r.AfterStage = func(string) error { return errors.New("crash before candidate commit") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing crash")
	}
	for _, path := range []string{c.TranscriptPath, child} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := state.Open(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := collector.Run(t.Context(), reopened, remote, collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("recover descendants %v %v", result.Errors, err)
	}
	regs, err := reopened.LoadRegistrations()
	if err != nil || len(regs) != 2 {
		t.Fatalf("lost child %#v %v", regs, err)
	}
	for _, reg := range regs {
		published, err := reopened.LoadPublishedState(reg.ArchiveSessionID)
		if err != nil || !published.Summary().Published {
			t.Fatalf("not remotely verified %s %v", reg.ArchiveSessionID, err)
		}
	}
}

type importMissingMetadataRemote struct {
	*storagetest.MemoryStore
	drop bool
}

func (s *importMissingMetadataRemote) Put(ctx context.Context, key string, data []byte) error {
	if s.drop && strings.HasSuffix(key, "/metadata.json") {
		return nil
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestStagedImportRequiresRemoteReadbackBeforeLocalDone(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	registered, err := r.Run([]Candidate{c})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := &importMissingMetadataRemote{MemoryStore: storagetest.NewMemoryStore(), drop: true}
	opts := collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession}
	result, err := collector.Run(t.Context(), r.Store, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	id := registered.Sessions[0]
	if !errors.Is(result.Errors[id], storage.ErrNotFound) {
		t.Fatalf("unverified ack: %v", result.Errors)
	}
	reg, _, _ := r.Store.LoadRegistration(id)
	if _, found, err := r.Store.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatalf("lost retry stage %t %v", found, err)
	}
	pending, found, err := r.Store.LoadPending(id)
	if err != nil || !found || !pending.Attempted {
		t.Fatalf("lost publication journal %t %v", found, err)
	}
	published, err := r.Store.LoadPublishedState(id)
	if err != nil || published.Summary().Published {
		t.Fatalf("unverified localdone %v", err)
	}
	remote.drop = false
	result, err = collector.Run(t.Context(), r.Store, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("retry %v %v", result.Errors, err)
	}
	if _, found, err := r.Store.LoadImportStage(t.Context(), reg); err != nil || found {
		t.Fatalf("verified stage retained %t %v", found, err)
	}
}

func TestImportStagingSurvivesNativeDeletionAtAdmissionCrash(t *testing.T) {
	for _, beforeAdmission := range []bool{false, true} {
		t.Run(map[bool]string{false: "after admission", true: "before admission"}[beforeAdmission], func(t *testing.T) {
			r, c, cfg := stagingFixture(t)
			crash := errors.New("crash after durable preparation")
			if beforeAdmission {
				r.AfterStage = func(string) error { return crash }
			}
			result, err := r.Run([]Candidate{c})
			if beforeAdmission {
				if !errors.Is(err, crash) || len(result.Sessions) != 0 {
					t.Fatalf("admitted before stage boundary: %#v %v", result, err)
				}
			} else if err != nil || len(result.Sessions) != 1 {
				t.Fatalf("registration: %#v %v", result, err)
			}
			if err := os.Remove(c.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(r.Home)
			if err != nil {
				t.Fatal(err)
			}
			remote := storagetest.NewMemoryStore()
			collected, err := collector.Run(t.Context(), reopened, remote, collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
			if err != nil || len(collected.Errors) != 0 {
				t.Fatalf("recover deleted native: %v %v", collected.Errors, err)
			}
			regs, err := reopened.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("registrations %#v %v", regs, err)
			}
			metadataKey, _ := archive.MetadataObjectKey(regs[0].Harness.Name, regs[0].ArchiveSessionID)
			raw, err := remote.Get(t.Context(), metadataKey)
			if err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err := json.Unmarshal(raw, &metadata); err != nil {
				t.Fatal(err)
			}
			source, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			text, _ := json.Marshal(bundle)
			if !bytes.Contains(text, []byte("please check it")) {
				t.Fatal("lost staged selected evidence")
			}
			if _, found, err := reopened.LoadImportStage(t.Context(), regs[0]); err != nil || found {
				t.Fatalf("verified stage not released: %t %v", found, err)
			}
		})
	}
}

func TestImportStagingFailureNeverAdmits(t *testing.T) {
	r, c, _ := stagingFixture(t)
	// A nonregular destination simulates failed durable staging without filling
	// the filesystem. Registration must not become durable after that failure.
	id, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: c.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.Home, "sessions", id, "import-stage", "stage.json"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run([]Candidate{c})
	if err == nil || len(result.Sessions) != 0 {
		t.Fatalf("unsafe admission %#v %v", result, err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("failed stage left owner %#v %v", regs, err)
	}
}

func TestUnadmittedStagedImportUndoPreventsRecovery(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	r.AfterStage = func(string) error { return errors.New("interrupted") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing injected interruption")
	}
	batches, err := LoadBatches(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	env := Environment{Sources: testSources, Home: r.Home, Now: func() time.Time { return fixedNow }}
	plan, err := PlanUndo(env, r.Store, cfg, batches, batches[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 1 {
		t.Fatalf("staged owner absent from undo: %#v", plan.Sessions)
	}
	result := plan.Remove(t.Context(), r.Store, storagetest.NewMemoryStore(), fixedNow)
	if len(result.Failed) != 0 {
		t.Fatalf("undo stage: %#v", result)
	}
	if err := r.Store.ResumeImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("undone stage re-admitted %#v %v", regs, err)
	}
}

func TestImportStagingQuotaFailureNeverAdmits(t *testing.T) {
	r, c, _ := stagingFixture(t)
	if err := os.MkdirAll(filepath.Join(r.Home, "pending"), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(r.Home, "pending", "quota-owner.json"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(1 << 29); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Run([]Candidate{c}); !errors.Is(err, state.ErrDurableStorageCapacity) {
		t.Fatalf("quota admitted: %v", err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("admitted without durable evidence %v %v", regs, err)
	}
}

func TestImportAdmissionCursorAndPausedRecovery(t *testing.T) {
	r, c, _ := stagingFixture(t)
	r.AfterStage = func(string) error { return errors.New("crash") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing crash")
	}
	// Populate ordinary session directories before the journal in scan order.
	for i := range 65 {
		if err := os.MkdirAll(filepath.Join(r.Home, "sessions", fmt.Sprintf("000%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(c.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := config.Load(r.Home)
	cfg.Paused = true
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.ResumeAllImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	regs, _ := r.Store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("pause admitted")
	}
	cfg.Paused = false
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.ResumeImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	regs, _ = r.Store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("scheduled recovery exceeded bounded directory group")
	}
	// The next scheduled group resumes the persisted cursor.
	if err := r.Store.ResumeImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	regs, _ = r.Store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("cursor lost selection: %v", regs)
	}
	stage, found, err := r.Store.LoadImportStage(t.Context(), regs[0])
	if err != nil || !found || stage.Bundle.NativeRecords == nil {
		t.Fatalf("recovery lost stage %v", err)
	}
}

func TestStagedPendingRefiltersStricterPrivacyAfterNativeDeletion(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	result, err := r.Run([]Candidate{c})
	if err != nil {
		t.Fatal(err)
	}
	id := result.Sessions[0]
	body := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: fixedNow, Provenance: "synthetic-test", Payload: map[string]any{"name": "SyntheticSkill", "body": "previously allowed synthetic skill body"}}
	if err := r.Store.SaveRequest(id, "synthetic body", fixedNow, body); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := &importMissingMetadataRemote{MemoryStore: storagetest.NewMemoryStore(), drop: true}
	opts := collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession, SkillEvidence: config.SkillEvidenceBody}
	first, err := collector.Run(t.Context(), r.Store, remote, opts)
	if err != nil || !errors.Is(first.Errors[id], storage.ErrNotFound) {
		t.Fatalf("missing uncertain transaction %v %v", first.Errors, err)
	}
	old, found, err := r.Store.LoadPending(id)
	if err != nil || !found || len(old.Bundle.SupplementalEvidence) == 0 {
		t.Fatalf("missing prior body %v", err)
	}
	cfg, _, _ = config.Load(r.Home)
	cfg.SkillEvidence = config.SkillEvidenceMetadata
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceMetadata
	remote.drop = false
	next, err := collector.Run(t.Context(), r.Store, remote, opts)
	if err != nil || len(next.Errors) > 0 || len(next.Published) != 1 {
		t.Fatalf("privacy retry failed %v %v", next.Errors, err)
	}
	published, err := r.Store.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, ok := published.LastPublished()
	if !ok {
		t.Fatal("no verified successor")
	}
	encoded, _ := json.Marshal(bundle)
	if bytes.Contains(encoded, []byte("previously allowed synthetic skill body")) {
		t.Fatal("broader pending body republished")
	}
	if _, found, err := r.Store.LoadPending(id); err != nil || found {
		t.Fatalf("unsettled retry %v", err)
	}
}

func TestStagedAcknowledgementCrashCleanupRequiresVerifiedRemote(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	registered, err := r.Run([]Candidate{c})
	if err != nil {
		t.Fatal(err)
	}
	id := registered.Sessions[0]
	reg, _, _ := r.Store.LoadRegistration(id)
	stage, found, err := r.Store.LoadImportStage(t.Context(), reg)
	if err != nil || !found {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession}
	if result, err := collector.Run(t.Context(), r.Store, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatal(result.Errors, err)
	}
	// Recreate the bytes a crash left after pending removal, before stage cleanup.
	if _, err := r.Store.SaveImportSourceSet(t.Context(), stage.Registration, stage.Bundle, stage.Preserved, stage.SkillEvidence, stage.Children...); err != nil {
		t.Fatal(err)
	}
	pub, err := r.Store.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := pub.LastPublishedSource()
	if !ok {
		t.Fatal("no source")
	}
	saved, err := remote.Get(t.Context(), ref.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(t.Context(), ref.Key); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.SaveRequest(id, "recover cleanup", fixedNow); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Run(t.Context(), r.Store, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Errors[id], storage.ErrNotFound) {
		t.Fatalf("cleanup without remote source %v", result.Errors)
	}
	if _, found, err := r.Store.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatalf("lost unverified stage %v", err)
	}
	if err := remote.Put(t.Context(), ref.Key, saved); err != nil {
		t.Fatal(err)
	}
	result, err = collector.Run(t.Context(), r.Store, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatal(result.Errors, err)
	}
	if _, found, err := r.Store.LoadImportStage(t.Context(), reg); err != nil || found {
		t.Fatalf("verified ack staging leaked %v", err)
	}
}

func TestImportStagingCapacityGroupCanResumeAdmission(t *testing.T) {
	r, c, _ := stagingFixture(t)
	dir := filepath.Join(r.Home, "pending")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "quota-owner.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 29); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.OnCapacity = func(ctx context.Context) error { calls++; return os.Remove(path) }
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 || calls != 1 {
		t.Fatalf("capacity progress not resumed %+v calls %d %v", result, calls, err)
	}
}

func TestImportCancellationBeforeAdmissionReleasesOnlyPreparedEvidence(t *testing.T) {
	r, c, _ := stagingFixture(t)
	stopped := false
	r.Stop = func() bool { return stopped }
	r.AfterStage = func(string) error { stopped = true; return nil }
	result, err := r.Run([]Candidate{c})
	if !errors.Is(err, ErrStopped) || len(result.Sessions) != 0 {
		t.Fatalf("cancel admitted %+v %v", result, err)
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil || len(stages) != 0 {
		t.Fatalf("unreferenced cancellation leaked %v %v", stages, err)
	}
	if err := r.Store.ResumeAllImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	regs, _ := r.Store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("canceled selection recovered as admission")
	}
}

func TestImportCancellationAfterAdmissionKeepsReferencedEvidence(t *testing.T) {
	r, c, _ := stagingFixture(t)
	stopped := false
	r.Stop = func() bool { return stopped }
	r.AfterHold = func(sessions, children []string) error {
		if len(sessions) > 0 {
			stopped = true
		}
		return nil
	}
	result, err := r.Run([]Candidate{c})
	if err != nil && !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("missing admitted group %+v", result)
	}
	reg, _, _ := r.Store.LoadRegistration(result.Sessions[0])
	if _, found, err := r.Store.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatalf("referenced stage lost on cancellation %v", err)
	}
}

func TestJournalledImportCancellationResumesCleanupWithoutAdmission(t *testing.T) {
	r, c, _ := stagingFixture(t)
	child := filepath.Join(c.ProjectRoot, "agent-cleanup-child.jsonl")
	if err := os.WriteFile(child, []byte(subagentTranscript(c.NativeSessionID, "cleanup-child", fixedNow.Add(-30*time.Second))), 0600); err != nil {
		t.Fatal(err)
	}
	c.Subagents = []Subagent{{Path: child, AgentID: "cleanup-child"}}
	r.AfterStage = func(string) error { return errors.New("pre-admission crash") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing stage crash")
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var parent state.ImportStage
	for _, stage := range stages {
		if stage.Registration.ParentSessionID == "" {
			parent = stage
		}
	}
	if len(parent.Children) != 1 {
		t.Fatal("missing durable child ownership")
	}
	path := filepath.Join(r.Home, "sessions", parent.Children[0].ArchiveSessionID, "import-stage", "stage.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("interrupted child cleanup fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.CancelImportStage(t.Context(), parent.Registration); err == nil {
		t.Fatal("damaged child allowed nonjournalled cleanup")
	}
	if _, err := os.Stat(filepath.Join(r.Home, "sessions", parent.Registration.ArchiveSessionID, "import-stage", "cancel.json")); err != nil {
		t.Fatalf("cancellation intent not durable %v", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.ResumeAllImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	regs, _ := r.Store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("canceled preparation admitted during recovery")
	}
	stages, _, err = r.Store.ImportStages(t.Context(), "", 50)
	if err != nil || len(stages) != 0 {
		t.Fatalf("cleanup not completed %v %v", stages, err)
	}
}

func TestStagedSelectionSurvivesHookObservationAndRejectsReplacement(t *testing.T) {
	r, c, _ := stagingFixture(t)
	result, err := r.Run([]Candidate{c})
	if err != nil {
		t.Fatal(err)
	}
	id := result.Sessions[0]
	before, _, _ := r.Store.LoadRegistration(id)
	if err := r.Store.RecordHookObservation(id, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, _, _ := r.Store.LoadRegistration(id)
	if after.StagedImportSHA256 != before.StagedImportSHA256 || after.StagedImportSHA256 == "" {
		t.Fatal("hook lost immutable selected evidence")
	}
	replacement := after
	replacement.StagedImportSHA256 = ""
	if err := r.Store.SaveRegistration(replacement); err == nil {
		t.Fatal("blind replacement cleared stage ownership")
	}
	if _, err := r.Store.UpdateRegistration(id, func(reg *archive.SessionRegistration) error { reg.StagedImportSHA256 = ""; return nil }); err == nil {
		t.Fatal("update cleared stage ownership")
	}
	if _, found, err := r.Store.LoadImportStage(t.Context(), after); err != nil || !found {
		t.Fatalf("mutation discarded durable evidence %v", err)
	}
}

func TestStagedAdmissionRequiresEverySelectedChild(t *testing.T) {
	r, c, _ := stagingFixture(t)
	child := filepath.Join(c.ProjectRoot, "agent-required-child.jsonl")
	if err := os.WriteFile(child, []byte(subagentTranscript(c.NativeSessionID, "required-child", fixedNow.Add(-30*time.Second))), 0600); err != nil {
		t.Fatal(err)
	}
	c.Subagents = []Subagent{{Path: child, AgentID: "required-child"}}
	r.AfterStage = func(string) error { return errors.New("pre-admission crash") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing crash")
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if stage.Registration.ParentSessionID != "" {
			if err := r.Store.RemoveImportStage(stage.Registration.ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := r.Store.ResumeAllImportAdmissions(t.Context()); err == nil {
		t.Fatal("missing selected evidence admitted")
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("incomplete source set admitted %v %v", regs, err)
	}
}

func TestStagedEvidenceRejectsDirectoryAliasForReadAndRemoval(t *testing.T) {
	r, c, _ := stagingFixture(t)
	result, err := r.Run([]Candidate{c})
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := r.Store.LoadRegistration(result.Sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Home, "sessions", reg.ArchiveSessionID, "import-stage")
	preserved := path + "-preserved"
	if err := os.Rename(path, preserved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(preserved, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.LoadImportStage(t.Context(), reg); err == nil {
		t.Fatal("stage reader followed directory alias")
	}
	if err := r.Store.RemoveImportStage(reg.ArchiveSessionID); err == nil {
		t.Fatal("stage cleanup followed directory alias")
	}
	if _, err := os.Stat(filepath.Join(preserved, "stage.json")); err != nil {
		t.Fatalf("aliased evidence removed %v", err)
	}
}

func TestStagedChildPreparationRetrySurvivesNativeDeletion(t *testing.T) {
	r, c, _ := stagingFixture(t)
	childPath := filepath.Join(c.ProjectRoot, "agent-prepared-child.jsonl")
	if err := os.WriteFile(childPath, []byte(subagentTranscript(c.NativeSessionID, "prepared-child", fixedNow.Add(-30*time.Second))), 0600); err != nil {
		t.Fatal(err)
	}
	c.Subagents = []Subagent{{Path: childPath, AgentID: "prepared-child"}}
	parentID, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: c.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	childNative := c.NativeSessionID + ":subagent:prepared-child"
	childID, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: childNative})
	if err != nil {
		t.Fatal(err)
	}
	parent := r.registration(c, parentID, "")
	candidate := state.SubagentCandidate{ArchiveSessionID: childID, NativeSessionID: childNative, ParentArchiveSessionID: parentID, ParentNativeSessionID: c.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: "prepared-child", TranscriptPath: childPath, ObservedAt: fixedNow, Origin: archive.SessionOriginImport}
	reg, bundle, err := collector.ReadImportSubagentBundle(t.Context(), parent, candidate, collector.Options{Sources: testSources})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.SaveImportStage(t.Context(), reg, bundle, config.SkillEvidenceMetadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 || len(result.Subagents) != 1 {
		t.Fatalf("prepared child was reread from vanished native source %+v %v", result, err)
	}
}

func TestImportCancellationUsesReservedCapacityAtQuotaLimit(t *testing.T) {
	r, c, _ := stagingFixture(t)
	r.AfterStage = func(string) error { return errors.New("pre-admission crash") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing crash")
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil || len(stages) != 1 {
		t.Fatalf("missing prepared evidence %v %v", stages, err)
	}
	stage := stages[0]
	info, err := os.Stat(filepath.Join(r.Home, "sessions", stage.Registration.ArchiveSessionID, "import-stage", "stage.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(r.Home, "sessions", stage.Registration.ArchiveSessionID, "pending-sources")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(root, strings.Repeat("a", 64)+".gz"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// The quota double-charges payload bytes and reserves 64 KiB controls.
	if err := errors.Join(file.Truncate(((1<<30)-(64<<10))/2-info.Size()), file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.CancelImportStage(t.Context(), stage.Registration); err != nil {
		t.Fatalf("full data quota consumed reserved cancellation capacity: %v", err)
	}
	remaining, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("cancel did not settle %v %v", remaining, err)
	}
}

func TestImportStagingRejectsChangedNativeIdentity(t *testing.T) {
	r, c, _ := stagingFixture(t)
	// The plan selected this parent, but its path now holds another native session.
	if err := os.WriteFile(c.TranscriptPath, []byte(claudeTranscript("different-parent", c.ProjectRoot, c.StartedAt)), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run([]Candidate{c})
	if err == nil || len(result.Sessions) != 0 {
		t.Fatalf("changed native owner admitted: %+v %v", result, err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("changed source left admission: %+v %v", regs, err)
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil || len(stages) != 0 {
		t.Fatalf("changed source durably selected: %+v %v", stages, err)
	}
}

func TestImportStagingAcceptsSelectedIdentityInCopiedTranscript(t *testing.T) {
	r, c, cfg := stagingFixture(t)
	// Claude resume/copy records may name earlier sessions as long as the
	// selected session remains among the observed ownership identities.
	raw := claudeTranscript("earlier-session", c.ProjectRoot, c.StartedAt) + claudeTranscript(c.NativeSessionID, c.ProjectRoot, c.StartedAt)
	if err := os.WriteFile(c.TranscriptPath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("selected membership refused: %+v %v", result, err)
	}
	if err := os.Remove(c.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	collected, err := collector.Run(t.Context(), r.Store, remote, collector.Options{Sources: testSources, Parsers: testSources, MachineID: cfg.MachineID, Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
	if err != nil || len(collected.Errors) != 0 || len(collected.Published) != 1 {
		t.Fatalf("selected copied evidence not recovered: %+v %v", collected, err)
	}
}

func TestCanceledStagingReadAndCleanupPreserveEvidence(t *testing.T) {
	r, c, _ := stagingFixture(t)
	r.AfterStage = func(string) error { return errors.New("pre-admission crash") }
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("missing preparation boundary")
	}
	stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
	if err != nil || len(stages) != 1 {
		t.Fatalf("missing prepared evidence: %+v %v", stages, err)
	}
	stage := stages[0]
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := r.Store.LoadImportStage(ctx, stage.Registration); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stage read: %v", err)
	}
	if _, _, err := r.Store.ImportStages(ctx, "", 50); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled inventory: %v", err)
	}
	if err := r.Store.DropUnadmittedImportStage(ctx, stage); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup: %v", err)
	}
	if _, found, err := r.Store.LoadImportStage(t.Context(), stage.Registration); err != nil || !found {
		t.Fatalf("canceled cleanup lost evidence: %t %v", found, err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("cancellation admitted evidence: %+v %v", regs, err)
	}
}

func TestImportStagingStaleProofAfterReservationMakesProgress(t *testing.T) {
	for _, changePolicy := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale proof", true: "changed authorization"}[changePolicy], func(t *testing.T) {
			r, c, cfg := stagingFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			r.Context = ctx
			checks := 0
			c.projectResolutionCurrent = func() bool {
				checks++
				if checks == 1 {
					return true // First hold reserves identity with valid proof.
				}
				if changePolicy {
					cfg.Archive.Projects[0].Included = false
					if err := config.Save(r.Home, cfg); err != nil {
						t.Fatal(err)
					}
					return true
				}
				return false
			}
			result, err := r.Run([]Candidate{c})
			if err != nil || result.NotAdmitted != 1 || len(result.Sessions) != 0 {
				t.Fatalf("stale reserved candidate did not finish without admission: %+v %v", result, err)
			}
			stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
			if err != nil || len(stages) != 0 {
				t.Fatalf("stale proof left staged evidence: %+v %v", stages, err)
			}
			regs, err := r.Store.LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("stale proof acquired ownership: %+v %v", regs, err)
			}
		})
	}
}
