package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func generationFixture(t *testing.T) (*Store, archive.SessionRegistration, time.Time) {
	t.Helper()
	s := newTestStore(t)
	reg := registration(t)
	reg.DestinationID = "original-destination"
	reg.AdmittedAt = reg.RegisteredAt
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("original-import")
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := reg.RegisteredAt.Add(time.Hour)
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "test", CapturedAt: reg.RegisteredAt}}
	p, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, err := archive.SourceObjectKey(bundle, source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: bundle.Capture.CapturedAt, SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: source.SHA256, CompressedBytes: len(source.Bytes)}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(bundle, reg.RegisteredAt, CacheStatusPublished, raw); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveBlocked(bundle, reg.RegisteredAt, BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	return s, reg, at
}

func generationBuilder(at time.Time) func(archive.SessionRegistration, string) (archive.SessionRegistration, PendingPublication, error) {
	return func(reg archive.SessionRegistration, id string) (archive.SessionRegistration, PendingPublication, error) {
		prev := reg.ArchiveSessionID
		reg.ArchiveSessionID = id
		reg.PreviousGenerationID = prev
		bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: id, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, PreviousGenerationID: prev, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "test", CapturedAt: at}}
		source, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			return reg, PendingPublication{}, err
		}
		sourceKey, err := archive.SourceObjectKey(bundle, source.SHA256)
		if err != nil {
			return reg, PendingPublication{}, err
		}
		metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, id)
		if err != nil {
			return reg, PendingPublication{}, err
		}
		pending := PendingPublication{Bundle: bundle, ReadyAt: at, SourceKey: sourceKey, MetadataKey: metadataKey, SourceSHA256: source.SHA256, SourceBytes: source.Bytes}
		metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, PreviousGenerationID: prev, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: at, SourceBundle: pending.SourceReference()}
		pending.MetadataBytes, err = json.Marshal(metadata)
		return reg, pending, err
	}
}

func TestGenerationRecoveryCrashMatrix(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"generation-journal", "generation-fenced", "generation-nodes", "generation-frozen", "generation-registration", "generation-pending", "generation-request", "generation-index", "generation-active", "generation-complete"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			s, reg, at := generationFixture(t)
			interrupted := errors.New("interrupted")
			s.onIndexStep = func(current string) error {
				if current == step {
					return interrupted
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
				t.Fatalf("did not interrupt %s: %v", step, err)
			}
			next, found, err := s.GenerationSuccessor(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatalf("receipt missing: %s %v", next, err)
			}
			// A hook can arrive after the interrupted process releases hooks.lock,
			// before a collector resumes. Neither old nor new routing is safe
			// until the fixed registration/request journal is complete.
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
			if step != "generation-complete" {
				if active, found, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) || found {
					t.Fatalf("interrupted transition allowed hook lookup: %s %v %v", active, found, err)
				}
				if active, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("interrupted transition allowed start reservation: %s %v", active, err)
				}
			}
			if step == "generation-journal" {
				if err := local.Write(nativeSessionIndexPath(s.home, key.NativeID), sessionIndexEntry{ArchiveSessionID: reg.ArchiveSessionID}); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(qualifiedSessionIndexPath(s.home, key)); err != nil {
					t.Fatal(err)
				}
				if _, found, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) || found {
					t.Fatalf("legacy index bypassed recovery journal: %v", err)
				}
			}
			s.onIndexStep = nil
			if err := s.ResumeGenerationRecoveries(context.Background()); err != nil {
				t.Fatal(err)
			}
			again, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at.Add(time.Hour), generationBuilder(at.Add(time.Hour)))
			if err != nil || again != next {
				t.Fatalf("duplicate recovery: %s %v", again, err)
			}
			old, _, err := s.LoadRegistration(reg.ArchiveSessionID)
			if err != nil || !old.CaptureFrozen {
				t.Fatalf("old capture not frozen: %#v %v", old, err)
			}
			successor, _, err := s.LoadRegistration(next)
			if err != nil {
				t.Fatal(err)
			}
			preserved := successor
			preserved.ArchiveSessionID = reg.ArchiveSessionID
			preserved.PreviousGenerationID = ""
			if !reflect.DeepEqual(preserved, reg) {
				t.Fatalf("provenance altered: %#v %#v", reg, preserved)
			}
			pending, found, err := s.LoadPending(next)
			if err != nil || !found || !pending.Bundle.Capture.CapturedAt.Equal(at) {
				t.Fatalf("pending time changed: %#v %v", pending, err)
			}
			active, found, err := s.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID})
			if err != nil || !found || active != next {
				t.Fatalf("route: %s %v %v", active, found, err)
			}
			regs, err := s.LoadRegistrations()
			if err != nil || len(regs) != 2 {
				t.Fatalf("registrations=%d %v", len(regs), err)
			}
		})
	}
}

func TestGenerationIndexLossKeepsUniqueActiveTip(t *testing.T) {
	t.Parallel()
	s, reg, at := generationFixture(t)
	next, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at))
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
	if err := os.Remove(qualifiedSessionIndexPath(s.home, key)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
	}
	active, found, err := s.ArchiveSessionID(key)
	if err != nil || !found || active != next {
		t.Fatalf("lost index restored wrong generation %s %v", active, err)
	}
	// An unrelated duplicate cannot use the valid lineage to gain authority.
	intruder := reg
	intruder.ArchiveSessionID = "unrelated"
	intruder.CaptureFrozen = true
	if err := local.Write(s.registrationPath(intruder.ArchiveSessionID), intruder); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverRegistrationOwners(key, []string{reg.ArchiveSessionID, next, intruder.ArchiveSessionID}); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func TestGenerationRecoveryReplayRefusesChangedAdmission(t *testing.T) {
	t.Parallel()
	s, original, at := generationFixture(t)
	interrupted := errors.New("interrupted before fence")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(original.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	s.onIndexStep = nil
	journal, found, err := readJSON[generationRecovery](s.generationRecoveryPath(original.ArchiveSessionID))
	if err != nil || !found {
		t.Fatal(err)
	}
	journal.Registration.NativeSessionID = "unrelated-native"
	if err := local.Write(s.generationRecoveryPath(original.ArchiveSessionID), journal); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("replayed changed admission: %v", err)
	}
	old, _, err := s.LoadRegistration(original.ArchiveSessionID)
	if err != nil || old.CaptureFrozen {
		t.Fatalf("corrupt replay froze original: %#v %v", old, err)
	}
	if _, found, err := s.LoadRegistration(journal.Next); err != nil || found {
		t.Fatalf("corrupt replay registered successor: %v", err)
	}
}

func TestGenerationRecoveryReplayRefusesChangedPublication(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*PendingPublication){
		"metadata key":    func(p *PendingPublication) { p.MetadataKey = "sessions/codex/previous/metadata.json" },
		"source key":      func(p *PendingPublication) { p.SourceKey = "sessions/codex/previous/source.jsonl.gz" },
		"source bytes":    func(p *PendingPublication) { p.SourceBytes = []byte("corrupt") },
		"metadata bytes":  func(p *PendingPublication) { p.MetadataBytes = []byte(`{}`) },
		"bundle evidence": func(p *PendingPublication) { p.Bundle.NativeRecords = []map[string]any{{"changed": true}} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, original, at := generationFixture(t)
			interrupted := errors.New("before freeze")
			s.onIndexStep = func(step string) error {
				if step == "generation-journal" {
					return interrupted
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(original.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			s.onIndexStep = nil
			journal, _, err := readJSON[generationRecovery](s.generationRecoveryPath(original.ArchiveSessionID))
			if err != nil {
				t.Fatal(err)
			}
			mutate(journal.Pending)
			if err := local.Write(s.generationRecoveryPath(original.ArchiveSessionID), journal); err != nil {
				t.Fatal(err)
			}
			if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("changed publication replayed: %v", err)
			}
			old, _, err := s.LoadRegistration(original.ArchiveSessionID)
			if err != nil || old.CaptureFrozen {
				t.Fatalf("invalid publication froze predecessor: %v", err)
			}
			if _, found, err := s.LoadRegistration(journal.Next); err != nil || found {
				t.Fatalf("invalid publication registered successor: %v", err)
			}
		})
	}
}

func TestGenerationExpiryNeverRoutesToRetainedAncestor(t *testing.T) {
	t.Parallel()
	s, reg, at := generationFixture(t)
	next, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at))
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
	if _, err := s.CompleteRequest(next, func() string { r, _, _ := s.LoadRequest(next); return r.Token }()); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePending(next); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := s.ForgetIdleSession(next, key, false, nil); err != nil || !forgotten {
		t.Fatalf("retire: %v %v", forgotten, err)
	}
	if err := s.recoverRegistrationOwners(key, []string{reg.ArchiveSessionID}); err != nil {
		t.Fatal(err)
	}
	if active, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("revived expired/frozen identity %s %v", active, err)
	}
	fresh, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration {
		r := reg
		r.ArchiveSessionID = id
		r.PreviousGenerationID = ""
		r.CaptureFrozen = false
		return r
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ArchiveSessionID == next || fresh.ArchiveSessionID == reg.ArchiveSessionID {
		t.Fatal("fresh start reused history")
	}
	if err := s.recoverRegistrationOwners(key, []string{reg.ArchiveSessionID, fresh.ArchiveSessionID}); err != nil {
		t.Fatal(err)
	}
	if repeated, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); err != nil || repeated != next {
		t.Fatalf("expired receipt created another successor: %s %v", repeated, err)
	}
}

func TestGenerationRetirementCrashAndEligibleRootReservation(t *testing.T) {
	t.Parallel()
	s, old, at := generationFixture(t)
	next, err := s.BeginGenerationRecovery(old.ArchiveSessionID, at, generationBuilder(at))
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: old.NativeSessionID}
	interrupted := errors.New("retirement interrupted")
	s.onIndexStep = func(step string) error {
		if step == "generation-retired" {
			return interrupted
		}
		return nil
	}
	if err := s.ForgetSession(next, key); !errors.Is(err, interrupted) {
		t.Fatalf("retirement seam: %v", err)
	}
	reg, found, err := s.LoadRegistration(next)
	if err != nil || !found {
		t.Fatalf("registration unexpectedly removed: %v", err)
	}
	if err := s.GenerationCaptureAllowed(reg); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("retired capture admitted: %v", err)
	}
	s.onIndexStep = nil
	if err := s.ForgetSession(next, key); err != nil {
		t.Fatal(err)
	}
	// The normal admission path commits its qualified reservation before the
	// root's immutable node/head. A crash must keep this exact ID, not admit twice.
	s.onIndexStep = func(step string) error {
		if step == "reservation" {
			return interrupted
		}
		return nil
	}
	if _, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, interrupted) {
		t.Fatalf("reservation seam: %v", err)
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil || !found || entry.Reservation == "" {
		t.Fatalf("reservation missing: %#v %v", entry, err)
	}
	s.onIndexStep = nil
	if err := s.recoverRegistrationOwners(key, []string{old.ArchiveSessionID}); err != nil {
		t.Fatal(err)
	}
	id, created, err := s.EnsureArchiveSessionID(key)
	if err != nil || created || id != entry.ArchiveSessionID {
		t.Fatalf("reservation replaced: %s %v %v", id, created, err)
	}
}

func TestFrozenGenerationRefusesUnsupportedNodeVersion(t *testing.T) {
	t.Parallel()
	for _, version := range []int{0, 99} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			t.Parallel()
			s, original, at := generationFixture(t)
			if _, err := s.BeginGenerationRecovery(original.ArchiveSessionID, at, generationBuilder(at)); err != nil {
				t.Fatal(err)
			}
			frozen, _, err := s.LoadRegistration(original.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			node, found, err := readJSON[generationNode](s.generationNodePath(original.ArchiveSessionID))
			if err != nil || !found {
				t.Fatalf("generation node: %v %v", found, err)
			}
			node.Version = version
			if err := local.Write(s.generationNodePath(original.ArchiveSessionID), node); err != nil {
				t.Fatal(err)
			}
			if err := s.FrozenGeneration(frozen); !errors.Is(err, ErrSessionIdentityConflict) {
				t.Fatalf("unsupported node accepted as frozen authority: %v", err)
			}
		})
	}
}
