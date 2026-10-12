package state

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// SupersededSource is a source object that was once the current snapshot
// for a session and no longer is. It stays eligible for deletion, not
// deleted immediately, so the spec's grace period can protect a reader
// mid-download of what was, until a moment ago, the current pointer.
type SupersededSource struct {
	Key          string    `json:"key"`
	SupersededAt time.Time `json:"superseded_at"`
	// PrivacySensitive marks a predecessor made with an older filter. It can
	// be removed after the new publication is read-back verified and the
	// reader grace interval has elapsed, even when it is the immediate
	// predecessor. Older ledger files omit this field and keep their policy.
	PrivacySensitive bool `json:"privacy_sensitive,omitempty"`
}

func (s *Store) supersededPath(archiveSessionID string) string {
	return filepath.Join(s.home, "superseded", archiveSessionID+".json")
}

// RecordSuperseded appends key to a session's superseded-source ledger, so
// a later retention sweep can delete it once its grace period elapses. A
// session republished more than once before a sweep ever runs must not
// lose track of an earlier supersession, so each key is tracked
// individually rather than only the most recent one. If key is already
// recorded (content reverted to an earlier snapshot and was then
// superseded again), the entry moves to the end of the ledger with a fresh
// SupersededAt: append order is the supersession order retention relies
// on to identify the immediate predecessor of the current snapshot, so
// the most recently superseded key must always be last.
func (s *Store) RecordSuperseded(archiveSessionID, key string, at time.Time) error {
	return s.RecordSupersededWithPrivacy(archiveSessionID, key, at, false)
}

// RecordSupersededWithPrivacy persists the stronger cleanup policy for a
// source replaced because its filter version changed.
func (s *Store) RecordSupersededWithPrivacy(archiveSessionID, key string, at time.Time, privacySensitive bool) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	existing, err := s.LoadSuperseded(archiveSessionID)
	// A ledger just moved aside is an empty one: this entry starts a new
	// ledger, and the loss is still reported.
	lost := err
	if err != nil && !errors.Is(err, ErrQuarantined) {
		return err
	}
	out := make([]SupersededSource, 0, len(existing)+1)
	for _, e := range existing {
		if e.Key == key {
			// A content reversion can reuse an earlier key. Once a source
			// is known to contain old-filter evidence, keep that marker
			// across later supersessions of the same object.
			privacySensitive = privacySensitive || e.PrivacySensitive
			continue
		}
		out = append(out, e)
	}
	out = append(out, SupersededSource{Key: key, SupersededAt: at, PrivacySensitive: privacySensitive})
	return errors.Join(lost, local.Write(s.supersededPath(archiveSessionID), out))
}

// LoadSuperseded returns a session's superseded-source ledger. In a
// collector pass a ledger that no longer decodes is moved aside (the error
// wraps ErrQuarantined, once) and reads as empty afterwards; the objects it
// listed stay until the whole session expires.
func (s *Store) LoadSuperseded(archiveSessionID string) ([]SupersededSource, error) {
	var out []SupersededSource
	if _, err := s.readOwned(s.supersededPath(archiveSessionID), &out); err != nil {
		return nil, fmt.Errorf("read superseded sources %q: %w", archiveSessionID, err)
	}
	return out, nil
}

// ClampSuperseded moves every entry of a session's ledger that claims to have
// been superseded after at back to at, keeping the ledger's order. Such a
// time was stamped by a clock running ahead; left alone, it would keep its
// object from ever reaching the grace period until that date came round.
func (s *Store) ClampSuperseded(archiveSessionID string, at time.Time) error {
	existing, err := s.LoadSuperseded(archiveSessionID)
	if err != nil {
		return err
	}
	changed := false
	for i := range existing {
		if existing[i].SupersededAt.After(at) {
			existing[i].SupersededAt = at.UTC()
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return local.Write(s.supersededPath(archiveSessionID), existing)
}

// RemoveSuperseded drops one entry from a session's ledger, after its
// object has actually been deleted from storage.
func (s *Store) RemoveSuperseded(archiveSessionID, key string) error {
	existing, err := s.LoadSuperseded(archiveSessionID)
	if err != nil {
		return err
	}
	out := existing[:0]
	for _, e := range existing {
		if e.Key != key {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		err := os.Remove(s.supersededPath(archiveSessionID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded ledger %q: %w", archiveSessionID, err)
		}
		return nil
	}
	return local.Write(s.supersededPath(archiveSessionID), out)
}

// ForgetIdleSession forgets a session under its request lock, the same lock a
// hook holds while it writes a request. Retention decides a session has
// expired from a snapshot taken earlier in the sweep; a hook can write a
// request for it after that snapshot. When deferForWork is set, the pending
// work is checked again under the lock, and a session that now has a request
// or a pending publication is kept (forgotten reports false) so the collector
// publishes that evidence, as is one with a subagent candidate whose lock a
// hook or the collector holds right then. A session the collector no longer
// publishes is forgotten regardless: its work would never be done (a held
// candidate lock then fails the attempt with ErrBusy, for the next to retry).
//
// A non-nil removal is recorded (see RecordRemoval) before anything is
// forgotten, so a record that cannot be written leaves the session
// registered and the caller's next attempt retries both. It is written
// before the request lock is taken, not under it: the record is a durable
// write, whose syncs can take seconds on a busy machine, and a hook waits only
// a second for that lock on the user's turn. A session the locked recheck
// keeps alive has the record taken back, so it gets none. Until then, or
// after a crash in between, the record sits beside a registration, where
// nothing reads it: backfill consults removal records only for sessions
// that are not registered.
func (s *Store) ForgetIdleSession(archiveSessionID string, key agentmeta.SessionKey, deferForWork bool, removal *RemovalRecord) (forgotten bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	if err := key.Validate(); err != nil {
		return false, err
	}
	if removal != nil && agentmeta.Canonical(agentmeta.Builtins(), removal.Harness) != string(key.Agent) {
		return false, ErrSessionIdentityConflict
	}
	if err := s.CheckDurableSessionRead(archiveSessionID); err != nil {
		return false, err
	}
	packedIDs, err := s.packedRemovalIDs(archiveSessionID)
	if err != nil {
		return false, err
	}
	revision, err := s.stageRegistrationRemovalRevision(archiveSessionID, key)
	if err != nil {
		return false, err
	}
	defer revision.Discard()
	takeBack := func() error { return nil }
	if removal != nil {
		if takeBack, err = s.recordRemovalRevocably(removal.Harness, key.NativeID, removal.Reason, removal.At); err != nil {
			return false, err
		}
		if s.afterRemovalRecord != nil {
			s.afterRemovalRecord()
		}
	}
	retirement, err := s.stageGenerationRetirement(key, archiveSessionID)
	if err != nil {
		return false, errors.Join(err, takeBack())
	}
	defer retirement.Discard()
	started, err := s.forgetIdleLocked(archiveSessionID, key, deferForWork, revision, retirement)
	if started {
		// A forget that failed part way keeps its record: the session may
		// be unregistered already.
		if revision != nil {
			s.writeSynced()
		}
		err = errors.Join(err, syncRegistrationRevision(revision), syncRegistrationRevision(retirement), s.removePackedIdentities(packedIDs))
		if err == nil {
			err = s.recordPackedExpiry(key, archiveSessionID)
		}
		return err == nil, err
	}
	// None of the session's own records are gone, so the record goes back.
	if err != nil {
		return false, errors.Join(err, takeBack())
	}
	// Keeping the session was right, and a record the take-back could not
	// remove sits inert beside its registration; it is not a failure.
	_ = takeBack()
	return false, nil
}

// forgetIdleLocked is the part of ForgetIdleSession done under the request
// lock: the recheck, then the forget. started reports whether the forget
// began removing the session's own records.
func (s *Store) forgetIdleLocked(archiveSessionID string, key agentmeta.SessionKey, deferForWork bool, revision, retirement *local.Staged) (started bool, err error) {
	unlock, err := s.lockRequest(archiveSessionID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := s.CheckDurableSessionRead(archiveSessionID); err != nil {
		return false, err
	}
	if deferForWork {
		if work, err := s.hasWork(archiveSessionID); err != nil || work {
			return false, err
		}
	}
	// ForgetSession would wait up to a second for each subagent candidate's
	// lock while this one is held, and hooks wait only a second for this
	// one. So the candidates go first, without waiting: a candidate lock
	// held now means a hook or the collector is at work on the session's
	// subagents, work that keeps the session like a request does.
	busy, err := s.removeSubagentCandidatesWithoutWaiting(archiveSessionID)
	switch {
	case err != nil:
		return false, err
	case busy && deferForWork:
		return false, nil
	case busy:
		return false, fmt.Errorf("forget session %q: a subagent candidate naming it is being recorded: %w", archiveSessionID, local.ErrBusy)
	}
	return true, s.forgetSession(archiveSessionID, key, false, revision, retirement)
}

// hasWork reports whether a session has a request or a pending publication:
// work the collector will do.
func (s *Store) hasWork(archiveSessionID string) (bool, error) {
	_, requested, err := s.LoadRequest(archiveSessionID)
	if err != nil || requested {
		return requested, err
	}
	return s.HasPending(archiveSessionID)
}

// orphanDirs are the directories whose files outlive a registration that is
// gone without the session being forgotten: moved aside because it no longer
// decoded, or lost to a crash in the middle of ForgetSession. They are the
// files that say the session may have objects in the bucket.
var orphanDirs = []string{"published", "pending", "superseded"}

// OrphanedSessions lists the sessions that have published, pending, or
// superseded state but no registration file, other than those in keep (the
// registrations that exist but could not be read this time). Nothing else
// records that their objects exist, so without this they would outlive
// retention in the bucket and on this machine.
func (s *Store) OrphanedSessions(keep map[string]bool) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, dir := range orphanDirs {
		ids, err := s.listJSONStems(dir)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
		for _, id := range ids {
			if seen[id] || keep[id] {
				continue
			}
			seen[id] = true
			if _, err := os.Lstat(s.registrationPath(id)); errors.Is(err, os.ErrNotExist) {
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// OrphanHarnesses returns the harness an orphaned session's objects are
// under, as its published state or superseded ledger records it, or nil when
// neither does. Neither is decoded beyond what that takes.
func (s *Store) OrphanHarnesses(archiveSessionID string) []string {
	if summary, found, err := s.LoadPublishedSummary(archiveSessionID); err == nil && found && summary.Harness != "" {
		return []string{summary.Harness}
	}
	ledger, _ := s.LoadSuperseded(archiveSessionID)
	for _, entry := range ledger {
		if parts := strings.Split(entry.Key, "/"); len(parts) > 3 && parts[0] == "sessions" && parts[2] == archiveSessionID {
			return []string{parts[1]}
		}
	}
	return nil
}

// OrphanChangedAt is the latest modification time of an orphaned session's
// files: when it was last known to change, for a session whose capture time
// is not recorded.
func (s *Store) OrphanChangedAt(archiveSessionID string) time.Time {
	var latest time.Time
	for _, dir := range orphanDirs {
		if info, err := os.Stat(filepath.Join(s.home, dir, archiveSessionID+".json")); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// ForgetOrphan forgets an orphaned session's local state (see
// OrphanedSessions), under its request lock, unless a registration for it has
// appeared meanwhile: a hook registering the native session again reuses its
// archive ID. forgotten reports whether it did. Like ForgetIdleSession, it
// does not wait for a subagent candidate's lock under the request lock: a
// held one fails the attempt with ErrBusy, for the next sweep to retry.
func (s *Store) ForgetOrphan(archiveSessionID string) (forgotten bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	if err := s.CheckDurableSessionRead(archiveSessionID); err != nil {
		return false, err
	}
	packedIDs, err := s.packedRemovalIDs(archiveSessionID)
	if err != nil {
		return false, err
	}
	defer func() {
		if forgotten {
			err = errors.Join(err, s.removePackedIdentities(packedIDs))
			forgotten = err == nil
		}
	}()
	unlock, err := s.lockRequest(archiveSessionID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Lstat(s.registrationPath(archiveSessionID)); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := s.CheckDurableSessionRead(archiveSessionID); err != nil {
		return false, err
	}
	busy, err := s.removeSubagentCandidatesWithoutWaiting(archiveSessionID)
	switch {
	case err != nil:
		return false, err
	case busy:
		return false, fmt.Errorf("forget session %q: a subagent candidate naming it is being recorded: %w", archiveSessionID, local.ErrBusy)
	}
	if err := s.forgetSession(archiveSessionID, agentmeta.SessionKey{}, false, nil, nil); err != nil {
		return false, err
	}
	return true, nil
}

// SessionDir is the per-session directory under the collector-owned
// sessions/ tree where other packages keep session-scoped evidence (the CLI's
// read-back verification record, for one). ForgetSession clears it.
func (s *Store) SessionDir(archiveSessionID string) string {
	return filepath.Join(s.home, "sessions", archiveSessionID)
}

// ForgetSession removes every local record of a session: its registration,
// request, request and subagent-candidate locks, published-bundle cache, pending publication and
// scan markers, superseded-source ledger, per-session evidence directory,
// and native-session index entry. A caller uses this only after successfully
// deleting that session's metadata and every source object from storage
// (whole-session retention); it never touches storage itself. Retention calls
// it through ForgetIdleSession, under the request lock.
//
// Every record, the native-session index included, is removed before the
// request lock file. The lock is a flock on that file's inode, so once the
// file is unlinked a waiting hook can lock a fresh one; by then the
// registration and the index entry are already gone, so saveRequest refuses
// to write for the session, UpdateRegistration reports it forgotten, and
// RegisterNewSession assigns a fresh archive ID instead of reusing this one.
func (s *Store) ForgetSession(archiveSessionID string, key agentmeta.SessionKey) error {
	if err := s.CheckDurableSessionRead(archiveSessionID); err != nil {
		return err
	}
	packedIDs, err := s.packedRemovalIDs(archiveSessionID)
	if err != nil {
		return err
	}
	revision, err := s.stageRegistrationRemovalRevision(archiveSessionID, key)
	if err != nil {
		return err
	}
	defer revision.Discard()
	retirement, err := s.stageGenerationRetirement(key, archiveSessionID)
	if err != nil {
		return err
	}
	defer retirement.Discard()
	err = s.forgetSession(archiveSessionID, key, true, revision, retirement)
	if revision != nil {
		s.writeSynced()
	}
	err = errors.Join(err, syncRegistrationRevision(revision), syncRegistrationRevision(retirement), s.removePackedIdentities(packedIDs))
	if err == nil {
		err = s.recordPackedExpiry(key, archiveSessionID)
	}
	return err
}

// forgetSession is ForgetSession; withCandidates false is for a caller that
// already removed the session's subagent candidates under their locks.
func (s *Store) forgetSession(archiveSessionID string, key agentmeta.SessionKey, withCandidates bool, revision, retirement *local.Staged) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if key.NativeID != "" {
		if err := key.Validate(); err != nil {
			return err
		}
		reg, found, err := s.LoadRegistration(archiveSessionID)
		if err != nil {
			return err
		}
		if found {
			actual, err := registrationKey(reg)
			if err != nil || actual != key || reg.ArchiveSessionID != archiveSessionID {
				return ErrSessionIdentityConflict
			}
		}
	}
	if err := s.CheckDurableSessionRead(archiveSessionID); err != nil {
		return err
	}
	if withCandidates {
		if err := s.removeSubagentCandidatesForSession(archiveSessionID); err != nil {
			return fmt.Errorf("remove linked subagent candidates: %w", err)
		}
	}
	if retirement != nil {
		if err := retirement.Commit(); err != nil {
			return err
		}
		if err := s.indexStep("generation-retired"); err != nil {
			return err
		}
	}
	paths := []string{
		s.registrationPath(archiveSessionID),
		s.requestPath(archiveSessionID),
		s.publishedPath(archiveSessionID),
		s.pendingPath(archiveSessionID),
		filepath.Join(s.home, "pending-scans", archiveSessionID+".json"),
		s.scanSignaturePath(archiveSessionID),
		s.supersededPath(archiveSessionID),
		s.refreshSkipPath(archiveSessionID),
		filepath.Join(s.SessionDir(archiveSessionID), "verification.json"),
		filepath.Join(s.home, importStageRelative(archiveSessionID)),
		filepath.Join(s.home, importCancelRelative(archiveSessionID)),
	}

	// The session's own candidate is gone (removed above, under this lock),
	// so its lock file goes too. Unlinking a lock file is safe:
	// local.NamedLock only reports a lock held once the path still names the
	// file it locked, so a caller that opened this file just before the
	// unlink retries on the new one instead of sharing the lock.
	paths = append(paths, filepath.Join(s.home, subagentLockName(archiveSessionID)))
	// Copies of its files moved aside go with it: they hold the same
	// evidence, which expiry must not leave on this machine.
	paths = append(paths, s.quarantinedCopies(archiveSessionID)...)
	// The request lock goes last. Unlinking it lets a waiting hook lock a
	// fresh file at once, so everything a hook rechecks under that lock (the
	// registration, the request, and the native-session index a new
	// registration would reuse) must already be gone by then.
	requestLockPath := filepath.Join(s.home, requestLockName(archiveSessionID))
	paths = append(paths, requestLockPath)
	for _, path := range paths {
		// Keep registration deletion before index deletion: interrupted expiry
		// must never leave an admitted owner invisible to bounded lookup.
		if path == requestLockPath && key.NativeID != "" {
			if err := s.removeSessionIndex(key, archiveSessionID); err != nil {
				return err
			}
			if err := s.indexStep("forget-indexes"); err != nil {
				return err
			}
		}
		remove := os.Remove
		if path == filepath.Join(s.home, importStageRelative(archiveSessionID)) {
			remove = func(string) error { return s.RemoveImportStage(archiveSessionID) }
		}
		if path == filepath.Join(s.home, importCancelRelative(archiveSessionID)) {
			remove = func(string) error { return s.removeImportCancellation(archiveSessionID) }
		}
		if path == s.registrationPath(archiveSessionID) {
			remove = func(path string) error { return s.removeRegistrationWithRevision(path, revision) }
		}
		if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("remove %q: %w", path, err), s.MarkSessionIndexRecoveryNeeded())
		}
	}
	// Drop the per-session directory only once nothing else lives in it;
	// anything unexpected there is preserved rather than deleted blindly.
	dir := s.SessionDir(archiveSessionID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session directory %q: %w", dir, err)
	}
	if len(entries) == 0 {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove session directory %q: %w", dir, err)
		}
	}
	return nil
}
