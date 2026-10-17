package state

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	aalocal "github.com/wangjohn/agent-archive/internal/local"
)

func TestForgetIdleSessionKeepsASessionThatGainedWork(t *testing.T) {
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		deferForWork bool
		forgotten    bool
	}{
		{"publishable: the request defers it", true, false},
		{"not publishable: its work would never be done", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
				t.Fatal(err)
			}
			forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, tc.deferForWork, nil)
			if err != nil || forgotten != tc.forgotten {
				t.Fatalf("forgotten=%t err=%v, want %t", forgotten, err, tc.forgotten)
			}
			_, registered, _ := local.LoadRegistration(reg.ArchiveSessionID)
			_, requested, _ := local.LoadRequest(reg.ArchiveSessionID)
			if registered == tc.forgotten || requested == tc.forgotten {
				t.Fatalf("registered=%t requested=%t after forgotten=%t", registered, requested, tc.forgotten)
			}
		})
	}
}

func TestForgetIdleSessionKeepsASessionWithAPendingPublication(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SavePending(reg.ArchiveSessionID, PendingPublication{SourceKey: "k", MetadataKey: "m", SourceSHA256: durableRef([]byte{1}).SHA256, SourceBytes: []byte{1}, MetadataBytes: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, nil); err != nil || forgotten {
		t.Fatalf("forgotten=%t err=%v", forgotten, err)
	}
}

// A hook that looked the registration up before retention forgot the session
// must not leave a request behind for it: nothing would ever read it, and it
// would count as pending forever.
func TestSaveRequestForAForgottenSessionLeavesNoOrphan(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, nil); err != nil || !forgotten {
		t.Fatalf("forgotten=%t err=%v", forgotten, err)
	}
	err := local.SaveRequest(reg.ArchiveSessionID, "stop", time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("err=%v, want ErrSessionNotRegistered", err)
	}
	if _, err := os.Stat(local.requestPath(reg.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an orphan request was written: %v", err)
	}
}

// The removal record is a durable write, whose syncs can outlast the second
// a hook waits for the request lock, so ForgetIdleSession writes it before
// taking that lock. A hook that arrives meanwhile finds the lock free, its
// request keeps the session, and the record goes back to what it was: none,
// or the one an earlier removal of the native session left.
//
// Regression: the record was written under the lock, and on a loaded machine
// a concurrent hook's request failed with ErrBusy.
func TestForgetIdleSessionWritesTheRemovalRecordOutsideTheRequestLock(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	earlier := at.Add(-time.Hour)
	removal := &RemovalRecord{Harness: "codex", Reason: RemovalReasonRetention, At: at}
	for _, tc := range []struct {
		name     string
		previous *RemovalRecord
	}{
		{"no earlier record", nil},
		{"an earlier record", &RemovalRecord{Harness: "codex", Reason: RemovalReasonUndo, At: earlier}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if tc.previous != nil {
				if err := local.RecordRemoval(tc.previous.Harness, reg.NativeSessionID, tc.previous.Reason, tc.previous.At); err != nil {
					t.Fatal(err)
				}
			}
			var hookErr error
			local.afterRemovalRecord = func() {
				unlock, err := local.lockRequest(reg.ArchiveSessionID)
				if err != nil {
					hookErr = err
					return
				}
				unlock()
				hookErr = local.SaveRequest(reg.ArchiveSessionID, "stop", at)
			}
			forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, removal)
			if hookErr != nil {
				t.Fatalf("the hook could not write its request while the record was written: %v", hookErr)
			}
			if err != nil || forgotten {
				t.Fatalf("forgotten=%t err=%v, want the session kept", forgotten, err)
			}
			if _, registered, _ := local.LoadRegistration(reg.ArchiveSessionID); !registered {
				t.Fatal("the session was forgotten over the hook's request")
			}
			record, found, err := local.Removal("codex", reg.NativeSessionID)
			switch {
			case err != nil:
				t.Fatal(err)
			case tc.previous == nil && found:
				t.Fatalf("a kept session has a removal record: %#v", record)
			case tc.previous != nil && (!found || record.Reason != tc.previous.Reason || !record.At.Equal(tc.previous.At)):
				t.Fatalf("record=%#v found=%t, want the earlier %#v back", record, found, *tc.previous)
			}

			// With the hook's request handled, the next attempt forgets it.
			local.afterRemovalRecord = nil
			if _, err := local.CompleteRequest(reg.ArchiveSessionID, mustRequestToken(t, local, reg.ArchiveSessionID)); err != nil {
				t.Fatal(err)
			}
			if forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, removal); err != nil || !forgotten {
				t.Fatalf("retry: forgotten=%t err=%v", forgotten, err)
			}
			if record, found, err := local.Removal("codex", reg.NativeSessionID); err != nil || !found || record.Reason != RemovalReasonRetention || !record.At.Equal(at) {
				t.Fatalf("record=%#v found=%t err=%v", record, found, err)
			}
		})
	}
}

func mustRequestToken(t *testing.T, local *Store, id string) string {
	t.Helper()
	req, found, err := local.LoadRequest(id)
	if err != nil || !found {
		t.Fatalf("request: found=%t err=%v", found, err)
	}
	return req.Token
}

// A forget that cannot take the request lock has forgotten nothing, so the
// removal record it wrote first goes back.
func TestForgetIdleSessionTakesTheRecordBackWhenTheLockIsBusy(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	unlock, err := local.lockRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	removal := &RemovalRecord{Harness: "codex", Reason: RemovalReasonRetention, At: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}
	forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, removal)
	if !errors.Is(err, aalocal.ErrBusy) || forgotten {
		t.Fatalf("forgotten=%t err=%v, want ErrBusy", forgotten, err)
	}
	if _, found, err := local.Removal("codex", reg.NativeSessionID); err != nil || found {
		t.Fatalf("a session that was not forgotten kept a removal record: found=%t err=%v", found, err)
	}
}

// A forget that fails part way may have unregistered the session already, so
// its removal record stays: without it backfill would import the session
// again.
func TestForgetIdleSessionKeepsTheRecordWhenTheForgetFailsPartWay(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	// The current durable preflight correctly refuses a non-file published
	// control before removing any registration. Inject this partial failure at
	// the existing later index boundary, after actual registration removal.
	local.onIndexStep = func(step string) error {
		if step == "forget-indexes" {
			return errors.New("synthetic partial forget after registration removal")
		}
		return nil
	}
	removal := &RemovalRecord{Harness: "codex", Reason: RemovalReasonRetention, At: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}
	forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, removal)
	if err == nil || forgotten {
		t.Fatalf("forgotten=%t err=%v, want a failure", forgotten, err)
	}
	if _, registered, _ := local.LoadRegistration(reg.ArchiveSessionID); registered {
		t.Fatal("the test did not fail the forget after the registration went")
	}
	if _, found, err := local.Removal("codex", reg.NativeSessionID); err != nil || !found {
		t.Fatalf("a half-forgotten session lost its removal record: found=%t err=%v", found, err)
	}
}

func TestForgetIdleSessionRefusesUnsafePublishedControlBeforeRemoval(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(local.publishedPath(reg.ArchiveSessionID), "blocker"), 0700); err != nil {
		t.Fatal(err)
	}
	removal := &RemovalRecord{Harness: "codex", Reason: RemovalReasonRetention, At: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}
	forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, true, removal)
	if !errors.Is(err, ErrDurableStorageRecovery) || forgotten {
		t.Fatal("unsafe control passed forget preflight", forgotten, err)
	}
	if _, found, err := local.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("unsafe control removed registration", found, err)
	}
	if _, found, err := local.Removal("codex", reg.NativeSessionID); err != nil || found {
		t.Fatal("refusal retained a new removal authority", found, err)
	}
}

// Under the request lock, which hooks wait on for a second, a forget does not
// wait for a subagent candidate's lock. A held one means a hook or the
// collector is recording a subagent of the session: a session retention
// would defer for work is kept, and any other attempt fails at once, for the
// next to retry.
func TestForgetIdleSessionDoesNotWaitForASubagentCandidateLock(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for _, deferForWork := range []bool{true, false} {
		t.Run(fmt.Sprintf("deferForWork=%t", deferForWork), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			child := SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-1:subagent:agent", ParentArchiveSessionID: reg.ArchiveSessionID, ParentNativeSessionID: reg.NativeSessionID, ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
			if err := local.SaveSubagentCandidate(child); err != nil {
				t.Fatal(err)
			}
			unlock, err := local.lockSubagentCandidate(child.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			removal := &RemovalRecord{Harness: "codex", Reason: RemovalReasonRetention, At: at}
			// Only the part under the request lock is timed: the record's
			// durable write before it can take seconds on a loaded machine.
			var started time.Time
			local.afterRemovalRecord = func() { started = time.Now() }
			forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, deferForWork, removal)
			took := time.Since(started)
			local.afterRemovalRecord = nil
			unlock()
			if forgotten || (deferForWork && err != nil) || (!deferForWork && !errors.Is(err, aalocal.ErrBusy)) {
				t.Fatalf("forgotten=%t err=%v", forgotten, err)
			}
			// NamedLockWait gives up after a second; not waiting at all is
			// far below that even on a loaded machine.
			if took >= 500*time.Millisecond {
				t.Fatalf("the forget waited %v for the candidate's lock", took)
			}
			if _, registered, _ := local.LoadRegistration(reg.ArchiveSessionID); !registered {
				t.Fatal("the session was forgotten")
			}
			if ids, err := local.subagentCandidatesForSession(reg.ArchiveSessionID); err != nil || len(ids) != 1 {
				t.Fatalf("candidates=%v err=%v, want the child kept", ids, err)
			}
			if _, found, err := local.Removal("codex", reg.NativeSessionID); err != nil || found {
				t.Fatalf("a kept session has a removal record: found=%t err=%v", found, err)
			}

			// Once the lock is free, the session and its candidate go.
			if forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, deferForWork, removal); err != nil || !forgotten {
				t.Fatalf("retry: forgotten=%t err=%v", forgotten, err)
			}
			if ids, err := local.subagentCandidatesForSession(reg.ArchiveSessionID); err != nil || len(ids) != 0 {
				t.Fatalf("candidates=%v err=%v, want none", ids, err)
			}
		})
	}
}
