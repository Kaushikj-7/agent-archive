package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func seedRecoveryInventory(t *testing.T, s *Store, count int) {
	t.Helper()
	for i := range count {
		key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("native-%04d", i)}
		if err := local.Write(s.registrationPath(fmt.Sprintf("owner-%04d", i)), migrationRegistration(key, fmt.Sprintf("owner-%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

// requestDamagedKey requests recovery for key after damaging its index entry.
// A request for an unowned key during a pending census keeps its generation;
// damage still starts a new one, which these resume tests rely on.
func requestDamagedKey(s *Store, key agentmeta.SessionKey) error {
	path := qualifiedSessionIndexPath(s.home, key)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		return err
	}
	return s.RequestSessionIndexRecovery(key)
}

func TestScheduledRecoveryApplicationAllowanceFollowsValidatedCensus(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 2)
	absent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "slow-census-unowned"}
	if err := s.RequestSessionIndexRecovery(absent); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		censuses := 0
		s.onIndexStep = func(step string) error {
			if step == "recovery-enumerated" {
				censuses++
				// Model complete validation costing more than application's
				// allowance while remaining inside the whole-stage context.
				time.Sleep(2 * time.Second)
			}
			return nil
		}
		complete := false
		for attempts := 0; attempts < 3 && !complete; attempts++ {
			var err error
			complete, err = s.RecoverSessionIndexScheduled(ctx, time.Second)
			if err != nil {
				t.Fatal(err)
			}
		}
		if !complete || censuses != 1 {
			t.Fatalf("validated census consumed application progress: complete=%v censuses=%d", complete, censuses)
		}
		for i := range 2 {
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("native-%04d", i)}
			id, found, err := s.ArchiveSessionID(key)
			if err != nil || !found || id != fmt.Sprintf("owner-%04d", i) {
				t.Fatalf("owner application: %q %v %v", id, found, err)
			}
		}
		if yes, err := s.SessionIndexAbsent(absent); !yes || err != nil {
			t.Fatalf("complete absence proof: %v %v", yes, err)
		}
	})
}

func TestScheduledRecoveryResumesAndDelaysAbsence(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 4)
	absent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "unowned"}
	if err := s.RequestSessionIndexRecovery(absent); err != nil {
		t.Fatal(err)
	}
	applied := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.onIndexStep = func(step string) error {
		if step == "recovery-entry" {
			applied++
			cancel()
		}
		return nil
	}
	complete, err := s.recoverSessionIndexSlice(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) || complete || applied != 1 {
		t.Fatalf("first slice complete=%v applied=%d err=%v", complete, applied, err)
	}
	if yes, err := s.SessionIndexAbsent(absent); yes || err != nil && !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("partial census absence: %v %v", yes, err)
	}
	if _, _, err := s.EnsureArchiveSessionID(absent); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("partial allocation: %v", err)
	}
	s.onIndexStep = nil
	// A fresh Store proves that only the durable cursor carries continuation.
	resumed := OpenReadOnly(s.home)
	resumed.onIndexStep = func(step string) error {
		if step == "recovery-entry" {
			applied++
		}
		return nil
	}
	complete, err = resumed.recoverSessionIndexSlice(context.Background(), time.Hour)
	if err != nil || !complete || applied != 4 {
		t.Fatalf("resume complete=%v applied=%d err=%v", complete, applied, err)
	}
	if yes, err := s.SessionIndexAbsent(absent); !yes || err != nil {
		t.Fatalf("completed absence: %v %v", yes, err)
	}
}

func TestScheduledRecoveryDetectsDuplicateOutsideAppliedChunk(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 4)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-0003"}
	if err := local.Write(s.registrationPath("foreign-owner"), migrationRegistration(key, "foreign-owner")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.onIndexStep = func(step string) error {
		if step == "recovery-entry" {
			cancel()
		}
		return nil
	}
	if complete, err := s.recoverSessionIndexSlice(ctx, time.Hour); !errors.Is(err, context.Canceled) || complete {
		t.Fatalf("first slice: %v %v", complete, err)
	}
	s.onIndexStep = nil
	if complete, err := s.recoverSessionIndexSlice(context.Background(), time.Hour); err != nil || !complete {
		t.Fatalf("resume: %v %v", complete, err)
	}
	if _, _, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("cross chunk duplicate: %v", err)
	}
}

func TestRecoveryMembershipMutationCannotCompleteStaleCensus(t *testing.T) {
	for _, step := range []string{"recovery-enumerated", "recovery-completing"} {
		t.Run(step, func(t *testing.T) {
			s := newTestStore(t)
			seedRecoveryInventory(t, s, 2)
			changed := false
			s.onIndexStep = func(actual string) error {
				if actual == step && !changed {
					changed = true
					key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "concurrent-new"}
					return s.SaveRegistration(migrationRegistration(key, "new-owner"))
				}
				return nil
			}
			complete, err := s.recoverSessionIndexSlice(context.Background(), time.Hour)
			if complete || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("stale certificate: %v %v", complete, err)
			}
			s.onIndexStep = nil
			if complete, err := s.recoverSessionIndexSlice(context.Background(), time.Hour); !complete || err != nil {
				t.Fatalf("fresh census: %v %v", complete, err)
			}
		})
	}
}

type cursorCorruption string

const (
	cursorMalformed cursorCorruption = "malformed"
	cursorOffset    cursorCorruption = "offset"
	cursorRemoved   cursorCorruption = "removal"
)

func TestScheduledRecoveryRestartsCorruptCursorAndRemovedMembership(t *testing.T) {
	for _, kind := range []cursorCorruption{cursorMalformed, cursorOffset, cursorRemoved} {
		t.Run(string(kind), func(t *testing.T) {
			s := newTestStore(t)
			seedRecoveryInventory(t, s, 4)
			applied := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.onIndexStep = func(step string) error {
				if step == "recovery-entry" {
					applied++
					cancel()
				}
				return nil
			}
			if complete, err := s.recoverSessionIndexSlice(ctx, time.Hour); complete || !errors.Is(err, context.Canceled) {
				t.Fatalf("first slice: %v %v", complete, err)
			}
			path := filepath.Join(s.home, sessionRecoveryCursorFile)
			switch kind {
			case cursorMalformed:
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case cursorOffset:
				var cursor sessionRecoveryCursor
				if err := local.Read(path, &cursor); err != nil {
					t.Fatal(err)
				}
				cursor.Offset = 3
				if err := local.Write(path, cursor); err != nil {
					t.Fatal(err)
				}
			case cursorRemoved:
				key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-0003"}
				if err := s.ForgetSession("owner-0003", key); err != nil {
					t.Fatal(err)
				}
			}
			s.onIndexStep = func(step string) error {
				if step == "recovery-entry" {
					applied++
				}
				return nil
			}
			if complete, err := s.recoverSessionIndexSlice(context.Background(), time.Hour); !complete || err != nil {
				t.Fatalf("restart: %v %v", complete, err)
			}
			expected := 5
			if kind == cursorRemoved {
				expected = 4
			}
			if applied != expected {
				t.Fatalf("skipped unchecked membership: %d want %d", applied, expected)
			}
		})
	}
}

func TestContinuationUpdateKeepsMembershipRevision(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "continuation"}
	if err := s.SaveRegistration(migrationRegistration(key, "owner")); err != nil {
		t.Fatal(err)
	}
	before, err := s.sessionMembershipRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRegistration("owner", func(reg *archive.SessionRegistration) error {
		reg.TranscriptPath = "/synthetic/updated.jsonl"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.sessionMembershipRevision()
	if err != nil || before != after {
		t.Fatalf("continuation rotated: %q %q %v", before, after, err)
	}
}

func TestRepeatedPendingRequestKeepsRecoveryCursorGeneration(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 2)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "pending-key"}
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); complete || err != nil {
		t.Fatalf("pending slice: %v %v", complete, err)
	}
	var before, after sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &before); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.RequestSessionIndexRecovery(key); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	if before.Generation != after.Generation {
		t.Fatal("same scanner request invalidated continuation")
	}
	other := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "new-pending-key"}
	if err := requestDamagedKey(s, other); err != nil {
		t.Fatal(err)
	}
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	if before.Generation == after.Generation {
		t.Fatal("new request did not invalidate census")
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); !complete || err != nil {
		t.Fatalf("fresh recovery: %v %v", complete, err)
	}
}

func TestMissingRecordedMembershipRevisionCannotCertifyAbsence(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(strconv.FormatBool(completed), func(t *testing.T) {
			s := newTestStore(t)
			seedRecoveryInventory(t, s, 2)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "missing-owner"}
			if err := s.RequestSessionIndexRecovery(key); err != nil {
				t.Fatal(err)
			}
			budget := time.Nanosecond
			if completed {
				budget = SessionIndexRecoverySlice
			}
			if _, err := s.RecoverSessionIndexScheduled(context.Background(), budget); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(s.home, sessionMembershipFile)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.sessionMembershipRevision(); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("deleted new fence reclassified legacy: %v", err)
			}
			if completed {
				if yes, err := s.SessionIndexAbsent(key); yes || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("deleted fence absence: %v %v", yes, err)
				}
				if _, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("deleted fence allocation: %v", err)
				}
			}
			if !completed {
				if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); complete || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("missing revision certificate: %v %v", complete, err)
				}
			}
		})
	}
}

func TestRegistrationAfterCertifiedAbsencePreservesOwnedIdentity(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "certified-then-admitted"}
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if absent, err := s.SessionIndexAbsent(key); !absent || err != nil {
		t.Fatalf("initial absence: %v %v", absent, err)
	}
	reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := s.SessionIndexAbsent(key); absent || err != nil {
		t.Fatalf("new owner retained absent proof: %v %v", absent, err)
	}
	id, created, err := s.EnsureArchiveSessionID(key)
	if err != nil || created || id != reg.ArchiveSessionID {
		t.Fatalf("stale certificate duplicated identity: %q %v %v", id, created, err)
	}
}

type ownerIndexState string

const (
	ownerCommitted   ownerIndexState = "committed"
	ownerCorrupt     ownerIndexState = "corrupt"
	ownerReservation ownerIndexState = "reservation"
	ownerConflict    ownerIndexState = "conflict"
	ownerRequested   ownerIndexState = "requested"
	ownerAbsent      ownerIndexState = "absent"
)

func TestRecoveryValidatesGoodOwnerWithoutRewritingItsIndex(t *testing.T) {
	for _, kind := range []ownerIndexState{ownerCommitted, ownerCorrupt, ownerReservation, ownerConflict, ownerRequested, ownerAbsent} {
		t.Run(string(kind), func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "existing-owner"}
			if err := s.SaveRegistration(migrationRegistration(key, "owner")); err != nil {
				t.Fatal(err)
			}
			path := qualifiedSessionIndexPath(s.home, key)
			entry := indexEntry(key, "owner")
			switch kind {
			case ownerCommitted:
				// Keep the valid committed control unchanged.
			case ownerCorrupt:
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case ownerReservation:
				entry.Reservation = "unfinished"
				if err := local.Write(path, entry); err != nil {
					t.Fatal(err)
				}
			case ownerConflict:
				entry.ArchiveSessionID = ""
				entry.Conflict = true
				if err := local.Write(path, entry); err != nil {
					t.Fatal(err)
				}
			case ownerRequested:
				entry.ArchiveSessionID = ""
				entry.Recovery = true
				if err := local.Write(path, entry); err != nil {
					t.Fatal(err)
				}
			case ownerAbsent:
				entry.ArchiveSessionID = ""
				entry.Absent = true
				if err := local.Write(path, entry); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			syncs := 0
			s.onWriteSync = func() { syncs++ }
			s.onIndexSync = func() { syncs++ }
			if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
				t.Fatal(err)
			}
			expected := 6
			if kind == ownerCommitted {
				expected = 4
			}
			if syncs != expected {
				t.Fatalf("durable sync boundaries=%d want%d", syncs, expected)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == ownerCommitted && string(before) != string(after) {
				t.Fatal("committed index rewritten")
			}
			actual, found, err := s.readQualifiedIndex(key)
			if err != nil || !found || actual.ArchiveSessionID != "owner" || actual.Reservation != "" || actual.Absent || actual.Recovery || actual.Conflict {
				t.Fatalf("index not repaired/validated: %#v %v %v", actual, found, err)
			}
		})
	}
}

func TestRecoveryOwnerRepairPreservesCompetingReservation(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "reserved-owner"}
	if err := s.SaveRegistration(migrationRegistration(key, "owner")); err != nil {
		t.Fatal(err)
	}
	entry := indexEntry(key, "other-owner")
	entry.Reservation = "pending-owner"
	path := qualifiedSessionIndexPath(s.home, key)
	if err := local.Write(path, entry); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("competing reservation overwritten: %v", err)
	}
	var after qualifiedSessionIndexEntry
	if err := local.Read(path, &after); err != nil || after != entry {
		t.Fatalf("reservation changed: %#v %v", after, err)
	}
	var marker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Complete {
		t.Fatalf("conflict certified: %#v %v", marker, err)
	}
}

func TestRetentionRevisionStagingAndSyncLeaveRequestLockAvailable(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "retained-then-forgotten"}
	if err := s.SaveRegistration(migrationRegistration(key, "owner")); err != nil {
		t.Fatal(err)
	}
	boundaries := 0
	s.onWriteSync = func() {
		unlock, err := local.NamedLock(s.home, requestLockName("owner"))
		if err != nil {
			t.Fatalf("retention revision sync held request lock: %v", err)
		}
		unlock()
		boundaries++
	}
	forgotten, err := s.ForgetIdleSession("owner", key, false, nil)
	if err != nil || !forgotten || boundaries != 2 {
		t.Fatalf("forget=%v boundaries=%d err=%v", forgotten, boundaries, err)
	}
}

func TestUnanticipatedRegistrationCannotBeRemovedWithoutMembershipFence(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "arrived-after-staging"}
	revision, err := s.stageRegistrationRemovalRevision("owner", key)
	if err != nil || revision != nil {
		t.Fatalf("empty snapshot stage: %v %v", revision, err)
	}
	if err := s.SaveRegistration(migrationRegistration(key, "owner")); err != nil {
		t.Fatal(err)
	}
	if err := s.removeRegistrationWithRevisionUsing(s.registrationPath("owner"), revision, os.Remove); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("unfenced removal: %v", err)
	}
	if _, found, err := s.LoadRegistration("owner"); err != nil || !found {
		t.Fatalf("new member lost: %v %v", found, err)
	}
}

func TestCanceledRecoveryCannotCommitFinalCertificate(t *testing.T) {
	for _, mode := range []string{"synchronous", "scheduled"} {
		for _, boundary := range []string{"before-stage", "after-stage"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				s := newTestStore(t)
				key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "cancel-before-certificate"}
				if err := s.RequestSessionIndexRecovery(key); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				completing := false
				s.onIndexStep = func(step string) error {
					if step == "recovery-completing" {
						completing = true
						if boundary == "before-stage" {
							cancel()
						}
					}
					return nil
				}
				s.onWriteSync = func() {
					if completing && boundary == "after-stage" {
						cancel()
					}
				}
				var err error
				if mode == "synchronous" {
					err = recoverytest.Exhaust(ctx, s, SessionIndexRecoverySlice, true)
				} else {
					_, err = s.RecoverSessionIndexScheduled(ctx, SessionIndexRecoverySlice)
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled certificate: %v", err)
				}
				if absent, err := s.SessionIndexAbsent(key); absent || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("canceled absence: %v %v", absent, err)
				}
				if _, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("canceled allocation: %v", err)
				}
				s.onIndexStep, s.onWriteSync = nil, nil
				if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); !complete || err != nil {
					t.Fatalf("fresh certificate: %v %v", complete, err)
				}
			})
		}
	}
}

func TestCertificateDirectorySyncErrorIsReportedWithoutClaimingSuccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary filesystem permissions")
	}
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "certificate-sync-error"}
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	completing, injected, completedStep := false, false, false
	s.onIndexStep = func(step string) error {
		if step == "recovery-completing" {
			completing = true
		}
		if step == "recovery-complete" {
			completedStep = true
		}
		return nil
	}
	s.onWriteSync = func() {
		if !completing || injected {
			return
		}
		var marker sessionIndexMarker
		if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker) == nil && marker.Complete {
			if err := os.Chmod(s.home, 0300); err != nil {
				t.Fatal(err)
			}
			injected = true
		}
	}
	defer func() { _ = os.Chmod(s.home, 0700) }()
	complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
	if restore := os.Chmod(s.home, 0700); restore != nil {
		t.Fatal(restore)
	}
	if !injected || err == nil || complete || completedStep {
		t.Fatalf("sync error fabricated success: injected=%v complete=%v step=%v err=%v", injected, complete, completedStep, err)
	}
	// Rename visibility precedes directory durability in the existing primitive.
	// The observed certificate still represents a fully validated census; the
	// returned IO error means its durability was not promised to this caller.
	var marker sessionIndexMarker
	if readErr := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); readErr != nil || !marker.Complete {
		t.Fatalf("certificate visibility: %#v %v", marker, readErr)
	}
}

func TestRecoveryMembershipFenceStaysInsideExplicitHome(t *testing.T) {
	// A custom archive home may itself be named registrations. Only its actual
	// registrations child is census membership, never every file in that home.
	s, err := Open(filepath.Join(t.TempDir(), "registrations"))
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "explicit-home-owner"}
	if err := s.SaveRegistration(migrationRegistration(key, "owner")); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.home), sessionMembershipFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fence escaped explicit home: %v", err)
	}
	if revision, err := s.sessionMembershipRevision(); err != nil || revision == "" {
		t.Fatalf("home fence missing: %q %v", revision, err)
	}
}

type membershipEvidence string

const (
	membershipValid   membershipEvidence = "valid"
	membershipMissing membershipEvidence = "missing"
	membershipCorrupt membershipEvidence = "corrupt"
)

func TestCompletedScheduledRecoveryRequiresRecordedMembership(t *testing.T) {
	for _, evidence := range []membershipEvidence{membershipValid, membershipMissing, membershipCorrupt} {
		t.Run(string(evidence), func(t *testing.T) {
			s := newTestStore(t)
			seedRecoveryInventory(t, s, 2)
			if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
				t.Fatalf("initial completion: %v %v", complete, err)
			}
			path := filepath.Join(s.home, sessionMembershipFile)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch evidence {
			case membershipValid:
				// Keep the valid evidence control unchanged.
			case membershipMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case membershipCorrupt:
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
			if evidence == membershipValid {
				if err != nil || !complete {
					t.Fatalf("valid completion: %v %v", complete, err)
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatalf("valid fence rewritten: %v", err)
				}
			} else if complete || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("damaged evidence certified: %v %v", complete, err)
			}
		})
	}
}

// Cancellation is ordinary pending only when its durable checkpoint succeeds.
func TestCanceledRecoveryRetainsCheckpointFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary filesystem permissions")
	}
	for _, writable := range []bool{true, false} {
		t.Run(strconv.FormatBool(writable), func(t *testing.T) {
			s := newTestStore(t)
			seedRecoveryInventory(t, s, 2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.onIndexStep = func(step string) error {
				if step == "recovery-entry" {
					cancel()
					if !writable {
						return os.Chmod(s.home, 0500)
					}
				}
				return nil
			}
			defer func() { _ = os.Chmod(s.home, 0700) }()
			complete, err := s.RecoverSessionIndexScheduled(ctx, SessionIndexRecoverySlice)
			if restoreErr := os.Chmod(s.home, 0700); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if complete || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled recovery: %v %v", complete, err)
			}
			if writable {
				if !SessionIndexRecoveryInterrupted(err) || errors.Unwrap(err) != nil {
					t.Fatalf("ordinary cancellation wrapped: %v", err)
				}
			} else if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("checkpoint IO failure lost: %v", err)
			}
			var marker sessionIndexMarker
			if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Complete {
				t.Fatalf("canceled recovery certified: %#v %v", marker, err)
			}
		})
	}
}

func TestRecoveryInterruptionKeepsJoinedFilesystemFailure(t *testing.T) {
	t.Parallel()
	_, ioErr := os.Open(filepath.Join(t.TempDir(), "missing-checkpoint"))
	if ioErr == nil {
		t.Fatal("filesystem failure control succeeded")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		wrapped := fmt.Errorf("recovery interrupted: %w", cause)
		if !SessionIndexRecoveryInterrupted(wrapped) || !SessionIndexRecoveryInterrupted(errors.Join(wrapped, cause)) {
			t.Fatal("pure wrapped interruption reported as IO failure")
		}
		mixed := fmt.Errorf("checkpoint: %w", errors.Join(wrapped, ioErr))
		if SessionIndexRecoveryInterrupted(mixed) || !errors.Is(mixed, os.ErrNotExist) {
			t.Fatal("real joined filesystem failure suppressed")
		}
	}
	if SessionIndexRecoveryInterrupted(nil) {
		t.Fatal("nil error classified as interruption")
	}
}

// A real recovery refusal joined with expiry is not an ordinary interruption.
func TestRecoveryInterruptionKeepsDurableFailure(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := errors.Join(ErrDurableStorageRecovery, cause)
		if SessionIndexRecoveryInterrupted(err) || !errors.Is(err, ErrDurableStorageRecovery) || !errors.Is(err, cause) {
			t.Fatalf("joined durable refusal suppressed: %v", err)
		}
	}
}

// A positive application allowance does not turn a real expired-context
// durable-root refusal into a harmless scheduling interruption.
func TestScheduledRecoveryPreservesDurableDeadline(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	complete, err := store.RecoverSessionIndexScheduled(ctx, SessionIndexRecoverySlice)
	if complete || !errors.Is(err, ErrDurableStorageRecovery) || !errors.Is(err, context.DeadlineExceeded) || SessionIndexRecoveryInterrupted(err) {
		t.Fatalf("scheduled recovery lost joined refusal: complete=%t err=%v", complete, err)
	}
}

func TestRecoveryCensusDoesNotInheritOmittedRegistrationFields(t *testing.T) {
	s := newTestStore(t)
	first := migrationRegistration(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "first"}, "a")
	if err := local.Write(s.registrationPath("a"), first); err != nil {
		t.Fatal(err)
	}
	// This later registration omits the start time. Reusing a decode target
	// must not borrow the earlier owner's validated start time.
	data := []byte(`{"archive_session_id":"b","native_session_id":"second","project_id":"p","project_root":"/synthetic","harness":{"name":"codex"},"transcript_path":"/synthetic/source.jsonl"}`)
	if err := os.WriteFile(s.registrationPath("b"), data, 0600); err != nil {
		t.Fatal(err)
	}
	inventory, err := s.sessionRegistrationInventory(t.Context())
	if !errors.Is(err, ErrSessionIndexRecoveryRequired) || inventory != nil {
		t.Fatalf("incomplete registration inherited authority: inventory=%v err=%v", inventory, err)
	}
}

// An applied absence remains uncertified until the complete census finishes.
// Reobserving it must retain the request without restarting all application.
func TestRepeatedUncertifiedAbsentRequestRetainsRecoveryProgress(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 3)
	if err := recoverytest.Exhaust(t.Context(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	// Choose a key before the existing owners in phase two, so there is
	// remaining application work after its absence is durably written.
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("owner indexes: %v %v", entries, err)
	}
	var key agentmeta.SessionKey
	for i := range 10000 {
		candidate := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("reobserved-%d", i)}
		if filepath.Base(qualifiedSessionIndexPath(s.home, candidate)) < entries[0].Name() {
			key = candidate
			break
		}
	}
	if key.NativeID == "" {
		t.Fatal("no early key found")
	}
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		delayed := false
		s.onWriteSync = func() {
			entry, found, err := s.readQualifiedIndex(key)
			if !delayed && err == nil && found && entry.Absent {
				delayed = true
				time.Sleep(time.Second)
			}
		}
		complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Second)
		if complete || err != nil || !delayed {
			t.Fatalf("partial absence: complete=%v delayed=%v err=%v", complete, delayed, err)
		}
		s.onWriteSync = nil
		var before sessionRecoveryCursor
		if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &before); err != nil || before.Phase != 2 || before.Offset != 1 {
			t.Fatalf("partial phase-two cursor: %#v %v", before, err)
		}
		if _, _, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
			t.Fatalf("uncertified miss gained authority: %v", err)
		}
		for range 3 {
			if err := s.RequestSessionIndexRecovery(key); err != nil {
				t.Fatal(err)
			}
		}
		var marker sessionIndexMarker
		if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Generation != before.Generation || marker.Complete {
			t.Fatalf("same uncertified key restarted recovery: %#v %v", marker, err)
		}
		if absent, err := s.SessionIndexAbsent(key); absent || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
			t.Fatalf("coalesced miss gained authority: %v %v", absent, err)
		}
		if complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Second); !complete || err != nil {
			t.Fatalf("finite reobservations stranded recovery: %v %v", complete, err)
		}
		if absent, err := s.SessionIndexAbsent(key); !absent || err != nil {
			t.Fatalf("completed absence: %v %v", absent, err)
		}
	})
}

// New keys must rotate the final-certificate generation while an equivalent
// fresh complete inventory preserves already applied owner progress.
func TestNewRecoveryRequestRetainsValidatedOwnerProgress(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(strconv.FormatBool(mutate), func(t *testing.T) {
			// These assertions exercise durable progress, not disk-sync speed.
			synctest.Test(t, func(t *testing.T) {
				s := newTestStore(t)
				seedRecoveryInventory(t, s, 3)
				first := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "first-request"}
				if err := s.RequestSessionIndexRecovery(first); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				applied := 0
				s.onIndexStep = func(step string) error {
					if step == "recovery-entry" {
						applied++
						cancel()
					}
					return nil
				}
				if complete, err := s.RecoverSessionIndexScheduled(ctx, time.Second); complete || !errors.Is(err, context.Canceled) || applied != 1 {
					t.Fatalf("first owner checkpoint: %v %v applied=%d", complete, err, applied)
				}
				cancel()
				s.onIndexStep = nil
				var before sessionRecoveryCursor
				if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &before); err != nil || before.Phase != 0 || before.Offset != 1 {
					t.Fatalf("owner checkpoint: %#v %v", before, err)
				}
				second := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "second-request"}
				if err := requestDamagedKey(s, second); err != nil {
					t.Fatal(err)
				}
				var marker sessionIndexMarker
				if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Generation == before.Generation || marker.Complete {
					t.Fatalf("distinct request lost generation fence: %#v %v", marker, err)
				}
				if mutate {
					key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "new-member"}
					if err := s.SaveRegistration(migrationRegistration(key, "new-owner")); err != nil {
						t.Fatal(err)
					}
				}
				s.onIndexStep = func(step string) error {
					if step == "recovery-entry" {
						applied++
					}
					return nil
				}
				if complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Second); !complete || err != nil {
					t.Fatalf("request recovery: %v %v", complete, err)
				}
				want := 3
				if mutate {
					want = 5 // New membership requires all four owners again.
				}
				if applied != want {
					t.Fatalf("owner applications=%d want%d; equivalent inventory must resume, changed membership must restart", applied, want)
				}
				for _, key := range []agentmeta.SessionKey{first, second} {
					if absent, err := s.SessionIndexAbsent(key); !absent || err != nil {
						t.Fatalf("new generation complete absence: %v %v", absent, err)
					}
				}
			})
		})
	}
}

func TestNewRecoveryRequestRepairsPreviouslyAppliedOwner(t *testing.T) {
	for _, phase := range []int{0, 2} {
		t.Run(strconv.Itoa(phase), func(t *testing.T) {
			// These assertions exercise durable progress, not disk-sync speed.
			synctest.Test(t, func(t *testing.T) {
				s := newTestStore(t)
				seedRecoveryInventory(t, s, 3)
				key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-0000"}
				ctx, cancel := context.WithCancel(t.Context())
				applied, stopAfter := 0, 1
				if phase == 2 {
					stopAfter = 3
				}
				s.onIndexStep = func(step string) error {
					if step == "recovery-entry" {
						applied++
						if applied == stopAfter {
							cancel()
						}
					}
					return nil
				}
				if complete, err := s.RecoverSessionIndexScheduled(ctx, time.Second); complete || !errors.Is(err, context.Canceled) {
					t.Fatalf("owner checkpoint: %v %v", complete, err)
				}
				cancel()
				s.onIndexStep = nil
				var cursor sessionRecoveryCursor
				if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor); err != nil || cursor.Phase != phase || applied != stopAfter {
					t.Fatalf("actual phase checkpoint: %#v applied=%d err=%v", cursor, applied, err)
				}
				if err := os.WriteFile(qualifiedSessionIndexPath(s.home, key), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.RequestSessionIndexRecovery(key); err != nil {
					t.Fatal(err)
				}
				// A new Store must recover from durable evidence, retaining the owner ID.
				resumed := OpenReadOnly(s.home)
				for attempt := range 2 {
					complete, err := resumed.RecoverSessionIndexScheduled(t.Context(), time.Second)
					if err != nil || !complete {
						t.Fatalf("requested owner recovery attempt %d: %v %v", attempt, complete, err)
					}
				}
				id, found, err := resumed.ArchiveSessionID(key)
				if err != nil || !found || id != "owner-0000" {
					t.Fatalf("owner identity: %q %v %v", id, found, err)
				}
				if absent, err := resumed.SessionIndexAbsent(key); absent || err != nil {
					t.Fatalf("owned key certified absent: %v %v", absent, err)
				}
			})
		})
	}
}

func TestNewRecoveryRequestRepairsPreviouslyAppliedCandidate(t *testing.T) {
	for _, phase := range []int{1, 2} {
		t.Run(strconv.Itoa(phase), func(t *testing.T) {
			s := newTestStore(t)
			parent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "parent"}
			if err := s.SaveRegistration(migrationRegistration(parent, "parent-owner")); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				candidate := SubagentCandidate{ArchiveSessionID: fmt.Sprintf("child-owner-%d", i), NativeSessionID: fmt.Sprintf("child-native-%d", i), ParentArchiveSessionID: "parent-owner", ParentNativeSessionID: "parent", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, AgentID: fmt.Sprintf("child-%d", i), TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
				if err := s.SaveSubagentCandidate(candidate); err != nil {
					t.Fatal(err)
				}
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "child-native-0"}
			cancelKey := key
			if phase == 2 {
				cancelKey.NativeID = "child-native-1"
			}
			ctx, cancel := context.WithCancel(t.Context())
			s.onIndexSync = func() {
				entry, found, err := s.readQualifiedIndex(cancelKey)
				if err == nil && found && entry.Reservation != "" {
					cancel()
				}
			}
			if complete, err := s.RecoverSessionIndexScheduled(ctx, time.Second); complete || !errors.Is(err, context.Canceled) {
				t.Fatalf("candidate checkpoint: %v %v", complete, err)
			}
			cancel()
			s.onIndexSync = nil
			var cursor sessionRecoveryCursor
			if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor); err != nil || cursor.Phase != phase || phase == 1 && cursor.Offset != 1 {
				t.Fatalf("actual candidate checkpoint: %#v %v", cursor, err)
			}
			if err := os.WriteFile(qualifiedSessionIndexPath(s.home, key), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.RequestSessionIndexRecovery(key); err != nil {
				t.Fatal(err)
			}
			resumed := OpenReadOnly(s.home)
			if complete, err := resumed.RecoverSessionIndexScheduled(t.Context(), time.Second); !complete || err != nil {
				t.Fatalf("candidate recovery: %v %v", complete, err)
			}
			entry, found, err := resumed.readQualifiedIndex(key)
			if err != nil || !found || entry.ArchiveSessionID != "child-owner-0" || entry.Reservation == "" || entry.Absent {
				t.Fatalf("candidate identity lost: %#v %v %v", entry, found, err)
			}
			if absent, err := resumed.SessionIndexAbsent(key); absent || err != nil {
				t.Fatalf("candidate certified absent: %v %v", absent, err)
			}
		})
	}
}

func TestNewRecoveryRequestRetainsOnlyMatchingCandidateProgress(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(strconv.FormatBool(mutate), func(t *testing.T) {
			s := newTestStore(t)
			parent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "parent"}
			if err := s.SaveRegistration(migrationRegistration(parent, "parent-owner")); err != nil {
				t.Fatal(err)
			}
			var candidates []SubagentCandidate
			for i := range 2 {
				candidate := SubagentCandidate{ArchiveSessionID: fmt.Sprintf("child-owner-%d", i), NativeSessionID: fmt.Sprintf("child-native-%d", i), ParentArchiveSessionID: "parent-owner", ParentNativeSessionID: "parent", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, AgentID: fmt.Sprintf("child-%d", i), TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
				if err := s.SaveSubagentCandidate(candidate); err != nil {
					t.Fatal(err)
				}
				candidates = append(candidates, candidate)
			}
			first := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: candidates[0].NativeSessionID}
			ctx, cancel := context.WithCancel(t.Context())
			s.onIndexSync = func() {
				entry, found, err := s.readQualifiedIndex(first)
				if err == nil && found && entry.Reservation != "" {
					cancel()
				}
			}
			if complete, err := s.RecoverSessionIndexScheduled(ctx, time.Second); complete || !errors.Is(err, context.Canceled) {
				t.Fatalf("candidate checkpoint: %v %v", complete, err)
			}
			cancel()
			s.onIndexSync = nil
			var before sessionRecoveryCursor
			if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &before); err != nil || before.Phase != 1 || before.Offset != 1 {
				t.Fatalf("candidate checkpoint: %#v %v", before, err)
			}
			request := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "new-request"}
			if err := requestDamagedKey(s, request); err != nil {
				t.Fatal(err)
			}
			if mutate {
				candidates[0].ObservedAt = candidates[0].ObservedAt.Add(time.Second)
				if err := s.SaveSubagentCandidate(candidates[0]); err != nil {
					t.Fatal(err)
				}
			}
			// No applications fit, but the fresh census and complete candidate
			// fingerprint must choose the correct resume point before checkpoint.
			if complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Nanosecond); complete || err != nil {
				t.Fatalf("new-generation checkpoint: %v %v", complete, err)
			}
			var after sessionRecoveryCursor
			if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &after); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mutate {
				want = 0
			}
			if after.Phase != 1 || after.Offset != want || after.Generation == before.Generation {
				t.Fatalf("candidate progress ignored equivalence or generation: %#v want offset%d", after, want)
			}
			if complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Second); !complete || err != nil {
				t.Fatalf("candidate convergence: %v %v", complete, err)
			}
			for _, candidate := range candidates {
				key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: candidate.NativeSessionID}
				entry, found, err := s.readQualifiedIndex(key)
				if err != nil || !found || entry.ArchiveSessionID != candidate.ArchiveSessionID || entry.Reservation == "" {
					t.Fatalf("candidate ownership lost: %#v %v %v", entry, found, err)
				}
			}
			if absent, err := s.SessionIndexAbsent(request); !absent || err != nil {
				t.Fatalf("new request omitted: %v %v", absent, err)
			}
		})
	}
}

func TestNewRecoveryGenerationRestartsRequestedMissCoverage(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 2)
	if err := recoverytest.Exhaust(t.Context(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("owner indexes: %v %v", entries, err)
	}
	keyBefore := func(name, prefix string) agentmeta.SessionKey {
		for i := range 10000 {
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("%s-%d", prefix, i)}
			if filepath.Base(qualifiedSessionIndexPath(s.home, key)) < name {
				return key
			}
		}
		t.Fatal("no earlier key found")
		return agentmeta.SessionKey{}
	}
	first := keyBefore(entries[0].Name(), "first-miss")
	second := keyBefore(filepath.Base(qualifiedSessionIndexPath(s.home, first)), "second-miss")
	if err := s.RequestSessionIndexRecovery(first); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		delayed := false
		s.onWriteSync = func() {
			entry, found, err := s.readQualifiedIndex(first)
			if !delayed && err == nil && found && entry.Absent {
				delayed = true
				time.Sleep(time.Second)
			}
		}
		if complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Second); complete || err != nil || !delayed {
			t.Fatalf("requested-miss checkpoint: %v %v delayed=%v", complete, err, delayed)
		}
		s.onWriteSync = nil
		var before sessionRecoveryCursor
		if err := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &before); err != nil || before.Phase != 2 || before.Offset != 1 {
			t.Fatalf("requested-miss cursor: %#v %v", before, err)
		}
		if err := requestDamagedKey(s, second); err != nil {
			t.Fatal(err)
		}
		resumed, complete, err := s.prepareRecoveryCursor(t.Context())
		if complete || err != nil || resumed.Phase != 2 || resumed.Offset != 0 || resumed.PhaseInventory != "" || resumed.Generation == before.Generation {
			t.Fatalf("new misses could be skipped: %#v %v %v", resumed, complete, err)
		}
		if complete, err := s.RecoverSessionIndexScheduled(t.Context(), time.Second); !complete || err != nil {
			t.Fatalf("new miss convergence: %v %v", complete, err)
		}
		for _, key := range []agentmeta.SessionKey{first, second} {
			if absent, err := s.SessionIndexAbsent(key); !absent || err != nil {
				t.Fatalf("new generation omitted key: %v %v", absent, err)
			}
		}
	})
}
