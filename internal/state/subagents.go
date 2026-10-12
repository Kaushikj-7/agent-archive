package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// SubagentCandidate is local operational state written by a bounded hook.
// TranscriptPath is never copied into an archive bundle or metadata sidecar.
type SubagentCandidate struct {
	HistoricalProject      *archive.HistoricalProjectProof `json:"historical_project,omitempty"`
	ArchiveSessionID       string                          `json:"archive_session_id"`
	NativeSessionID        string                          `json:"native_session_id"`
	ParentArchiveSessionID string                          `json:"parent_archive_session_id"`
	ParentNativeSessionID  string                          `json:"parent_native_session_id"`
	ProjectID              string                          `json:"project_id"`
	ProjectRoot            string                          `json:"project_root"`
	Harness                archive.Harness                 `json:"harness"`
	AgentID                string                          `json:"agent_id"`
	TranscriptPath         string                          `json:"transcript_path"`
	ObservedAt             time.Time                       `json:"observed_at"`
	// Origin is who found the subagent: a SubagentStop hook (empty or
	// SessionOriginHook) or backfill (SessionOriginImport). Only a hook
	// candidate carries a SubagentStop lifecycle event.
	Origin archive.SessionOrigin `json:"origin,omitempty"`
	// AgentType is the subagent's type as the SubagentStop hook reported it
	// ("Explore", "general-purpose", "my-plugin:reviewer"), already passed
	// through archive.SanitizeSubagentType, or empty when unknown. It is
	// informational and local: status reports it, it is never uploaded, and
	// it never decides anything.
	AgentType          string `json:"agent_type,omitempty"`
	StagedImportSHA256 string `json:"staged_import_sha256,omitempty"`
}

var (
	// ErrSubagentCandidateIncomplete reports a candidate missing a required
	// field.
	ErrSubagentCandidateIncomplete = errors.New("subagent candidate is incomplete")
	// ErrSubagentCandidateConflict reports that an earlier candidate for the
	// same archive ID has a different path or owner, which a later one never
	// replaces.
	ErrSubagentCandidateConflict = errors.New("subagent candidate ownership changed")
)

func (s *Store) subagentCandidatePath(id string) string {
	return filepath.Join(s.home, "subagent-candidates", id+".json")
}

// SaveSubagentCandidate coalesces duplicate stop deliveries without allowing
// a later event to replace the path or ownership established by the first.
// AgentType is not part of ownership: the first non-empty one is kept.
func (s *Store) SaveSubagentCandidate(candidate SubagentCandidate) error {
	if !safeFileComponent(candidate.ArchiveSessionID) || candidate.NativeSessionID == "" || !safeFileComponent(candidate.ParentArchiveSessionID) || candidate.ParentNativeSessionID == "" || candidate.ProjectID == "" || !candidate.validProjectOwnership() || candidate.Harness.Name == "" || candidate.AgentID == "" || candidate.TranscriptPath == "" || candidate.ObservedAt.IsZero() {
		return ErrSubagentCandidateIncomplete
	}

	if _, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID); err != nil {
		return err
	}
	if _, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID); err != nil {
		return err
	}
	// The write syncs outside the candidate's lock, which a hook waits only
	// a second for (see writeUnderLock).
	return s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return s.lockSubagentCandidate(candidate.ArchiveSessionID) },
		path: s.subagentCandidatePath(candidate.ArchiveSessionID),
		change: func(current fileSnapshot) (any, bool, error) {
			merged := candidate
			if !current.found {
				return merged, true, nil
			}
			var prior SubagentCandidate
			if err := json.Unmarshal(current.data, &prior); err != nil {
				return nil, false, fmt.Errorf("read subagent candidate: %w", err)
			}
			if prior.NativeSessionID != merged.NativeSessionID || prior.ParentArchiveSessionID != merged.ParentArchiveSessionID || prior.ParentNativeSessionID != merged.ParentNativeSessionID || prior.ProjectID != merged.ProjectID || prior.ProjectRoot != merged.ProjectRoot || archive.CanonicalHarness(prior.Harness.Name) != archive.CanonicalHarness(merged.Harness.Name) || prior.AgentID != merged.AgentID || prior.TranscriptPath != merged.TranscriptPath {
				return nil, false, ErrSubagentCandidateConflict
			}
			if prior.StagedImportSHA256 != "" {
				if merged.StagedImportSHA256 != "" && merged.StagedImportSHA256 != prior.StagedImportSHA256 {
					return nil, false, ErrSubagentCandidateConflict
				}
				merged.StagedImportSHA256 = prior.StagedImportSHA256
			}
			if prior.ObservedAt.After(merged.ObservedAt) {
				merged.ObservedAt = prior.ObservedAt
			}
			// The first stop that named a type keeps it: like the path and
			// owner, a later delivery never replaces it.
			if prior.AgentType != "" {
				merged.AgentType = prior.AgentType
			}
			return merged, true, nil
		},
	})
}

// LoadSubagentCandidate reads one established child owner without enumerating
// candidates on the hook path.
func (s *Store) LoadSubagentCandidate(id string) (SubagentCandidate, bool, error) {
	if !safeFileComponent(id) {
		return SubagentCandidate{}, false, ErrSubagentCandidateIncomplete
	}
	return readJSON[SubagentCandidate](s.subagentCandidatePath(id))
}

// LoadSubagentCandidates returns every subagent candidate hooks and backfill
// have left, sorted by archive session ID. Unlike ScanSubagentCandidates it
// fails on the first one it cannot read, and never moves a file aside.
func (s *Store) LoadSubagentCandidates() ([]SubagentCandidate, error) {
	dir := filepath.Join(s.home, "subagent-candidates")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list subagent candidates: %w", err)
	}
	var out []SubagentCandidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var candidate SubagentCandidate
		if err := local.Read(filepath.Join(dir, entry.Name()), &candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Acknowledged between listing and reading.
				continue
			}
			return nil, fmt.Errorf("read subagent candidate %q: %w", entry.Name(), err)
		}
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

func (s *Store) lockSubagentCandidate(id string) (func(), error) {
	if !safeFileComponent(id) {
		return nil, errors.New("invalid subagent candidate ID")
	}
	return s.namedLockWait(subagentLockName(id), time.Second)
}

// subagentLockName is the lock file guarding one subagent candidate, relative
// to home. ForgetSession removes it with the session.
func subagentLockName(id string) string {
	return filepath.Join("request-locks", "subagent-"+id+".lock")
}

// RemoveSubagentCandidate removes one subagent candidate, under its lock.
func (s *Store) RemoveSubagentCandidate(id string) error {
	return s.removeSubagentCandidateContext(context.Background(), id)
}

func (s *Store) removeSubagentCandidateContext(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := s.lockSubagentCandidate(id)
	if err != nil {
		return err
	}
	err = s.removeSubagentCandidate(id)
	unlock()
	if err != nil {
		return err
	}
	return s.pruneAcknowledgedPackedCandidateContext(ctx, id)
}

// AcknowledgeSubagentCandidate marks a candidate handled. A later stop may
// arrive while background transcript validation is running, so it
// acknowledges only the exact observed generation, under the writer's short
// lock.
func (s *Store) AcknowledgeSubagentCandidate(expected SubagentCandidate) error {
	removed, err := func() (bool, error) {
		unlock, err := s.lockSubagentCandidate(expected.ArchiveSessionID)
		if err != nil {
			return false, err
		}
		defer unlock()
		var current SubagentCandidate
		if err := local.Read(s.subagentCandidatePath(expected.ArchiveSessionID), &current); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, nil // Also finish a prior interrupted acknowledgement.
			}
			return false, err
		}
		if !current.ObservedAt.Equal(expected.ObservedAt) || current.TranscriptPath != expected.TranscriptPath || current.NativeSessionID != expected.NativeSessionID || current.ParentArchiveSessionID != expected.ParentArchiveSessionID {
			return false, nil
		}
		return true, s.removeSubagentCandidate(expected.ArchiveSessionID)
	}()
	if err != nil || !removed {
		return err
	}
	// Collector acknowledgement owns no hooks/request lock. Native packed
	// staging and directory sync happen only after the candidate lock releases.
	return s.pruneAcknowledgedPackedCandidate(expected.ArchiveSessionID)
}

func (s *Store) pruneAcknowledgedPackedCandidate(id string) error {
	return s.pruneAcknowledgedPackedCandidateContext(context.Background(), id)
}

func (s *Store) pruneAcknowledgedPackedCandidateContext(ctx context.Context, id string) error {
	var marker sessionIndexMarker
	err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) || (err == nil && marker.Version != 2) {
		return nil
	}
	if err != nil {
		return err
	}
	if exists(s.registrationPath(id)) {
		return nil // Admitted children retain their exact packed ownership.
	}
	return s.removePackedIdentitiesContext(ctx, map[string]bool{id: true})
}

// removeSubagentCandidate removes a candidate and then its lock file, under
// that lock. Unlinking a held lock file is safe (see local.NamedLock): a
// writer waiting on it retries on a fresh file. Without this every
// candidate ever seen, the rejected ones above all, left a lock file behind
// for good.
func (s *Store) removeSubagentCandidate(id string) error {
	err := os.Remove(s.subagentCandidatePath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove subagent candidate: %w", err)
	}
	if err := os.Remove(filepath.Join(s.home, subagentLockName(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove subagent candidate lock: %w", err)
	}
	return nil
}

// removeSubagentCandidatesForSession removes the candidates naming id as the
// subagent or its parent. Another session's unreadable candidate does not
// stand in the way (the scan quarantines one that does not decode).
func (s *Store) removeSubagentCandidatesForSession(id string) error {
	ids, err := s.subagentCandidatesForSession(id)
	if err != nil {
		return err
	}
	for _, candidateID := range ids {
		// The retention caller may hold an outer request lock. Its outer
		// Forget operation prunes packed records after that lock releases.
		unlock, err := s.lockSubagentCandidate(candidateID)
		if err != nil {
			return err
		}
		err = s.removeSubagentCandidate(candidateID)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// removeSubagentCandidatesWithoutWaiting is removeSubagentCandidatesForSession
// for a caller holding the session's request lock, which hooks wait on for a
// second: it takes each candidate's lock without waiting, and removes nothing
// unless it has them all. busy reports a candidate lock held by a hook or the
// collector, at work on the session's subagents right now.
func (s *Store) removeSubagentCandidatesWithoutWaiting(id string) (busy bool, err error) {
	ids, err := s.subagentCandidatesForSession(id)
	if errors.Is(err, local.ErrBusy) {
		// The session's own candidate does not decode, and its lock is held
		// right now: a writer is replacing it.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	for _, candidateID := range ids {
		if !safeFileComponent(candidateID) {
			return false, errors.New("invalid subagent candidate ID")
		}
		unlock, err := local.NamedLock(s.home, subagentLockName(candidateID))
		if errors.Is(err, local.ErrBusy) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		unlocks = append(unlocks, unlock)
	}
	for _, candidateID := range ids {
		if err := s.removeSubagentCandidate(candidateID); err != nil {
			return false, err
		}
	}
	return false, nil
}

// subagentCandidatesForSession lists the candidates naming id as the
// subagent or its parent.
//
// Its callers hold the session's request lock, which hooks wait only a
// second for, so the scan does not wait for the lock of a candidate that
// does not decode: a held one is left for a later scan to move aside, and
// when it is the session's own the forget fails, for the next attempt to
// retry.
func (s *Store) subagentCandidatesForSession(id string) ([]string, error) {
	candidates, issues, err := s.scanSubagentCandidates(0)
	if err != nil {
		return nil, err
	}
	// The session's own candidate could not be read, so it cannot be
	// removed: forgetting the session anyway would leave a candidate that
	// registers it again once readable. A quarantined one is already gone.
	if issue := issues[id]; issue != nil && !errors.Is(issue, ErrQuarantined) {
		return nil, issue
	}
	var ids []string
	for _, candidate := range candidates {
		if candidate.ArchiveSessionID == id || candidate.ParentArchiveSessionID == id {
			ids = append(ids, candidate.ArchiveSessionID)
		}
	}
	return ids, nil
}

func (candidate SubagentCandidate) validProjectOwnership() bool {
	if candidate.HistoricalProject == nil {
		return candidate.ProjectRoot != ""
	}
	return candidate.Origin == archive.SessionOriginImport && candidate.ProjectRoot == "" && candidate.ProjectID == candidate.HistoricalProject.ProjectID && candidate.StagedImportSHA256 != ""
}
