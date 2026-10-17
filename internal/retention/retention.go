// Package retention deletes local-machine-owned archive data that has aged
// out: source snapshots superseded more than a grace period ago, and,
// separately, entire sessions once their most recently captured evidence is
// older than a configured retention window.
//
// Before deleting anything it fetches the session's remote metadata.json
// and treats that, not the local cache, as the current pointer: the current
// source is never deleted regardless of age. Its immediate predecessor is
// normally retained; a privacy-sensitive predecessor can be deleted after
// a verified read-back and the reader grace interval. Whole-session
// expiry deletes metadata before sources, so an interruption leaves at
// worst unreferenced objects, never a live pointer to missing data; a
// failed expiry keeps the local registration (ownership) so the next sweep
// retries it. It never considers another machine's sessions — registrations
// only ever exist on the machine that created them.
//
// Every age it compares was stamped by this machine's clock, so the clock alone
// can never be the reason something is deleted: before the first deletion
// decided by age, a sweep checks the clock against the storage service's
// and against the previous collector pass, and holds every such deletion
// when they disagree (see clock.go).
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Options configures one Sweep call.
type Options struct {
	// PrivacyVerified reports whether the current publication for this
	// session has passed a full metadata-and-source read-back under the
	// current destination. Nil preserves privacy-sensitive predecessors.
	PrivacyVerified func(archive.SessionRegistration, archive.Metadata) bool
	// CurrentDestination reports whether a registration's published evidence
	// lives in the bucket this sweep is connected to. It deliberately is not
	// the collector's AcceptSession: that decides what may still be published,
	// and a session this machine has stopped publishing (its project was
	// excluded, its application deselected) still owns objects here that must
	// age out. Only a session admitted into another storage destination (by
	// its recorded destination ID, or for an older registration without one,
	// admitted before the current destination was configured) has its
	// objects somewhere else; its local state is still pruned when it
	// expires, but no delete is ever issued against the current bucket on
	// its behalf. Nil means every registration belongs to the current
	// destination.
	CurrentDestination func(archive.SessionRegistration) bool
	// Publishable is the collector's AcceptSession: whether this session's
	// outstanding work will ever be published. It never decides whether a
	// session is swept. It only decides whether a pending request or
	// publication defers expiry, because deferring for work the collector
	// will never do would keep that session forever. Nil means publishable.
	Publishable func(archive.SessionRegistration) bool
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// ServerClock returns the storage service's current time, the
	// independent check on Now that any deletion decided by age needs (see
	// clock.go). Nil reads it from the bucket itself: the modification time
	// the service stamps on a tiny object written under .setup-test/ and
	// removed again at once.
	ServerClock func(context.Context) (time.Time, error)
	// PreviousScanAt is when the collector pass before this one ran (the
	// Status.LastScanAt read before this pass began). A Now more than
	// MaxPassGap past it means the clock jumped, or the machine was off: either
	// way age-driven deletion waits one pass. Zero skips the check.
	PreviousScanAt time.Time
	// GracePeriod bounds how long a superseded (no longer current) source
	// snapshot stays downloadable before deletion. The ordinary immediate
	// predecessor remains; a privacy-sensitive predecessor becomes eligible
	// after verification and this interval. Defaults to 24h.
	GracePeriod time.Duration
	// SessionMaxAge is whole-session retention: once a session's most
	// recently captured evidence is older than this, its metadata and every
	// source snapshot are deleted together, never a source-only rule that
	// could leave a live metadata pointer dangling. Zero disables
	// whole-session deletion.
	SessionMaxAge time.Duration
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) gracePeriod() time.Duration {
	if o.GracePeriod > 0 {
		return o.GracePeriod
	}
	return 24 * time.Hour
}

// Result summarizes one Sweep call.
type Result struct {
	DeletedSnapshots int
	DeletedSessions  []string
	// PrunedSessions expired only locally, with no call to the current bucket
	// for their objects: nothing of theirs is in it, either because their
	// evidence was published to a destination this machine no longer uses or
	// because nothing was ever published (the transcript vanished before the
	// first capture).
	PrunedSessions []string
	Errors         map[string]error
	// Held, when set, is why this sweep deleted nothing on account of age:
	// it wraps ErrClockAhead, ErrClockUnverified, or ErrClockJumped. The
	// sessions it would have expired are not failures; the next sweep whose
	// clock checks out expires them.
	Held error
	// Unfinished counts the sessions the sweep did not reach before its
	// context ended. They are not failures either: the next sweep carries
	// on with them.
	Unfinished int
}

// Sweep processes every session registered on this machine, including ones the
// collector would no longer publish: a registration this machine owns is the
// only record that its remote objects exist, so skipping it would leave that
// session's data and local state behind forever. One session's failure is
// isolated in Result.Errors and left to retry on the next call, like
// collector.Run. A sweep whose context ends stops where it is, as a
// collector pass does: the session in flight and the rest are left for the
// next sweep (Result.Unfinished), not reported as failing.
func Sweep(ctx context.Context, local *state.Store, store storage.ObjectStore, opts Options) (Result, error) {
	// The caller holds the collector lock, as for collector.Run.
	local = local.ForCollectorPass()
	s := &sweeper{ctx: ctx, local: local, store: store, opts: opts, now: opts.now(), result: Result{Errors: map[string]error{}}}

	// An unreadable registration or request fails only its own session, as
	// in collector.Run.
	registrations, registrationIssues, err := local.ScanRegistrations()
	if err != nil {
		return Result{}, err
	}
	maps.Copy(s.result.Errors, registrationIssues)
	// One directory read for the whole sweep, rather than one per session.
	requests, requestIssues, err := local.ScanRequests()
	if err != nil {
		return Result{}, err
	}
	s.requested = make(map[string]bool, len(requests))
	for _, req := range requests {
		s.requested[req.ArchiveSessionID] = true
	}
	for id, issue := range requestIssues {
		s.result.Errors[id] = issue
		// A request that could not be read may still hold evidence, so it
		// defers expiry like any other; a quarantined one no longer exists.
		if !errors.Is(issue, state.ErrQuarantined) {
			s.requested[id] = true
		}
	}
	for i, reg := range registrations {
		if ctx.Err() != nil {
			s.result.Unfinished += len(registrations) - i
			break
		}
		s.record(reg.ArchiveSessionID, s.session(reg))
	}
	// A registration that exists but could not be read this time is still
	// registered; one that was moved aside is not, and its session is an
	// orphan like any other.
	keep := map[string]bool{}
	for _, reg := range registrations {
		keep[reg.ArchiveSessionID] = true
	}
	for id, issue := range registrationIssues {
		keep[id] = keep[id] || !errors.Is(issue, state.ErrQuarantined)
	}
	s.orphans(keep)
	return s.result, nil
}

// record files one session's outcome: an error, unless the sweep's context
// ended with the session in flight, which is the deadline failing rather than
// the session.
func (s *sweeper) record(id string, err error) {
	switch {
	case err == nil:
	case s.ctx.Err() != nil:
		s.result.Unfinished++
	default:
		s.result.Errors[id] = err
	}
}

// orphans expires the sessions whose registration is gone while their
// published, pending, or superseded state remains (see
// state.OrphanedSessions): a registration moved aside because it no longer
// decoded, or a crash in the middle of forgetting one. Nothing else records
// that their objects exist, so without this they would outlive retention.
// Such a session ages from its capture when that is recorded, otherwise from
// the last change to its files, and is deleted from the bucket (under every
// harness when its own is not recorded: another harness has nothing at its
// random ID) before its local state is forgotten.
func (s *sweeper) orphans(keep map[string]bool) {
	ids, err := s.local.OrphanedSessions(keep)
	if err != nil {
		s.result.Errors["orphaned-state"] = err
		return
	}
	for i, id := range ids {
		if s.ctx.Err() != nil {
			s.result.Unfinished += len(ids) - i
			return
		}
		s.record(id, s.orphan(id))
	}
}

var knownHarnesses = agentmeta.Names(agentmeta.Builtins())

func (s *sweeper) orphan(id string) error {
	if err := s.local.CheckDurableSessionRead(id); err != nil {
		return err
	}
	ageFrom := s.local.OrphanChangedAt(id)
	summary, found, err := s.local.LoadPublishedSummary(id)
	if err != nil {
		return fmt.Errorf("load published cache of an unregistered session: %w", err)
	}
	if found && !summary.RetentionAge().IsZero() {
		ageFrom = summary.RetentionAge()
	}
	if !s.expired(ageFrom) || !s.clockAllowsDeletion() {
		return nil
	}
	harnesses := s.local.OrphanHarnesses(id)
	if len(harnesses) == 0 {
		harnesses = knownHarnesses
	}
	for _, harness := range harnesses {
		if err := s.local.CheckDurableSessionRead(id); err != nil {
			return err
		}
		if err := DeleteWholeSession(s.ctx, s.store, harness, id); err != nil {
			return fmt.Errorf("delete unregistered session: %w", err)
		}
	}
	forgotten, err := s.local.ForgetOrphan(id)
	if forgotten {
		s.result.DeletedSessions = append(s.result.DeletedSessions, id)
	}
	if err != nil {
		return fmt.Errorf("forget unregistered session: %w", err)
	}
	return nil
}

// sweeper is one Sweep: its inputs, the clock verdict it reaches at most
// once, and its result.
type sweeper struct {
	ctx       context.Context
	local     *state.Store
	store     storage.ObjectStore
	opts      Options
	now       time.Time
	requested map[string]bool
	clock     clockVerdict
	result    Result
}

// session sweeps one registered session.
func (s *sweeper) session(reg archive.SessionRegistration) error {
	id := reg.ArchiveSessionID
	if err := s.local.CheckDurableSessionRead(id); err != nil {
		return err
	}
	summary, found, err := s.local.LoadPublishedSummary(id)
	if err != nil {
		return fmt.Errorf("load published cache: %w", err)
	}
	ageFrom, err := s.sessionAge(reg, summary, found)
	if err != nil {
		return err
	}
	locallyExpired, deferForWork, owed, err := s.sessionExpiry(reg, found, ageFrom)
	if err != nil {
		return err
	}

	currentDestination := s.opts.CurrentDestination == nil || s.opts.CurrentDestination(reg)
	// A lost publication record may be the only evidence of remote objects.
	// Read it only when an expired session belongs to this destination.
	hasRemoteEvidence := false
	if locallyExpired && currentDestination {
		hasRemoteEvidence = summary.Published || owed.Upload || s.local.LostPublication(id)
	}
	action := chooseSessionAction(locallyExpired, currentDestination, hasRemoteEvidence)
	switch action {
	case forgetPreviousDestination:
		return s.forget(reg, deferForWork, &s.result.PrunedSessions, "forget session from a previous destination")
	case forgetNeverPublished:
		return s.forget(reg, deferForWork, &s.result.PrunedSessions, "forget never-published session")
	case leaveSession:
		return nil
	case sweepRemote:
		return s.remote(reg, summary, ageFrom, locallyExpired, deferForWork)
	default:
		return fmt.Errorf("unknown retention action %d", action)
	}
}

// sessionAge keeps the admission fallback for a session with no capture.
func (s *sweeper) sessionAge(reg archive.SessionRegistration, summary state.PublishedSummary, found bool) (time.Time, error) {
	// A session ages from its cached capture when it has one. A registration
	// that never produced a capture (the transcript vanished before the first
	// scan, or was never readable) ages from its admission instead; otherwise
	// it would be the one kind of local state that never expires. Admission,
	// not the start: an imported session can have started years ago, and a
	// failed first upload must not expire it at once.
	ageFrom := reg.Admitted()
	if found && !summary.RetentionAge().IsZero() {
		var err error
		if ageFrom, err = s.captureAge(reg.ArchiveSessionID, summary); err != nil {
			return time.Time{}, err
		}
	}
	return ageFrom, nil
}

// sessionExpiry reads outstanding work only when it can affect expiry.
func (s *sweeper) sessionExpiry(reg archive.SessionRegistration, found bool, ageFrom time.Time) (bool, bool, state.Outstanding, error) {
	id := reg.ArchiveSessionID
	locallyExpired := s.expired(ageFrom)
	// deferForWork says whether a queued request or pending publication
	// postpones expiry: only when the collector will actually do that work.
	// A session it no longer publishes is one case. A registration that never
	// received a transcript path is the other: Cursor registers a chat at its
	// first prompt and the path normally arrives with a later hook, but with
	// the app's transcripts disabled it never does, so nothing can ever be
	// captured for it. Once such a session is itself older than the retention
	// window, its request (which can carry hook text) stops deferring expiry;
	// otherwise the registration and that text would stay on this machine
	// forever. It never published, so forgetting it needs no bucket call. A
	// transcript file the app created but never wrote is the same case one
	// step later: nothing of it was ever captured, unless a first
	// publication is still waiting to upload (built before the file was
	// emptied). A session read from Cursor's database has no path by design
	// and is captured all the same, so its work still defers expiry.
	deferForWork := s.opts.Publishable == nil || s.opts.Publishable(reg)
	if reg.ReadsTranscriptFile() && s.expired(reg.Admitted()) {
		neverCapturable := reg.TranscriptPath == ""
		if !neverCapturable && !found && transcriptEmpty(reg.TranscriptPath) {
			owed, err := s.local.Outstanding(reg, s.requested[id])
			if err != nil {
				return false, false, state.Outstanding{}, fmt.Errorf("check outstanding work: %w", err)
			}
			neverCapturable = !owed.Upload
		}
		if neverCapturable {
			deferForWork = false
		}
	}
	// What the session still owes (state.Outstanding), read only for a
	// session old enough to expire: the steady state stays a summary read.
	var owed state.Outstanding
	if locallyExpired {
		var err error
		if owed, err = s.local.Outstanding(reg, s.requested[id]); err != nil {
			return false, false, state.Outstanding{}, fmt.Errorf("check outstanding work: %w", err)
		}
	}
	if locallyExpired && deferForWork {
		// Unpublished work is not expired evidence. A session that crosses the
		// retention boundary on the same pass its publication fails
		// transiently would otherwise have its new source, its pending
		// publication, its request, and that request's hook evidence all
		// deleted together, silently. Say nothing and retry next pass: either
		// the publication lands and the session expires with its evidence
		// intact, or it keeps failing and reports itself as a failure. A
		// session the collector no longer publishes is not deferred: its
		// outstanding work will never be done, so waiting on it would keep
		// the session forever. A scan that never finishes does not defer it
		// either (see state.Outstanding.DefersExpiry).
		locallyExpired = !owed.DefersExpiry()
	}
	return locallyExpired, deferForWork, owed, nil
}

type sessionAction uint8

const (
	sweepRemote sessionAction = iota
	leaveSession
	forgetPreviousDestination
	forgetNeverPublished
)

// chooseSessionAction decides which side effect is allowed for this session.
// A previous destination can only lose local state. A never-published session
// can be forgotten without a remote call only when no remote evidence exists.
func chooseSessionAction(expired, currentDestination, hasRemoteEvidence bool) sessionAction {
	if !currentDestination {
		if expired {
			return forgetPreviousDestination
		}
		return leaveSession
	}
	if expired && !hasRemoteEvidence {
		return forgetNeverPublished
	}
	return sweepRemote
}

// transcriptEmpty reports a transcript file that exists and holds nothing:
// the collector waits on it without capturing anything (collector.read).
func transcriptEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == 0
}

// remote does the part of a session's sweep that consults the bucket:
// whole-session expiry and the deletion of superseded snapshots past their
// grace period.
func (s *sweeper) remote(reg archive.SessionRegistration, summary state.PublishedSummary, ageFrom time.Time, locallyExpired, deferForWork bool) error {
	id := reg.ArchiveSessionID
	superseded, err := s.supersededLedger(id)
	if err != nil {
		return err
	}
	// Skip the remote round trip when this pass could not delete anything:
	// ordinary predecessors stay in the ledger forever, so without this
	// every session ever republished would cost one GET per sync indefinitely.
	if !locallyExpired && !anySupersededExpirable(superseded, s.now, s.opts.gracePeriod()) {
		return nil
	}

	metadata, remoteErr := s.currentMetadata(reg)
	if remoteErr != nil && !errors.Is(remoteErr, storage.ErrNotFound) {
		return remoteErr
	}
	// Protect evidence published remotely just before a local acknowledgement
	// failed. Remote metadata for the capture already cached says nothing
	// new: its time is the cached one, clamped or not.
	capturedAt := ageFrom
	if remoteErr == nil {
		remoteAge, err := metadata.MeaningfulCapturedAt()
		if err != nil {
			return err
		}
		cachedAge := summary.MeaningfulCapturedAt
		if cachedAge.IsZero() {
			cachedAge = summary.CapturedAt
		}
		if remoteAge.After(capturedAt) && !remoteAge.Equal(cachedAge) {
			capturedAt = remoteAge
		}
	}

	// locallyExpired already carries SessionMaxAge, the cached capture time,
	// and the unfinished-work deferral; remote metadata can only make
	// capturedAt later, so it can only withdraw expiry, never grant it.
	if locallyExpired && s.expired(capturedAt) {
		if !s.clockAllowsDeletion() {
			return nil
		}
		if err := s.local.CheckDurableSessionRead(id); err != nil {
			return err
		}
		if err := DeleteWholeSession(s.ctx, s.store, reg.Harness.Name, id); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		// The remote deletion takes network time and is not done under the
		// request lock, because a hook waits for that lock on the user's
		// turn. A hook that fired meanwhile has left a request, and the
		// locked recheck keeps the session for it. The next collector pass
		// retains a complete pending bundle carrying that evidence. Exact
		// publication reconciliation refuses a formerly present sidecar that
		// is now absent; a durable deletion journal must authorize restoration.
		// If the request adds nothing, it is acknowledged and the next sweep
		// finishes expiry.
		return s.forget(reg, deferForWork, &s.result.DeletedSessions, "forget session")
	}

	if len(superseded) == 0 {
		return nil
	}
	if remoteErr != nil {
		return fmt.Errorf("current metadata is missing; preserve superseded sources")
	}
	return s.deleteSuperseded(reg, superseded, metadata)
}

// currentMetadata reads the session's live metadata, the pointer every
// deletion is checked against. A missing object is storage.ErrNotFound;
// metadata that does not describe this session is an error.
func (s *sweeper) currentMetadata(reg archive.SessionRegistration) (archive.Metadata, error) {
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return archive.Metadata{}, err
	}
	var data []byte
	if reg.Harness.Name == "codex" {
		data, err = boundedHistoryMetadata(s.ctx, s.store, metadataKey)
	} else {
		data, err = s.store.Get(s.ctx, metadataKey)
	}
	if errors.Is(err, storage.ErrNotFound) {
		return archive.Metadata{}, err
	}
	if err != nil {
		return archive.Metadata{}, fmt.Errorf("read current metadata before cleanup: %w", err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return archive.Metadata{}, fmt.Errorf("decode current metadata: %w", err)
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.Metadata{}, fmt.Errorf("invalid current metadata: %w", err)
	}
	if metadata.SessionID != reg.ArchiveSessionID || metadata.Harness.Name != reg.Harness.Name || !strings.HasPrefix(metadata.SourceBundle.Key, fmt.Sprintf("sessions/%s/%s/", reg.Harness.Name, reg.ArchiveSessionID)) {
		return archive.Metadata{}, fmt.Errorf("current metadata belongs to another session")
	}
	if _, err := metadata.SourceReferences(); err != nil {
		return archive.Metadata{}, err
	}
	if metadata.History != nil && (metadata.NativeSessionID != reg.NativeSessionID || metadata.ProjectID != reg.ProjectID || metadata.CapturedAt.IsZero()) {
		return archive.Metadata{}, errors.New("complete cleanup history authority required")
	}
	return metadata, nil
}

// deleteSuperseded deletes the ledger's snapshots past their grace period,
// keeping the current source and the ordinary immediate predecessor.
func (s *sweeper) deleteSuperseded(reg archive.SessionRegistration, superseded []state.SupersededSource, current archive.Metadata) error {
	if err := s.local.CheckDurableSessionRead(reg.ArchiveSessionID); err != nil {
		return err
	}
	refs, err := current.SourceReferences()
	if err != nil {
		return err
	}
	protected := map[string]bool{}
	for _, ref := range refs {
		protected[ref.Key] = true
	}
	currentDigest, err := current.SourceSetDigest()
	if err != nil {
		return err
	}
	id := reg.ArchiveSessionID
	// Append order records supersession order even if the clock moves backward.
	var predecessorKey string
	for _, entry := range superseded {
		if !strings.HasPrefix(entry.Key, fmt.Sprintf("sessions/%s/%s/source.", reg.Harness.Name, id)) {
			return fmt.Errorf("superseded source belongs to another session")
		}
		if !protected[entry.Key] {
			predecessorKey = entry.Key
		}
	}
	for _, entry := range superseded {
		if protected[entry.Key] {
			// Defensive: a key must never be both current and superseded;
			// if it somehow is, trust "current" and just clean the ledger.
			if err := s.local.RemoveSuperseded(id, entry.Key); err != nil {
				return fmt.Errorf("clean up ledger entry: %w", err)
			}
			continue
		}
		if (entry.Key == predecessorKey && !entry.PrivacySensitive) || s.now.Sub(entry.SupersededAt) < s.opts.gracePeriod() {
			continue
		}
		if entry.PrivacySensitive {
			if s.opts.PrivacyVerified == nil {
				continue
			}
			fresh, err := s.currentMetadata(reg)
			if err != nil {
				return err
			}
			freshDigest, err := fresh.SourceSetDigest()
			if err != nil {
				return err
			}
			if freshDigest != currentDigest {
				return fmt.Errorf("current metadata changed during privacy cleanup")
			}
			if !s.opts.PrivacyVerified(reg, fresh) {
				continue
			}
		}
		// Recheck after receipt callbacks and immediately before deletion. A
		// reactivated ordinary reference is protected just like a privacy ref.
		fresh, err := s.currentMetadata(reg)
		if err != nil {
			return err
		}
		freshDigest, err := fresh.SourceSetDigest()
		if err != nil {
			return err
		}
		if freshDigest != currentDigest {
			return errors.New("current metadata changed during source cleanup")
		}
		if !s.clockAllowsDeletion() {
			return nil
		}
		if err := s.local.CheckDurableSessionRead(id); err != nil {
			return err
		}
		if err := s.store.Delete(s.ctx, entry.Key); err != nil {
			return fmt.Errorf("delete superseded source %q: %w", entry.Key, err)
		}
		if err := s.local.RemoveSuperseded(id, entry.Key); err != nil {
			return fmt.Errorf("update superseded ledger: %w", err)
		}
		s.result.DeletedSnapshots++
	}
	return nil
}

// expired reports whether evidence captured at at is past the retention
// window by this sweep's clock.
func (s *sweeper) expired(at time.Time) bool {
	return s.opts.SessionMaxAge > 0 && !at.IsZero() && s.now.Sub(at) >= s.opts.SessionMaxAge
}

// forget forgets an expired session once the clock allows it, and lists it
// in into when it was forgotten.
func (s *sweeper) forget(reg archive.SessionRegistration, deferForWork bool, into *[]string, what string) error {
	if !s.clockAllowsDeletion() {
		return nil
	}
	forgotten, err := forgetExpired(s.local, reg, deferForWork, s.now)
	if forgotten {
		*into = append(*into, reg.ArchiveSessionID)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// forgetExpired forgets an expired session and leaves a removal record so
// backfill does not import it again. Every path that calls it is
// whole-session expiry past the retention window: after deleting the session
// from the current bucket, for a session that never published, and for one
// published to a previous destination. ForgetIdleSession writes the record
// before it forgets anything, so a failed write keeps the session registered
// for the next sweep to retry, and takes it back from a session a hook kept
// alive.
func forgetExpired(local *state.Store, reg archive.SessionRegistration, deferForWork bool, now time.Time) (bool, error) {
	return local.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, deferForWork, &state.RemovalRecord{
		Harness: reg.Harness.Name, Reason: state.RemovalReasonRetention, At: now,
	})
}

// anySupersededExpirable reports whether a sweep could delete at least one
// ledger entry now. The last entry is normally the immediate predecessor,
// which is retained unless marked privacy-sensitive. This is a necessary
// condition: it may say yes for an entry the full check then retains, but
// never no when a deletion is possible, so skipping on false is safe.
func anySupersededExpirable(superseded []state.SupersededSource, now time.Time, grace time.Duration) bool {
	for i, s := range superseded {
		if i == len(superseded)-1 && !s.PrivacySensitive {
			continue
		}
		if now.Sub(s.SupersededAt) >= grace {
			return true
		}
	}
	return false
}
