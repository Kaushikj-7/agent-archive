// Package setupjournal is setup's transaction: the journal setup writes
// before it changes any hook file or the LaunchAgent (Commit), putting an
// interrupted or failed setup back from it (Restore, Recover), recording the
// jobs earlier installations left that it retires (RetireeJobs; which jobs
// those are is the scheduler's to say, see scheduler.Inspector.Installed),
// and the check every other command and the hook make that one is pending.
// The scheduler is reached only through the Backends a caller passes; prompts
// and output stay in internal/cli.
package setupjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/state"
)

// JournalPath is where setup records its transaction in the data directory home.
func JournalPath(home string) string { return filepath.Join(home, "setup-transaction.json") }

// TransactionPending reports whether an interrupted or running setup's
// journal exists (or cannot be checked) in home.
func TransactionPending(home string) bool {
	_, err := os.Stat(JournalPath(home))
	return !os.IsNotExist(err)
}

// Journal is setup's record of one transaction (setup-transaction.json):
// every file change with its before and after bytes, the collector's plist
// and whether its job was loaded, and the jobs the setup retires.
type Journal struct {
	// NativeAuthorityProtection permanently fences old recovery before file actions.
	NativeAuthorityProtection bool       `json:"native_authority_protection,omitempty"`
	Legacy                    *LegacyJob `json:"legacy,omitempty"`
	// Relabeled and MoreRelabeled are the collectors earlier releases
	// installed for this data directory under other labels (see
	// scheduler.EarlierLabel); setup retires them in favor of the
	// directory's own label. The first stays in Relabeled, where releases
	// that retired at most one recorded it, so either reads the other's
	// journal of one.
	Relabeled     *LegacyJob         `json:"relabeled,omitempty"`
	MoreRelabeled []*LegacyJob       `json:"more_relabeled,omitempty"`
	Changes       []fileapply.Change `json:"changes"`
	Plist         string             `json:"plist"`
	WasLoaded     bool               `json:"was_loaded"`
	// Backend is the name of the scheduler that runs the collector's job (see
	// scheduler.Definer.Name), and JobRef the job's ref in it. Both are
	// optional: a journal written before they existed has neither, and then
	// the backend is DefaultBackend and the job is the one Plist names (see
	// scheduler.Definer.Locate), which JobRef must agree with when it is
	// recorded (see Backends.target). Every field before them is still written, so
	// a release that does not know them recovers the journal all the same
	// (encoding/json skips unknown fields).
	Backend string `json:"backend,omitempty"`
	JobRef  string `json:"job_ref,omitempty"`
	// FilesOnly is a transaction of files alone (setup --refresh, when it
	// leaves the collector's job as it is): neither Commit nor Restore asks
	// the scheduler anything, or starts, stops, or reloads the job. Without it a
	// commit starts the collector, and a rollback stops the one a failed
	// commit started.
	//
	// A release before this field existed ignores it (encoding/json skips
	// unknown fields) and recovers such a journal as an ordinary one: it
	// puts the files back, but stops a loaded collector and, since WasLoaded
	// is false here, does not start it again, so the collector stays off
	// until setup runs. Only an interrupted refresh (a crash or SIGKILL;
	// refresh absorbs the signals) followed by a downgrade can meet that.
	FilesOnly bool `json:"files_only,omitempty"`
}

// relabeled lists every collector the journal retires under another label.
func (j Journal) relabeled() []*LegacyJob {
	if j.Relabeled == nil {
		return j.MoreRelabeled
	}
	return append([]*LegacyJob{j.Relabeled}, j.MoreRelabeled...)
}

// retired lists every job the journal retires: the prototype's, then the
// collectors under other labels.
func (j Journal) retired() []*LegacyJob {
	if j.Legacy == nil {
		return j.relabeled()
	}
	return append([]*LegacyJob{j.Legacy}, j.relabeled()...)
}

// Commit records journal, then makes the changes it holds: it stops the
// collector when it was loaded, applies every file change, retires the jobs
// the journal retires, and starts the collector. A failure on the way puts
// everything back from the journal (Restore); success removes it.
func Commit(home string, journal Journal, backends Backends) error {
	// A job the journal could not drive is refused before anything is
	// recorded or changed, rather than found halfway.
	var collector target
	if !journal.FilesOnly {
		var err error
		if collector, err = backends.resolve(journal); err != nil {
			return fmt.Errorf("nothing was changed: %w", err)
		}
	}
	if err := local.Write(JournalPath(home), journal); err != nil {
		return err
	}
	fail := func(cause error) error {
		if rb := Restore(home, journal, backends); rb != nil {
			return errors.Join(cause, fmt.Errorf("rollback incomplete; run setup again: %w", rb))
		}
		return fmt.Errorf("previous installation restored: %w", cause)
	}
	if journal.FilesOnly {
		if err := fileapply.Apply(journal.Changes); err != nil {
			return fail(err)
		}
		if err := os.Remove(JournalPath(home)); err != nil {
			return fail(err)
		}
		return nil
	}
	if journal.WasLoaded {
		if err := collector.unload(); err != nil {
			return fail(fmt.Errorf("stop previous collector: %w", err))
		}
	}
	if err := fileapply.Apply(journal.Changes); err != nil {
		return fail(err)
	}
	if err := retireLegacyJob(journal.Legacy, backends); err != nil {
		return fail(err)
	}
	for _, job := range journal.relabeled() {
		if err := retireLegacyJob(job, backends); err != nil {
			return fail(fmt.Errorf("retire the %s: %w", relabeledJobName, err))
		}
	}
	if err := collector.load(); err != nil {
		return fail(fmt.Errorf("start background collector: %w", err))
	}
	if err := os.Remove(JournalPath(home)); err != nil {
		return fail(err)
	}
	return nil
}

// Restore puts back what journal records. It restores only files still
// equal to their before or after snapshots: a user's later edits are never
// overwritten by crash recovery. The journal is removed once it is done.
func Restore(home string, journal Journal, backends Backends) error {
	// Every file is checked before anything, the collector included, is
	// touched: a recovery that stops halfway would leave less to go on.
	var changed []fileapply.Change
	for _, c := range journal.Changes {
		if fileapply.Unapplied(c) {
			continue
		}
		if !fileapply.Applied(c) {
			return &RecoveryBlockedError{home: home, cause: c.Path + " changed outside setup, and recovery never overwrites your edits"}
		}
		changed = append(changed, c)
	}
	if journal.FilesOnly {
		if err := fileapply.Rollback(changed); err != nil {
			return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("the files setup changed could not all be put back (%v)", err)}
		}
		return os.Remove(JournalPath(home))
	}
	if err := checkLegacyJob(home, journal.Legacy, legacyJobName); err != nil {
		return err
	}
	for _, job := range journal.relabeled() {
		if err := checkLegacyJob(home, job, relabeledJobName); err != nil {
			return err
		}
	}
	// So is every job it drives: one it names that cannot be driven here (a
	// backend this system does not have, a definition no plan of that
	// backend writes) is never asked about, stopped or started.
	collector, err := backends.resolve(journal)
	if err != nil {
		return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("%v, so recovery changed nothing", err)}
	}
	state := collector.state()
	if state.Active() {
		if err := collector.unload(); err != nil {
			return collector.blocked(home, "stop the background collector", err)
		}
	} else if state == scheduler.Unknown {
		return &RecoveryBlockedError{home: home, cause: "the background collector's state is unknown, so recovery cannot safely continue; restore access to " + collector.tool() + " and rerun setup"}
	}
	if err := fileapply.Rollback(changed); err != nil {
		return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("the files setup changed could not all be put back (%v)", err)}
	}
	if err := collector.removeStranded(journal.Changes); err != nil {
		return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("what the failed setup left of the background collector's definition could not be removed (%v)", err)}
	}
	// A job another installation runs from its own definition is not this
	// one's to restart: the scheduler refuses to load over it, and stopping
	// it is not ours to do. The files are back and the record is removed
	// below, so the journal never outlives the point where anything more
	// can be done for the job. Setup, run again, refuses to install over
	// the other installation and says what to do (uninstall it, or set
	// AGENT_ARCHIVE_HOME).
	if journal.WasLoaded && state != scheduler.AnotherInstallation {
		if err := collector.load(); err != nil {
			return collector.blocked(home, "restart the background collector", err)
		}
	}
	if err := restoreLegacyJob(home, journal.Legacy, legacyJobName, backends); err != nil {
		return err
	}
	for _, job := range journal.relabeled() {
		if err := restoreLegacyJob(home, job, relabeledJobName, backends); err != nil {
			return err
		}
	}
	return os.Remove(JournalPath(home))
}

// Names of the jobs a setup journal can retire, as recovery errors call them.
const (
	legacyJobName    = "legacy upload job"
	relabeledJobName = "background collector installed under an earlier label"
)

// RecoveryBlockedError is a recovery that cannot proceed without the user:
// a file changed outside setup, or the scheduler cannot be asked. Rerunning setup
// alone would stop at the same place, so it carries the way out.
type RecoveryBlockedError struct {
	home  string
	cause string
}

// Error says why recovery cannot go on.
func (e *RecoveryBlockedError) Error() string {
	return "cannot recover the interrupted setup: " + e.cause
}

// Guidance says where the interrupted setup is recorded and the way out:
// discarding the record with --abandon-recovery, then running setup again.
func (e *RecoveryBlockedError) Guidance() string {
	return fmt.Sprintf("The interrupted setup is recorded in %s.\nTo keep every file as it is now and discard that record, run: agent-archive setup --abandon-recovery\nThen run agent-archive setup to review your settings.", JournalPath(e.home))
}

// Recover puts back the setup an interrupted journal in home records,
// if there is one. It reads the journal first, then takes the collector lock
// (lockCollector) and hooks.lock, so nothing runs a pass or a hook while
// files are restored.
func Recover(home string, backends Backends, lockCollector func() (func(), error)) error {
	var journal Journal
	err := local.Read(JournalPath(home), &journal)
	if os.IsNotExist(err) {
		return nil
	}
	if state.IsUndecodable(err) {
		return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("its record %s could not be read (%v), so nothing in it can be put back", JournalPath(home), err)}
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", JournalPath(home), err)
	}
	unlock, err := lockCollector()
	if err != nil {
		return err
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	return Restore(home, journal, backends)
}
