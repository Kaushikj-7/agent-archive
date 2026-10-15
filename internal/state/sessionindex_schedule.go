package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

const sessionRecoveryCursorFile = "session-index-recovery.json"

// SessionIndexRecoverySlice is the scheduled recovery allowance. Enumeration
// remains complete on each slice; the expensive derived-index application resumes.
const SessionIndexRecoverySlice = 4 * time.Second

// SessionIndexRecoveryInterrupted reports ordinary cancellation or expiry,
// including wrappers, only when no checkpoint or other failure is joined to it.
func SessionIndexRecoveryInterrupted(err error) bool {
	return sessionIndexRecoveryInterrupted(err, 0)
}

func sessionIndexRecoveryInterrupted(err error, depth int) bool {
	if err == nil || depth >= 32 {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !sessionIndexRecoveryInterrupted(cause, depth+1) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return sessionIndexRecoveryInterrupted(cause, depth+1)
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type sessionRecoveryCursor struct {
	Checksum       string `json:"checksum"`
	Version        int    `json:"version"`
	Generation     string `json:"generation"`
	Revision       string `json:"revision,omitempty"`
	Inventory      string `json:"inventory"`
	Phase          int    `json:"phase"`
	Offset         int    `json:"offset"`
	PhaseInventory string `json:"phase_inventory,omitempty"`
}

func inventoryFingerprint(inventory map[agentmeta.SessionKey][]string) ([]agentmeta.SessionKey, string) {
	keys := make([]agentmeta.SessionKey, 0, len(inventory))
	for key := range inventory {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Agent != keys[j].Agent {
			return keys[i].Agent < keys[j].Agent
		}
		return keys[i].NativeID < keys[j].NativeID
	})
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for _, key := range keys {
		_ = encoder.Encode(key)
		_ = encoder.Encode(inventory[key])
	}
	return keys, hex.EncodeToString(h.Sum(nil))
}

func phaseFingerprint(value any) string {
	h := sha256.New()
	encoder := json.NewEncoder(h)
	switch values := value.(type) {
	case []SubagentCandidate:
		for _, candidate := range values {
			_ = encoder.Encode(candidate)
		}
	case []os.DirEntry:
		for _, entry := range values {
			_ = encoder.Encode(entry.Name())
		}
	case []string:
		for _, name := range values {
			_ = encoder.Encode(name)
		}
	default:
		_ = encoder.Encode(value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecoverSessionIndexScheduled resumes one bounded application slice. false
// means more work remains and is never an absence certificate. The caller owns
// collector.lock; requests and registration mutations may still run concurrently.
func (s *Store) RecoverSessionIndexScheduled(ctx context.Context, allowance time.Duration) (bool, error) {
	if allowance <= 0 || allowance > SessionIndexRecoverySlice {
		return false, errors.New("session index recovery allowance must be positive and at most four seconds")
	}
	return s.recoverSessionIndexSlice(ctx, allowance)
}

func (s *Store) recoverSessionIndexSlice(ctx context.Context, allowance time.Duration) (bool, error) {
	if err := s.ResumeGenerationRecoveries(ctx); err != nil {
		return false, err
	}
	cursor, complete, err := s.prepareRecoveryCursor(ctx)
	if err != nil {
		return false, err
	}
	if complete {
		// A complete census stays authoritative for every other key while
		// discovery's per-key absence requests are certified.
		return s.certifyRequestedAbsences(ctx, allowance)
	}
	revision, err := s.ensureSessionMembershipRevision()
	if err != nil {
		return false, err
	}
	inventory, err := s.sessionRegistrationInventory(ctx)
	if err != nil {
		return false, err
	}
	keys, fingerprint := inventoryFingerprint(inventory)
	if err := s.indexStep("recovery-enumerated"); err != nil {
		return false, err
	}
	if cursor.Revision != revision || cursor.Inventory != fingerprint {
		cursor.Revision, cursor.Inventory = revision, fingerprint
		cursor.Phase, cursor.Offset, cursor.PhaseInventory = 0, 0, ""
	}
	// Complete registration validation is mandatory before application. Charge
	// one aggregate application allowance after that census; the caller's
	// context independently bounds the whole recovery stage.
	deadline := time.Now().Add(allowance)

	var candidates []SubagentCandidate
	var currentMarker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &currentMarker); err != nil {
		return false, err
	}
	packed := currentMarker.Version == 2 || cursor.Version == 2 || len(keys) >= packedSessionIndexThreshold
	if packed {
		var complete bool
		candidates, complete, err = s.applyPackedRecovery(ctx, &cursor, keys, inventory, revision, fingerprint, deadline)
		if err != nil || !complete {
			return false, err
		}

	} else {
		var complete bool
		candidates, complete, err = s.applyOrdinaryRecovery(ctx, &cursor, keys, inventory, deadline)
		if err != nil || !complete {
			return false, err
		}
	}
	candidateInventory, err := subagentCandidateInventory(candidates)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	fingerprint = phaseFingerprint(entries)
	if cursor.PhaseInventory != fingerprint || cursor.Offset > len(entries) {
		cursor.Offset = 0
	}
	cursor.PhaseInventory = fingerprint
	// The fingerprint covers the full sorted listing before consumed directory
	// entries are released. Only unapplied entries are needed by this slice.
	clear(entries[:cursor.Offset])
	complete, err = s.applyRecoveryPhase(ctx, &cursor, deadline, len(entries), func(i int) error {
		entry := entries[i]
		entries[i] = nil
		return s.recoverRequestedMiss(ctx, entry, inventory, candidateInventory, nil)
	})
	if err != nil || !complete {
		return false, err
	}
	if err := s.indexStep("recovery-completing"); err != nil {
		return false, err
	}
	if cursor.Version == 2 {
		if err := s.clearPackedExpiryProofs(); err != nil {
			return false, err
		}
	}
	if err := s.completeSessionIndexRecovery(ctx, cursor.Generation, revision); err != nil {
		return false, err
	}
	if err := s.indexStep("recovery-complete"); err != nil {
		return false, err
	}
	return true, nil
}

func subagentCandidateInventory(candidates []SubagentCandidate) (map[agentmeta.SessionKey][]SubagentCandidate, error) {
	inventory := make(map[agentmeta.SessionKey][]SubagentCandidate, len(candidates))
	for _, candidate := range candidates {
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil {
			return nil, err
		}
		inventory[key] = append(inventory[key], candidate)
	}
	return inventory, nil
}

func (s *Store) applyOrdinaryRecovery(ctx context.Context, cursor *sessionRecoveryCursor, keys []agentmeta.SessionKey, inventory map[agentmeta.SessionKey][]string, deadline time.Time) ([]SubagentCandidate, bool, error) {

	if cursor.Phase == 0 {
		if cursor.Offset > len(keys) {
			cursor.Offset = 0
		}
		complete, err := s.applyRecoveryPhase(ctx, cursor, deadline, len(keys), func(i int) error { return s.recoverRegistrationOwners(keys[i], inventory[keys[i]]) })
		if err != nil || !complete {
			return nil, false, err
		}
		cursor.Phase, cursor.Offset = 1, 0
	}
	// Requested keys may refer to an already-applied candidate whose index
	// changed after its checkpoint. Keep complete candidate authority available
	// in phase two, including slices that resume there directly.
	candidates, err := s.LoadSubagentCandidates()
	if err != nil {
		return nil, false, err
	}
	if cursor.Phase == 1 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ArchiveSessionID < candidates[j].ArchiveSessionID })
		fingerprint := phaseFingerprint(candidates)
		if cursor.PhaseInventory != fingerprint || cursor.Offset > len(candidates) {
			cursor.Offset = 0
		}
		cursor.PhaseInventory = fingerprint
		complete, err := s.applyRecoveryPhase(ctx, cursor, deadline, len(candidates), func(i int) error { return s.recoverCandidateIndex(ctx, candidates[i]) })
		if err != nil || !complete {
			return nil, false, err
		}
		cursor.Phase, cursor.Offset, cursor.PhaseInventory = 2, 0, ""
	}
	return candidates, true, nil
}

func (s *Store) applyPackedRecovery(ctx context.Context, cursor *sessionRecoveryCursor, keys []agentmeta.SessionKey, inventory map[agentmeta.SessionKey][]string, revision, fingerprint string, deadline time.Time) ([]SubagentCandidate, bool, error) {

	candidates, err := s.LoadSubagentCandidates()
	if err != nil {
		return candidates, false, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ArchiveSessionID < candidates[j].ArchiveSessionID })
	combined := phaseFingerprint([]string{fingerprint, phaseFingerprint(candidates)})
	var prior sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &prior); err != nil {
		return candidates, false, err
	}
	marker, err := s.preparePackedSessionIndex(ctx, revision, combined)
	if err != nil {
		return candidates, false, err
	}
	if cursor.Version != 2 || prior.PackedEpoch != marker.PackedEpoch {
		cursor.Version = 2
		cursor.Phase = 0
		cursor.Offset = 0
		cursor.PhaseInventory = ""
	}

	// A durable offset alone cannot authorize lost or damaged aggregate files.
	// Revalidate the retained prefix and restart at the first damaged shard.
	prefix := packedSessionIndexShards
	if cursor.Phase == 0 {
		prefix = cursor.Offset
	}
	if prefix > packedSessionIndexShards {
		prefix = 0
		cursor.Offset = 0
	}
	if first, err := s.validatePackedPrefix(marker, prefix); err != nil {
		cursor.Phase = 0
		cursor.Offset = first
		cursor.PhaseInventory = ""
	}
	if cursor.Phase == 0 {
		owners, children, err := packedRecoverySources(inventory, candidates)
		if err != nil {
			return candidates, false, err
		}
		if cursor.Offset > packedSessionIndexShards {
			cursor.Offset = 0
		}
		complete, err := s.applyPackedShardPhase(ctx, cursor, deadline, marker, owners, children)
		if err != nil || !complete {
			return candidates, false, err
		}
		cursor.Phase = 1
		cursor.Offset = 0
		cursor.PhaseInventory = ""
	}
	if cursor.Phase == 1 {
		var fallbackOwners []agentmeta.SessionKey
		var fallbackCandidates []SubagentCandidate
		var fallback [packedSessionIndexShards]bool
		for i := range packedSessionIndexShards {
			reader, err := s.openPackedIndex(packedShardName(i), marker)
			if err != nil {
				return candidates, false, err
			}
			fallback[i] = reader.fallback
			_ = reader.file.Close()
		}
		for _, key := range keys {
			shard, _ := hex.DecodeString(packedIndexHash(key)[:2])
			if fallback[int(shard[0])] {
				fallbackOwners = append(fallbackOwners, key)
			}
		}
		for _, candidate := range candidates {
			key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
			if err != nil {
				return candidates, false, err
			}
			shard, _ := hex.DecodeString(packedIndexHash(key)[:2])
			if fallback[int(shard[0])] {
				fallbackCandidates = append(fallbackCandidates, candidate)
			}
		}
		complete, err := s.applyRecoveryPhase(ctx, cursor, deadline, len(fallbackOwners)+len(fallbackCandidates), func(i int) error {
			if i < len(fallbackOwners) {
				key := fallbackOwners[i]
				return s.recoverRegistrationOwners(key, inventory[key])
			}
			return s.recoverCandidateIndex(ctx, fallbackCandidates[i-len(fallbackOwners)])
		})
		if err != nil || !complete {
			return candidates, false, err
		}
		cursor.Phase, cursor.Offset, cursor.PhaseInventory = 2, 0, ""
	}

	return candidates, true, nil
}

func (s *Store) prepareRecoveryCursor(ctx context.Context) (sessionRecoveryCursor, bool, error) {
	var marker sessionIndexMarker
	err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if err == nil && marker.Version > 2 {
		return sessionRecoveryCursor{}, false, ErrSessionIndexRecoveryRequired
	}
	if err == nil && recoveryMarkerVersion(marker.Version) && marker.Complete {
		complete, healthErr := s.completedRecoveryCurrent(marker)
		if healthErr != nil {
			return sessionRecoveryCursor{}, false, healthErr
		}
		if complete {
			return sessionRecoveryCursor{}, true, nil
		}
		err = ErrSessionIndexRecoveryRequired
	}
	var cursor sessionRecoveryCursor
	cursorErr := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor)
	validCursor := cursorErr == nil && cursor.validChecksum() && recoveryMarkerVersion(cursor.Version) && cursor.Generation != "" && cursor.Phase >= 0 && cursor.Phase <= 2 && cursor.Offset >= 0
	if err != nil || !recoveryMarkerVersion(marker.Version) || marker.Generation == "" || !validCursor || cursor.Generation != marker.Generation {
		// New requests fence the final certificate, but do not change owner
		// application already covered by an equivalent complete inventory.
		// The fresh census below must match both membership and fingerprint
		// before retaining phase zero/one progress; candidate facts also match
		// their phase fingerprint. Requested misses restart for the new generation.
		resume := err == nil && recoveryMarkerVersion(marker.Version) && !marker.Complete && marker.Generation != "" && validCursor
		if err := s.indexStep("recovery-begin"); err != nil {
			return sessionRecoveryCursor{}, false, err
		}
		generation, err := s.beginCollectorSessionIndexRecovery(ctx)
		if err != nil {
			return sessionRecoveryCursor{}, false, err
		}
		if resume {
			cursor.Generation = generation
			if cursor.Phase == 2 {
				cursor.Offset, cursor.PhaseInventory = 0, ""
			}
		} else {
			cursor = sessionRecoveryCursor{Version: 1, Generation: generation}
		}
		if err := s.indexStep("recovery-incomplete"); err != nil {
			return sessionRecoveryCursor{}, false, err
		}
	}
	return cursor, false, nil
}

func (s *Store) completedRecoveryCurrent(marker sessionIndexMarker) (bool, error) {
	var revision string
	if marker.MembershipFenced || marker.Version == 2 {
		var err error
		revision, err = s.sessionMembershipRevision()
		if err != nil {
			return false, err
		}
		if marker.Version == 2 && revision == "" {
			return false, ErrSessionIndexRecoveryRequired
		}
	}
	complete, healthErr := s.completedRecoveryHealthy(marker)
	if healthErr != nil {
		return false, healthErr
	}
	// A completed packed census certifies its recorded membership only.
	// A valid newer revision must restart the authoritative recovery in the caller.
	return complete && (marker.Version != 2 || revision == marker.PackedRevision), nil
}

func (s *Store) completedRecoveryHealthy(marker sessionIndexMarker) (bool, error) {
	invalidPacked := false
	if marker.Version == 2 {
		if _, err := s.validatePackedPrefix(marker, packedSessionIndexShards); err == nil && s.packedOverlaysHealthy(marker) {
			if err := s.reconcilePackedExpiryProofs(marker); err != nil {
				return false, err
			}
			return true, nil
		}
		invalidPacked = true
	}
	lost, err := s.sessionIndexDirectoriesLost()
	return !lost && !invalidPacked, err
}

func (s *Store) saveRecoveryCursor(cursor *sessionRecoveryCursor) error {
	cursor.Checksum = ""
	cursor.Checksum = phaseFingerprint(*cursor)
	return local.Write(filepath.Join(s.home, sessionRecoveryCursorFile), *cursor)
}

func (s *Store) applyRecoveryPhase(ctx context.Context, cursor *sessionRecoveryCursor, deadline time.Time, count int, apply func(int) error) (bool, error) {
	for cursor.Offset < count {
		if !time.Now().Before(deadline) {
			return false, s.saveRecoveryCursor(cursor)
		}
		if err := ctx.Err(); err != nil {
			if checkpointErr := s.saveRecoveryCursor(cursor); checkpointErr != nil {
				return false, errors.Join(err, checkpointErr)
			}
			return false, err
		}
		if err := apply(cursor.Offset); err != nil {
			if errors.Is(err, errPackedSlicePending) {
				return false, s.saveRecoveryCursor(cursor)
			}
			return false, err
		}
		cursor.Offset++
	}
	return true, nil
}

func (cursor sessionRecoveryCursor) validChecksum() bool {
	checksum := cursor.Checksum
	cursor.Checksum = ""
	return checksum != "" && checksum == phaseFingerprint(cursor)
}

// applyPackedShardPhase retains a durable contiguous cursor prefix while at most
// two shards overlap native file and directory sync. The allowance is shared by
// the entire phase, and all publications finish before checkpoint or return.
func (s *Store) applyPackedShardPhase(ctx context.Context, cursor *sessionRecoveryCursor, deadline time.Time, marker sessionIndexMarker, owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string, children [packedSessionIndexShards][]SubagentCandidate) (bool, error) {
	for cursor.Offset < packedSessionIndexShards {
		batch := make([]packedPublication, 0, 2)
		var prepareErr error
		for i := cursor.Offset; i < min(cursor.Offset+2, packedSessionIndexShards); i++ {
			if !time.Now().Before(deadline) {
				prepareErr = errPackedSlicePending
				break
			}
			if prepareErr = ctx.Err(); prepareErr != nil {
				break
			}
			shard := packedShardName(i)
			data, err := s.preparePackedShardSlice(ctx, marker, owners[i], children[i], deadline)
			if err != nil {
				prepareErr = fmt.Errorf("packed shard %s: %w", shard, err)
				break
			}
			path := packedIndexPath(s.home, shard)
			before, err := readSnapshot(path)
			if err != nil {
				prepareErr = err
				break
			}
			batch = append(batch, packedPublication{path: path, before: before, data: data})
		}
		// An exhausted allowance is control flow, not a preparation failure.
		// Unwrap only single-cause wrappers: joined real failures must survive.
		pendingCause := prepareErr
		for errors.Unwrap(pendingCause) != nil {
			pendingCause = errors.Unwrap(pendingCause)
		}
		_, joinedCause := pendingCause.(interface{ Unwrap() []error })
		slicePending := !joinedCause && errors.Is(pendingCause, errPackedSlicePending)
		if slicePending {
			prepareErr = nil
		}
		errs := s.publishPackedBatch(ctx, marker, batch)
		for _, err := range errs {
			if err != nil {
				break
			}
			cursor.Offset++
		}
		publicationErr := errors.Join(errs...)
		if publicationErr != nil {
			// A canceled later commit must retain the already durable prefix,
			// including any checkpoint failure, after every worker is joined.
			if errors.Is(publicationErr, context.Canceled) || errors.Is(publicationErr, context.DeadlineExceeded) {
				return false, errors.Join(publicationErr, prepareErr, s.saveRecoveryCursor(cursor))
			}
			return false, errors.Join(publicationErr, prepareErr)
		}
		if slicePending {
			return false, errors.Join(ctx.Err(), s.saveRecoveryCursor(cursor))
		}
		if prepareErr != nil {
			if errors.Is(prepareErr, context.Canceled) || errors.Is(prepareErr, context.DeadlineExceeded) {
				return false, errors.Join(prepareErr, s.saveRecoveryCursor(cursor))
			}
			return false, prepareErr
		}
	}
	return true, nil
}
