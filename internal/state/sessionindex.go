package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrSessionIndexRecoveryRequired asks the collector to repair derived identity
// bookkeeping. Hooks never enumerate registrations to resolve it.
var ErrSessionIndexRecoveryRequired = errors.New("session index recovery is required")

// ErrSessionIdentityConflict reports incompatible owners or a known duplicate.
var ErrSessionIdentityConflict = errors.New("session identity has conflicting registrations")

func nativeSessionIndexPath(home, nativeSessionID string) string {
	sum := sha256.Sum256([]byte(nativeSessionID))
	return filepath.Join(home, "sessions", hex.EncodeToString(sum[:])+".json")
}

func qualifiedSessionIndexPath(home string, key agentmeta.SessionKey) string {
	sum := sha256.Sum256(key.Encoding())
	return filepath.Join(home, "sessions-v1", hex.EncodeToString(sum[:])+".json")
}

type sessionIndexEntry struct {
	ArchiveSessionID string `json:"archive_session_id"`
}

type qualifiedSessionIndexEntry struct {
	Version          int          `json:"version"`
	Agent            agentmeta.ID `json:"agent"`
	NativeID         string       `json:"native_id"`
	ArchiveSessionID string       `json:"archive_session_id,omitempty"`
	// Reservation owns an unfinished ID, distinct from a committed index whose
	// registration disappeared. A durable registration wins even before cleanup.
	Reservation string `json:"reservation,omitempty"`
	Conflict    bool   `json:"conflict,omitempty"`
	Recovery    bool   `json:"recovery,omitempty"`
	Absent      bool   `json:"absent,omitempty"`
}

func indexEntry(key agentmeta.SessionKey, id string) qualifiedSessionIndexEntry {
	return qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, ArchiveSessionID: id}
}

func (e qualifiedSessionIndexEntry) validate(key agentmeta.SessionKey) error {
	if e.Version != 1 || e.Agent != key.Agent || e.NativeID != key.NativeID {
		return ErrSessionIndexRecoveryRequired
	}
	if e.Recovery {
		return ErrSessionIndexRecoveryRequired
	}
	if e.Absent {
		if e.ArchiveSessionID != "" || e.Reservation != "" || e.Conflict {
			return ErrSessionIndexRecoveryRequired
		}
		return nil
	}
	if e.Conflict {
		return ErrSessionIdentityConflict
	}
	if !safeFileComponent(e.ArchiveSessionID) || (e.Reservation != "" && !safeFileComponent(e.Reservation)) {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

func (s *Store) readQualifiedIndex(key agentmeta.SessionKey) (qualifiedSessionIndexEntry, bool, error) {
	var entry qualifiedSessionIndexEntry
	err := s.readIndexControlJSON(qualifiedSessionIndexPath(s.home, key), &entry)
	if errors.Is(err, os.ErrNotExist) {
		return s.packedSessionIndexEntry(key)
	}
	if IsUndecodable(err) {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	if err != nil {
		return entry, false, fmt.Errorf("read qualified session index: %w", err)
	}
	if err := entry.validate(key); err != nil {
		return entry, false, err
	}
	return entry, true, nil
}

func registrationKey(reg archive.SessionRegistration) (agentmeta.SessionKey, error) {
	return agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
}

func (s *Store) matchingRegistration(key agentmeta.SessionKey, id string) (bool, error) {
	if !safeFileComponent(id) {
		return false, ErrSessionIndexRecoveryRequired
	}
	reg, found, err := s.LoadRegistration(id)
	if IsUndecodable(err) {
		return false, ErrSessionIndexRecoveryRequired
	}
	if err != nil || !found {
		return false, err
	}
	if err := reg.Validate(); err != nil {
		return false, ErrSessionIndexRecoveryRequired
	}
	if err := s.generationRegistrationAllowed(key, reg); err != nil {
		return false, err
	}
	if reg.CaptureFrozen {
		return false, ErrSessionIndexRecoveryRequired
	}
	if reg.PreviousGenerationID != "" {
		if err := s.generationLookup(key, id); err != nil {
			return false, err
		}
		if _, found, err := s.loadGenerationHead(key); err != nil || !found {
			return false, s.indexRecoveryError(err)
		}
	}
	actual, err := registrationKey(reg)
	if err != nil || actual != key || reg.ArchiveSessionID != id {
		return false, ErrSessionIdentityConflict
	}
	return true, nil
}

// legacySessionID reads one historical entry and validates its owner. A legacy
// collision belonging to another agent is a miss, never that agent's ID.
func (s *Store) legacySessionID(key agentmeta.SessionKey) (string, bool, error) {
	var entry sessionIndexEntry
	err := local.Read(nativeSessionIndexPath(s.home, key.NativeID), &entry)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if IsUndecodable(err) || (err == nil && !safeFileComponent(entry.ArchiveSessionID)) {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if err != nil {
		return "", false, fmt.Errorf("read legacy session index: %w", err)
	}
	reg, found, err := s.LoadRegistration(entry.ArchiveSessionID)
	if IsUndecodable(err) {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if err != nil {
		return "", false, err
	}
	if !found {
		// Older writers also indexed unfinished children. Their directly
		// referenced candidate retains the archive ID even before admission;
		// let collector recovery promote that evidence to a reservation.
		return "", false, s.legacyCandidateRecovery(key, entry.ArchiveSessionID)
	}
	actual, err := registrationKey(reg)
	if reg.Validate() != nil {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if err != nil || reg.ArchiveSessionID != entry.ArchiveSessionID {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if actual.NativeID != key.NativeID {
		return "", false, ErrSessionIdentityConflict
	}
	if actual.Agent != key.Agent {
		return "", false, nil
	}
	registered, err := s.matchingRegistration(key, entry.ArchiveSessionID)
	if err != nil || !registered {
		return "", false, err
	}
	return entry.ArchiveSessionID, true, nil
}

func (s *Store) legacyCandidateRecovery(key agentmeta.SessionKey, id string) error {
	candidate, found, err := readJSON[SubagentCandidate](s.subagentCandidatePath(id))
	if IsUndecodable(err) {
		return ErrSessionIndexRecoveryRequired
	}
	if err != nil || !found {
		return err
	}
	actual, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
	if err != nil || candidate.ArchiveSessionID != id {
		return ErrSessionIndexRecoveryRequired
	}
	if actual.NativeID != key.NativeID {
		return ErrSessionIdentityConflict
	}
	if actual.Agent != key.Agent {
		return nil
	}
	return ErrSessionIndexRecoveryRequired
}

// ArchiveSessionID returns only a validated registration, reading at most one
// qualified entry, one legacy entry and their directly referenced registration
// or unfinished legacy child candidate.
// It performs no writes, enumeration or adoption outside the writer's locks.
func (s *Store) ArchiveSessionID(key agentmeta.SessionKey) (string, bool, error) {
	if h, found, err := s.loadGenerationHead(key); err != nil {
		return "", false, err
	} else if found {
		if h.Transition != "" {
			return "", false, ErrSessionIndexRecoveryRequired
		}
		if h.Retired {
			if _, present, err := s.LoadRegistration(h.Active); err != nil || present {
				return "", false, ErrSessionIndexRecoveryRequired
			}
			return "", false, s.sessionIndexMissAllowed()
		}
	}
	if err := key.Validate(); err != nil {
		return "", false, err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return "", false, err
	}
	if found && entry.Absent {
		return "", false, s.sessionIndexMissAllowed()
	}
	if found {
		registered, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		if err != nil || !registered {
			return "", false, err
		}
		if err := s.generationLookup(key, entry.ArchiveSessionID); err != nil {
			return "", false, err
		}
		return entry.ArchiveSessionID, true, nil
	}
	id, found, err := s.legacySessionID(key)
	if err != nil || found {
		if err == nil && found {
			err = s.generationLookup(key, id)
		}
		return id, found, err
	}
	if err := s.sessionIndexMissAllowed(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// EnsureArchiveSessionID reserves an ID for a proven fresh start, import or
// child candidate. A reservation is not proof of an admitted registration.
// Callers hold hooks.lock, or the collector lock for child materialization.
func (s *Store) EnsureArchiveSessionID(key agentmeta.SessionKey) (id string, created bool, err error) {
	if err := s.genericNativeReservationAllowed(key); err != nil {
		return "", false, err
	}
	if err := s.generationReservationAllowed(key); err != nil {
		return "", false, err
	}

	if err := key.Validate(); err != nil {
		return "", false, err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return "", false, err
	}
	if found && !entry.Absent {
		registered, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		if err != nil {
			return "", false, err
		}
		if registered || entry.Reservation != "" {
			if err := s.ensureGenerationReservation(key, entry.ArchiveSessionID); err != nil {
				return "", false, err
			}
			return entry.ArchiveSessionID, false, nil
		}
		// A committed registration disappeared; its old ID must not be revived.
	}
	if !found {
		legacy, adopted, err := s.adoptLegacySessionIndex(key)
		if err != nil || adopted {
			return legacy, false, err
		}
	}
	if err := s.sessionIndexMissAllowed(); err != nil {
		return "", false, err
	}
	fresh, err := local.ID()
	if err != nil {
		return "", false, fmt.Errorf("generate archive session ID: %w", err)
	}
	owner, err := local.ID()
	if err != nil {
		return "", false, err
	}
	next := indexEntry(key, fresh)
	next.Reservation = owner
	// A stale entry is guarded by its old request lock so expiry cannot remove a
	// replacement after our comparison. Fresh absent keys are serialized by hooks.
	lockID := fresh
	if found && !entry.Absent {
		lockID = entry.ArchiveSessionID
	}
	err = s.writeIndexUnderRequestLock(lockID, qualifiedSessionIndexPath(s.home, key), nil, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var existing qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &existing); err != nil {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			if err := existing.validate(key); err != nil {
				return nil, false, err
			}
			if !found || existing != entry {
				return nil, false, errIndexMoved
			}
			if err := s.replaceableReservation(key, existing); err != nil {
				return nil, false, err
			}
		} else if found {
			return nil, false, errIndexMoved
		}
		return next, true, nil
	})
	if err != nil {
		return "", false, err
	}
	if err := s.indexStep("reservation"); err != nil {
		return "", false, err
	}
	if err := s.ensureGenerationReservation(key, fresh); err != nil {
		return "", false, err
	}
	return fresh, true, nil
}

func (s *Store) finishSessionIndex(key agentmeta.SessionKey, id string) error {
	err := s.writeIndexUnderRequestLock(id, qualifiedSessionIndexPath(s.home, key), func() error {
		valid, err := s.matchingRegistration(key, id)
		if err != nil {
			return err
		}
		if !valid {
			return ErrSessionNotRegistered
		}
		return nil
	}, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var entry qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &entry); err != nil {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			if err := entry.validate(key); err != nil {
				return nil, false, err
			}
			if entry.ArchiveSessionID != id {
				return nil, false, ErrSessionIdentityConflict
			}
			if entry.Reservation == "" {
				return nil, false, nil
			}
		}
		return indexEntry(key, id), true, nil
	})
	if err != nil {
		return err
	}
	return s.indexStep("committed")
}

// removeSessionIndex runs under the removed archive ID's request lock. Neither
// namespace is deleted when another registration replaced its mapping.
func (s *Store) removeSessionIndex(key agentmeta.SessionKey, id string) error {
	paths := []string{qualifiedSessionIndexPath(s.home, key), nativeSessionIndexPath(s.home, key.NativeID)}
	for i, path := range paths {
		var entryID string
		if i == 0 {
			var qualified qualifiedSessionIndexEntry
			err := local.Read(path, &qualified)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if qualified.Agent != key.Agent || qualified.NativeID != key.NativeID {
				continue
			}
			entryID = qualified.ArchiveSessionID
		} else {
			var legacy sessionIndexEntry
			err := local.Read(path, &legacy)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			entryID = legacy.ArchiveSessionID
		}
		if entryID != id {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) indexStep(step string) error {
	if s.onIndexStep != nil {
		return s.onIndexStep(step)
	}
	return nil
}

func (s *Store) writeIndexUnderRequestLock(id, path string, check func() error, change func(fileSnapshot) (any, bool, error)) error {
	indexStore := *s
	indexStore.onWriteSync = s.onIndexSync
	indexStore.indexSnapshots = true
	return indexStore.writeUnderRequestLock(id, path, check, change)
}

func (s *Store) adoptLegacySessionIndex(key agentmeta.SessionKey) (string, bool, error) {
	legacy, adopted, err := s.legacySessionID(key)
	if err != nil {
		return "", false, err
	}
	if adopted {
		err := s.writeIndexUnderRequestLock(legacy, qualifiedSessionIndexPath(s.home, key), func() error {
			valid, err := s.matchingRegistration(key, legacy)
			if err != nil {
				return err
			}
			if !valid {
				return errIndexMoved
			}
			return nil
		}, func(current fileSnapshot) (any, bool, error) {
			if current.found {
				return nil, false, errIndexMoved
			}
			return indexEntry(key, legacy), true, nil
		})
		if err != nil {
			return "", false, err
		}
		if err := s.indexStep("adoption"); err != nil {
			return "", false, err
		}
		return legacy, true, nil
	}
	return "", false, nil
}

func (s *Store) replaceableReservation(key agentmeta.SessionKey, existing qualifiedSessionIndexEntry) error {
	if existing.Absent {
		return nil
	}
	valid, err := s.matchingRegistration(key, existing.ArchiveSessionID)
	if err != nil {
		return err
	}
	if valid || existing.Reservation != "" {
		return errIndexMoved
	}
	return nil
}
