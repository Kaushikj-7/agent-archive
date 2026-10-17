package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const sessionIndexMarkerFile = "session-index.json"

type sessionIndexMarker struct {
	Version          int    `json:"version"`
	Complete         bool   `json:"complete"`
	Generation       string `json:"generation,omitempty"`
	MembershipFenced bool   `json:"membership_fenced,omitempty"`
	PackedEpoch      string `json:"packed_epoch,omitempty"`
	PackedRevision   string `json:"packed_revision,omitempty"`
	PackedInventory  string `json:"packed_inventory,omitempty"`
}

func (s *Store) sessionIndexMissAllowed() error {
	var marker sessionIndexMarker
	err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !recoveryMarkerVersion(marker.Version) || !marker.Complete {
		return ErrSessionIndexRecoveryRequired
	}
	if marker.MembershipFenced {
		if _, err := s.sessionMembershipRevision(); err != nil {
			return err
		}
	}
	return nil
}

// MarkSessionIndexRecoveryNeeded records known damaged or incomplete migration
// evidence. It does not claim released old binaries understand this marker.
func (s *Store) MarkSessionIndexRecoveryNeeded() error {
	_, err := s.beginSessionIndexRecovery()
	return err
}

// Recovery requests already hold hooks.lock; retention holds collector.lock
// and may hold a request lock. This helper must not acquire hooks.lock.
func (s *Store) beginSessionIndexRecovery() (string, error) {
	generation, err := local.ID()
	if err != nil {
		return "", err
	}
	err = local.Write(filepath.Join(s.home, sessionIndexMarkerFile), s.newSessionIndexMarker(generation))
	return generation, err
}

// Collector begin commits under hooks.lock so a request's marker and key
// writes finish before the census starts. Staging and sync stay outside that
// lock; the shared request/retention helper cannot take it recursively.
func (s *Store) beginCollectorSessionIndexRecovery(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	generation, err := local.ID()
	if err != nil {
		return "", err
	}
	err = s.writeUnderLock(lockedWrite{
		lock:  func() (func(), error) { return s.namedLockWait("hooks.lock", time.Second) },
		path:  filepath.Join(s.home, sessionIndexMarkerFile),
		check: ctx.Err,
		change: func(fileSnapshot) (any, bool, error) {
			return s.newSessionIndexMarker(generation), true, nil
		},
		blind: true,
	})
	return generation, err
}

func (s *Store) completeSessionIndexRecovery(ctx context.Context, generation string, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) {
			hooks, err := local.NamedLockWait(s.home, "hooks.lock", time.Second)
			if err != nil {
				return nil, err
			}
			membership, err := local.NamedLockWait(s.home, sessionMembershipLock, time.Second)
			if err != nil {
				hooks()
				return nil, err
			}
			return func() { membership(); hooks() }, nil
		},
		check: func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, err := s.sessionMembershipRevision()
			if err != nil || current != revision {
				return ErrSessionIndexRecoveryRequired
			}
			return nil
		},
		path: filepath.Join(s.home, sessionIndexMarkerFile),
		change: func(current fileSnapshot) (any, bool, error) {
			var marker sessionIndexMarker
			if !current.found || json.Unmarshal(current.data, &marker) != nil || !recoveryMarkerVersion(marker.Version) || marker.Complete || marker.Generation != generation {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			marker.Complete = true
			marker.MembershipFenced = revision != ""
			return marker, true, nil
		},
	})
}

// RequestSessionIndexRecovery retains the damaged key so a complete census
// can durably record an absent result, without rescanning on a later hook.
// The caller holds hooks.lock; no native identity is used as a filename.
func (s *Store) RequestSessionIndexRecovery(key agentmeta.SessionKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := s.guardNativeReservationRequest(key); err != nil {
		return err
	}
	// The same retained request is already covered by the incomplete census,
	// including an absence applied before its final certificate. Retaining that
	// request does not authorize the miss: readers still require Complete.
	var prior qualifiedSessionIndexEntry
	readErr := readRecoveryJSON(qualifiedSessionIndexPath(s.home, key), &prior)
	var marker sessionIndexMarker
	if readErr == nil && prior.Version == 1 && prior.Agent == key.Agent && prior.NativeID == key.NativeID && (prior.Recovery != prior.Absent) && prior.ArchiveSessionID == "" && prior.Reservation == "" && !prior.Conflict && readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker) == nil && recoveryMarkerVersion(marker.Version) {
		if !marker.Complete && marker.Generation != "" {
			return nil
		}
		// A retained absence request under a complete census is not damage to
		// the census. It is already scheduled while a token is pending; with
		// the token lost or certified it is scheduled again, since its entry
		// has refused every later registration of key and the next slice
		// enumerates after reading the fresh token.
		if marker.Complete && marker.Version == 1 && prior.Recovery {
			if _, pending, err := s.readAbsenceRequest(); err == nil && pending {
				return nil
			}
			return s.retainAbsenceRequest(key)
		}
	}
	if handled, err := s.requestDuringPendingCensus(key); handled || err != nil {
		return err
	}
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		return err
	}
	if err := s.indexStep("recovery-request-marked"); err != nil {
		return err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if refusal := nativeReservationRepairError(entry, err); refusal != nil {
		return refusal
	}
	if err == nil && found && !entry.Absent {
		valid, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		if err == nil && valid {
			return nil
		}
	}
	return local.Write(qualifiedSessionIndexPath(s.home, key), qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Recovery: true})
}

// requestDuringPendingCensus keeps a request whose hook was refused only
// because a census is pending from restarting that census. A new generation
// fences the census's completion, so on a busy machine refused session
// starts and subagents, and each SubagentStop's healthy parent, could keep
// recovery from finishing. While the marker is incomplete no miss is allowed,
// so no registration of a key without an owner is admitted after the census
// enumerates: an owned key needs nothing, and a key with no entry or a
// certified absence is retained with an absence token. The census certifies
// that entry, or the next slice does when the census listing missed it.
// Anything else takes the full request. The caller holds hooks.lock.
func (s *Store) requestDuringPendingCensus(key agentmeta.SessionKey) (bool, error) {
	version, pending := s.pendingCensusVersion()
	if !pending {
		return false, nil
	}
	owned, unowned := s.pendingCensusRequestKind(key)
	if owned {
		return true, nil
	}
	if !unowned || version != 1 {
		return false, nil
	}
	return true, s.retainAbsenceRequest(key)
}

// pendingCensusVersion reports the marker version of a census in progress.
func (s *Store) pendingCensusVersion() (int, bool) {
	var marker sessionIndexMarker
	if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker) != nil {
		return 0, false
	}
	return marker.Version, recoveryMarkerVersion(marker.Version) && !marker.Complete && marker.Generation != ""
}

// pendingCensusRequestKind reports a key whose entry names a valid owner, or
// one with no entry or a certified absence. Damaged or uncertain evidence is
// neither and keeps the full request.
func (s *Store) pendingCensusRequestKind(key agentmeta.SessionKey) (owned, unowned bool) {
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return false, false
	}
	if found && !entry.Absent {
		valid, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		return err == nil && valid, false
	}
	return false, true
}

// sessionIndexDirectoriesLost detects total derived-index loss even when Open
// recreated empty directories. It does not infer any individual key's absence:
// the existing complete census must restore authoritative owners first.
func (s *Store) sessionIndexDirectoriesLost() (bool, error) {
	for _, name := range []string{"sessions-v1", "sessions"} {
		present, err := directoryHasEntry(filepath.Join(s.home, name))
		if err != nil || present {
			return false, err
		}
	}
	return directoryHasEntry(filepath.Join(s.home, "registrations"))
}

// directoryHasEntry reads only one directory name, without listing or reading
// its entries. A healthy empty store therefore remains a completed no-op.
func directoryHasEntry(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("session index availability cannot be checked")
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, errors.New("session index availability cannot be checked")
	}
	return len(names) > 0, nil
}

func (s *Store) sessionRegistrationInventory(ctx context.Context) (map[agentmeta.SessionKey][]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.home, "registrations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("recover session identities: %w", err)
	}
	inventory := make(map[agentmeta.SessionKey][]string, len(entries))
	// JSON decoding needs an addressable registration. Reuse that allocation
	// across the census, clearing every field so omitted fields never inherit
	// authority from a previously validated registration.
	var reg archive.SessionRegistration
	var data bytes.Buffer
	var reads []recoveryRegistrationRead
	var reader *recoveryRegistrationReader
	if len(entries) >= packedSessionIndexThreshold {
		reader = s.newRecoveryRegistrationReader()
		defer reader.close()
	}
	for i, file := range entries {
		if reader != nil && i%recoveryReadAheadWorkers == 0 {
			end := min(i+recoveryReadAheadWorkers, len(entries))
			reads = reader.readChunk(entries[i:end])
		}
		entries[i] = nil
		if filepath.Ext(file.Name()) == quarantineSuffix {
			return nil, ErrSessionIndexRecoveryRequired
		}
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reg = archive.SessionRegistration{}
		var readErr error
		if reads != nil {
			result := reads[i%recoveryReadAheadWorkers]
			reads[i%recoveryReadAheadWorkers] = recoveryRegistrationRead{}
			reg, readErr = result.reg, result.err
			if errors.Is(readErr, errRecoveryReadAheadLarge) {
				reg = archive.SessionRegistration{}
				readErr = readRecoveryRegistration(s.registrationPath(id), &data, &reg)
			}
		} else {
			readErr = readRecoveryRegistration(s.registrationPath(id), &data, &reg)
		}
		if err := readErr; err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrSessionIndexRecoveryRequired
			}
			return nil, fmt.Errorf("read registration %q: %w", id, err)
		}
		key, err := registrationKey(reg)
		if err != nil || reg.Validate() != nil || reg.ArchiveSessionID != id || !safeFileComponent(id) {
			return nil, fmt.Errorf("invalid registration identity %q: %w", id, ErrSessionIndexRecoveryRequired)
		}
		inventory[key] = append(inventory[key], id)
	}
	return inventory, nil
}

// readRecoveryRegistration retains only a bounded input buffer between census
// files. Unmarshal still validates the entire JSON document, including trailing
// input, and the caller clears the decoded registration before every read.
func readRecoveryRegistration(path string, data *bytes.Buffer, reg *archive.SessionRegistration) error {
	data.Reset()
	if data.Cap() > 64*1024 {
		*data = bytes.Buffer{}
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, err = data.ReadFrom(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	return json.Unmarshal(data.Bytes(), reg)
}

func (s *Store) recoverRegistrationOwners(key agentmeta.SessionKey, owners []string) error {
	var existing qualifiedSessionIndexEntry
	if err := s.readIndexControlJSON(qualifiedSessionIndexPath(s.home, key), &existing); errors.Is(err, errUnsupportedNativeReservation) {
		return err
	}
	if protected, err := s.recoverGenerationOwners(key, owners); protected || err != nil {
		return err
	}
	var failures []error
	if len(owners) > 1 {
		entry := qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Conflict: true}
		// Registration additions are serialized by hooks.lock for live/import
		// writers. Recovery does not hold that lock across its census or syncs.
		err := s.writeIndexUnderRequestLock(owners[0], qualifiedSessionIndexPath(s.home, key), func() error {
			for _, id := range owners {
				valid, err := s.matchingRegistration(key, id)
				if err != nil {
					return err
				}
				if !valid {
					return ErrSessionIndexRecoveryRequired
				}
			}
			return nil
		}, func(fileSnapshot) (any, bool, error) { return entry, true, nil })
		if err != nil {
			failures = append(failures, err)
		}
		return errors.Join(failures...)
	}
	id := owners[0]
	err := s.writeIndexUnderRequestLock(id, qualifiedSessionIndexPath(s.home, key), func() error {
		valid, err := s.matchingRegistration(key, id)
		if err != nil {
			return err
		}
		if !valid {
			return ErrSessionIndexRecoveryRequired
		}
		return nil
	}, func(current fileSnapshot) (any, bool, error) {
		// Existing valid reservations/committed registrations belonging to another
		// archive ID may have arrived after enumeration: never overwrite them.
		if current.found {
			var prior qualifiedSessionIndexEntry
			if jsonErr := json.Unmarshal(current.data, &prior); jsonErr == nil && prior.validate(key) == nil {
				// A fully committed exact owner needs validation, not another durable
				// rewrite. Reservations and uncertain/conflicting entries still repair.
				if prior.ArchiveSessionID == id && prior.Reservation == "" && !prior.Conflict && !prior.Recovery && !prior.Absent {
					return nil, false, nil
				}
				if prior.ArchiveSessionID != "" && prior.ArchiveSessionID != id {
					valid, err := s.matchingRegistration(key, prior.ArchiveSessionID)
					if err != nil {
						return nil, false, err
					}
					if valid || prior.Reservation != "" {
						return nil, false, ErrSessionIdentityConflict
					}
				}
			}
		}
		return indexEntry(key, id), true, nil
	})
	if err != nil {
		failures = append(failures, err)
	}
	if err := s.indexStep("recovery-entry"); err != nil {
		return err
	}
	return errors.Join(failures...)
}

func (s *Store) recoverCandidateIndex(ctx context.Context, candidate SubagentCandidate) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
	if err != nil || !safeFileComponent(candidate.ArchiveSessionID) {
		return ErrSessionIndexRecoveryRequired
	}
	entry, found, err := s.readQualifiedIndex(key)
	if errors.Is(err, errUnsupportedNativeReservation) {
		return err
	}
	if err != nil && !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		return err
	}
	if found && !entry.Absent {
		if entry.ArchiveSessionID != candidate.ArchiveSessionID {
			return ErrSessionIdentityConflict
		}
		return nil
	}
	parentKey, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID)
	if err != nil {
		return err
	}
	parent, valid, err := s.LoadRegistration(candidate.ParentArchiveSessionID)
	if err != nil {
		return err
	}
	if !valid {
		return nil
	} // A candidate never recreates its missing parent.
	actual, err := registrationKey(parent)
	if err != nil || actual != parentKey || parent.ArchiveSessionID != candidate.ParentArchiveSessionID {
		return ErrSessionIdentityConflict
	}
	owner, err := local.ID()
	if err != nil {
		return err
	}
	next := indexEntry(key, candidate.ArchiveSessionID)
	next.Reservation = owner
	if err := s.writeIndexUnderRequestLock(candidate.ArchiveSessionID, qualifiedSessionIndexPath(s.home, key), func() error {
		current, present, err := readJSON[SubagentCandidate](s.subagentCandidatePath(candidate.ArchiveSessionID))
		if err != nil {
			return err
		}
		if !present || current.NativeSessionID != key.NativeID || current.ParentArchiveSessionID != candidate.ParentArchiveSessionID || agentmeta.Canonical(agentmeta.Builtins(), current.Harness.Name) != string(key.Agent) {
			return ErrSessionIndexRecoveryRequired
		}
		return nil
	}, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var latest qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &latest); err == nil && latest.validate(key) == nil && !latest.Absent {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
		}
		return next, true, nil
	}); err != nil {
		return err
	}
	return nil
}

// fence, when set, runs under hooks.lock right before an absence is written;
// an error from it leaves the request in place and is returned as is.
func (s *Store) recoverRequestedMiss(ctx context.Context, file os.DirEntry, inventory map[agentmeta.SessionKey][]string, candidates map[agentmeta.SessionKey][]SubagentCandidate, fence func() error) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
		return nil
	}
	path := filepath.Join(s.home, "sessions-v1", file.Name())
	var entry qualifiedSessionIndexEntry
	if err := local.Read(path, &entry); err != nil {
		return ErrSessionIndexRecoveryRequired
	}
	if !entry.Recovery {
		return nil
	}
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	if err := key.Validate(); err != nil {
		return err
	}
	if path != qualifiedSessionIndexPath(s.home, key) || entry.Version != 1 {
		return ErrSessionIndexRecoveryRequired
	}
	registrationOwners := inventory[key]
	if len(registrationOwners) != 0 {
		// A new request can replace an already-applied owner's damaged index
		// while an equivalent registration census retains its owner offset.
		// Repair from that complete authority instead of stranding the request
		// or treating the owned key as an absent identity.
		if err := s.recoverRegistrationOwners(key, registrationOwners); err != nil {
			return err
		}
	}
	if owners := candidates[key]; len(owners) != 0 {
		// Candidate progress can also precede a new exact-key request. A
		// retained candidate is reservation evidence, never an absent key.
		for _, candidate := range owners {
			if err := s.recoverCandidateIndex(ctx, candidate); err != nil {
				return err
			}
		}
		return nil
	}
	if len(registrationOwners) != 0 {
		return nil
	}
	// Capture/import registration is serialized by hooks.lock. This narrow
	// application lock never covers the inventory enumeration or disk sync.
	err := s.writeUnderLock(lockedWrite{
		lock:  func() (func(), error) { return local.NamedLockWait(s.home, "hooks.lock", time.Second) },
		path:  path,
		check: fence,
		change: func(current fileSnapshot) (any, bool, error) {
			var latest qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &latest); err != nil {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			if !latest.Recovery {
				return nil, false, nil
			}
			return qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Absent: true}, true, nil
		},
	})
	if err != nil {
		return err
	}
	return nil
}

// Retain recorded membership history when a request or collector changes only
// the recovery generation. A missing recorded revision must never become legacy.
func (s *Store) newSessionIndexMarker(generation string) sessionIndexMarker {
	fenced := false
	var prior sessionIndexMarker
	if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &prior) == nil {
		fenced = prior.MembershipFenced
	}
	var cursor sessionRecoveryCursor
	if readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor) == nil && cursor.Revision != "" {
		fenced = true
	}
	if _, err := os.Stat(filepath.Join(s.home, sessionMembershipFile)); err == nil {
		fenced = true
	}
	if prior.Version == 2 {
		inventory := prior.PackedInventory
		if prior.Complete && !s.packedOverlaysHealthy(prior) {
			inventory = ""
		}
		return sessionIndexMarker{Version: 2, Generation: generation, MembershipFenced: fenced, PackedEpoch: prior.PackedEpoch, PackedRevision: prior.PackedRevision, PackedInventory: inventory}
	}
	return sessionIndexMarker{Version: 1, Generation: generation, MembershipFenced: fenced}
}
