package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const sessionMembershipFile = "session-membership.json"

const sessionMembershipLock = "session-membership.lock"

type sessionMembershipRevision struct {
	Version  int    `json:"version"`
	Revision string `json:"revision"`
}

func (s *Store) sessionMembershipRevision() (string, error) {
	var revision sessionMembershipRevision
	err := readRecoveryJSON(filepath.Join(s.home, sessionMembershipFile), &revision)
	if errors.Is(err, os.ErrNotExist) {
		var marker sessionIndexMarker
		if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker) == nil && marker.MembershipFenced {
			return "", ErrSessionIndexRecoveryRequired
		}
		var cursor sessionRecoveryCursor
		if readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor) == nil && cursor.Revision != "" {
			return "", ErrSessionIndexRecoveryRequired
		}
		return "", nil // Legacy stores precede this fence.
	}
	if err != nil || revision.Version != 1 || !safeFileComponent(revision.Revision) {
		return "", ErrSessionIndexRecoveryRequired
	}
	return revision.Revision, nil
}

// Only identity membership and record validity affect census authority. Ordinary
// continuation updates retain the revision because they cannot change identity.
func sameRegistrationMembership(before fileSnapshot, value any) bool {
	var prior archive.SessionRegistration
	next, ok := value.(archive.SessionRegistration)
	if !ok || !before.found || json.Unmarshal(before.data, &prior) != nil {
		return false
	}
	a, aerr := registrationKey(prior)
	b, berr := registrationKey(next)
	return aerr == nil && berr == nil && a == b && prior.ArchiveSessionID == next.ArchiveSessionID && prior.Validate() == nil && next.Validate() == nil && prior.PreviousGenerationID == next.PreviousGenerationID && prior.CaptureFrozen == next.CaptureFrozen
}

func stageRegistrationRevision(home, path string, before fileSnapshot, value any, write bool) (*local.Staged, error) {
	if !write || filepath.Dir(path) != filepath.Join(home, "registrations") {
		return nil, nil
	}
	// Blind registration replacement does not read the prior contents for its
	// ordinary commit. Its bounded fence decision still compares identity facts;
	// the request-lock check prevents a removed owner from being resurrected.
	if !before.found && value != nil {
		current, err := readSnapshot(path)
		if err != nil {
			return nil, err
		}
		before = current
	}
	if sameRegistrationMembership(before, value) {
		return nil, nil
	}
	revision, err := local.ID()
	if err != nil {
		return nil, err
	}
	return local.Stage(filepath.Join(home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: revision})
}

// The caller already owns the request lock. No request or hooks lock is acquired
// inside the membership lock; the only operations inside are atomic renames.
func commitRegistrationRevision(w lockedWrite, registration *local.Staged) error {
	if w.membership == nil {
		return registration.Commit()
	}
	unlock, err := local.NamedLockWait(w.membershipHome, sessionMembershipLock, time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	if err := w.membership.Commit(); err != nil {
		return err
	}
	return registration.Commit()
}

func syncRegistrationRevision(revision *local.Staged) error {
	if revision == nil {
		return nil
	}
	return revision.SyncDir()
}

func (s *Store) stageRegistrationRemovalRevision(id string, key agentmeta.SessionKey) (*local.Staged, error) {
	if !safeFileComponent(id) {
		return nil, errors.New("archive session ID is not a safe file name component")
	}
	if key.NativeID != "" {
		if err := key.Validate(); err != nil {
			return nil, err
		}
		reg, found, err := s.LoadRegistration(id)
		if err != nil {
			return nil, err
		}
		if found {
			actual, err := registrationKey(reg)
			if err != nil || actual != key || reg.ArchiveSessionID != id {
				return nil, ErrSessionIdentityConflict
			}
		}
	}
	path := s.registrationPath(id)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	revision, err := stageRegistrationRevision(s.home, path, fileSnapshot{}, nil, true)
	if err == nil {
		s.writeSynced()
	}
	return revision, err
}

// Retention prestages before the request lock and syncs after releasing it. An
// unexpected new member cannot be removed without a prepared durable revision.
func (s *Store) removeRegistrationWithRevision(path string, revision *local.Staged) error {
	return s.removeRegistrationWithRevisionUsing(path, revision, os.Remove)
}

func (s *Store) removeRegistrationWithRevisionUsing(path string, revision *local.Staged, remove func(string) error) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if revision == nil {
		return ErrSessionIndexRecoveryRequired
	}
	unlock, err := local.NamedLockWait(s.home, sessionMembershipLock, time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	if err := revision.Commit(); err != nil {
		return err
	}
	err = remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Recovery bookkeeping is content-free and small; malformed oversized files
// cannot turn the final membership critical section into an unbounded read.
func readRecoveryJSON(path string, value any) error {
	return readRecoveryJSONLimit(path, value, 2048)
}

func readRecoveryJSONLimit(path string, value any, limit int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return ErrSessionIndexRecoveryRequired
	}
	return json.Unmarshal(data, value)
}

func (s *Store) ensureSessionMembershipRevision() (string, error) {
	revision, err := s.sessionMembershipRevision()
	if err != nil || revision != "" {
		return revision, err
	}
	next, err := local.ID()
	if err != nil {
		return "", err
	}
	err = s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return local.NamedLockWait(s.home, sessionMembershipLock, time.Second) },
		path: filepath.Join(s.home, sessionMembershipFile),
		change: func(current fileSnapshot) (any, bool, error) {
			if current.found {
				var prior sessionMembershipRevision
				if json.Unmarshal(current.data, &prior) != nil || prior.Version != 1 || !safeFileComponent(prior.Revision) {
					return nil, false, ErrSessionIndexRecoveryRequired
				}
				return nil, false, nil
			}
			return sessionMembershipRevision{Version: 1, Revision: next}, true, nil
		},
	})
	if err != nil {
		return "", err
	}
	return s.sessionMembershipRevision()
}
