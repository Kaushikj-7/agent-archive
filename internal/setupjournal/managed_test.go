package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// frozenLegacyJournal retains precisely the known field types of the prior
// release. Unknown version/jobs fields alone must not protect new recovery.
type frozenLegacyJournal struct {
	Legacy        *LegacyJob         `json:"legacy,omitempty"`
	Relabeled     *LegacyJob         `json:"relabeled,omitempty"`
	MoreRelabeled []*LegacyJob       `json:"more_relabeled,omitempty"`
	Changes       []fileapply.Change `json:"changes"`
	Plist         string             `json:"plist"`
	WasLoaded     bool               `json:"was_loaded"`
	Backend       string             `json:"backend,omitempty"`
	JobRef        string             `json:"job_ref,omitempty"`
	FilesOnly     bool               `json:"files_only,omitempty"`
}

type managedCrashPoint string

const (
	managedCrashPointJournal          managedCrashPoint = "journal"
	managedCrashPointCollectorStopped managedCrashPoint = "collector_stopped"
	managedCrashPointBothStopped      managedCrashPoint = "both_stopped"
	managedCrashPointPartialFiles     managedCrashPoint = "partial_files"
	managedCrashPointAllFiles         managedCrashPoint = "all_files"
	managedCrashPointCollectorLoaded  managedCrashPoint = "collector_loaded"
	managedCrashPointBothLoaded       managedCrashPoint = "both_loaded"
)

func TestManagedJournalFencesFrozenOldRecoveryBeforeAnyAction(t *testing.T) {
	t.Parallel()
	for _, intent := range []Intent{Keep, EnsureRunning, RestartIfLoaded, EnsureStopped} {
		j := Journal{Jobs: []ManagedJob{{Role: SearchRole, Backend: "launchd", Ref: "com.agent-archive.search", Definition: "/synthetic/Library/LaunchAgents/com.agent-archive.search.plist", Intent: intent}}}
		raw, err := json.Marshal(j)
		if err != nil {
			t.Fatal(err)
		}
		mutations := 0
		frozenRecover := func(data []byte) error {
			var old frozenLegacyJournal
			if err := json.Unmarshal(data, &old); err != nil {
				return err
			}
			mutations++
			return nil
		}
		if err = frozenRecover(raw); err == nil || mutations != 0 {
			t.Fatal("old recovery accepted new lifecycle wire", intent, string(raw))
		}
		var current Journal
		if err = json.Unmarshal(raw, &current); err != nil || len(current.Jobs) != 1 || current.Jobs[0].Intent != intent {
			t.Fatal(current, err)
		}
	}
	// Negative control proves extra unknown fields provide no downgrade guard.
	var old frozenLegacyJournal
	if err := json.Unmarshal([]byte(`{"schema_version":2,"changes":[],"jobs":[{"role":"search"}],"plist":"/old.plist","was_loaded":false}`), &old); err != nil {
		t.Fatal("negative control did not exercise permissive old decoder", err)
	}
}

func TestManagedJournalRefusesInvalidShapesWithoutRecoveryActions(t *testing.T) {
	t.Parallel()
	valid := `{"schema_version":2,"changes":{"files":[]},"jobs":[{"role":"search","backend":"launchd","ref":"com.agent-archive.search","definition":"/synthetic/Library/LaunchAgents/com.agent-archive.search.plist","was_loaded":false,"intent":"keep"}]}`
	for _, raw := range []string{
		strings.Replace(valid, `"schema_version":2`, `"schema_version":3`, 1),
		strings.Replace(valid, `"changes":{"files":[]}`, `"changes":[]`, 1),
		strings.Replace(valid, `"changes":{"files":[]}`, `"changes":null`, 1),
		strings.Replace(valid, `"changes":{"files":[]}`, `"changes":{}`, 1),
		strings.Replace(valid, `"role":"search"`, `"role":"other"`, 1),
		strings.Replace(valid, `"intent":"keep"`, `"intent":"arbitrary"`, 1),
		strings.Replace(valid, `"ref":"com.agent-archive.search"`, `"ref":""`, 1),
	} {
		home := t.TempDir()
		if err := os.WriteFile(JournalPath(home), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		lockTaken := false
		err := Recover(home, func(string) (scheduler.Scheduler, error) { t.Fatal("invalid record reached manager"); return nil, nil }, func() (func(), error) { lockTaken = true; return func() {}, nil })
		if err == nil || lockTaken || !TransactionPending(home) {
			t.Fatal("invalid recovery changed state", err, lockTaken)
		}
	}
}

func twoRoleFixture(t *testing.T) (*txFixture, string) {
	t.Helper()
	f := newTxFixture(t)
	search := filepath.Join(filepath.Dir(f.own), "com.agent-archive.search.plist")
	if err := local.WriteBytes(search, []byte("old search plist")); err != nil {
		t.Fatal(err)
	}
	f.sim.loaded[simLabel(search)] = search
	f.journal.Changes = append(f.journal.Changes, fileapply.Change{Path: search, Before: []byte("old search plist"), After: []byte("new search plist"), Existed: true, Mode: 0o600})
	f.journal.Jobs = []ManagedJob{
		{Role: CollectorRole, Backend: "launchd", Ref: simLabel(f.own), Definition: f.own, WasLoaded: true, Intent: EnsureRunning},
		{Role: SearchRole, Backend: "launchd", Ref: simLabel(search), Definition: search, WasLoaded: true, Intent: EnsureRunning},
	}
	return f, search
}

func TestManagedJournalRollsBackEveryRoleAfterSecondLoadFailure(t *testing.T) {
	t.Parallel()
	f, search := twoRoleFixture(t)
	f.sim.failLoad[search] = 1
	if err := Commit(f.home, f.journal, managedBackends(f.sim)); err == nil || !strings.Contains(err.Error(), "previous installation restored") {
		t.Fatal(err)
	}
	f.loaded[simLabel(search)] = search
	f.requireAsFound(t)
	data, err := os.ReadFile(search)
	if err != nil || string(data) != "old search plist" || !JobActive(f.sim.JobState(search)) {
		t.Fatal(string(data), err, f.sim.JobState(search))
	}
}

func TestManagedRecoveryChecksAllRoleOwnershipBeforeStoppingAny(t *testing.T) {
	t.Parallel()
	f, search := twoRoleFixture(t)
	f.sim.unknown[search] = true
	if err := local.Write(JournalPath(f.home), f.journal); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.own)
	err := Restore(f.home, f.journal, managedBackends(f.sim))
	var blocked *RecoveryBlockedError
	if !errors.As(err, &blocked) || !TransactionPending(f.home) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.own)
	if !bytes.Equal(before, after) || !JobActive(f.sim.JobState(f.own)) {
		t.Fatal("collector changed before search ownership was known")
	}
	for _, call := range f.sim.calls {
		if strings.HasPrefix(call, "load ") || strings.HasPrefix(call, "unload ") {
			t.Fatal("scheduler mutated before all-role preflight", call)
		}
	}
}

func TestManagedMixedRefreshDoesNotEnableMissingRole(t *testing.T) {
	t.Parallel()
	f, search := twoRoleFixture(t)
	delete(f.sim.loaded, simLabel(search))
	f.journal.Jobs[0].Intent = Keep
	f.journal.Changes = f.journal.Changes[1:]
	f.journal.Jobs[1].Intent = RestartIfLoaded
	f.journal.Jobs[1].WasLoaded = false
	if err := Commit(f.home, f.journal, managedBackends(f.sim)); err != nil {
		t.Fatal(err)
	}
	if f.sim.JobState(search) != string(scheduler.Missing) || !JobActive(f.sim.JobState(f.own)) {
		t.Fatal("refresh changed loaded state")
	}
	data, err := os.ReadFile(search)
	if err != nil || string(data) != "new search plist" {
		t.Fatal(string(data), err)
	}
}

func TestManagedSearchRefCannotNameACollectorOrOtherInstallation(t *testing.T) {
	t.Parallel()
	f, _ := twoRoleFixture(t)
	f.journal.Jobs[1].Ref = simLabel(f.own)
	f.journal.Jobs[1].Definition = f.own
	if err := Commit(f.home, f.journal, managedBackends(f.sim)); err == nil {
		t.Fatal("search role accepted collector identity")
	}
	if TransactionPending(f.home) {
		t.Fatal("invalid role journal was written")
	}
}

type managedPlistScheduler struct{ plistScheduler }

func (managedPlistScheduler) Ref(inst scheduler.Installation) scheduler.Ref {
	return (launchd.Scheduler{}).Ref(inst)
}

func managedBackends(sim *launchdSim) Backends {
	return func(string) (scheduler.Scheduler, error) { return managedPlistScheduler{plistScheduler{sim}}, nil }
}

func TestManagedRecoveryRestoresBothRolesAtEveryCrashBoundary(t *testing.T) {
	t.Parallel()
	for _, point := range []managedCrashPoint{managedCrashPointJournal, managedCrashPointCollectorStopped, managedCrashPointBothStopped, managedCrashPointPartialFiles, managedCrashPointAllFiles, managedCrashPointCollectorLoaded, managedCrashPointBothLoaded} {
		t.Run(string(point), func(t *testing.T) {
			t.Parallel()
			f, search := twoRoleFixture(t)
			f.loaded[simLabel(search)] = search
			must(t, local.Write(JournalPath(f.home), f.journal))
			if point != managedCrashPointJournal {
				must(t, f.sim.Unload(f.own))
			}
			if point != managedCrashPointJournal && point != managedCrashPointCollectorStopped {
				must(t, f.sim.Unload(search))
			}
			switch point {
			case managedCrashPointJournal, managedCrashPointCollectorStopped, managedCrashPointBothStopped:
			case managedCrashPointPartialFiles:
				must(t, fileapply.Apply(f.journal.Changes[:1]))
			case managedCrashPointAllFiles, managedCrashPointCollectorLoaded, managedCrashPointBothLoaded:
				must(t, fileapply.Apply(f.journal.Changes))
			}
			if point == managedCrashPointCollectorLoaded || point == managedCrashPointBothLoaded {
				must(t, f.sim.Load(f.own))
			}
			if point == managedCrashPointBothLoaded {
				must(t, f.sim.Load(search))
			}
			must(t, Recover(f.home, managedBackends(f.sim), noLock))
			f.requireAsFound(t)
			data, err := os.ReadFile(search)
			if err != nil || string(data) != "old search plist" || !JobActive(f.sim.JobState(search)) {
				t.Fatal("search role not restored", string(data), err)
			}
			must(t, Recover(f.home, managedBackends(f.sim), noLock))
		})
	}
}

func TestManagedRecoveryRetainsJournalOnPartialRollbackAndExternalEdit(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"reload", "external_edit"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			f, search := twoRoleFixture(t)
			must(t, local.Write(JournalPath(f.home), f.journal))
			must(t, f.sim.Unload(f.own))
			must(t, f.sim.Unload(search))
			must(t, fileapply.Apply(f.journal.Changes))
			if failure == "reload" {
				f.sim.failLoad[search] = 1
			} else {
				must(t, os.WriteFile(search, []byte("external edit"), 0o600))
			}
			err := Recover(f.home, managedBackends(f.sim), noLock)
			if err == nil || !TransactionPending(f.home) {
				t.Fatal("incomplete rollback lost journal", err)
			}
			if failure == "external_edit" {
				data, readErr := os.ReadFile(search)
				if readErr != nil || string(data) != "external edit" {
					t.Fatal("recovery overwrote external edit", string(data), readErr)
				}
				return
			}
			must(t, Recover(f.home, managedBackends(f.sim), noLock))
			if TransactionPending(f.home) || !JobActive(f.sim.JobState(search)) {
				t.Fatal("retry did not finish rollback")
			}
		})
	}
}
