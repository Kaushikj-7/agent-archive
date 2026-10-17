package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

// sessionIndexAbsenceFile schedules certification of discovery's per-key
// absence requests. It holds two opaque tokens and no identity.
const sessionIndexAbsenceFile = "session-index-absence.json"

// errAbsenceRequestMoved stops an absence slice whose census no longer
// covers every retained request; the next slice enumerates again.
var errAbsenceRequestMoved = errors.New("session index absence request changed")

type sessionIndexAbsenceRequest struct {
	Version   int    `json:"version"`
	Requested string `json:"requested"`
	Certified string `json:"certified,omitempty"`
}

func (r sessionIndexAbsenceRequest) pending() bool {
	return r.Requested != "" && r.Requested != r.Certified
}

func (r sessionIndexAbsenceRequest) valid() bool {
	return r.Version == 1 && safeFileComponent(r.Requested) && (r.Certified == "" || safeFileComponent(r.Certified))
}

// readAbsenceRequest returns ErrSessionIndexRecoveryRequired for damaged
// evidence; a missing file has nothing pending.
func (s *Store) readAbsenceRequest() (sessionIndexAbsenceRequest, bool, error) {
	var request sessionIndexAbsenceRequest
	err := readRecoveryJSONLimit(filepath.Join(s.home, sessionIndexAbsenceFile), &request, 512)
	if errors.Is(err, os.ErrNotExist) {
		return sessionIndexAbsenceRequest{}, false, nil
	}
	if err != nil || !request.valid() {
		return sessionIndexAbsenceRequest{}, false, ErrSessionIndexRecoveryRequired
	}
	return request, request.pending(), nil
}

// RequestSessionIndexAbsence asks the collector to certify that key has no
// registration. Unlike RequestSessionIndexRecovery it does not withdraw a
// complete census: other keys' misses stay allowed, so an unrelated hook (a
// Claude Code subagent's first SubagentStop, a new session) is not refused
// while discovery waits for its own proof. Only key itself is held until the
// collector's next recovery slice records it absent or repairs its owner.
// Damaged, owned or uncertified evidence takes the full recovery request.
// The caller holds hooks.lock.
func (s *Store) RequestSessionIndexAbsence(key agentmeta.SessionKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	var marker sessionIndexMarker
	if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Version != 1 || !marker.Complete {
		return s.RequestSessionIndexRecovery(key)
	}
	if _, found, err := s.readQualifiedIndex(key); err != nil || found {
		return s.RequestSessionIndexRecovery(key)
	}
	return s.retainAbsenceRequest(key)
}

// retainAbsenceRequest writes a fresh token and then key's retained request
// entry. The caller holds hooks.lock and has established that no registration
// of key can be admitted after the census that certifies it enumerates: that
// census is complete and the token makes the next slice enumerate again, or
// misses have been refused since the pending census began.
func (s *Store) retainAbsenceRequest(key agentmeta.SessionKey) error {
	if err := s.guardNativeReservationRequest(key); err != nil {
		return err
	}
	var prior qualifiedSessionIndexEntry
	readErr := local.Read(qualifiedSessionIndexPath(s.home, key), &prior)
	if errors.Is(readErr, errUnsupportedNativeReservation) || prior.NativeReservation != "" {
		return errors.Join(errUnsupportedNativeReservation, readErr)
	}
	// Damaged scheduling evidence is replaced: the fresh token is uncertified,
	// so the next slice still enumerates every retained request.
	request, _, err := s.readAbsenceRequest()
	if err != nil {
		request = sessionIndexAbsenceRequest{}
	}
	token, err := local.ID()
	if err != nil {
		return err
	}
	request.Version, request.Requested = 1, token
	// The token is durable before the request entry, so a collector slice
	// that read an older token cannot certify an absence it did not enumerate.
	if err := local.Write(filepath.Join(s.home, sessionIndexAbsenceFile), request); err != nil {
		return err
	}
	if err := s.indexStep("absence-request-marked"); err != nil {
		return err
	}
	return local.Write(qualifiedSessionIndexPath(s.home, key), qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Recovery: true})
}

// certifyRequestedAbsences runs after prepareRecoveryCursor found a complete
// current census. It applies retained absence requests from a complete
// registration and candidate census taken after the request token was read,
// without changing the complete marker. The caller owns collector.lock.
//
// Every absence write is fenced under hooks.lock, which requests also hold:
// the marker must still be the same complete version-1 census and the token
// unchanged. The token itself is read under hooks.lock, after its requester
// wrote both token and entry. A request therefore preceded the token read,
// its key's earlier registrations (an index lost silently) are in this
// census, and its retained entry has refused every later registration of that
// key. Owner and candidate repairs use the existing request-lock validation.
func (s *Store) certifyRequestedAbsences(ctx context.Context, allowance time.Duration) (bool, error) {
	if _, pending, err := s.readAbsenceRequest(); err == nil && !pending {
		return true, nil
	}
	request, requestErr, marker, err := s.readAbsenceSchedule()
	if err != nil {
		return false, err
	}
	if requestErr == nil && !request.pending() {
		return true, nil
	}
	if requestErr != nil || marker.Version != 1 || !marker.Complete {
		// Without a usable token only a full census covers retained requests.
		return false, s.restartForAbsenceRequests(ctx)
	}
	fence := func() error {
		var current sessionIndexMarker
		if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &current); err != nil || current.Version != 1 || !current.Complete || current.Generation != marker.Generation {
			return errAbsenceRequestMoved
		}
		latest, _, err := s.readAbsenceRequest()
		if err != nil || latest.Requested != request.Requested {
			return errAbsenceRequestMoved
		}
		return nil
	}
	inventory, err := s.sessionRegistrationInventory(ctx)
	if err != nil {
		return false, err
	}
	candidates, err := s.LoadSubagentCandidates()
	if err != nil {
		return false, err
	}
	candidateInventory, err := subagentCandidateInventory(candidates)
	if err != nil {
		return false, err
	}
	if err := s.indexStep("absence-enumerated"); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	deadline := time.Now().Add(allowance)
	for i, entry := range entries {
		entries[i] = nil
		if !time.Now().Before(deadline) {
			return false, nil
		}
		if err := s.recoverRequestedMiss(ctx, entry, inventory, candidateInventory, fence); err != nil {
			if errors.Is(err, errAbsenceRequestMoved) {
				return false, nil
			}
			return false, err
		}
	}
	return s.certifyAbsenceToken(request.Requested, fence)
}

// certifyAbsenceToken records token certified, fenced like each absence
// write. A moved token or marker leaves the work for the next slice.
func (s *Store) certifyAbsenceToken(token string, fence func() error) (bool, error) {
	err := s.writeUnderLock(lockedWrite{
		lock:  func() (func(), error) { return local.NamedLockWait(s.home, "hooks.lock", time.Second) },
		path:  filepath.Join(s.home, sessionIndexAbsenceFile),
		check: fence,
		change: func(current fileSnapshot) (any, bool, error) {
			var latest sessionIndexAbsenceRequest
			if !current.found || json.Unmarshal(current.data, &latest) != nil || latest.Requested != token {
				return nil, false, errAbsenceRequestMoved
			}
			latest.Certified = latest.Requested
			return latest, true, nil
		},
	})
	if errors.Is(err, errAbsenceRequestMoved) || errors.Is(err, errWriteOvertaken) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.indexStep("absence-certified")
}

// readAbsenceSchedule reads the token and marker under hooks.lock. A
// requester writes its token and then its entry under one hooks.lock hold,
// so every entry carrying the token read here already exists before the
// caller lists sessions-v1. requestErr reports unusable scheduling evidence;
// err reports a lock or marker failure.
func (s *Store) readAbsenceSchedule() (request sessionIndexAbsenceRequest, requestErr error, marker sessionIndexMarker, err error) {
	unlock, err := s.namedLockWait("hooks.lock", time.Second)
	if err != nil {
		return request, nil, marker, err
	}
	defer unlock()
	request, _, requestErr = s.readAbsenceRequest()
	err = readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	return request, requestErr, marker, err
}

// restartForAbsenceRequests begins a full census, which certifies every
// retained request, then retires the unusable scheduling evidence. Requests
// arriving meanwhile see the incomplete marker and take the full path.
func (s *Store) restartForAbsenceRequests(ctx context.Context) error {
	if _, err := s.beginCollectorSessionIndexRecovery(ctx); err != nil {
		return err
	}
	token, err := local.ID()
	if err != nil {
		return err
	}
	return s.writeUnderLock(lockedWrite{
		lock:  func() (func(), error) { return local.NamedLockWait(s.home, "hooks.lock", time.Second) },
		path:  filepath.Join(s.home, sessionIndexAbsenceFile),
		blind: true,
		change: func(fileSnapshot) (any, bool, error) {
			return sessionIndexAbsenceRequest{Version: 1, Requested: token, Certified: token}, true, nil
		},
	})
}
