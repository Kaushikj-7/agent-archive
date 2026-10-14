package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
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
