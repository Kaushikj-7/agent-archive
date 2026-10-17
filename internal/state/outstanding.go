package state

import (
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Outstanding is what one registered session still owes the bucket, as local
// state records it. It is the one definition of a "pending" session: status,
// setup's destination guard, uninstall's warning, backfill's upload progress
// and history, the collector's scan journal, and retention's expiry deferral
// (DefersExpiry) all read it through Store.Outstanding, and none re-derives it from the
// files underneath (a guard test in this package enforces that).
//
// The facets are what the files say; the methods are the questions callers
// ask of them:
//
//   - Owed: work the collector will do on its next pass. A queued request, an
//     interrupted scan, a publication built and not yet accepted by storage
//     (in flight, or held back by the upload interval), or a cached
//     candidate still waiting for that interval. Evidence exists locally
//     that is not in the bucket yet.
//   - Pending: Owed, or a session never captured at all. This is the count
//     users see as "pending".
//   - Rechecking: Pending, for a session already published that owes only
//     an interrupted scan; backfill history counts it apart from the
//     sessions not in the archive yet.
//   - SyncCanFinish: Pending, less a session only waiting for its
//     transcript, which nothing a sync does can publish.
//
// A recorded capture gap (Blocked) or a policy decline is not owed work: the
// collector has done everything it can until the source changes. A session
// the configuration no longer publishes is the caller's to leave out (see
// config.Config.AcceptSession); Outstanding does not read the configuration.
type Outstanding struct {
	// Removal reports a durable owner-bound deletion or restoration still owed.
	Removal bool
	// Requested: a hook or import request is queued and not yet
	// acknowledged. The caller supplies it (see QueuedRequests), since every
	// caller lists the requests once for all sessions.
	Requested bool
	// Scan: a scan was journaled (SetScanPending) and never completed.
	Scan bool
	// Upload: a publication was built and storage has not accepted it yet,
	// in flight or held back by the upload interval (the pending/ file).
	Upload bool
	// RateLimited: the cached candidate differs from the last publication
	// and waits for the upload interval.
	RateLimited bool
	// NeverCaptured: nothing is cached for the session: it has never been
	// captured, not even as a gap or a decline.
	NeverCaptured bool
	// WaitingForTranscript: the session reads a transcript file, has no path
	// to one yet, has never published, and has no publication in flight. A
	// Cursor chat whose transcript never arrived (transcripts turned off) is
	// the case: nothing of it can be published anywhere until a path does.
	// A Cursor database chat has no path by design and never waits.
	WaitingForTranscript bool
	// Blocked: the cached state is a recorded capture gap.
	Blocked bool
	// Published: a publication of the session was ever recorded.
	Published bool
}

// Owed reports work the collector will do for the session on its next pass:
// a queued request, an interrupted scan, an upload not yet accepted, or a
// candidate waiting for the upload interval.
func (o Outstanding) Owed() bool {
	return o.OwedAfterScan() || o.Scan
}

// OwedAfterScan is Owed without the scan journal: what decides, once a scan
// has run, whether its journal entry may be cleared.
func (o Outstanding) OwedAfterScan() bool {
	return o.Requested || o.Upload || o.RateLimited || o.Removal
}

// DefersExpiry reports work that postpones the session's expiry: a queued
// request or a publication storage has not accepted, evidence that exists
// only locally and that the collector delivers on a later pass whatever
// the transcript holds by then. It is the rule retention had before
// Outstanding existed. An interrupted scan and a rate-limited cache do not
// defer expiry: a scan that fails on every pass (a transcript the filter
// refuses since an upgrade, say) leaves both standing for good, so
// deferring on them would keep that session, locally and in the bucket,
// past its retention window forever.
func (o Outstanding) DefersExpiry() bool {
	return o.Requested || o.Upload
}

// Pending reports a session users see as pending: owed work, or never
// captured at all.
func (o Outstanding) Pending() bool {
	return o.Owed() || o.NeverCaptured
}

// Rechecking reports a pending session that is already in the archive and
// owes nothing but an interrupted scan: a re-scan that failed or was put off
// (a Codex session whose history coverage was not complete, say). It is
// pending, but unlike a session never published, a copy of it is already in
// the bucket.
func (o Outstanding) Rechecking() bool {
	return o.Published && o.Scan && !o.OwedAfterScan()
}

// SyncCanFinish reports a pending session a sync could finish: every pending
// one except a session only waiting for its transcript.
func (o Outstanding) SyncCanFinish() bool {
	return o.Pending() && !o.WaitingForTranscript
}

// QueuedRequests is the set of sessions with a queued request, the
// Requested input of Outstanding for each.
func QueuedRequests(reqs []Request) map[string]bool {
	queued := make(map[string]bool, len(reqs))
	for _, r := range reqs {
		queued[r.ArchiveSessionID] = true
	}
	return queued
}

// Outstanding reads what reg still owes the bucket (see Outstanding).
// requested is whether a request for it is queued. It costs a few stats and
// the published state's leading summary, never a full decode of its source
// bundles, so it is cheap enough to ask for every registered session.
func (s *Store) Outstanding(reg archive.SessionRegistration, requested bool) (Outstanding, error) {
	id := reg.ArchiveSessionID
	summary, found, err := s.LoadPublishedSummary(id)
	if err != nil {
		return Outstanding{}, err
	}
	scan, err := s.ScanPending(id)
	if err != nil {
		return Outstanding{}, err
	}
	upload, err := s.HasPending(id)
	if err != nil {
		return Outstanding{}, err
	}
	removal, err := s.hasDeletionWork(id)
	if err != nil {
		return Outstanding{}, err
	}
	o := Outstanding{
		Removal:       removal,
		Requested:     requested,
		Scan:          scan,
		Upload:        upload,
		RateLimited:   found && summary.Status == CacheStatusRateLimited,
		NeverCaptured: !found,
		Blocked:       found && summary.Status == CacheStatusBlocked,
		Published:     summary.Published,
	}
	o.WaitingForTranscript = reg.ReadsTranscriptFile() && reg.TranscriptPath == "" && !o.Published && !o.Upload
	return o, nil
}
