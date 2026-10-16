package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func migrationRegistration(key agentmeta.SessionKey, id string) archive.SessionRegistration {
	return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: key.NativeID, ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: string(key.Agent)}, TranscriptPath: "/synthetic/t.jsonl", SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func legacyRegistration(t *testing.T, s *Store, key agentmeta.SessionKey, id string) {
	t.Helper()
	reg := migrationRegistration(key, id)
	if err := local.Write(s.registrationPath(id), reg); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(nativeSessionIndexPath(s.home, key.NativeID), sessionIndexEntry{ArchiveSessionID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestQualifiedLegacyAdoptionAndCrossAgentCollision(t *testing.T) {
	s := newTestStore(t)
	claude := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: " ABC /\x00界 "}
	legacyRegistration(t, s, claude, "historical")
	reg, _, _ := s.LoadRegistration("historical")
	reg.Harness.Name = "CLAUDE-CODE"
	if err := local.Write(s.registrationPath("historical"), reg); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []agentmeta.ID{agentmeta.Codex, agentmeta.Cursor} {
		key := agentmeta.SessionKey{Agent: agent, NativeID: claude.NativeID}
		if id, found, err := s.ArchiveSessionID(key); err != nil || found || id != "" {
			t.Fatalf("wrong-agent adopted %q %t %v", id, found, err)
		}
		created, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
		if err != nil || created.ArchiveSessionID == "historical" {
			t.Fatalf("collision %#v %v", created, err)
		}
	}
	id, created, err := s.EnsureArchiveSessionID(claude)
	if err != nil || created || id != "historical" {
		t.Fatalf("adoption %q %t %v", id, created, err)
	}
	reload := OpenReadOnly(s.home)
	if id, found, err := reload.ArchiveSessionID(claude); err != nil || !found || id != "historical" {
		t.Fatalf("reload %q %t %v", id, found, err)
	}
	var legacy sessionIndexEntry
	if err := local.Read(nativeSessionIndexPath(s.home, claude.NativeID), &legacy); err != nil || legacy.ArchiveSessionID != "historical" {
		t.Fatalf("legacy changed %#v %v", legacy, err)
	}
	if reg, _, _ := s.LoadRegistration("historical"); reg.Harness.Name != "CLAUDE-CODE" {
		t.Fatal("registration rewritten")
	}
}

type migrationDefect string

const (
	defectAgent   migrationDefect = "agent"
	defectNative  migrationDefect = "native"
	defectArchive migrationDefect = "archive"
	defectMissing migrationDefect = "missing"
	defectCorrupt migrationDefect = "corrupt"
	defectVersion migrationDefect = "version"
	defectUTF8    migrationDefect = "utf8"
)

func TestQualifiedLookupRejectsWrongReferencedOwner(t *testing.T) {
	for _, defect := range []migrationDefect{defectAgent, defectNative, defectArchive, defectMissing, defectCorrupt, defectVersion, defectUTF8} {
		t.Run(string(defect), func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "s"}
			reg := migrationRegistration(key, "old")
			entry := indexEntry(key, "old")
			switch defect {
			case defectAgent:
				reg.Harness.Name = "claude"
			case defectNative:
				reg.NativeSessionID = " S "
			case defectArchive:
				reg.ArchiveSessionID = "other"
			case defectVersion:
				entry.Version = 2
			case defectMissing, defectCorrupt:
			// These defects are applied to the persisted files below.
			case defectUTF8:
				key.NativeID = string([]byte{0xff})
			}
			if defect != defectMissing {
				if err := local.Write(s.registrationPath("old"), reg); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.Write(qualifiedSessionIndexPath(s.home, key), entry); err != nil {
				t.Fatal(err)
			}
			if defect == defectCorrupt {
				writeCorrupt(t, s.registrationPath("old"))
			}
			if _, found, err := s.ArchiveSessionID(key); found || (err == nil && defect != defectMissing) {
				t.Fatalf("accepted defect found=%t err=%v", found, err)
			}
		})
	}
}

func TestQualifiedLookupNeverScansRegistrations(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "new"}
	// An unreadable enumeration target would fail a full scan, but bounded
	// ordinary miss never even touches the registration directory.
	if err := os.Remove(filepath.Join(s.home, "registrations")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "registrations"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("miss scanned %t %v", found, err)
	}
	writeCorrupt(t, qualifiedSessionIndexPath(s.home, key))
	if _, _, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("corruption scanned: %v", err)
	}
}

func TestQualifiedRegistrationDurableInterruption(t *testing.T) {
	for _, boundary := range []string{"reservation", "registration", "committed"} {
		t.Run(boundary, func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "fresh"}
			injected := errors.New("interrupted")
			s.onIndexStep = func(step string) error {
				if step == boundary {
					return injected
				}
				return nil
			}
			if _, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) }); !errors.Is(err, injected) {
				t.Fatalf("failure not injected: %v", err)
			}
			entry, present, err := s.readQualifiedIndex(key)
			if err != nil || !present {
				t.Fatalf("reservation absent %#v %t %v", entry, present, err)
			}
			retained := entry.ArchiveSessionID
			if _, found, err := s.ArchiveSessionID(key); err != nil || found != (boundary != "reservation") {
				t.Fatalf("public reservation admitted %t %v", found, err)
			}
			s = OpenReadOnly(s.home)
			reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
			if err != nil || reg.ArchiveSessionID != retained {
				t.Fatalf("retry changed ID %#v %v", reg, err)
			}
			regs, err := s.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("duplicate orphan %#v %v", regs, err)
			}
		})
	}
}

func TestQualifiedAdoptionInterruptionAndExpiry(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "after durable adoption", true: "expiry during stage"}[expire], func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "old"}
			legacyRegistration(t, s, key, "historic")
			injected := errors.New("interrupted")
			if !expire {
				s.onIndexStep = func(step string) error {
					if step == "adoption" {
						return injected
					}
					return nil
				}
			} else {
				fired := false
				s.onIndexSync = func() {
					if fired {
						return
					}
					fired = true
					if err := s.ForgetSession("historic", key); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _, err := s.EnsureArchiveSessionID(key)
			if !expire && !errors.Is(err, injected) {
				t.Fatalf("interruption %v", err)
			}
			if expire && !errors.Is(err, errIndexMoved) {
				t.Fatalf("expiry ignored: %v", err)
			}
			s.onIndexStep = nil
			s.onIndexSync = nil
			id, created, err := s.EnsureArchiveSessionID(key)
			if err != nil || created != expire || (id == "historic") == expire {
				t.Fatalf("retry %q %t %v", id, created, err)
			}
			if expire {
				if _, found, _ := s.LoadRegistration("historic"); found {
					t.Fatal("expired registration revived")
				}
			}
		})
	}
}

func TestQualifiedRecoveryConflictsAndIncompleteEnumeration(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "duplicate"}
	for _, id := range []string{"one", "two"} {
		if err := local.Write(s.registrationPath(id), migrationRegistration(key, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("picked duplicate %v", err)
	}
	if err := os.Remove(s.registrationPath("two")); err != nil {
		t.Fatal(err)
	}
	absent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "absent"}
	if err := s.RequestSessionIndexRecovery(absent); err != nil {
		t.Fatal(err)
	}
	writeCorrupt(t, s.registrationPath("broken"))
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err == nil {
		t.Fatal("incomplete census succeeded")
	}
	if _, _, err := s.EnsureArchiveSessionID(absent); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("partial inventory proved absence: %v", err)
	}
	if err := os.Remove(s.registrationPath("broken")); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.ArchiveSessionID(key); err != nil || !found || id != "one" {
		t.Fatalf("resolved conflict %q %t %v", id, found, err)
	}
	if _, created, err := s.EnsureArchiveSessionID(absent); err != nil || !created {
		t.Fatalf("complete absent %t %v", created, err)
	}
}

func TestQualifiedRecoveryResumesEveryDurableBoundary(t *testing.T) {
	for _, tc := range []struct {
		boundary   string
		incomplete bool
	}{
		{"recovery-begin", false}, {"recovery-incomplete", true}, {"recovery-enumerated", true}, {"recovery-entry", true}, {"recovery-completing", true}, {"recovery-complete", false},
	} {
		boundary := tc.boundary
		t.Run(boundary, func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "old"}
			legacyRegistration(t, s, key, "stable")
			interrupted := errors.New("interrupted")
			s.onIndexStep = func(step string) error {
				if step == boundary {
					return interrupted
				}
				return nil
			}
			if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); !errors.Is(err, interrupted) {
				t.Fatalf("missing boundary %v", err)
			}
			//lint:ignore LV1001 boundary names match the string-valued durable interruption seam
			if boundary != "recovery-begin" && boundary != "recovery-complete" {
				if err := s.sessionIndexMissAllowed(); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("partial census marked complete %v", err)
				}
			}
			s.onIndexStep = nil
			if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
				t.Fatal(err)
			}
			if id, found, err := s.ArchiveSessionID(key); err != nil || !found || id != "stable" {
				t.Fatalf("resume %q %t %v", id, found, err)
			}
		})
	}
}

func TestQualifiedForgetPreservesOtherAgentLegacyAndReplacement(t *testing.T) {
	s := newTestStore(t)
	claude := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "collision"}
	codex := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: claude.NativeID}
	legacyRegistration(t, s, claude, "claude-old")
	reg, err := s.RegisterNewSession(codex, func(id string) archive.SessionRegistration { return migrationRegistration(codex, id) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession(reg.ArchiveSessionID, codex); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.ArchiveSessionID(claude); err != nil || !found || id != "claude-old" {
		t.Fatalf("deleted collision %q %t %v", id, found, err)
	}
	if err := local.Write(qualifiedSessionIndexPath(s.home, claude), indexEntry(claude, "replacement")); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession("claude-old", claude); err != nil {
		t.Fatal(err)
	}
	entry, found, err := s.readQualifiedIndex(claude)
	if err != nil || !found || entry.ArchiveSessionID != "replacement" {
		t.Fatalf("deleted replacement %#v %t %v", entry, found, err)
	}
}

func TestQualifiedIndexSyncNeverHoldsRequestLock(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "fresh"}
	syncs := 0
	s.onIndexSync = func() {
		syncs++
		entries, err := os.ReadDir(filepath.Join(s.home, "request-locks"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			unlock, err := local.NamedLock(s.home, filepath.Join("request-locks", entry.Name()))
			if err != nil {
				t.Fatalf("sync held lock: %v", err)
			}
			unlock()
		}
	}
	if _, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) }); err != nil {
		t.Fatal(err)
	}
	if syncs != 4 {
		t.Fatalf("index syncs %d", syncs)
	}
}

func TestQualifiedRecoveryPreservesLegacyChildReservation(t *testing.T) {
	s := newTestStore(t)
	parentKey := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent"}
	childKey := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent:subagent:child"}
	legacyRegistration(t, s, parentKey, "parent-archive")
	if err := local.Write(nativeSessionIndexPath(s.home, childKey.NativeID), sessionIndexEntry{ArchiveSessionID: "child-archive"}); err != nil {
		t.Fatal(err)
	}
	candidate := SubagentCandidate{ArchiveSessionID: "child-archive", NativeSessionID: childKey.NativeID, ParentArchiveSessionID: "parent-archive", ParentNativeSessionID: parentKey.NativeID, ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude-code"}, AgentID: "child", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	if err := s.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	// A hook can arrive before the collector's first migration pass. It must
	// defer this legacy candidate, rather than reserve a second child identity.
	if id, created, err := s.EnsureArchiveSessionID(childKey); !errors.Is(err, ErrSessionIndexRecoveryRequired) || created || id != "" {
		t.Fatalf("legacy candidate reassigned before recovery: %q %t %v", id, created, err)
	}
	// Known corruption is requested before maintenance; its negative census must
	// not discard the candidate's stronger durable positive reservation evidence.
	if err := s.RequestSessionIndexRecovery(childKey); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(childKey); err != nil || found {
		t.Fatalf("candidate admitted %t %v", found, err)
	}
	if id, created, err := s.EnsureArchiveSessionID(childKey); err != nil || created || id != "child-archive" {
		t.Fatalf("candidate reassigned %q %t %v", id, created, err)
	}
	reg := migrationRegistration(childKey, "child-archive")
	reg.ParentSessionID, reg.ParentNativeSessionID, reg.SubagentID = "parent-archive", parentKey.NativeID, "child"
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.ArchiveSessionID(childKey); err != nil || !found || id != "child-archive" {
		t.Fatalf("child commit %q %t %v", id, found, err)
	}
}

func TestQualifiedRegistrationUpdateAndRemovalCannotChangeOwner(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "owner"}
	reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.NativeSessionID = "other"; return nil }); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("identity update %v", err)
	}
	wrong := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: key.NativeID}
	if err := s.ForgetSession(reg.ArchiveSessionID, wrong); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("wrong removal %v", err)
	}
	if id, found, err := s.ArchiveSessionID(key); err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("owner lost %q %t %v", id, found, err)
	}
}

// A newer hook request cannot be certified by an earlier recovery census.
// A damaged key restarts the census; an unowned key (a refused new session)
// lets it complete and stays held until the next slice certifies it.
// Regression: phase 3a P3A-R1.
func TestQualifiedRecoveryKeepsRequestDuringCompletion(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		for _, duringSync := range []bool{false, true} {
			name := map[bool]string{false: "unowned", true: "damaged"}[damaged] + " " + map[bool]string{false: "before stage", true: "staged before commit"}[duringSync]
			t.Run(name, func(t *testing.T) {
				testRecoveryKeepsRequestDuringCompletion(t, damaged, duringSync)
			})
		}
	}
}

func testRecoveryKeepsRequestDuringCompletion(t *testing.T, damaged, duringSync bool) {
	t.Helper()
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "arriving-at-completion"}
	requested := false
	request := func() error {
		if requested {
			return nil
		}
		requested = true
		unlock, err := local.NamedLock(s.home, "hooks.lock")
		if err != nil {
			return err
		}
		defer unlock()
		if damaged {
			return requestDamagedKey(s, key)
		}
		return s.RequestSessionIndexRecovery(key)
	}
	s.onIndexStep = func(step string) error {
		if step == "recovery-completing" {
			if duringSync {
				s.onWriteSync = func() {
					if err := request(); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			return request()
		}
		return nil
	}
	err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true)
	if !requested {
		t.Fatal("request boundary not reached")
	}
	if damaged && !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("newer generation accepted: %v", err)
	}
	s.onIndexStep, s.onWriteSync = nil, nil
	if !damaged {
		if err != nil {
			t.Fatalf("unowned request failed the census: %v", err)
		}
		if _, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
			t.Fatalf("earlier census certified a newer request: %v", err)
		}
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.EnsureArchiveSessionID(key); err != nil || !created {
		t.Fatalf("newer request stranded by completed census: created=%t err=%v", created, err)
	}
}

func TestQualifiedRecoveryAcceptsExistingMarkerWithoutGeneration(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		s := newTestStore(t)
		if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), sessionIndexMarker{Version: 1, Complete: complete}); err != nil {
			t.Fatal(err)
		}
		s.onWriteSync = func() {
			unlock, err := local.NamedLock(s.home, "hooks.lock")
			if err != nil {
				t.Fatalf("marker sync held hooks lock: %v", err)
			}
			unlock()
		}
		if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
			t.Fatal(err)
		}
		if err := s.sessionIndexMissAllowed(); err != nil {
			t.Fatal(err)
		}
	}
}

// Forgetting never leaves a live registration after its index was removed.
// Regression: phase 3a P3A-R2.
func TestQualifiedForgetInterruptionCannotLeaveUnindexedRegistration(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "forgotten"}
	reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted after index removal")
	s.onIndexStep = func(step string) error {
		if step == "forget-indexes" {
			return interrupted
		}
		return nil
	}
	if err := s.ForgetSession(reg.ArchiveSessionID, key); !errors.Is(err, interrupted) {
		t.Fatalf("interruption: %v", err)
	}
	s.onIndexStep = nil
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("index disappeared before registration: found=%t err=%v", found, err)
	}
	fresh, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil || fresh.ArchiveSessionID == reg.ArchiveSessionID {
		t.Fatalf("fresh retry: %#v %v", fresh, err)
	}
	regs, err := s.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("orphan duplicate: %#v %v", regs, err)
	}
}

// Collector begin must wait for a hook's marker and requested-key writes as a
// unit. Completion-only generation checks cannot cover a request begun earlier.
// Regression: phase 3a P3A-R7-BEGIN (R3A-BEGIN-01).
func TestQualifiedRecoveryBeginWaitsForInFlightRequest(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "in-flight-request"}
	requester := OpenReadOnly(s.home)
	marked := make(chan struct{})
	allowed := make(chan struct{})
	finished := make(chan error, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowed) }) }
	defer release()
	requester.onIndexStep = func(step string) error {
		if step == "recovery-request-marked" {
			close(marked)
			<-allowed
		}
		return nil
	}
	go func() {
		unlock, err := local.NamedLock(s.home, "hooks.lock")
		if err == nil {
			err = requester.RequestSessionIndexRecovery(key)
			unlock()
		}
		finished <- err
	}()
	select {
	case <-marked:
	case err := <-finished:
		t.Fatalf("request did not reach marker: %v", err)
	}
	// A fixed begin releases the paused request before acquiring hooks.lock.
	// Before the repair, only the completed census reaches this fallback seam.
	s.onLockWait = func(name string) {
		if name == "hooks.lock" {
			release()
		}
	}
	s.onIndexStep = func(step string) error {
		if step == "recovery-completing" {
			release()
		}
		return nil
	}
	recoveryErr := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true)
	release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if recoveryErr != nil {
		t.Fatal(recoveryErr)
	}
	s.onIndexStep, s.onLockWait = nil, nil
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("in-flight request was not recovered: found=%t err=%v", found, err)
	}
	if _, created, err := s.EnsureArchiveSessionID(key); err != nil || !created {
		t.Fatalf("recovered absence stranded: created=%t err=%v", created, err)
	}
}

func TestQualifiedRecoveryBeginRetriesBusyHooks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "busy-request"}
	unlock, err := local.NamedLock(s.home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	var before sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &before); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); !errors.Is(err, local.ErrBusy) {
		t.Fatalf("busy begin: %v", err)
	}
	var after sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("busy begin replaced requested generation")
	}
	unlock()
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("busy retry stranded: found=%t err=%v", found, err)
	}
}

func TestQualifiedRecoveryBeginStagingLeavesHooksAvailable(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "request-during-begin-stage"}
	requested := false
	s.onWriteSync = func() {
		unlock, err := local.NamedLock(s.home, "hooks.lock")
		if err != nil {
			t.Fatalf("marker sync held hooks lock: %v", err)
		}
		defer unlock()
		if !requested {
			requested = true
			if err := s.RequestSessionIndexRecovery(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if !requested {
		t.Fatal("begin staging was not observed")
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("staged begin lost request: found=%t err=%v", found, err)
	}
}

func TestQualifiedRecoveryBeginCancellationPreservesRequest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "cancelled-begin"}
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	var before sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.onWriteSync = cancel
	if err := recoverytest.Exhaust(ctx, s, SessionIndexRecoverySlice, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled begin: %v", err)
	}
	s.onWriteSync = nil
	var after sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("cancelled begin replaced requested generation")
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("cancelled retry stranded: found=%t err=%v", found, err)
	}
}

// A foreign agent is unrelated only when its native ID matches the legacy
// pointer's key. A different native ID means the pointer is corrupt.
func TestLegacyForeignPointerRejectsDifferentNativeIdentity(t *testing.T) {
	t.Parallel()
	for _, candidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "registration", true: "child-candidate"}[candidate], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "requested"}
			foreign := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "different"}
			if candidate {
				if err := s.SaveSubagentCandidate(SubagentCandidate{ArchiveSessionID: "foreign", NativeSessionID: foreign.NativeID, ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: string(foreign.Agent)}, AgentID: "child", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			} else if err := local.Write(s.registrationPath("foreign"), migrationRegistration(foreign, "foreign")); err != nil {
				t.Fatal(err)
			}
			if err := local.Write(nativeSessionIndexPath(s.home, key.NativeID), sessionIndexEntry{ArchiveSessionID: "foreign"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, ErrSessionIdentityConflict) {
				t.Fatalf("corrupt foreign pointer bypassed: %v", err)
			}
			if _, err := os.Stat(qualifiedSessionIndexPath(s.home, key)); !os.IsNotExist(err) {
				t.Fatalf("corrupt pointer allocated a qualified identity: %v", err)
			}
		})
	}
}
