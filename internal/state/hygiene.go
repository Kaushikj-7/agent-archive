package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrQuarantined marks a local state file that no longer decoded and was
// moved aside, renamed with quarantineSuffix, so it stops failing every pass
// (and every hook that would rewrite it). The file is kept, not deleted, for
// someone to inspect; Status.QuarantinedFiles lists it until then.
var ErrQuarantined = errors.New("unreadable local state file was moved aside")

// quarantineSuffix ends a quarantined file's name, after the time it was
// moved aside (see quarantinePath). Listings only read *.json, so a
// quarantined file is never read again.
const quarantineSuffix = ".corrupt"

// corruption says what a reader does with a file of one state directory once
// its bytes no longer decode (see isCorruptJSON). Every entry a Store owns
// has one (corruptionPolicies, checked by a test against OwnedEntries), so a
// new directory cannot be added without deciding it: a corrupt file must
// never fail its session on every pass with no way out.
type corruption int

const (
	// quarantineUnderLock: moved aside by the collector's scan, under the
	// lock its writers (hooks among them) hold; the session carries on
	// without it, and a hook can write it again.
	quarantineUnderLock corruption = iota
	// quarantineInPass: moved aside by a collector pass or retention sweep
	// (a Store from ForCollectorPass), whose collector lock makes it the
	// file's only writer. What it recorded is treated as never having been
	// recorded: a lost published state as never
	// published, with retention still deleting whatever may have reached the
	// bucket (see LostPublication), and a lost superseded ledger as empty,
	// leaving its objects to whole-session expiry.
	quarantineInPass
	// readAsAbsent: derived bookkeeping, read as missing and rewritten.
	readAsAbsent
	// readAsPending: a scan journal, read as a scan still owed, which the
	// next scan rewrites.
	readAsPending
	// rebuiltFromRegistrations: the native-session index, recovered from the
	// registration naming the native session, or assigned afresh.
	rebuiltFromRegistrations
	// readAsRemoved: a removal record; its existence is the record.
	readAsRemoved
	// replacedByNextPass: status.json, rewritten whole by the next pass.
	replacedByNextPass
	// holdsNoContent: lock files, never decoded.
	holdsNoContent
	// readAsRecoveryRequired: damaged authoritative evidence requires repair.
	readAsRecoveryRequired
)

// corruptionPolicies is the corruption policy of every entry OwnedEntries
// names.
var corruptionPolicies = map[string]corruption{
	deletionDirectory:         readAsRecoveryRequired,
	generationHeadsDir:        readAsRecoveryRequired,
	generationNodesDir:        readAsRecoveryRequired,
	generationRecoveryDir:     readAsRecoveryRequired,
	"registrations":           quarantineUnderLock,
	"requests":                quarantineUnderLock,
	"subagent-candidates":     quarantineUnderLock,
	"published":               quarantineInPass,
	"pending":                 readAsRecoveryRequired,
	"superseded":              quarantineInPass,
	"scan-signatures":         readAsAbsent,
	refreshSkipDir:            readAsAbsent,
	listingRepairDir:          quarantineInPass,
	"pending-scans":           readAsPending,
	"sessions":                rebuiltFromRegistrations,
	"sessions-v1":             rebuiltFromRegistrations,
	packedSessionIndexDir:     rebuiltFromRegistrations,
	sessionIndexMarkerFile:    rebuiltFromRegistrations,
	sessionRecoveryCursorFile: rebuiltFromRegistrations,
	sessionIndexAbsenceFile:   rebuiltFromRegistrations,
	sessionMembershipFile:     readAsRecoveryRequired,
	sessionMembershipLock:     holdsNoContent,
	"forgotten":               readAsRemoved,
	"status.json":             replacedByNextPass,
	storageClockFile:          readAsAbsent,
	"request-locks":           holdsNoContent,
	"temporary-quota":         holdsNoContent,
}

// quarantineDirs are the directories whose files a reader may move aside.
var quarantineDirs = func() []string {
	// Older releases quarantined pending journals. Keep those copies visible
	// to status, cleanup and loss detection, without quarantining new damage.
	dirs := []string{"pending"}
	for dir, policy := range corruptionPolicies {
		if policy == quarantineUnderLock || policy == quarantineInPass {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}()

// ForCollectorPass returns the store as a collector pass or a retention
// sweep uses it: under the collector lock, which makes it the only writer of
// published/ and superseded/, so a file there that no longer
// decodes can be moved aside on the spot (see quarantineInPass). Any other
// Store only reports such a file.
func (s *Store) ForCollectorPass() *Store {
	pass := *s
	pass.collectorPass = true
	return &pass
}

// readOwned reads a collector-owned JSON file at path into value. found is
// false when it does not exist. A file that does not decode is moved aside
// when the store belongs to a collector pass (the error then wraps
// ErrQuarantined, once) and reported otherwise.
func (s *Store) readOwned(path string, value any) (found bool, err error) {
	err = s.readBudgeted(path, value, true)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case !s.collectorPass || !isCorruptJSON(err):
		return false, err
	}
	return false, s.moveAside(path, err)
}

// moveAside renames a corrupt file out of the way and returns the error that
// reports it, wrapping ErrQuarantined and the decoding error.
func (s *Store) moveAside(path string, decodeErr error) error {
	s.invalidatePublishedAccounting(path)
	rel, relErr := filepath.Rel(s.home, path)
	if relErr != nil {
		rel = path
	}
	aside := quarantinePath(path)
	if renameErr := os.Rename(path, aside); renameErr != nil {
		return fmt.Errorf("%w (and it could not be moved aside: %w)", decodeErr, renameErr)
	}
	pruneQuarantine(path)
	return fmt.Errorf("%w: %s did not decode (%w) and is now %s", ErrQuarantined, rel, decodeErr, filepath.Join(filepath.Dir(rel), filepath.Base(aside)))
}

// LostPublication reports whether a published state or pending publication
// of the session was ever moved aside: the session may then have objects in
// the bucket that nothing local records any more, so retention must not
// take it for one that never published.
func (s *Store) LostPublication(archiveSessionID string) bool {
	for _, dir := range []string{"published", "pending"} {
		if matches, _ := filepath.Glob(filepath.Join(s.home, dir, archiveSessionID+".json.*"+quarantineSuffix)); len(matches) > 0 {
			return true
		}
	}
	return false
}

// quarantinedCopies lists the moved-aside copies of a session's files.
func (s *Store) quarantinedCopies(archiveSessionID string) []string {
	var out []string
	for _, dir := range quarantineDirs {
		matches, _ := filepath.Glob(filepath.Join(s.home, dir, archiveSessionID+".json.*"+quarantineSuffix))
		out = append(out, matches...)
	}
	return out
}

// quarantinePath names the file path is moved aside to. The time in the name
// keeps a second quarantine of the same file (a hook rewrote it, and it was
// corrupted again) from replacing the first.
func quarantinePath(path string) string {
	return path + "." + time.Now().UTC().Format("20060102T150405.000000000Z") + quarantineSuffix
}

// isCorruptJSON reports whether err means the file's bytes are not JSON at
// all: truncated, empty, or garbled, which no version of this program could
// read either. A well-formed document of an unexpected shape
// (json.UnmarshalTypeError) is not corruption: it is likely a file a newer
// version wrote before a downgrade, and it is reported, never moved aside.
func isCorruptJSON(err error) bool {
	var syntaxErr *json.SyntaxError
	return errors.As(err, &syntaxErr)
}

// IsUndecodable reports whether err is any JSON decoding failure, corrupt or
// of an unexpected shape.
func IsUndecodable(err error) bool {
	var typeErr *json.UnmarshalTypeError
	return isCorruptJSON(err) || errors.As(err, &typeErr)
}

// readOrQuarantine reads the JSON file at path. found is false when it does
// not exist. A file that does not decode is read again under lockName, the
// lock its writers hold, so a writer replacing it at that moment is not
// mistaken for corruption; if it still does not decode it is renamed aside
// and the error wraps ErrQuarantined. Any other read failure is returned as
// is and leaves the file where it is, to be retried.
func readOrQuarantine[T any](s *Store, path, lockName string) (value T, found bool, err error) {
	return readOrQuarantineWaiting[T](s, path, lockName, time.Second)
}

// readOrQuarantineWaiting is readOrQuarantine waiting at most lockWait for
// lockName; with none, a lock held right now leaves the file where it is
// and the error wraps local.ErrBusy.
func readOrQuarantineWaiting[T any](s *Store, path, lockName string, lockWait time.Duration) (value T, found bool, err error) {
	value, found, err = readJSON[T](path)
	if err == nil || !isCorruptJSON(err) {
		return value, found, err
	}
	unlock, lockErr := s.namedLockWait(lockName, lockWait)
	if lockErr != nil {
		return value, false, fmt.Errorf("%w (and it could not be locked to move it aside: %w)", err, lockErr)
	}
	defer unlock()
	value, found, err = readJSON[T](path)
	if err == nil || !isCorruptJSON(err) {
		return value, found, err
	}
	return value, false, s.moveAside(path, err)
}

// quarantineKeep is how many quarantined copies of one file are kept. A file
// a hook keeps rewriting and something keeps corrupting would otherwise pile
// up a copy per pass.
const quarantineKeep = 3

// pruneQuarantine removes all but the newest quarantineKeep quarantined
// copies of path. Their names sort by the time they were moved aside. Best
// effort: a copy left behind is only clutter.
func pruneQuarantine(path string) {
	copies, err := filepath.Glob(path + ".*" + quarantineSuffix)
	if err != nil || len(copies) <= quarantineKeep {
		return
	}
	sort.Strings(copies)
	for _, old := range copies[:len(copies)-quarantineKeep] {
		_ = os.Remove(old)
	}
}

func readJSON[T any](path string) (T, bool, error) {
	var value T
	err := local.Read(path, &value)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	return value, true, nil
}

// listJSONStems returns the names, without ".json", of the JSON files in one
// of the store's directories.
func (s *Store) listJSONStems(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.home, dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stems []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), ".json")
		if safeFileComponent(stem) {
			stems = append(stems, stem)
		}
	}
	return stems, nil
}

// ScanRegistrations is LoadRegistrations for the collector pass and the
// retention sweep, which run under the collector lock: one registration that
// cannot be read fails only its own session. It is left out of regs and
// reported in issues under its archive session ID, and one that does not
// decode is quarantined (see ErrQuarantined), so a hook for the same native
// session can register it again. err is reserved for a directory that cannot
// be listed at all.
func (s *Store) ScanRegistrations() (regs []archive.SessionRegistration, issues map[string]error, err error) {
	ids, err := s.listJSONStems("registrations")
	if err != nil {
		return nil, nil, fmt.Errorf("list registrations: %w", err)
	}
	issues = map[string]error{}
	regs = make([]archive.SessionRegistration, 0, len(ids))
	for _, id := range ids {
		reg, found, err := readOrQuarantine[archive.SessionRegistration](s, s.registrationPath(id), requestLockName(id))
		if err != nil {
			issues[id] = fmt.Errorf("read registration %q: %w", id, err)
			continue
		}
		if found {
			regs = append(regs, reg)
		}
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].ArchiveSessionID < regs[j].ArchiveSessionID })
	return regs, issues, nil
}

// ScanRequests is LoadRequests with ScanRegistrations' isolation: a request
// that cannot be read is reported in issues under its archive session ID,
// and one that does not decode is quarantined. Its hook evidence is lost
// either way, but hooks can queue the session's requests again, which they
// cannot while it sits there.
func (s *Store) ScanRequests() (requests []Request, issues map[string]error, err error) {
	ids, err := s.listJSONStems("requests")
	if err != nil {
		return nil, nil, fmt.Errorf("list requests: %w", err)
	}
	issues = map[string]error{}
	requests = make([]Request, 0, len(ids))
	for _, id := range ids {
		req, found, err := readOrQuarantine[Request](s, s.requestPath(id), requestLockName(id))
		if err != nil {
			issues[id] = fmt.Errorf("read request %q: %w", id, err)
			continue
		}
		if found {
			requests = append(requests, req)
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ArchiveSessionID < requests[j].ArchiveSessionID })
	return requests, issues, nil
}

// ScanSubagentCandidates is LoadSubagentCandidates with ScanRegistrations'
// isolation, keyed by the candidate's archive session ID.
func (s *Store) ScanSubagentCandidates() ([]SubagentCandidate, map[string]error, error) {
	return s.scanSubagentCandidates(time.Second)
}

// scanSubagentCandidates is ScanSubagentCandidates waiting at most lockWait
// for the lock of a candidate that does not decode before it moves it aside.
func (s *Store) scanSubagentCandidates(lockWait time.Duration) ([]SubagentCandidate, map[string]error, error) {
	ids, err := s.listJSONStems("subagent-candidates")
	if err != nil {
		return nil, nil, fmt.Errorf("list subagent candidates: %w", err)
	}
	issues := map[string]error{}
	var out []SubagentCandidate
	for _, id := range ids {
		candidate, found, err := readOrQuarantineWaiting[SubagentCandidate](s, s.subagentCandidatePath(id), subagentLockName(id), lockWait)
		if err != nil {
			issues[id] = fmt.Errorf("read subagent candidate %q: %w", id, err)
			continue
		}
		if found {
			out = append(out, candidate)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, issues, nil
}

// QuarantinedFiles lists the files ever quarantined that are still present,
// relative to the archive directory.
func (s *Store) QuarantinedFiles() []string {
	var out []string
	for _, dir := range quarantineDirs {
		matches, _ := filepath.Glob(filepath.Join(s.home, dir, "*"+quarantineSuffix))
		for _, match := range matches {
			if rel, err := filepath.Rel(s.home, match); err == nil {
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}

// RemoveOrphanedLocks removes the lock files under request-locks/ whose
// session or subagent candidate no longer exists: a request lock with no
// registration (other than those in keep, registrations that exist but could
// not be read), and a subagent lock with no candidate. Each is removed only
// while this caller holds it, after checking again, so a writer that is
// about to use it retries on a fresh file (see local.NamedLock). Best
// effort: a lock still held, or one that cannot be removed, waits for the
// next pass.
func (s *Store) RemoveOrphanedLocks(keep map[string]bool) {
	dir := filepath.Join(s.home, "request-locks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		id := strings.TrimSuffix(name, ".lock")
		owner := s.registrationPath(id)
		if candidate, ok := strings.CutPrefix(id, "subagent-"); ok {
			owner = s.subagentCandidatePath(candidate)
		} else if keep[id] {
			continue
		}
		if !safeFileComponent(id) || exists(owner) {
			continue
		}
		unlock, err := local.NamedLock(s.home, filepath.Join("request-locks", name))
		if err != nil {
			continue
		}
		if !exists(owner) {
			_ = os.Remove(filepath.Join(dir, name))
		}
		unlock()
	}
}

// exists reports whether path names anything; an error other than "does not
// exist" counts as existing, the conservative answer for a caller about to
// remove something because it is gone.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// staleTempAge is how old an atomic-write temporary file (see
// local.RemoveStaleTemps) must be before a pass removes it. A write takes
// well under a second, so anything this old was left by a process that
// died mid-write.
const staleTempAge = time.Hour

// RemoveStaleTemps removes atomic-write temporaries that crashed writers
// left in the directories this store owns. Best effort: a failure only
// leaves the file for the next pass.
func (s *Store) RemoveStaleTemps() {
	protect := true
	if home, err := local.OpenRootedHome(s.home); err == nil {
		cfg, found, e := config.LoadRooted(home)
		protect = e != nil || !found || cfg.DurableStorageProtection
		if e = home.Close(); e != nil {
			protect = true
		}
	}
	dirs := []string{s.home}
	for _, dir := range append(append([]string{}, storeDirs...), lazyStoreDirs...) {
		if protect && protectedDurableRoot(dir) {
			continue
		}
		dirs = append(dirs, filepath.Join(s.home, dir))
	}
	// Per-session evidence directories (see SessionDir) sit one level down.
	if entries, err := os.ReadDir(filepath.Join(s.home, "sessions")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(s.home, "sessions", entry.Name()))
			}
		}
	}
	for _, dir := range dirs {
		_ = local.RemoveStaleTemps(dir, staleTempAge)
	}
}
