package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func completedAbsenceStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 2)
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if status, err := s.SessionIndexRecoveryStatus(); err != nil || !status.Complete {
		t.Fatalf("seed census: %#v %v", status, err)
	}
	return s
}

func readMarkerGeneration(t *testing.T, s *Store) string {
	t.Helper()
	var marker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		t.Fatal(err)
	}
	return marker.Generation
}

// Discovery asks for one key's census-certified absence. A live hook's
// unrelated miss (a Claude Code subagent's first SubagentStop, a new
// session) must stay admitted meanwhile instead of being refused until the
// collector's next recovery pass.
func TestAbsenceRequestKeepsCompleteCensusForOtherKeys(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	generation := readMarkerGeneration(t, s)
	wanted := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "discovered-unowned"}
	if err := s.RequestSessionIndexAbsence(wanted); err != nil {
		t.Fatal(err)
	}
	if status, err := s.SessionIndexRecoveryStatus(); err != nil || !status.Complete || status.Pending {
		t.Fatalf("absence request withdrew the census: %#v %v", status, err)
	}
	child := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent:subagent:a1"}
	if _, created, err := s.EnsureArchiveSessionID(child); err != nil || !created {
		t.Fatalf("unrelated hook miss refused: created=%v err=%v", created, err)
	}
	// The requested key itself stays held until the collector certifies it.
	if yes, err := s.SessionIndexAbsent(wanted); yes || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("uncertified absence: %v %v", yes, err)
	}
	if _, _, err := s.EnsureArchiveSessionID(wanted); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("requested key allocated before certification: %v", err)
	}
	// A hook or discovery retry for the same retained request is not damage.
	if err := s.RequestSessionIndexRecovery(wanted); err != nil {
		t.Fatal(err)
	}
	if got := readMarkerGeneration(t, s); got != generation {
		t.Fatal("retained absence request started a new census")
	}
	steps := map[string]int{}
	s.onIndexStep = func(step string) error { steps[step]++; return nil }
	complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
	if err != nil || !complete {
		t.Fatalf("absence slice complete=%v err=%v", complete, err)
	}
	if steps["recovery-begin"] != 0 || steps["absence-certified"] != 1 {
		t.Fatalf("absence slice steps: %v", steps)
	}
	if yes, err := s.SessionIndexAbsent(wanted); !yes || err != nil {
		t.Fatalf("certified absence: %v %v", yes, err)
	}
	if got := readMarkerGeneration(t, s); got != generation {
		t.Fatal("absence certification replaced the census")
	}
	// Nothing is pending afterwards: a later slice enumerates nothing.
	clear(steps)
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete || steps["absence-enumerated"] != 0 {
		t.Fatalf("idle slice complete=%v err=%v steps=%v", complete, err, steps)
	}
}

// A key whose index entry was silently lost keeps its registration: the
// census repairs the owner rather than certifying absence.
func TestAbsenceRequestRepairsSilentlyLostOwner(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	owned := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-0000"}
	if err := os.Remove(qualifiedSessionIndexPath(s.home, owned)); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestSessionIndexAbsence(owned); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
		t.Fatalf("absence slice complete=%v err=%v", complete, err)
	}
	if id, found, err := s.ArchiveSessionID(owned); err != nil || !found || id != "owner-0000" {
		t.Fatalf("owner not repaired: %q %v %v", id, found, err)
	}
	if yes, err := s.SessionIndexAbsent(owned); yes || err != nil {
		t.Fatalf("owned key certified absent: %v %v", yes, err)
	}
}

// A request arriving after the slice enumerated registrations is not covered
// by that census and must wait for the next slice.
func TestAbsenceRequestAfterEnumerationWaitsForNextSlice(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	first := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "first-unowned"}
	late := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "late-owned"}
	if err := s.RequestSessionIndexAbsence(first); err != nil {
		t.Fatal(err)
	}
	s.onIndexStep = func(step string) error {
		if step != "absence-enumerated" {
			return nil
		}
		s.onIndexStep = nil
		// An owner this census did not see, whose index entry is missing.
		if err := local.Write(s.registrationPath("late-owner"), migrationRegistration(late, "late-owner")); err != nil {
			return err
		}
		return s.RequestSessionIndexAbsence(late)
	}
	complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
	if err != nil || complete {
		t.Fatalf("stale slice complete=%v err=%v", complete, err)
	}
	if yes, _ := s.SessionIndexAbsent(late); yes {
		t.Fatal("stale census certified an owned key absent")
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
		t.Fatalf("next slice complete=%v err=%v", complete, err)
	}
	if id, found, err := s.ArchiveSessionID(late); err != nil || !found || id != "late-owner" {
		t.Fatalf("late owner: %q %v %v", id, found, err)
	}
	if yes, err := s.SessionIndexAbsent(first); !yes || err != nil {
		t.Fatalf("first absence: %v %v", yes, err)
	}
}

// Unusable scheduling evidence falls back to a full census, which certifies
// every retained request.
func TestAbsenceRequestDamagedScheduleRunsFullCensus(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	wanted := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "damaged-schedule"}
	if err := s.RequestSessionIndexAbsence(wanted); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, sessionIndexAbsenceFile), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || complete {
		t.Fatalf("damaged schedule complete=%v err=%v", complete, err)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if yes, err := s.SessionIndexAbsent(wanted); !yes || err != nil {
		t.Fatalf("absence after full census: %v %v", yes, err)
	}
	if _, pending, err := s.readAbsenceRequest(); err != nil || pending {
		t.Fatalf("schedule not retired: pending=%v err=%v", pending, err)
	}
}

// Without a complete census the request keeps the existing full path.
func TestAbsenceRequestWithoutCensusRequestsRecovery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	wanted := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "no-census"}
	if err := s.RequestSessionIndexAbsence(wanted); err != nil {
		t.Fatal(err)
	}
	if status, _ := s.SessionIndexRecoveryStatus(); status.Complete || !status.Pending {
		t.Fatalf("full recovery not requested: %#v", status)
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if yes, err := s.SessionIndexAbsent(wanted); !yes || err != nil {
		t.Fatalf("absence after full census: %v %v", yes, err)
	}
}

// A slice that read the token a request had just written, before that
// request's entry existed, would certify the token without the entry and
// strand it under a complete census with nothing pending.
func TestAbsenceRequestTokenReadBeforeEntryStaysPending(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	wanted := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "token-before-entry"}
	var sliceErr error
	done := make(chan struct{})
	s.onIndexStep = func(step string) error {
		if step != "absence-request-marked" {
			return nil
		}
		s.onIndexStep = nil
		// The requester holds hooks.lock between the token and its entry. A
		// collector slice running now must not certify the new token.
		go func() {
			defer close(done)
			collector := *s
			collector.onIndexStep = nil
			_, sliceErr = collector.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		}()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	}
	unlock, err := local.NamedLock(s.home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	err = s.RequestSessionIndexAbsence(wanted)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if sliceErr != nil && !errors.Is(sliceErr, local.ErrBusy) {
		t.Fatalf("concurrent slice: %v", sliceErr)
	}
	// Either the slice waited for the entry and certified it, or the token
	// is still pending; never a certified token with a retained entry.
	_, pending, err := s.readAbsenceRequest()
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := s.SessionIndexAbsent(wanted); !pending && (!absent || err != nil) {
		t.Fatalf("token certified before its entry: absent=%v err=%v", absent, err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
		t.Fatalf("next slice complete=%v err=%v", complete, err)
	}
	if yes, err := s.SessionIndexAbsent(wanted); !yes || err != nil {
		t.Fatalf("absence: %v %v", yes, err)
	}
}

// Hooks refused while a census is pending ask for recovery: a SubagentStop
// for its healthy parent and its new child, a new session for itself. None
// of them may start a new census generation, which would fail the pending
// census's completion and, on a busy machine, keep recovery from finishing.
func TestRefusedHookRequestsDuringPendingCensusLetItComplete(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	parent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-0000"}
	child := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent:subagent:refused"}
	if _, _, err := s.EnsureArchiveSessionID(child); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("miss allowed during a pending census: %v", err)
	}
	steps := map[string]int{}
	requested := false
	s.onIndexStep = func(step string) error {
		steps[step]++
		if step != "recovery-completing" || requested {
			return nil
		}
		requested = true
		// After the census listed sessions-v1, so its listing misses the child.
		return errors.Join(s.RequestSessionIndexRecovery(parent), s.RequestSessionIndexRecovery(child))
	}
	complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
	if err != nil || !complete || !requested {
		t.Fatalf("census complete=%v err=%v requested=%v", complete, err, requested)
	}
	if steps["recovery-request-marked"] != 0 {
		t.Fatalf("refused hooks restarted the census: %v", steps)
	}
	if id, found, err := s.ArchiveSessionID(parent); err != nil || !found || id != "owner-0000" {
		t.Fatalf("parent: %q %v %v", id, found, err)
	}
	// The child's retained request is certified by the next slice without
	// another full census.
	clear(steps)
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
		t.Fatalf("certification slice complete=%v err=%v", complete, err)
	}
	if steps["recovery-begin"] != 0 || steps["absence-certified"] != 1 {
		t.Fatalf("certification slice steps: %v", steps)
	}
	if _, created, err := s.EnsureArchiveSessionID(child); err != nil || !created {
		t.Fatalf("child after certification: created=%v err=%v", created, err)
	}
}

// A request retained during a pending census, before that census lists
// sessions-v1, is certified by it; the later token check finds nothing left.
func TestRequestBeforePendingCensusListingIsCertifiedByIt(t *testing.T) {
	t.Parallel()
	s := completedAbsenceStore(t)
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	generation := readMarkerGeneration(t, s)
	fresh := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "refused-start"}
	if err := s.RequestSessionIndexRecovery(fresh); err != nil {
		t.Fatal(err)
	}
	if got := readMarkerGeneration(t, s); got != generation {
		t.Fatal("refused start restarted the pending census")
	}
	if err := recoverytest.Exhaust(context.Background(), s, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if yes, err := s.SessionIndexAbsent(fresh); !yes || err != nil {
		t.Fatalf("absence: %v %v", yes, err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
		t.Fatalf("slice complete=%v err=%v", complete, err)
	}
	if _, pending, err := s.readAbsenceRequest(); err != nil || pending {
		t.Fatalf("token left pending=%v err=%v", pending, err)
	}
}

// A retained request whose scheduling token was lost (deleted, or moved aside
// by hygiene) or already certified must not be stranded under a complete
// census, nor withdraw that census for every other key: the next request for
// the key, from discovery's lookup or a hook, schedules it again.
func TestAbsenceRequestWithoutPendingTokenIsRescheduled(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"deleted", "certified"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := completedAbsenceStore(t)
			generation := readMarkerGeneration(t, s)
			wanted := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "stranded-" + name}
			if err := s.RequestSessionIndexAbsence(wanted); err != nil {
				t.Fatal(err)
			}
			strandAbsenceRequest(t, s, name == "deleted")
			// Nothing is scheduled: a slice alone leaves the entry held.
			if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
				t.Fatalf("idle slice complete=%v err=%v", complete, err)
			}
			// Discovery's lookup fails on the retained entry and asks again.
			if _, _, err := s.ArchiveSessionID(wanted); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("retained entry lookup: %v", err)
			}
			if err := s.RequestSessionIndexRecovery(wanted); err != nil {
				t.Fatal(err)
			}
			if status, err := s.SessionIndexRecoveryStatus(); err != nil || !status.Complete || status.Pending {
				t.Fatalf("stranded request withdrew the census: %#v %v", status, err)
			}
			if got := readMarkerGeneration(t, s); got != generation {
				t.Fatal("stranded request started a new census")
			}
			other := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "unrelated-" + name}
			if _, created, err := s.EnsureArchiveSessionID(other); err != nil || !created {
				t.Fatalf("unrelated miss refused: created=%v err=%v", created, err)
			}
			steps := map[string]int{}
			s.onIndexStep = func(step string) error { steps[step]++; return nil }
			if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
				t.Fatalf("certification slice complete=%v err=%v", complete, err)
			}
			if steps["recovery-begin"] != 0 || steps["absence-certified"] != 1 {
				t.Fatalf("certification slice steps: %v", steps)
			}
			if yes, err := s.SessionIndexAbsent(wanted); !yes || err != nil {
				t.Fatalf("absence: %v %v", yes, err)
			}
		})
	}
}

// strandAbsenceRequest leaves the retained entry with no pending token.
func strandAbsenceRequest(t *testing.T, s *Store, deleted bool) {
	t.Helper()
	path := filepath.Join(s.home, sessionIndexAbsenceFile)
	if deleted {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	request, _, err := s.readAbsenceRequest()
	if err != nil {
		t.Fatal(err)
	}
	request.Certified = request.Requested
	if err := local.Write(path, request); err != nil {
		t.Fatal(err)
	}
}

// A rescheduled request applies the same requested-miss repair as a full
// census: a key whose registration survived gets its owner back, and a
// candidate whose parent is gone stays held rather than becoming absent.
// Neither starts a full census, however often the key is requested again.
func TestRescheduledAbsenceRequestNeverCertifiesOwnedKeys(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"owner", "orphan-candidate"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := completedAbsenceStore(t)
			generation := readMarkerGeneration(t, s)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-0000"}
			if name == "owner" {
				if err := os.Remove(qualifiedSessionIndexPath(s.home, key)); err != nil {
					t.Fatal(err)
				}
			} else {
				key = agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "gone-parent:subagent:a1"}
				candidate := SubagentCandidate{ArchiveSessionID: "orphan-child", NativeSessionID: key.NativeID, ParentArchiveSessionID: "gone-parent-archive", ParentNativeSessionID: "gone-parent", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude"}, AgentID: "a1", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Now()}
				if err := local.Write(s.subagentCandidatePath(candidate.ArchiveSessionID), candidate); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RequestSessionIndexAbsence(key); err != nil {
				t.Fatal(err)
			}
			// A repaired owner is no longer a retained request; the orphan
			// stays one and is requested again.
			rounds := 1
			if name != "owner" {
				rounds = 2
			}
			for range rounds {
				strandAbsenceRequest(t, s, true)
				if err := s.RequestSessionIndexRecovery(key); err != nil {
					t.Fatal(err)
				}
				if got := readMarkerGeneration(t, s); got != generation {
					t.Fatal("rescheduled request started a new census")
				}
				if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); err != nil || !complete {
					t.Fatalf("certification slice complete=%v err=%v", complete, err)
				}
				if yes, _ := s.SessionIndexAbsent(key); yes {
					t.Fatal("owned key certified absent")
				}
			}
			id, found, err := s.ArchiveSessionID(key)
			if name == "owner" && (err != nil || !found || id != "owner-0000") {
				t.Fatalf("owner not repaired: %q %v %v", id, found, err)
			}
			if name != "owner" && !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("orphan candidate key released: %q %v %v", id, found, err)
			}
		})
	}
}
