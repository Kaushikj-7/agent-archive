package discovery

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// RefusedStartLookback is how far before discovery's start floor
// (config.DiscoveryStartFloor) a refused session is remembered: the 7 days
// setup imports (backfill --since 7d). status names a local day, so the
// scan keeps one more day and status narrows to that day's midnight.
const RefusedStartLookback = 7 * 24 * time.Hour

const (
	refusedStartsName = "discovery-refused-starts.json"
	// maxRefusedStarts bounds the record. It holds only top-level sessions
	// from about one week, far fewer than this; past it the earliest
	// starts are dropped.
	maxRefusedStarts = 256
	// maxRefusedLocator keeps the record's size bounded with it: a session
	// whose source path is longer is not recorded.
	maxRefusedLocator     = 1024
	maxRefusedStartsBytes = 512 * 1024
	// maxRefusedProofs bounds the native admission proofs one pass runs for
	// refused sessions not yet recorded; the rest wait for later passes.
	maxRefusedProofs = 16
)

// RefusedStart is one top-level session discovery classified as capturable
// and refused as start_not_authorized because it started shortly before the
// start floor, and that was not archived when it was refused. It holds no
// transcript content: the identity status checks against registrations and
// removals, the native start, and the source the catalog observed it at.
type RefusedStart struct {
	Agent           string    `json:"agent"`
	NativeSessionID string    `json:"native_session_id"`
	StartedAt       time.Time `json:"started_at"`
	Locator         string    `json:"locator"`
}

type refusedStartsFile struct {
	Version  int            `json:"version"`
	Sessions []RefusedStart `json:"sessions"`
}

// ReadRefusedStarts reads the sessions the scans recorded as refused for
// starting shortly before discovery's start floor, or none before the first
// scan that records any.
func ReadRefusedStarts(home string) ([]RefusedStart, error) {
	f, err := os.Open(filepath.Join(home, refusedStartsName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxRefusedStartsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxRefusedStartsBytes {
		return nil, errors.New("discovery refused-start record is oversized")
	}
	var file refusedStartsFile
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, err
	}
	if file.Version != 1 || len(file.Sessions) > maxRefusedStarts {
		return nil, errors.New("invalid discovery refused-start record version or limits")
	}
	return file.Sessions, nil
}

// refusedStarts is a scan's copy of the record. Scans are bounded, so one
// pass decides only the sessions it reaches: those keep or lose their entry
// by what admission decided now, and the rest keep theirs.
type refusedStarts struct {
	floor    time.Time
	sessions map[string]RefusedStart
	changed  bool
	// proofs counts the native admission proofs this pass ran for refusals.
	proofs int
}

// admissionRefusedStarts is the record for a pass that decides admissions,
// or nil for a native-only or inventory-only pass, which decides none.
func admissionRefusedStarts(home string, cfg config.Config, agent string, o Options) *refusedStarts {
	if o.nativeOnly || o.inventoryOnly {
		return nil
	}
	return loadRefusedStarts(home, cfg, agent)
}

func loadRefusedStarts(home string, cfg config.Config, agent string) *refusedStarts {
	floor, _ := cfg.DiscoveryStartFloor(agent)
	// An unreadable record is rebuilt from what the scans see next.
	sessions, err := ReadRefusedStarts(home)
	byKey := make(map[string]RefusedStart, len(sessions))
	for _, session := range sessions {
		byKey[refusedStartKey(session.Agent, session.NativeSessionID)] = session
	}
	return &refusedStarts{floor: floor, sessions: byKey, changed: err != nil}
}

func refusedStartKey(agent, native string) string { return agent + "\x00" + native }

// refuse records a top-level candidate refused for its start, when it
// started within the lookback before the floor. A subagent goes with its
// parent, as backfill imports it.
func (r *refusedStarts) refuse(candidate Candidate, loc string) {
	if r == nil {
		return
	}
	if !r.eligible(candidate, loc) {
		r.settle(candidate)
		return
	}
	session := RefusedStart{Agent: candidate.Agent, NativeSessionID: candidate.NativeSessionID, StartedAt: candidate.StartedAt.UTC(), Locator: loc}
	key := refusedStartKey(candidate.Agent, candidate.NativeSessionID)
	if r.sessions[key] != session {
		r.sessions[key] = session
		r.changed = true
	}
}

// eligible reports whether a refused candidate is one the record keeps: a
// top-level session that started within the lookback before the floor.
func (r *refusedStarts) eligible(candidate Candidate, loc string) bool {
	return !r.floor.IsZero() && !candidate.NativeChild && candidate.ParentNativeID == "" && len(loc) <= maxRefusedLocator && r.inWindow(candidate.StartedAt)
}

// recorded reports whether the record already holds the candidate at loc,
// which it does only once the candidate's native admission proof passed.
func (r *refusedStarts) recorded(candidate Candidate, loc string) bool {
	session, found := r.sessions[refusedStartKey(candidate.Agent, candidate.NativeSessionID)]
	return found && session.Locator == loc
}

// settle drops a candidate whose admission was decided otherwise: admitted,
// already archived, removed, or outside the included projects.
func (r *refusedStarts) settle(candidate Candidate) {
	if r == nil {
		return
	}
	key := refusedStartKey(candidate.Agent, candidate.NativeSessionID)
	if _, found := r.sessions[key]; found {
		delete(r.sessions, key)
		r.changed = true
	}
}

func (r *refusedStarts) inWindow(started time.Time) bool {
	return started.Before(r.floor) && !started.Before(r.floor.Add(-RefusedStartLookback-24*time.Hour))
}

// save drops sessions whose rollout is gone or that fall outside the current
// window, keeps the latest starts within the bound, and writes the record
// when anything changed.
func (r *refusedStarts) save(home string, cache map[string]cached) error {
	if r == nil {
		return nil
	}
	sessions := make([]RefusedStart, 0, len(r.sessions))
	for key, session := range r.sessions {
		if sourceGone(session.Locator, cache) || r.floor.IsZero() || !r.inWindow(session.StartedAt) {
			delete(r.sessions, key)
			r.changed = true
			continue
		}
		sessions = append(sessions, session)
	}
	if !r.changed {
		return nil
	}
	slices.SortFunc(sessions, func(a, b RefusedStart) int {
		if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(refusedStartKey(a.Agent, a.NativeSessionID), refusedStartKey(b.Agent, b.NativeSessionID))
	})
	for _, dropped := range sessions[min(len(sessions), maxRefusedStarts):] {
		delete(r.sessions, refusedStartKey(dropped.Agent, dropped.NativeSessionID))
	}
	sessions = sessions[:min(len(sessions), maxRefusedStarts)]
	path := filepath.Join(home, refusedStartsName)
	if len(sessions) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := local.Write(path, refusedStartsFile{Version: 1, Sessions: sessions}); err != nil {
		return err
	}
	r.changed = false
	return nil
}

// sourceGone reports whether a recorded session's rollout no longer exists.
// The catalog cache is bounded and evicts sources that still exist, so a
// locator it does not hold is checked on disk: only a missing file drops the
// session, and any other error keeps it. A save costs one lstat per recorded
// session the cache does not hold.
func sourceGone(locator string, cache map[string]cached) bool {
	if _, observed := cache[locator]; observed {
		return false
	}
	_, err := os.Lstat(locator)
	return errors.Is(err, os.ErrNotExist)
}
