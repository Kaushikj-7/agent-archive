// Package state is agent-archive's per-machine local state: the small files
// under the private data directory (local.Home) that hooks, the collector,
// retention, backfill, and the CLI share. It owns their layout, the
// per-session locks that keep a hook and the collector from interleaving,
// and every rule about what a file may hold:
//
//   - registrations/: one per session a hook (or backfill) admitted;
//   - requests/: hook evidence waiting for the next collector pass;
//   - published/: the last bundle built and the last one published, with
//     the uploaded source reference and metadata (see Published);
//   - pending/: a publication transaction frozen before its first upload;
//   - scan-signatures/, pending-scans/: the collector's change detection;
//   - superseded/: source objects retention may delete after a grace period;
//   - sessions/ (the native-session index and per-session evidence),
//     subagent-candidates/, forgotten/, refresh-skips/, and status.json.
//
// It never reads transcripts or talks to storage; the collector does both
// and records the outcome here. It holds no credentials and no content
// beyond what published bundles already carry. A Store is safe for the
// processes that share a data directory as long as each follows the
// documented locks; the collector lock (local.Lock) additionally makes the
// collector the only writer of published/ and pending/ during a pass.
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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourceidentity"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// Store persists small operational files under a private home
// directory (see local.Home): registrations, upload requests, and a
// per-session cache of the last published source bundle, used to detect
// unchanged input without redownloading or reparsing published history. It
// never stores credentials or a second copy of conversation content beyond
// what the published source bundle itself already contains.
type Store struct {
	durableInspection *durableInspectionCache
	resourceBudget    *agentapi.NativeReadBudget
	resourceContext   context.Context
	resourceReleases  *[]func()
	// indexSnapshots uses logical packed authority for qualified-index writes.
	indexSnapshots bool
	// onPackedEnumeration observes collector-only physical-index directory probes.
	onPackedEnumeration func()

	home string
	// hook marks a Store from ForHook.
	hook bool
	// onWriteSync, when set by a test, runs where writeUnderLock syncs to
	// disk outside the lock: once its temporary file is synced, and once it
	// is renamed into place, before the directory is synced.
	onWriteSync func()
	// onLockWait, when set by a test, runs before any wait for a lock that
	// can last (see namedLockWait), with the lock's name.
	onLockWait func(name string)
	// collectorPass marks a Store from ForCollectorPass, which may move a
	// corrupt collector-owned file aside.
	collectorPass bool
	// afterRemovalRecord, when set by a test, runs once ForgetIdleSession
	// has written the removal record and before it takes the request lock.
	afterRemovalRecord func()
	// onIndexStep injects interruption at durable identity conversion boundaries.
	onIndexStep func(string) error
	// onIndexSync tests index staging without changing registration sync seams.
	onIndexSync func()
}

// OpenReadOnly returns a handle to an existing local store under
// home without creating any of its directories, for commands that only read
// it (status, handoff, backfill planning). A missing directory reads as
// nothing recorded.
func OpenReadOnly(home string) *Store {
	return &Store{home: home, durableInspection: &durableInspectionCache{}}
}

// Open creates (if needed) the local store's directory layout under
// home — ordinarily the result of local.Home() — and returns a handle to it.
// home is caller-owned; this package never deletes it.
func Open(home string) (*Store, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("local store home is required")
	}
	for _, dir := range storeDirs {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create local store directory %q: %w", dir, err)
		}
	}
	return &Store{home: home, durableInspection: &durableInspectionCache{}}, nil
}

// Home returns the data directory the store keeps its files in.
func (s *Store) Home() string { return s.home }

// storeDirs are the directories Open creates under home.
var storeDirs = []string{"registrations", "requests", "request-locks", "published", "pending", "sessions", "sessions-v1", packedSessionIndexDir, "pending-scans", "scan-signatures", "subagent-candidates"}

// lazyStoreDirs are the directories the store creates under home on first
// use rather than up front.
var lazyStoreDirs = []string{generationHeadsDir, generationNodesDir, generationRecoveryDir, "superseded", "forgotten", refreshSkipDir, listingRepairDir}

// OwnedEntries lists every top-level entry a Store can create under its
// home: its directories, its status file, and the storage clock reading
// retention caches. Uninstall deletes a data
// directory entry by entry and must know all of them; a test there checks
// its list against this one, so a new directory cannot be left behind.
func OwnedEntries() []string {
	entries := append(append([]string{}, storeDirs...), lazyStoreDirs...)
	return append(entries, "status.json", storageClockFile, sessionIndexMarkerFile, sessionMembershipFile, sessionMembershipLock, sessionRecoveryCursorFile, "temporary-quota")
}

func safeFileComponent(value string) bool {
	//lint:ignore LV1001 value is an arbitrary file name component; these are the reserved names it must not be
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") {
		return false
	}
	return true
}

// SaveRegistration durably records a hook-observed session start. Re-saving
// the same archive session ID overwrites its prior registration.
func (s *Store) SaveRegistration(reg archive.SessionRegistration) error {
	if err := reg.Validate(); err != nil {
		return err
	}
	if !safeFileComponent(reg.ArchiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	key, err := registrationKey(reg)
	if err != nil {
		return err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return err
	}
	if found && entry.ArchiveSessionID != reg.ArchiveSessionID {
		return ErrSessionIdentityConflict
	}
	if !found {
		owner, err := local.ID()
		if err != nil {
			return err
		}
		next := indexEntry(key, reg.ArchiveSessionID)
		next.Reservation = owner
		err = s.writeIndexUnderRequestLock(reg.ArchiveSessionID, qualifiedSessionIndexPath(s.home, key), nil, func(current fileSnapshot) (any, bool, error) {
			if current.found {
				return nil, false, errIndexMoved
			}
			return next, true, nil
		})
		if err != nil {
			return err
		}
	}
	_, saved, err := s.registerUnderLock(key, reg.ArchiveSessionID, func(string) archive.SessionRegistration { return reg }, false)
	if err == nil && !saved {
		return ErrSessionNotRegistered
	}
	return err
}

// UpdateRegistration changes an existing registration atomically with respect
// to the per-session request lock, the lock retention holds while it forgets
// a session (see writeUnderRequestLock). It reports found=false, without
// calling update, when the registration is gone: a hook that looked the
// session up before the lock must then treat it as never seen. Otherwise
// update edits the loaded registration and it is saved; an error from update
// saves nothing and is returned as is, with found true. Any other error
// reports found false, whatever was read: only a nil error or update's own
// says whether the registration exists. update may run more than once, each
// time on a freshly loaded registration, when another writer changes it
// meanwhile.
//
// A plain load then SaveRegistration from a hook could write the registration
// back after retention forgot it, leaving it without its native-session index
// entry, so the session's next start would be given a second archive ID.
func (s *Store) UpdateRegistration(archiveSessionID string, update func(*archive.SessionRegistration) error) (found bool, err error) {
	return s.updateRegistration(archiveSessionID, update, nil)
}

// RecordHookObservation records actual hook execution without synchronizing a
// registration whose observation was already persisted by the same event.
func (s *Store) RecordHookObservation(archiveSessionID string, at time.Time) error {
	_, err := s.updateRegistration(archiveSessionID, func(reg *archive.SessionRegistration) error {
		reg.HookObservedAt = at
		return nil
	}, func(reg archive.SessionRegistration) bool { return reg.HookObservedAt.Equal(at) })
	return err
}

func (s *Store) updateRegistration(archiveSessionID string, update func(*archive.SessionRegistration) error, unchanged func(archive.SessionRegistration) bool) (found bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	updateFailed := false
	err = s.writeUnderRequestLock(archiveSessionID, s.registrationPath(archiveSessionID), nil, func(current fileSnapshot) (any, bool, error) {
		found, updateFailed = current.found, false
		if !found {
			return nil, false, nil
		}
		var reg archive.SessionRegistration
		if err := json.Unmarshal(current.data, &reg); err != nil {
			return nil, false, fmt.Errorf("read registration %q: %w", archiveSessionID, err)
		}
		originalStage := reg.StagedImportSHA256
		originalBinding := reg.CodexBinding
		if originalBinding != nil {
			copyBinding := *originalBinding
			if copyBinding.OwnStart != nil {
				boundary := *copyBinding.OwnStart
				copyBinding.OwnStart = &boundary
			}
			originalBinding = &copyBinding
		}
		var originalProof *archive.CodexAdmissionProof
		if reg.CodexAdmission != nil {
			proof := *reg.CodexAdmission
			originalProof = &proof
		}
		originalKey, err := registrationKey(reg)
		if err != nil {
			return nil, false, err
		}
		if unchanged != nil && unchanged(reg) {
			return nil, false, nil
		}
		if err := update(&reg); err != nil {
			updateFailed = true
			return nil, false, err
		}
		if !reg.CodexBinding.PreservesFacts(originalBinding) {
			return nil, false, errors.New("a registration update cannot change native Codex binding facts")
		}
		if originalStage != reg.StagedImportSHA256 {
			return nil, false, errors.New("a registration update cannot change import staging selection")
		}
		if !sameCodexAdmission(originalProof, reg.CodexAdmission) {
			return nil, false, errors.New("a registration update cannot change Codex admission proof")
		}
		if reg.ArchiveSessionID != archiveSessionID {
			return nil, false, errors.New("a registration update cannot change its archive session ID")
		}
		updatedKey, err := registrationKey(reg)
		if err != nil || updatedKey != originalKey {
			return nil, false, ErrSessionIdentityConflict
		}
		if err := reg.Validate(); err != nil {
			return nil, false, err
		}
		return reg, true, nil
	})
	if err != nil && !updateFailed {
		return false, err
	}
	return found, err
}

// RegisterNewSession assigns (or reuses) the archive session ID indexed for a
// native session and saves the registration build returns for it, under that
// ID's request lock. Under the lock it rechecks that the index still maps the
// native session to that ID: retention may have been forgetting a previous
// registration of the same native session, whose index entry the lookup saw
// just before retention removed it. Reusing that ID would leave the new
// registration with no index entry. If the entry changed or disappeared, a
// fresh ID is assigned and the check repeats.
func (s *Store) RegisterNewSession(key agentmeta.SessionKey, build func(archiveSessionID string) archive.SessionRegistration) (archive.SessionRegistration, error) {
	return s.registerSession(key, build, false)
}

// RegisterOrMerge creates a reservation-backed registration or returns its
// compatible existing owner without rewriting original admission or provenance.
// The caller serializes admission with hooks.lock.
func (s *Store) RegisterOrMerge(key agentmeta.SessionKey, build func(string) archive.SessionRegistration) (archive.SessionRegistration, error) {
	return s.registerSession(key, build, true)
}

func (s *Store) registerSession(key agentmeta.SessionKey, build func(string) archive.SessionRegistration, merge bool) (archive.SessionRegistration, error) {
	for range 3 {
		id, _, err := s.EnsureArchiveSessionID(key)
		if errors.Is(err, errIndexMoved) {
			continue
		}
		if err != nil {
			return archive.SessionRegistration{}, err
		}
		reg, saved, err := s.registerUnderLock(key, id, build, merge)
		if err != nil || saved {
			return reg, err
		}
	}
	return archive.SessionRegistration{}, errors.New("session index kept changing while registering; this start was not recorded")
}

// RegisterReservedSession commits an import's already linked reservation. If
// ownership moved between its short hooks-lock holds, it refuses stale links
// rather than assigning a different parent archive ID.
func (s *Store) RegisterReservedSession(key agentmeta.SessionKey, id string, build func(string) archive.SessionRegistration) (archive.SessionRegistration, error) {
	if err := key.Validate(); err != nil {
		return archive.SessionRegistration{}, err
	}
	reg, saved, err := s.registerUnderLock(key, id, build, true)
	if err == nil && !saved {
		return archive.SessionRegistration{}, errIndexMoved
	}
	return reg, err
}

// registerUnderLock saves build's registration for id, atomically with
// respect to the request lock (see writeUnderRequestLock), if the index
// still maps nativeSessionID to id there; saved is false when it does not.
func (s *Store) registerUnderLock(key agentmeta.SessionKey, id string, build func(string) archive.SessionRegistration, merge bool) (archive.SessionRegistration, bool, error) {
	if !safeFileComponent(id) {
		return archive.SessionRegistration{}, false, errors.New("archive session ID is not a safe file name component")
	}
	reg := build(id)
	if reg.ArchiveSessionID != id || reg.NativeSessionID != key.NativeID || agentmeta.Canonical(agentmeta.Builtins(), reg.Harness.Name) != string(key.Agent) {
		return archive.SessionRegistration{}, false, errors.New("registration does not match the session index")
	}
	if err := reg.Validate(); err != nil {
		return archive.SessionRegistration{}, false, err
	}
	// Replacement staging remains blind; the locked check protects immutable
	// admission proof before committing the staged registration.
	err := s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return s.lockRequest(id) },
		path: s.registrationPath(id),
		check: func() error {
			current, found, err := s.readQualifiedIndex(key)
			if err == nil && (!found || current.ArchiveSessionID != id) {
				return errIndexMoved
			}
			if err != nil {
				return err
			}
			registered, err := s.matchingRegistration(key, id)
			if err != nil {
				return err
			}
			if !registered && current.Reservation == "" {
				return errIndexMoved
			}
			if !merge {
				prior, found, err := s.LoadRegistration(id)
				if err != nil {
					return err
				}
				if found && prior.StagedImportSHA256 != reg.StagedImportSHA256 {
					return errors.New("a registration replacement cannot change import staging selection")
				}
				if found && !sameCodexAdmission(prior.CodexAdmission, reg.CodexAdmission) {
					return errors.New("a registration replacement cannot change Codex admission proof")
				}
			}
			return nil
		},
		change: func(current fileSnapshot) (any, bool, error) {
			if !merge {

				return reg, true, nil
			}
			var err error
			reg, err = s.mergeAdmissionRegistration(key, reg, current)
			return reg, true, err
		},
		blind: !merge,
	})
	if errors.Is(err, errIndexMoved) {
		return archive.SessionRegistration{}, false, nil
	}
	if err != nil {
		return archive.SessionRegistration{}, false, err
	}
	if err := s.indexStep("registration"); err != nil {
		return archive.SessionRegistration{}, false, err
	}
	if err := s.finishSessionIndex(key, id); err != nil {
		return archive.SessionRegistration{}, false, err
	}
	return reg, true, nil
}

// mergeAdmissionRegistration runs inside the request-lock update. Existing
// registration attribution is immutable; only a missing compatible file locator
// can be filled. Removal records forbid fresh automatic admission.
func (s *Store) mergeAdmissionRegistration(key agentmeta.SessionKey, reg archive.SessionRegistration, current fileSnapshot) (archive.SessionRegistration, error) {
	if current.found {
		var existing archive.SessionRegistration
		if err := json.Unmarshal(current.data, &existing); err != nil {
			return reg, err
		}
		if existing.NativeSessionID != key.NativeID || agentmeta.Canonical(agentmeta.Builtins(), existing.Harness.Name) != string(key.Agent) || existing.ProjectRoot != reg.ProjectRoot || existing.DestinationID != "" && existing.DestinationID != reg.DestinationID {
			return reg, ErrSessionIdentityConflict
		}
		if existing.TranscriptPath == "" && existing.ReadsTranscriptFile() && reg.ReadsTranscriptFile() {
			existing.TranscriptPath = reg.TranscriptPath
		}
		return existing, nil
	}
	if _, removed, err := s.Removal(reg.Harness.Name, reg.NativeSessionID); err != nil {
		return reg, err
	} else if removed && !reg.Imported() {
		return reg, errors.New("session was removed")
	}
	return reg, nil
}

// errIndexMoved is registerUnderLock's check failing: the native session's
// index entry no longer names the archive ID being registered.
var errIndexMoved = errors.New("the session index no longer maps this session to that archive ID")

func (s *Store) registrationPath(archiveSessionID string) string {
	return filepath.Join(s.home, "registrations", archiveSessionID+".json")
}

// LoadRegistrations returns every registered session, sorted by archive
// session ID for deterministic scan order.
func (s *Store) LoadRegistrations() ([]archive.SessionRegistration, error) {
	span := trace.Start("load registrations")
	defer span.End()
	dir := filepath.Join(s.home, "registrations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	out := make([]archive.SessionRegistration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var reg archive.SessionRegistration
		if err := local.Read(filepath.Join(dir, entry.Name()), &reg); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Forgotten between listing and reading.
				continue
			}
			return nil, fmt.Errorf("read registration %q: %w", entry.Name(), err)
		}
		out = append(out, reg)
	}
	span.Count("registrations", len(out))
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// LoadRegistration returns one session's registration, if it has been saved.
func (s *Store) LoadRegistration(archiveSessionID string) (archive.SessionRegistration, bool, error) {
	var reg archive.SessionRegistration
	err := local.Read(s.registrationPath(archiveSessionID), &reg)
	if errors.Is(err, os.ErrNotExist) {
		return archive.SessionRegistration{}, false, nil
	}
	if err != nil {
		return archive.SessionRegistration{}, false, fmt.Errorf("read registration %q: %w", archiveSessionID, err)
	}
	return reg, true, nil
}

// Request is a small durable marker left by a stop, failure, or end hook. It
// exists to carry hook-only supplemental evidence (a final response, model
// selection) through to the next scan; periodic scanning already covers
// every registered session regardless of whether a request is pending, so a
// request with no new evidence is a no-op beyond confirming the session was
// checked.
type Request struct {
	ArchiveSessionID string                         `json:"archive_session_id"`
	Token            string                         `json:"token"`
	Reasons          []string                       `json:"reasons"`
	RequestedAt      time.Time                      `json:"requested_at"`
	HookEvidence     []archive.SupplementalEvidence `json:"hook_evidence,omitempty"`
	// Deferred marks a request that only carries lifecycle activity evidence
	// (a session start, a prompt submitted) and has not asked for a debounce
	// flush: the collector folds it in on its normal MinUploadInterval
	// schedule instead of uploading on every prompt. A stop, end, or response
	// request for the same session clears it. Requests written before the
	// field existed are urgent, as they always were.
	Deferred bool `json:"deferred,omitempty"`
}

// Urgent reports whether the request asks the collector to flush the upload
// debounce now rather than wait for the next scheduled publication.
func (r Request) Urgent() bool {
	return r.Token != "" && !r.Deferred
}

// SaveRequest merges a new hook event into any already-pending request for
// the same session: reasons accumulate, evidence appends, and RequestedAt
// advances to the latest event. This is the coalescing the spec asks for
// when a stop and a session-end hook both fire for the same session. The
// resulting request is urgent: the collector publishes it as soon as it is
// scanned, bypassing MinUploadInterval.
func (s *Store) SaveRequest(archiveSessionID, reason string, requestedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	return s.saveRequest(archiveSessionID, reason, requestedAt, false, evidence...)
}

// SaveEvidence appends lifecycle evidence to the session's request without
// asking for a flush. A request it creates is deferred; a request that is
// already pending keeps its urgency and RequestedAt, so a stop that is
// waiting to publish is neither delayed nor re-triggered by a later prompt.
func (s *Store) SaveEvidence(archiveSessionID, reason string, observedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	return s.saveRequest(archiveSessionID, reason, observedAt, true, evidence...)
}

// ErrSessionNotRegistered is returned when a request is written for a session
// whose registration no longer exists, typically because retention forgot it
// between the caller's own lookup and the write.
var ErrSessionNotRegistered = errors.New("session is no longer registered")

// lockRequest takes the per-session request lock. Hooks writing a request,
// the collector acknowledging one, and retention forgetting the session all
// hold it, so none of them can interleave with another. It waits a second,
// which hooks can afford inside their harness's budget, so nothing may sync
// to disk while holding it: a write of a file it guards goes through
// writeUnderRequestLock.
func (s *Store) lockRequest(archiveSessionID string) (func(), error) {
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return nil, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	return unlock, nil
}

func requestLockName(archiveSessionID string) string {
	return filepath.Join("request-locks", archiveSessionID+".lock")
}

// saveRequest always assigns a fresh token, even for deferred evidence: the
// token is what CompleteRequest checks, so a hook that lands while a scan is
// publishing the previous token keeps its evidence pending for the next
// pass instead of being acknowledged away with it. Evidence identical to an
// item the pending request already carries is dropped, and a call that adds
// nothing at all leaves the request untouched.
func (s *Store) saveRequest(archiveSessionID, reason string, requestedAt time.Time, deferred bool, evidence ...archive.SupplementalEvidence) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if requestedAt.IsZero() {
		return errors.New("requested_at is required")
	}
	// Retention forgets a session under the request lock, and this check
	// runs under it right before the write (see writeUnderRequestLock). A
	// caller that looked the registration up before may be writing for a
	// session that is gone now; its request would be an orphan nothing ever
	// reads.
	registered := func() error {
		if _, err := os.Stat(s.registrationPath(archiveSessionID)); errors.Is(err, os.ErrNotExist) {
			// Taking the lock created its file; a session that is not
			// registered must not keep one (a rejected subagent notifying a
			// forgotten parent did). Unlinking it while held is safe
			// (local.NamedLock).
			_ = os.Remove(filepath.Join(s.home, requestLockName(archiveSessionID)))
			return ErrSessionNotRegistered
		} else if err != nil {
			return fmt.Errorf("check registration %q: %w", archiveSessionID, err)
		}
		return nil
	}
	return s.writeUnderRequestLock(archiveSessionID, s.requestPath(archiveSessionID), registered, func(current fileSnapshot) (any, bool, error) {
		return mergeRequest(archiveSessionID, current, reason, requestedAt, deferred, evidence)
	})
}

// mergeRequest is saveRequest's new request, computed from the one current
// holds; write is false when nothing would change.
func mergeRequest(archiveSessionID string, current fileSnapshot, reason string, requestedAt time.Time, deferred bool, evidence []archive.SupplementalEvidence) (merged Request, write bool, err error) {
	existing, found := Request{}, current.found
	if found {
		if err := json.Unmarshal(current.data, &existing); err != nil {
			return Request{}, false, fmt.Errorf("read request %q: %w", archiveSessionID, err)
		}
	}
	merged = Request{ArchiveSessionID: archiveSessionID, RequestedAt: requestedAt, Deferred: deferred}
	urgencyChanged := false
	if found {
		merged = existing
		if !deferred {
			urgencyChanged = merged.Deferred
			merged.Deferred = false
			if requestedAt.After(merged.RequestedAt) {
				merged.RequestedAt = requestedAt
			}
		}
	}
	merged.Token, err = local.ID()
	if err != nil {
		return Request{}, false, fmt.Errorf("generate request token: %w", err)
	}
	reasonAlready := true
	if reason != "" && !slices.Contains(merged.Reasons, reason) {
		reasonAlready = false
		merged.Reasons = append(merged.Reasons, reason)
	}
	added := 0
	for _, item := range evidence {
		if requestHasEvidence(merged.HookEvidence, item) {
			continue
		}
		merged.HookEvidence = append(merged.HookEvidence, item)
		added++
	}
	// A repeated notification that changes nothing about the pending request
	// is not written back at all. A retry loop — the collector re-announcing a
	// published child to a parent whose own request never clears — would
	// otherwise append an identical evidence item on every pass and grow the
	// request file without bound.
	if found && existing.Token != "" && added == 0 && reasonAlready && !urgencyChanged && !requestedAt.After(existing.RequestedAt) {
		return Request{}, false, nil
	}
	return merged, true, nil
}

func requestHasEvidence(have []archive.SupplementalEvidence, candidate archive.SupplementalEvidence) bool {
	for _, item := range have {
		if archive.SupplementalEvidenceEqual(item, candidate) {
			return true
		}
	}
	return false
}

func (s *Store) requestPath(archiveSessionID string) string {
	return filepath.Join(s.home, "requests", archiveSessionID+".json")
}

// LoadRequest returns a session's pending request, if any. Unlike ScanRequests
// it never moves a file aside, so hooks may call it.
func (s *Store) LoadRequest(archiveSessionID string) (Request, bool, error) {
	var req Request
	err := local.Read(s.requestPath(archiveSessionID), &req)
	if errors.Is(err, os.ErrNotExist) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, fmt.Errorf("read request %q: %w", archiveSessionID, err)
	}
	return req, true, nil
}

// EnsureRequestToken upgrades a request written by an older collector. The
// token is assigned atomically with respect to the request lock hooks and
// acknowledgements hold (see writeUnderRequestLock), so migration cannot
// overwrite a concurrent hook update. found is false when the request
// disappeared between listing and upgrade.
func (s *Store) EnsureRequestToken(archiveSessionID string) (request Request, found bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return Request{}, false, errors.New("archive session ID is not a safe file name component")
	}
	err = s.writeUnderRequestLock(archiveSessionID, s.requestPath(archiveSessionID), nil, func(current fileSnapshot) (any, bool, error) {
		request, found = Request{}, current.found
		if !found {
			return nil, false, nil
		}
		if err := json.Unmarshal(current.data, &request); err != nil {
			return nil, false, fmt.Errorf("read request %q: %w", archiveSessionID, err)
		}
		if request.Token != "" {
			return nil, false, nil
		}
		token, err := local.ID()
		if err != nil {
			return nil, false, fmt.Errorf("generate request token: %w", err)
		}
		request.Token = token
		return request, true, nil
	})
	if err != nil {
		return Request{}, false, fmt.Errorf("upgrade request %q: %w", archiveSessionID, err)
	}
	return request, found, nil
}

// LoadRequests returns every pending request, sorted by archive session ID.
func (s *Store) LoadRequests() ([]Request, error) {
	dir := filepath.Join(s.home, "requests")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	out := make([]Request, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		req, found, err := s.LoadRequest(id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// CompleteRequest removes a request only if it still has the random token
// covered by a durable publish or policy decision. A hook that arrives while
// a scan is in progress assigns a new token under the same lock and therefore
// remains pending for the next pass. Tokens do not repeat when a request file
// was removed between events, unlike a per-file revision counter.
func (s *Store) CompleteRequest(archiveSessionID, coveredToken string) (bool, error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return false, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	current, found, err := s.LoadRequest(archiveSessionID)
	if err != nil || !found {
		return false, err
	}
	if coveredToken == "" || current.Token != coveredToken {
		return false, nil
	}
	err = os.Remove(s.requestPath(archiveSessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove request %q: %w", archiveSessionID, err)
	}
	return true, nil
}

// PendingPublication is one fully rendered publication transaction. Source
// and metadata bytes are persisted together before the first remote write, so
// every retry uses the same hash and timestamps even after process restart.
// Bundle remains available for change detection and future parser-only rebuilds.
type PendingPublication struct {
	// ImportExpectedMetadataSHA256 freezes staged imports' predecessor. Retired
	// privacy sources become cleanup obligations only after successor readback.
	ImportExpectedMetadataSHA256 string                    `json:"import_expected_metadata_sha256,omitempty"`
	ImportRetired                []archive.SourceReference `json:"import_retired,omitempty"`
	// ScanSignature freezes the consumed native observation for resumed history
	// acknowledgement. It never licenses newer input or a privacy successor.
	ScanSignature *ScanSignature       `json:"scan_signature,omitempty"`
	History       *PendingHistory      `json:"history,omitempty"`
	SkillEvidence string               `json:"skill_evidence,omitempty"`
	MetadataOnly  bool                 `json:"metadata_only,omitempty"`
	Bundle        archive.SourceBundle `json:"bundle"`
	SourceKey     string               `json:"source_key"`
	MetadataKey   string               `json:"metadata_key"`
	SourceSHA256  string               `json:"source_sha256"`
	SourceBytes   []byte               `json:"source_bytes"`
	// SourceSize is the source's compressed size when SourceBytes is empty:
	// a metadata-only publication over a source this build cannot reproduce
	// byte for byte, which is checked in storage instead of re-uploaded.
	SourceSize    int       `json:"source_size,omitempty"`
	MetadataBytes []byte    `json:"metadata_bytes"`
	RequestToken  string    `json:"request_token,omitempty"`
	ReadyAt       time.Time `json:"ready_at"`
	Attempted     bool      `json:"attempted,omitempty"`
}

// SourceReference is the reference the publication's metadata carries for
// its source object.
func (p PendingPublication) SourceReference() archive.SourceReference {
	size := len(p.SourceBytes)
	if p.CarriesNoSource() {
		size = p.SourceSize
	}
	return archive.SourceReference{Key: p.SourceKey, SHA256: p.SourceSHA256, CompressedBytes: size}
}

// CarriesNoSource reports a metadata-only publication that points at an
// existing source without carrying its bytes (see SourceSize).
func (p PendingPublication) CarriesNoSource() bool {
	return p.MetadataOnly && len(p.SourceBytes) == 0
}

func (s *Store) pendingPath(id string) string {
	return filepath.Join(s.home, "pending", id+".json")
}

// validateComplete is the supported transaction structure shared by writes
// and protected reads. History validation keeps its separate budget ownership.
func (pending PendingPublication) validateComplete() error {
	if pending.ImportExpectedMetadataSHA256 != "" {
		decoded, err := hex.DecodeString(pending.ImportExpectedMetadataSHA256)
		if err != nil || len(decoded) != sha256.Size {
			return ErrDurableStorageRecovery
		}
	}
	if len(pending.ImportRetired) > 64 {
		return ErrDurableStorageRecovery
	}
	for _, ref := range pending.ImportRetired {
		key, err := archive.SourceObjectKey(pending.Bundle, ref.SHA256)
		if err != nil || ref.Key != key || ref.CompressedBytes <= 0 || ref.CompressedBytes > maxPendingHistoryBytes {
			return ErrDurableStorageRecovery
		}
	}
	if len(pending.SourceBytes) > maxPendingHistoryBytes {
		return ErrDurableStorageCapacity
	}
	if pending.SourceKey == "" || pending.MetadataKey == "" || pending.SourceSHA256 == "" || len(pending.MetadataBytes) == 0 || (len(pending.SourceBytes) == 0 && (!pending.MetadataOnly || pending.SourceSize <= 0)) {
		return errors.New("pending publication is incomplete")
	}
	return nil
}

// validateReadablePending applies the existing publication checksum and JSON
// preconditions without deriving new ownership or remote-source authority.
func (s *Store) validateReadablePending(pending PendingPublication) error {
	if err := pending.validateComplete(); err != nil {
		return err
	}
	if !pending.CarriesNoSource() {
		sum := sha256.Sum256(pending.SourceBytes)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(pending.SourceSHA256)) {
			return errors.New("pending source checksum does not match its persisted bytes")
		}
	}
	scratch, closeScratch := s.WithReadBudget(s.durableContext(), s.resourceBudget)
	defer closeScratch()
	var metadata archive.Metadata
	if err := scratch.unmarshalOwned(pending.MetadataBytes, &metadata); err != nil {
		return err
	}
	if pending.History == nil {
		return archive.CheckHistoryMutation(pending.Bundle, metadata)
	}
	return nil
}

// SavePending durably records a session's publication transaction before its
// first remote write. It refuses an incomplete one: every retry must upload
// exactly the same bytes under exactly the same keys.
func (s *Store) SavePending(id string, pending PendingPublication) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if err := pending.validateComplete(); err != nil {
		return err
	}
	if err := pending.ValidateHistoryBudgeted(id, s.resourceBudget); err != nil {
		return err
	}
	return config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		return s.savePendingGuard(s.durableContext(), g, id, pending)
	})
}

// LoadPending returns a session's outstanding publication transaction, if
// any. Decoding it reads the whole compressed source; HasPending answers
// whether one exists without that cost.
//
// A supported legacy collector pass may quarantine damaged ordinary state.
// Protected state stays present and returns ErrDurableStorageRecovery instead.
func (s *Store) LoadPending(id string) (PendingPublication, bool, error) {
	if !safeFileComponent(id) {
		return PendingPublication{}, false, errors.New("archive session ID is not a safe file name component")
	}
	protected, probeErr := s.protectedStorage(id)
	if probeErr != nil {
		return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, probeErr)
	}
	cfg, cfgErr := s.inspectDurableReadRoots()
	if cfgErr != nil {
		return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, cfgErr)
	}
	if evidence, e := s.hasPublicationEvidence(id); e != nil || evidence {
		return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, e)
	}
	protected = protected || cfg.DurableStorageProtection
	if err := s.checkPendingHistoryVersion(id); err != nil {
		return PendingPublication{}, true, err
	}
	var pending PendingPublication
	var found bool
	var err error
	if protected {
		err = s.readBudgeted(s.pendingPath(id), &pending, true)
		if errors.Is(err, os.ErrNotExist) {
			owed, e := s.protectedStorage(id)
			if owed || e != nil {
				return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, e)
			}
			return PendingPublication{}, false, nil
		}
		found = true
	} else {
		found, err = s.readOwned(s.pendingPath(id), &pending)
	}
	if errors.Is(err, agentapi.ErrReadBudget) && !errors.Is(err, ErrDurableStorageRecovery) {
		// The memory ledger refused the read: the publication is intact and
		// is read again when there is room, never recovered.
		return PendingPublication{}, true, fmt.Errorf("read pending publication %q: %w", id, err)
	}
	if err != nil {
		return PendingPublication{}, protected || cfg.DurableStorageProtection || errors.Is(err, ErrDurableStorageRecovery), fmt.Errorf("read pending publication %q: %w", id, errors.Join(ErrDurableStorageRecovery, err))
	}
	if found {
		if protected {
			if err := s.validateReadablePending(pending); err != nil {
				return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, err)
			}
		}
		if err := pending.ValidateHistoryBudgeted(id, s.resourceBudget); err != nil {
			if protected {
				err = errors.Join(ErrDurableStorageRecovery, err)
			}
			return PendingPublication{}, true, err
		}
	}
	return pending, found, nil
}

// afterLoss follows a collector-owned file of the session being moved aside:
// the session is no longer settled at what that file recorded, so its scan
// signature goes and the next pass scans it. err is returned as is, with any
// failure to do so.
func (s *Store) afterLoss(id string, err error) error {
	if !errors.Is(err, ErrQuarantined) {
		return err
	}
	return errors.Join(err, s.RemoveScanSignature(id))
}

// HasPending reports whether a publication is still outstanding for a session
// without decoding it. The pending file carries the compressed source bytes,
// so a stat is the only way to ask this question cheaply enough to ask it for
// every registered session on every pass.
func (s *Store) HasPending(id string) (owed bool, err error) {
	if !safeFileComponent(id) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, home.Close())
		if err != nil {
			owed = true
		}
	}()
	info, err := home.Root.Lstat(filepath.Join("pending", id+".json"))
	if err == nil {
		if !info.Mode().IsRegular() {
			return true, ErrDurableStorageRecovery
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	history, herr := rootHasEntries(home.Root, filepath.Join("sessions", id, "pending-sources"))
	evidence, eerr := rootHasEntries(home.Root, filepath.Join("publication-evidence", id))
	return history || evidence || herr != nil || eerr != nil, errors.Join(herr, eerr)
}

// RemovePending discards a session's publication transaction once it has
// been published and acknowledged locally. A missing one is not an error.
func (s *Store) RemovePending(id string) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if _, err := os.Lstat(s.pendingPath(id)); errors.Is(err, os.ErrNotExist) {
		if owed, e := s.protectedStorage(id); owed || e != nil {
			return errors.Join(ErrDurableStorageRecovery, e)
		}
		return nil
	} else if err != nil {
		return err
	}
	// Keep the journal until its private cleanup succeeds. After a crash, final
	// remote bytes can restore any stage already removed by this cleanup.
	if err := s.removePendingSources(id, nil); err != nil {
		return err
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	dir, err := privateDirectory(home.Root, "pending", false)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err = dir.Remove(id + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// Status summarizes the collector's local state for a future `status`
// command. It never includes transcript content.
type Status struct {
	// SessionIssues gives, per archive session ID, the kind of failure that
	// kept the session from being scanned, published, or cleaned up in the
	// last pass, as a code (see internal/cli's issueCodes), never the error
	// itself.
	SessionIssues map[string]string `json:"session_issues,omitempty"`
	// IssueCounts counts SessionIssues by code, subagents included: the
	// numbers the last error's summary of failed sessions is built from. It
	// is cleared when a later failure replaces that summary, so status can
	// trust it to describe the last error.
	IssueCounts map[string]int `json:"issue_counts,omitempty"`
	// DeferredSessions gives, per archive session ID, why the last pass put
	// off its scan without failing it (a collector.Deferred* reason, such as
	// codex_history_coverage): the source could not be read completely yet,
	// and the next pass retries. Such a session is pending; a deferral alone
	// never makes it one of the SessionIssues.
	DeferredSessions map[string]string `json:"deferred_sessions,omitempty"`
	// DeferredSince gives, for each session whose scan the last pass
	// deferred (one of the DeferredSessions, or of the SessionIssues once
	// deferred for collector.MaxDeferral) or did not reach, when the run of
	// passes that each deferred its scan began. A session deferred for
	// collector.MaxDeferral fails instead, on every pass that defers it.
	DeferredSince   map[string]time.Time `json:"deferred_since,omitempty"`
	LastScanAt      time.Time            `json:"last_scan_at,omitzero"`
	LastPublishedAt time.Time            `json:"last_published_at,omitzero"`
	PendingCount    int                  `json:"pending_count"`
	// LastError is every problem the last pass recorded, joined with "; ".
	// It is kept, with the same contents, for older readers; LastErrors
	// holds the same problems one per entry. Set both with SetLastErrors
	// or AddLastError.
	LastError string `json:"last_error,omitempty"`
	// LastErrors lists the problems the last pass recorded, one per entry,
	// so a reader never has to split LastError (whose entries can
	// themselves contain "; "). A status file written before this field
	// existed has only LastError.
	LastErrors []string `json:"last_errors,omitempty"`
	// QuarantinedFiles lists, relative to the archive directory, the local
	// state files a pass found undecodable and moved aside (see
	// ErrQuarantined). They stay listed until someone inspects and deletes
	// them.
	QuarantinedFiles []string `json:"quarantined_files,omitempty"`
	// UnrefreshableSummaries counts the sessions whose published metadata
	// the current parser cannot refresh: the retained bundle cannot be
	// read by this build, or its recorded source is gone from storage and
	// cannot be rebuilt. Their metadata stays as published until the
	// session changes. It names no session and no content.
	UnrefreshableSummaries int `json:"unrefreshable_summaries,omitempty"`
	// WaitingSubagents counts the subagents a SubagentStop hook reported whose
	// transcripts were not written yet at the last pass. It is not a problem:
	// each is registered once its transcript appears, or rejected when it
	// never does, so only status --verbose and --json show it.
	WaitingSubagents int `json:"waiting_subagents,omitempty"`
	// PendingNativeLinks counts unresolved native child parent links without capture failures.
	PendingNativeLinks int `json:"pending_native_links,omitempty"`
	// NativeLinkPendingReasons contains fixed content-free link reconciliation reasons.
	NativeLinkPendingReasons map[string]int `json:"native_link_pending_reasons,omitempty"`
	// RunningSubagents counts the subagents that were resumed after their
	// last SubagentStop and were still writing at the last pass. It is not
	// a problem: each publishes after its next stop, or once its transcript
	// is quiet for 30 minutes, so only status --verbose and --json show it.
	RunningSubagents int `json:"running_subagents,omitempty"`
	// ExpiredSubagents lists the subagents dropped in the last
	// ExpiredSubagentWindow because Claude Code never wrote their
	// transcripts, newest last and at most MaxExpiredSubagents of them. It
	// is not a problem (there is nothing to do), so only status --verbose
	// and --json show it. Each pass carries the list forward with
	// CarryExpiredSubagents.
	ExpiredSubagents []ExpiredSubagent `json:"expired_subagents,omitempty"`
}

// ExpiredSubagent is a subagent the collector stopped waiting for because
// its transcript was never written. It names no content.
type ExpiredSubagent struct {
	ArchiveSessionID string `json:"archive_session_id"`
	// AgentType is the sanitized type the SubagentStop hook reported, or
	// empty when it named none. Like the rest of status.json, it is never
	// uploaded.
	AgentType string    `json:"agent_type,omitempty"`
	ExpiredAt time.Time `json:"expired_at"`
}

const (
	// ExpiredSubagentWindow is how long an ExpiredSubagent stays listed.
	ExpiredSubagentWindow = 7 * 24 * time.Hour
	// MaxExpiredSubagents caps the list; the oldest entries go first.
	MaxExpiredSubagents = 100
)

// CarryExpiredSubagents returns previous followed by fresh, keeping only the
// entries that expired within ExpiredSubagentWindow of now (an entry more
// than the window in the future, from a clock that has since jumped back,
// goes too), and one entry per subagent, the latest. It keeps the newest
// MaxExpiredSubagents, sorted oldest first.
func CarryExpiredSubagents(previous, fresh []ExpiredSubagent, now time.Time) []ExpiredSubagent {
	latest := map[string]ExpiredSubagent{}
	for _, entry := range slices.Concat(previous, fresh) {
		age := now.Sub(entry.ExpiredAt)
		if entry.ArchiveSessionID == "" || age >= ExpiredSubagentWindow || age <= -ExpiredSubagentWindow {
			continue
		}
		if prior, found := latest[entry.ArchiveSessionID]; found && prior.ExpiredAt.After(entry.ExpiredAt) {
			continue
		}
		latest[entry.ArchiveSessionID] = entry
	}
	out := make([]ExpiredSubagent, 0, len(latest))
	for _, entry := range latest {
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b ExpiredSubagent) int {
		if c := a.ExpiredAt.Compare(b.ExpiredAt); c != 0 {
			return c
		}
		return strings.Compare(a.ArchiveSessionID, b.ArchiveSessionID)
	})
	if len(out) > MaxExpiredSubagents {
		out = out[len(out)-MaxExpiredSubagents:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetLastErrors replaces the problems the status records with problems,
// keeping LastError and LastErrors in step. An empty problem is left out, and
// no problems clears both.
func (s *Status) SetLastErrors(problems ...string) {
	problems = slices.DeleteFunc(slices.Clone(problems), func(problem string) bool { return problem == "" })
	if len(problems) == 0 {
		s.LastError, s.LastErrors = "", nil
		return
	}
	s.LastErrors = problems
	s.LastError = strings.Join(problems, "; ")
}

// AddLastError adds problem after the problems the status already records.
// A status file from before LastErrors existed has its LastError split where
// the collector joined problems, on "; ", as status reads it.
func (s *Status) AddLastError(problem string) {
	problems := s.LastErrors
	if len(problems) == 0 && s.LastError != "" {
		problems = strings.Split(s.LastError, "; ")
	}
	s.SetLastErrors(append(problems[:len(problems):len(problems)], problem)...)
}

// ReplaceLastError puts problem in place of the first recorded problem equal
// to old, keeping the others and their order, or adds problem after them
// when none is.
func (s *Status) ReplaceLastError(old, problem string) {
	if i := slices.Index(s.LastErrors, old); i >= 0 {
		problems := slices.Clone(s.LastErrors)
		problems[i] = problem
		s.SetLastErrors(problems...)
		return
	}
	s.AddLastError(problem)
}

func (s *Store) statusPath() string { return filepath.Join(s.home, "status.json") }

// SaveStatus durably records the latest Status.
func (s *Store) SaveStatus(status Status) error {
	return local.Write(s.statusPath(), status)
}

// LoadStatus returns the last saved Status, or the zero value if none exists
// yet.
func (s *Store) LoadStatus() (Status, error) {
	var status Status
	err := local.Read(s.statusPath(), &status)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	return status, nil
}

// SetScanPending journals work before scanning. A failed or interrupted update
// remains pending even when the previous published cache is still valid.
func (s *Store) SetScanPending(id string, pending bool) error {
	if !safeFileComponent(id) {
		return errors.New("invalid session ID")
	}
	path := filepath.Join(s.home, "pending-scans", id+".json")
	if pending {
		return local.Write(path, true)
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ScanPending reports whether a scan of the session was journaled by
// SetScanPending and never completed, so its outcome is still owed.
func (s *Store) ScanPending(id string) (bool, error) {
	if !safeFileComponent(id) {
		return false, errors.New("invalid session ID")
	}
	var pending bool
	err := local.Read(filepath.Join(s.home, "pending-scans", id+".json"), &pending)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case IsUndecodable(err):
		// The journal exists, so a scan was started; that it no longer
		// decodes changes nothing (readAsPending). The next scan rewrites it.
		return true, nil
	}
	return pending, err
}

// ScanSignature is one session's "nothing to do" token: the exact size and
// nanosecond modification time of the transcript the last completed scan
// consumed, plus the versions that scan ran under. Its presence asserts that
// the scan ended settled — published, declined, or unchanged — and never
// blocked, so the next pass can skip the session on a matching stat alone.
//
// It lives in its own small file rather than inside the published cache
// because reading it has to stay cheap: the published cache holds a whole
// source bundle (hundreds of kilobytes), and decoding one per registered
// session per pass is precisely the cost this token exists to remove.
// Anything that invalidates the assertion removes the token (see
// RemoveScanSignature's callers).
type ScanSignature struct {
	// PublishedLabel identifies retained safe native name evidence, independently of lookup time.
	PublishedLabel string `json:"published_label,omitempty"`
	// A settled complete authority token carries only compact acknowledged facts.
	SourceSetDigest       string    `json:"source_set_digest,omitempty"`
	CurrentRevision       string    `json:"current_revision,omitempty"`
	SourceSchemaVersion   int       `json:"source_schema_version,omitempty"`
	MetadataSchemaVersion int       `json:"metadata_schema_version,omitempty"`
	SourceSetComplete     bool      `json:"source_set_complete,omitempty"`
	MeaningfulCapturedAt  time.Time `json:"meaningful_captured_at,omitzero"`

	// SourceSetVersion invalidates earlier Codex signatures without decoding bundles.
	SourceSetVersion int `json:"source_set_version,omitempty"`
	// Frozen marks completed retained-history maintenance, independently of
	// the live native source's stat. Ordinary capture never trusts this token.
	Frozen          bool                      `json:"frozen,omitempty"`
	SourceSignature *agentapi.SourceSignature `json:"source_signature,omitempty"`
	SkillEvidence   string                    `json:"skill_evidence,omitempty"`
	TranscriptSize  int64                     `json:"transcript_size"`
	TranscriptMtime int64                     `json:"transcript_mtime_unix_nano"`
	// The derivation versions are part of the signature: a parser, filter, or
	// adapter upgrade changes what an unchanged transcript would produce, so
	// it must re-scan rather than skip.
	ParserVersion  string `json:"parser_version"`
	FilterVersion  string `json:"filter_version"`
	AdapterVersion string `json:"adapter_version"`
	// SourceFormat is the format the scan actually produced, which decides
	// whether a stat is trustworthy evidence at all (see unchangedSinceLastScan).
	SourceFormat string `json:"source_format,omitempty"`
	// SourceKind is the registration's source. A Cursor database chat is
	// identified by its sourceidentity.CursorSignature instead of a file stat.
	SourceKind            archive.SourceKind `json:"source_kind,omitempty"`
	CursorLastUpdatedAt   int64              `json:"cursor_last_updated_at,omitempty"`
	CursorHeaderCount     int                `json:"cursor_header_count,omitempty"`
	CursorLastBubbleID    string             `json:"cursor_last_bubble_id,omitempty"`
	CursorMessageRows     int                `json:"cursor_message_rows,omitempty"`
	CursorLastMessageHash string             `json:"cursor_last_message_hash,omitempty"`
	// Failed marks a Cursor chat whose read at this state could not be
	// captured (see rememberFailedRead); FailedError is that error's text,
	// "" for a recorded gap.
	Failed      bool   `json:"failed,omitempty"`
	FailedError string `json:"failed_error,omitempty"`
	// FailedMaxBytes and FailedRecordLimit are the size limits the failed
	// read, or the size-limit gap (Blocked), ran under; the failure or gap
	// stands only while they do.
	FailedMaxBytes    int64 `json:"failed_max_bytes,omitempty"`
	FailedRecordLimit int64 `json:"failed_record_limit,omitempty"`
	// FailedRawLimit is the raw size limit for the session's harness under
	// which the failure or gap was reached: raising it reads the source
	// again. Zero (a signature from before the field) stands under none.
	FailedRawLimit int64 `json:"failed_raw_limit,omitempty"`
	// SizeLimit names the limit a size-limit gap (Blocked) stopped at, and
	// how far over it the source was, for status.
	SizeLimit *SizeLimitGap `json:"size_limit,omitempty"`
	// Blocked marks a scan that ended in a recorded capture gap (see
	// CacheStatusBlocked) at this source state, so an unchanged source is
	// skipped rather than read again to reach the same gap. For
	// BlockedReasonTranscriptMissing the state is the source's absence: the
	// signature holds while the source is still missing.
	Blocked BlockedReason `json:"blocked,omitempty"`
	// PublishedLastHead is the last-HEAD observation (commit and first-seen
	// time) the published metadata held when this signature was recorded.
	// A registration whose observation differs owes a metadata update, so
	// the session is scanned again however unchanged its source is.
	PublishedLastHead string `json:"published_last_head,omitempty"`
}

// SizeLimitKind names which size limit a size-limit gap stopped at. Once
// written each keeps its spelling.
type SizeLimitKind string

// Size limits a capture gap can stop at.
const (
	// SizeLimitRaw: the source (a Codex thread: every rollout its history
	// spans) is larger than its harness's raw transcript limit.
	SizeLimitRaw SizeLimitKind = "raw"
	// SizeLimitFiltered: what the filter keeps is larger than the filtered
	// transcript limit.
	SizeLimitFiltered SizeLimitKind = "filtered"
	// SizeLimitRecord: one record is larger than the record size limit.
	SizeLimitRecord SizeLimitKind = "record"
	// SizeLimitReadBudget: reading the session needs more memory than one
	// session's share of a pass (collector.SessionReadCeiling).
	SizeLimitReadBudget SizeLimitKind = "read_budget"
)

// SizeLimitGap is the limit a size-limit gap stopped at.
type SizeLimitGap struct {
	Kind       SizeLimitKind `json:"kind"`
	LimitBytes int64         `json:"limit_bytes"`
	// SizeBytes is the size measured against the limit, when known: the
	// raw or filtered size. Record and read_budget gaps have none.
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

// CursorSignature is the Cursor chat state the signature was recorded at.
func (s ScanSignature) CursorSignature() sourceidentity.CursorSignature {
	return sourceidentity.CursorSignature{
		LastUpdatedAt: s.CursorLastUpdatedAt, HeaderCount: s.CursorHeaderCount, LastBubbleID: s.CursorLastBubbleID,
		MessageRows: s.CursorMessageRows, LastMessageHash: s.CursorLastMessageHash,
	}
}

func (s *Store) scanSignaturePath(id string) string {
	return filepath.Join(s.home, "scan-signatures", id+".json")
}

// SaveScanSignature records the token, skipping the write (and its two fsyncs)
// when nothing about it changed.
func (s *Store) SaveScanSignature(id string, signature ScanSignature) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if existing, found, err := s.LoadScanSignature(id); err != nil {
		return err
	} else if found && sameScanSignature(existing, signature) {
		return nil
	}
	return local.Write(s.scanSignaturePath(id), signature)
}

// LoadScanSignature returns a session's scan signature. One that cannot be
// read counts as absent: the session is then scanned, the safe answer.
func (s *Store) LoadScanSignature(id string) (ScanSignature, bool, error) {
	if !safeFileComponent(id) {
		return ScanSignature{}, false, errors.New("archive session ID is not a safe file name component")
	}
	signature, found := readScanSignature(s.scanSignaturePath(id))
	return signature, found, nil
}

// readScanSignature reads the token at path, or returns found=false when
// there is none to trust: a missing token, or a corrupt one. A corrupt token
// is not a failure: it only means this session cannot be skipped, which is
// the safe answer.
func readScanSignature(path string) (ScanSignature, bool) {
	var signature ScanSignature
	if err := local.Read(path, &signature); err != nil {
		return ScanSignature{}, false
	}
	return signature, true
}

// RemoveScanSignature drops a session's scan signature, so the next pass
// scans it rather than skipping it on a stat.
func (s *Store) RemoveScanSignature(id string) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	err := os.Remove(s.scanSignaturePath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove scan signature %q: %w", id, err)
	}
	return nil
}

func sameCodexAdmission(a, b *archive.CodexAdmissionProof) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameScanSignature(a, b ScanSignature) bool {
	at, bt := a.SourceSignature, b.SourceSignature
	a.SourceSignature, b.SourceSignature = nil, nil
	if a != b {
		return false
	}
	if at == nil || bt == nil {
		return at == bt
	}
	return *at == *bt
}
