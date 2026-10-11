// Package collector runs the local scan/build/publish loop that turns a
// hook-registered session into a published metadata sidecar and source
// bundle. It owns publication cadence and change detection; it keeps its
// local state through internal/state, builds on internal/local for atomic
// file I/O and the machine-level lock, and does not own transcript reading
// (archive adapters), privacy filtering (archive adapters), or storage
// upload mechanics (storage.PutSourceThenMetadataIndexed). A caller runs Run under
// local.Lock(home) so only one collector process acts on a given home at a
// time; Run itself does not take that lock.
//
// A pass (Run) lists the registered sessions and scans each in turn; one
// scan of one session is a sessionScan, whose steps and cache-status
// transitions are described in session.go.
package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Options configures one Run call. The caller is responsible for holding
// local.Lock(home) around Run; Run itself does not acquire it, so it stays
// simple to call directly from tests.
type Options struct {
	labelReadObserver func(int64)
	// Labels supplies optional bounded native metadata for existing retained sessions.
	Labels           agentapi.LabelsLookup
	LabelEnvironment agentapi.LabelEnvironment
	labels           map[string]state.LabelEntry
	// PrepareCodexCoverage advances caller-owned qualified coverage once after admission work loads.
	PrepareCodexCoverage func(context.Context, []archive.SessionRegistration) error
	// CodexRollouts is one caller-owned bounded locator view shared by the pass.
	CodexRollouts agentapi.CodexRolloutLookup
	// PendingCodexRollouts observes locator evidence only after a history fence.
	// The caller shares one lazy catalog; it must not grant publication authority.
	PendingCodexRollouts func() agentapi.CodexRolloutLookup
	// ConfiguredCodexHomes supplies confined migration roots from configuration,
	// independently of current-locator hints. Native evidence still validates identity.
	ConfiguredCodexHomes []string
	// ResolveCodexReadHomes revalidates trusted default/hook/import/config roots
	// against current configuration; lookup observations never supply authority.
	ResolveCodexReadHomes func(config.Config) ([]string, error)
	// SkipSessionIndexRecovery is set after the CLI has already attempted its
	// bounded local recovery stage. Direct collector callers recover once.
	SkipSessionIndexRecovery bool
	// Parsers resolves pure derivation separately from native source access.
	Parsers       agentapi.ParsersLookup
	parserCache   map[string]agentapi.TranscriptParser
	Sources       agentapi.SourcesLookup
	sourcePasses  *sourcePassSet
	retainedOwner *sessionScan
	// Decoders translates retained legacy admission intents; no lookup is needed for new generic effects.
	Decoders agentapi.DecodersLookup
	// ParserVersion identifies metadata derivation independently of source capture.
	ParserVersion string
	AcceptSession func(archive.SessionRegistration) bool
	// MachineID identifies this machine in published metadata. Required.
	MachineID string
	// Now returns the current time. Defaults to time.Now; tests override it
	// for deterministic timestamps and rate-limit behavior.
	Now func() time.Time
	// Retry controls storage retry/backoff. Zero value uses storage's default.
	Retry storage.RetryPolicy
	// MinUploadInterval bounds how often one session may be republished.
	// Defaults to three minutes, matching the spec's cadence.
	MinUploadInterval time.Duration
	// RequireSkillUse, when true, skips publishing a session that has no
	// detected skill use. The spec's default is to capture such sessions
	// anyway (to preserve comparison evidence), so the zero value (false)
	// matches that default rather than requiring every caller to opt in.
	RequireSkillUse bool
	// SkillEvidence limits filesystem skill evidence in new publications.
	// Empty means body for older programmatic callers.
	SkillEvidence config.SkillEvidence
	// SupplementalEvidence observes non-transcript evidence such as the
	// installed skill inventory. The collector merges stable observations
	// without letting a new polling timestamp manufacture a new snapshot.
	SupplementalEvidence func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error)
	// MaxTranscriptBytes caps the size of a transcript after filtering: what
	// the published bundle carries. A larger one is recorded as a capture gap
	// (CacheStatusBlocked with BlockedReasonTranscriptTooLarge) rather than
	// an error. The raw transcript may be rawSizeFactor times larger, since
	// the bulk of a long session is tool output the filter drops; past that
	// it is the same gap without being read. Zero uses
	// DefaultMaxTranscriptBytes.
	MaxTranscriptBytes int64
	// Progress, when set, is called after each session the pass processes,
	// with whether it published. Backfill draws its upload progress from it.
	Progress func(Progress)
	// Stop, when set, is checked before each session; once it reports true
	// the pass ends early. The session in flight always finishes, and the
	// rest keep their pending work for the next pass.
	Stop func() bool
	// CursorDatabase is Cursor's state.vscdb, which cursor-sqlite sessions
	// are read from. Empty means the one under the user's home directory.
	CursorDatabase string
	// RepoKey returns archive.RepoKey of the git repository at a project
	// root, or "" when it has none. It is asked for a registration that
	// carries no key (one from before the field), once per project root per
	// pass. Nil runs git (internal/gitremote); tests set it.
	RepoKey func(root string) string

	// repoKeys is the pass's memory of RepoKey's answers, set by Run.
	repoKeys *repoKeyCache
	// afterCursorPass, set by a test, runs as a pass ends with how many
	// snapshots of Cursor's database the pass took.
	afterCursorPass func(snapshots int)
}

// Progress reports one session a pass has processed.
type Progress struct {
	ArchiveSessionID string
	Published        bool
}

// DefaultMaxTranscriptBytes is the filtered transcript size ceiling when
// Options.MaxTranscriptBytes is zero. It is archive.MaxRecordBytes, so a
// published bundle stays well inside what a reader decompresses by default.
const DefaultMaxTranscriptBytes int64 = archive.MaxRecordBytes

// rawSizeFactor is how much larger than the filtered size limit a raw
// transcript may be. The filter streams, one record at a time (each bounded
// by recordLimit), so memory follows what it keeps rather than the file's
// size; the factor bounds the read itself, and what a read that keeps
// nearly everything can hold before the filtered limit is checked.
const rawSizeFactor = 4

// DefaultMaxRawTranscriptBytes is the raw transcript size past which the
// collector, with its default limits, records a transcript as too large
// without reading it. Under it a transcript can still be too large once
// filtered, or hold one record over the record size limit: FilterTranscriptFile
// reports both.
const DefaultMaxRawTranscriptBytes = DefaultMaxTranscriptBytes * rawSizeFactor

// maxRawBytes is the raw size ceiling for a filtered size limit.
func maxRawBytes(maxFiltered int64) int64 {
	if maxFiltered > (1<<62)/rawSizeFactor {
		return 1 << 62
	}
	return maxFiltered * rawSizeFactor
}

// recordLimit is the largest single transcript record the collector reads,
// archive.MaxRecordBytes; a variable only so a test can lower it.
var recordLimit int64 = archive.MaxRecordBytes

func (o Options) maxTranscriptBytes() int64 {
	if o.MaxTranscriptBytes > 0 {
		return o.MaxTranscriptBytes
	}
	return DefaultMaxTranscriptBytes
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) minUploadInterval() time.Duration {
	if o.MinUploadInterval > 0 {
		return o.MinUploadInterval
	}
	return 3 * time.Minute
}

// Result summarizes one Run call. It never includes transcript content.
type Result struct {
	Scanned   int
	Published []string
	// Skipped lists sessions with nothing new to publish.
	Skipped []string
	// Waiting lists sessions whose new evidence is saved and held for the
	// upload interval (Options.MinUploadInterval); NextReadyAt is when the
	// earliest of them becomes due.
	Waiting     []string
	NextReadyAt time.Time
	// WaitingSubagents lists the subagents a SubagentStop hook reported whose
	// transcripts are not written yet. Each is retried on the next pass, and
	// rejected once subagentTranscriptGrace has passed; neither is an error.
	WaitingSubagents []string
	// RunningSubagents lists the registered subagents that were resumed
	// after their last SubagentStop and are still writing. Each is
	// captured, or its published snapshot extended, when it stops again or
	// its transcript goes quiet; it is not an error.
	RunningSubagents []string
	// RejectedSubagents maps each subagent candidate rejected this pass to
	// its code. A rejection that lost nothing (the transcript was never
	// written, say) is only counted here; any other is in Errors too.
	// PendingNativeLinks counts unresolved conversational links separately from capture failures.
	PendingNativeLinks       int
	NativeLinkPendingReasons map[string]int
	RejectedSubagents        map[string]string
	Errors                   map[string]error
	// Deferred maps each session whose scan was put off this pass to why (a
	// Deferred* reason): its source could not be read completely yet for a
	// reason the next pass is expected to clear, so it is not a failure and
	// is not in Errors. Like a failed scan, the session keeps its scan
	// journal and is counted as pending; that journal still does not defer
	// its expiry (see state.Outstanding.DefersExpiry).
	Deferred map[string]string
}

// Run performs one collector pass over every registered session: for each,
// read its transcript, filter it, and compare the result against the last
// published bundle. A session with no meaningful change is left untouched at
// no storage cost. A session with new evidence is published source-before-
// metadata, subject to MinUploadInterval. One session's failure does not
// stop the pass; it is recorded in Result.Errors and left retryable on the
// next call.
func Run(ctx context.Context, local *state.Store, store storage.ObjectStore, opts Options) (_ Result, runErr error) {
	if local == nil || store == nil {
		return Result{}, errors.New("local store and object store are required")
	}
	if opts.MachineID == "" {
		return Result{}, errors.New("machine ID is required")
	}
	_, durableErr := local.DurableStorageObligations()
	if durableErr != nil {
		return Result{Errors: map[string]error{"durable-storage": durableErr}}, durableErr
	}
	if err := local.ResumeImportAdmissions(ctx); err != nil {
		return Result{Errors: map[string]error{"staged-imports": err}}, err
	}
	// Recovery can finish cancellation or establish selected admissions. Report
	// their current obligations, rather than the pre-recovery orphan snapshot.
	durableObligations, durableErr := local.DurableStorageObligations()
	if durableErr != nil {
		return Result{Errors: map[string]error{"durable-storage": durableErr}}, durableErr
	}
	for _, obligation := range durableObligations {
		if obligation.SessionID == "" {
			return Result{Errors: map[string]error{"durable-storage": state.ErrDurableStorageRecovery}}, state.ErrDurableStorageRecovery
		}
	}
	recoveryLocal, closeRecovery := local.WithReadBudget(ctx, (&sessionScan{opts: opts}).readBudget())
	var generationRecoveryErr error
	if ctx.Err() == nil {
		generationRecoveryErr = recoveryLocal.ResumeGenerationRecoveries(ctx)
	}
	if generationRecoveryStopsPass(generationRecoveryErr) {
		closeRecovery()
		return Result{}, generationRecoveryErr
	}
	opts.parserCache = make(map[string]agentapi.TranscriptParser)
	now := opts.now()
	// Recover first-start events that could not obtain hooks.lock on the
	// user's turn before scanning registrations for this pass.
	var recoveryErr error
	if ctx.Err() == nil && !opts.SkipSessionIndexRecovery {
		_, recoveryErr = recoveryLocal.RecoverSessionIndexScheduled(ctx, state.SessionIndexRecoverySlice)
	}
	closeRecovery()
	if state.SessionIndexRecoveryInterrupted(recoveryErr) {
		recoveryErr = nil
	}
	replayErr := capture.ReplayAdmissionIntents(local.Home(), now, opts.Decoders)
	// The caller holds the collector lock, so this pass is the only writer
	// of the files it owns and may move a corrupt one aside.
	local = local.ForCollectorPass()
	local.RemoveStaleTemps()
	// A copy of Cursor's database a killed collector or backfill left
	// behind goes on every pass, whether or not this one reads Cursor.
	if sweeper, ok := opts.Sources.(agentapi.SourceSweeper); ok {
		sweeper.SweepSources()
	}
	// Establish one lazy source scope before child admission borrows its ledger.
	closeCursorPass := openCursorPass(nil, &opts)
	defer func() {
		if err := closeCursorPass(); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	subagents := materializeSubagentCandidates(ctx, local, opts, now)
	opts.repoKeys = newRepoKeyCache(opts.RepoKey)
	p := &pass{
		ctx:                ctx,
		local:              local,
		remote:             store,
		opts:               opts,
		now:                now,
		durableObligations: durableObligations,
		result:             Result{Errors: subagents.errors, WaitingSubagents: subagents.waiting, RejectedSubagents: subagents.rejected},
		expiredSubagents:   subagents.expired,
	}
	defer p.releaseLabelResources()
	if generationRecoveryErr != nil {
		p.result.Errors["generation-recovery"] = generationRecoveryErr
	}
	if recoveryErr != nil {
		p.result.Errors["session-index"] = recoveryErr
	}
	if replayErr != nil {
		p.result.Errors["admission-intents"] = replayErr
	}
	// A loadWork failure returns before saveStatus, so this pass's expired
	// subagents never reach status.json's list. That is accepted: the list
	// is informational, and each parent's capture gap is already saved.
	if err := p.loadWork(); err != nil {
		return Result{}, err
	}
	p.loadDeferredSince()
	p.reconcileNativeLinks()
	if p.opts.PrepareCodexCoverage != nil {
		if err := p.opts.PrepareCodexCoverage(ctx, p.registrations); err != nil {
			p.result.Errors["native-coverage"] = err
		}

	}
	p.observeLabels(ctx)
	p.repairListingIndex()
	orderOldestRequestsFirst(p.registrations, p.requests)
	for i, reg := range p.registrations {
		// A pass past its deadline ends like a stopped one: nothing more can
		// reach storage, so the rest keep their work rather than each
		// failing on the expired context.
		if (p.opts.Stop != nil && p.opts.Stop()) || ctx.Err() != nil {
			p.leaveForNextPass(p.registrations[i:])
			break
		}
		if p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		p.scan(reg)
	}
	return p.result, p.saveStatus()
}

// Cancellation remains fatal even when a refusal also identifies owed work.
func generationRecoveryStopsPass(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (!errors.Is(err, state.ErrDurableStorageRecovery) && !errors.Is(err, agentapi.ErrReadBudget)))
}

// pass is one Run: its inputs, what it has found so far, and its result.
type pass struct {
	labelLocal          *state.Store
	closeLabelResources func()
	labelStates         map[string]*state.Published
	labelBytes          int64
	ctx                 context.Context
	local               *state.Store
	remote              storage.ObjectStore
	opts                Options
	now                 time.Time

	registrations []archive.SessionRegistration
	requests      map[string]state.Request
	// unreadable holds the sessions this pass must leave alone: their state
	// could not be read, so there is nothing safe to act on.
	unreadable map[string]bool
	// registered holds every session registered, readable or not.
	registered map[string]bool
	// parents caches the summaries of subagents' parents (see linkOwed).
	parents map[string]state.PublishedSummary
	// sizeLimited lists the sessions sitting in a size-limit gap, which
	// status reports: capture of them has stopped.
	sizeLimited []string
	// pending counts the sessions left with outstanding work.
	pending int
	// expiredSubagents lists the subagents this pass stopped waiting for
	// (see state.Status.ExpiredSubagents).
	expiredSubagents   []state.ExpiredSubagent
	result             Result
	durableObligations []state.DurableStorageObligation
	durableCounted     map[string]bool
	// priorDeferredSince is the previous pass's state.Status.DeferredSince,
	// and deferredAt this pass's (see deferredSince, keepDeferredSince).
	priorDeferredSince map[string]time.Time
	deferredAt         map[string]time.Time
}

// loadWork lists the registered sessions and their pending requests. One
// unreadable state file fails only its own session: the pass, and the
// retention sweep after it, go on for every other one.
func (p *pass) loadWork() error {
	registrations, registrationIssues, err := p.local.ScanRegistrations()
	if err != nil {
		return err
	}
	requests, requestIssues, err := p.local.ScanRequests()
	if err != nil {
		return err
	}
	p.registrations, p.requests, p.unreadable = registrations, map[string]state.Request{}, map[string]bool{}
	// A session whose registration could not be read is left for the next
	// pass like any other outstanding work.
	p.pending = len(registrationIssues)
	registered := make(map[string]bool, len(registrations)+len(registrationIssues))
	for _, reg := range registrations {
		registered[reg.ArchiveSessionID] = true
	}
	for id, issue := range registrationIssues {
		addError(p.result.Errors, id, issue)
		registered[id] = !errors.Is(issue, state.ErrQuarantined)
	}
	p.durableCounted = map[string]bool{}
	obligations := p.durableObligations
	orphaned := map[string]bool{}
	for _, obligation := range obligations {
		id := obligation.SessionID
		if id == "" {
			addError(p.result.Errors, "durable-storage", state.ErrDurableStorageRecovery)
			continue
		}
		if registered[id] && obligation.Namespace == state.GenerationRecoveryStorage && !orphaned[id] {
			if err := checkDurableSessionRead(p.ctx, p.local, id, p.opts); err != nil {
				orphaned[id] = true
				p.pending++
				p.durableCounted[id] = true
				p.unreadable[id] = true
				addError(p.result.Errors, id, err)
			}
		}
		if !registered[id] && !orphaned[id] {
			orphaned[id] = true
			p.pending++
			addError(p.result.Errors, id, state.ErrDurableStorageRecovery)
		}
	}
	// Lock files of sessions and candidates that are gone go now, while the
	// registrations just listed say which those are.
	p.local.RemoveOrphanedLocks(registered)
	p.registered, p.parents = registered, map[string]state.PublishedSummary{}
	for id, issue := range requestIssues {
		addError(p.result.Errors, id, issue)
		// A quarantined request is gone, and the session carries on without
		// it; one that could not be read may still hold evidence.
		if !errors.Is(issue, state.ErrQuarantined) {
			p.unreadable[id] = true
		}
	}
	for _, req := range requests {
		if req.Token == "" {
			id := req.ArchiveSessionID
			var found bool
			req, found, err = p.local.EnsureRequestToken(id)
			if err != nil {
				addError(p.result.Errors, id, fmt.Errorf("upgrade pending request: %w", err))
				p.unreadable[id] = true
				continue
			}
			if !found {
				// Acknowledged by a concurrent process between listing and
				// upgrade; nothing is pending for it any more.
				continue
			}
		}
		p.requests[req.ArchiveSessionID] = req
	}
	return nil
}

// leaveForNextPass counts the sessions an early end did not reach whose
// work (a request) is left for the next pass.
func (p *pass) leaveForNextPass(rest []archive.SessionRegistration) {
	for _, reg := range rest {
		accepted := p.opts.AcceptSession == nil || p.opts.AcceptSession(reg)
		if accepted {
			p.carryDeferredSince(reg.ArchiveSessionID)
		}
		if !p.durableCounted[reg.ArchiveSessionID] && p.requests[reg.ArchiveSessionID].Token != "" && accepted {
			p.pending++
		}
	}
}

// fail records a local failure that ends a session's turn in the pass; the
// session is retried on the next one.
func (p *pass) fail(id string, err error) {
	addError(p.result.Errors, id, err)
	p.pending++
	p.opts.progress(id, false)
}

// deferScan ends a session's turn in the pass without a failure: its source
// could not be read completely yet (see deferralReason). Like a failure, it
// leaves the scan journal standing for the next pass to retry, and counts
// as pending. A session that already has a failure recorded this pass, or
// whose scans have been deferred on every pass for MaxDeferral, fails with
// err instead (see deferredSince).
func (p *pass) deferScan(id, reason string, err error) {
	since := p.deferredSince(id)
	// The clock runs on through a failure for it: the next pass that defers
	// the session fails it again rather than starting a new run of
	// "rechecking" passes.
	p.keepDeferredSince(id, since)
	if p.result.Errors[id] != nil {
		// One failure already makes it a failed session; it is counted once.
		p.fail(id, err)
		return
	}
	if p.now.Sub(since) >= MaxDeferral {
		p.fail(id, fmt.Errorf("%s: %w", DeferralExpiredProblem, err))
		return
	}
	if p.result.Deferred == nil {
		p.result.Deferred = map[string]string{}
	}
	p.result.Deferred[id] = reason
	p.pending++
	p.opts.progress(id, false)
}

// scan gives one session its turn in the pass: skip it if nothing about it
// changed, otherwise scan it (sessionScan.run) and account for the outcome.
func (p *pass) scan(reg archive.SessionRegistration) {
	priorLocal := p.local
	scopedLocal, closeLocal := p.local.WithReadBudget(p.ctx, (&sessionScan{opts: p.opts}).readBudget())
	p.local = scopedLocal
	defer func() { closeLocal(); p.local = priorLocal }()
	id := reg.ArchiveSessionID
	p.result.Scanned++
	if p.unreadable[id] {
		if !p.durableCounted[id] {
			p.pending++
		}
		p.opts.progress(id, false)
		return
	}
	req := p.requests[id]
	if done := p.skipUnchanged(reg, req); done {
		return
	}
	if err := p.local.SetScanPending(id, true); err != nil {
		p.fail(id, fmt.Errorf("journal pending scan: %w", err))
		return
	}
	// The session's published state, read once for the whole scan.
	published := p.labelStates[id]
	delete(p.labelStates, id)
	var err error
	if published == nil {
		published, err = p.local.LoadPublishedState(id)
	}
	if err != nil {
		p.fail(id, fmt.Errorf("load published cache: %w", err))
		return
	}
	if err := announceSubagent(p.local, reg, published); err != nil {
		addError(p.result.Errors, id, err)
		p.pending++
	}
	scan := newSessionScan(p.ctx, p.local, p.remote, reg, req, published, p.now, p.opts)
	defer scan.releaseRetained()
	outcome, err := scan.run()
	for _, warning := range scan.warnings {
		addError(p.result.Errors, id, warning)
	}
	if err != nil && p.ctx.Err() != nil {
		// The pass ran out of time (or was cancelled) with this session in
		// flight. That is not the session failing: its pending publication
		// is intact and the next pass carries on with it.
		p.carryDeferredSince(id)
		p.pending++
		p.opts.progress(id, false)
		return
	}
	if reason, deferred := deferralReason(err); deferred {
		p.deferScan(id, reason, err)
		return
	}
	if err != nil {
		p.fail(id, err)
		return
	}
	p.noteGap(id, scan.gap)
	p.finishScan(reg)
	switch outcome {
	case outcomePublished:
		p.result.Published = append(p.result.Published, id)
		if err := announceSubagent(p.local, reg, published); err != nil {
			addError(p.result.Errors, id, err)
			p.pending++
		}
	case outcomeRateLimited:
		p.result.Waiting = append(p.result.Waiting, id)
		if !scan.readyAt.IsZero() && (p.result.NextReadyAt.IsZero() || scan.readyAt.Before(p.result.NextReadyAt)) {
			p.result.NextReadyAt = scan.readyAt
		}
	case outcomeSkipped:
		if scan.subagentRunning {
			p.result.RunningSubagents = append(p.result.RunningSubagents, id)
		} else {
			p.result.Skipped = append(p.result.Skipped, id)
		}
	}
	p.opts.progress(id, outcome == outcomePublished)
}

// finishScan clears the scan journal once the session owes no further work
// (state.Outstanding's OwedAfterScan), and counts it as pending otherwise.
// The session's own outcome stands whatever happens here: a publication has
// reached storage even if this bookkeeping fails, and the next pass redoes
// it.
func (p *pass) finishScan(reg archive.SessionRegistration) {
	id := reg.ArchiveSessionID
	_, requested, err := p.local.LoadRequest(id)
	var owed state.Outstanding
	if err == nil {
		owed, err = p.local.Outstanding(reg, requested)
	}
	switch {
	case err != nil:
		addError(p.result.Errors, id, fmt.Errorf("check outstanding work: %w", err))
		p.pending++
	case !owed.OwedAfterScan():
		if err := p.local.SetScanPending(id, false); err != nil {
			addError(p.result.Errors, id, fmt.Errorf("complete pending scan: %w", err))
			p.pending++
		}
	default:
		p.pending++
	}
}

// SizeLimitProblem is the problem a pass records in its Status when n
// sessions sit in a size-limit gap: not a failure, and nothing to do, but
// capture of them has stopped at their last snapshot.
func SizeLimitProblem(n int) string {
	return fmt.Sprintf("%d session(s) stopped being captured: over the transcript size limit, kept at their last snapshot", n)
}

// IsSizeLimitProblem reports whether problem is a SizeLimitProblem, for any
// count.
func IsSizeLimitProblem(problem string) bool {
	var n int
	if _, err := fmt.Sscanf(problem, "%d ", &n); err != nil || n < 1 {
		return false
	}
	return problem == SizeLimitProblem(n)
}

// FailedSessionsProblem is the problem a pass records in its Status when n
// sessions failed to scan or publish. A caller that reports those sessions
// again, with a fuller count, replaces this entry rather than adding one.
func FailedSessionsProblem(n int) string {
	return fmt.Sprintf("%d session(s) failed to scan or publish", n)
}

// saveStatus records the pass's summary for `status`.
func (p *pass) saveStatus() error {
	// A status file that no longer decodes is replaced below; it only ever
	// carries the previous pass's summary.
	previous, err := p.local.LoadStatus()
	if err != nil && !state.IsUndecodable(err) {
		return err
	}
	lastPublishedAt := previous.LastPublishedAt
	if len(p.result.Published) > 0 {
		lastPublishedAt = p.now.UTC()
	}
	var problems []string
	if len(p.result.Errors) > 0 {
		problems = append(problems, FailedSessionsProblem(len(p.result.Errors)))
	}
	// A size-limit gap is not a failure, but capture of the session has
	// stopped at its last snapshot, which status must say rather than look
	// healthy.
	if len(p.sizeLimited) > 0 {
		problems = append(problems, SizeLimitProblem(len(p.sizeLimited)))
	}
	status := state.Status{
		LastScanAt:             p.now.UTC(),
		PendingCount:           p.pending,
		LastPublishedAt:        lastPublishedAt,
		QuarantinedFiles:       p.local.QuarantinedFiles(),
		UnrefreshableSummaries: p.countRefreshSkips(),
		PendingNativeLinks:     p.result.PendingNativeLinks, NativeLinkPendingReasons: p.result.NativeLinkPendingReasons,
		DeferredSessions: p.result.Deferred,
		DeferredSince:    p.deferredAt,
		WaitingSubagents: len(p.result.WaitingSubagents),
		RunningSubagents: len(p.result.RunningSubagents),
		// The pass rebuilds everything else from scratch; this list is a
		// week of history, so it carries the previous pass's forward.
		ExpiredSubagents: state.CarryExpiredSubagents(previous.ExpiredSubagents, p.expiredSubagents, p.now),
	}
	status.SetLastErrors(problems...)
	if err := p.local.SaveStatus(status); err != nil {
		return fmt.Errorf("save status: %w", err)
	}
	return nil
}

func (o Options) progress(archiveSessionID string, published bool) {
	if o.Progress != nil {
		o.Progress(Progress{ArchiveSessionID: archiveSessionID, Published: published})
	}
}

// addError records err against a session, keeping any error the pass already
// recorded for it.
func addError(errs map[string]error, archiveSessionID string, err error) {
	if previous := errs[archiveSessionID]; previous != nil {
		err = errors.Join(previous, err)
	}
	errs[archiveSessionID] = err
}

// orderOldestRequestsFirst puts the sessions with a pending request first,
// oldest start first, and leaves the rest in archive session ID order. An
// import queues its whole history at once; the oldest transcripts are the
// ones the apps delete next, so they are uploaded first.
func orderOldestRequestsFirst(registrations []archive.SessionRegistration, requests map[string]state.Request) {
	sort.SliceStable(registrations, func(i, j int) bool {
		a, b := registrations[i], registrations[j]
		aRequested, bRequested := requests[a.ArchiveSessionID].Token != "", requests[b.ArchiveSessionID].Token != ""
		if aRequested != bRequested {
			return aRequested
		}
		return aRequested && a.SessionStartedAt.Before(b.SessionStartedAt)
	})
}

// cursorTextSourceFormat labels a Cursor transcript captured as plain text.
const cursorTextSourceFormat = "cursor-text"

// harnessAdapterVersion returns the version of the adapter that reads
// harness, or known=false when no adapter does.
func harnessAdapterVersion(sources agentapi.SourcesLookup, harness string) (string, bool) {
	adapter, err := sourceAdapter(sources, harness)
	if err != nil {
		return "", false
	}
	return adapter.Version(), true
}

// countRefreshSkips matches the parser actually bound to each registration.
func (p *pass) countRefreshSkips() int {
	n := 0
	skips := p.local.RefreshSkips()
	if len(skips) == 0 {
		return 0
	}
	for _, reg := range p.registrations {
		skipped, found := skips[reg.ArchiveSessionID]
		if found && skipped.ParserVersion == p.opts.parserVersionFor(reg.Harness.Name) {
			n++
		}
	}
	return n
}

// repairListingIndex gives auxiliary work a bounded slice without blocking capture.
func (p *pass) repairListingIndex() {
	// Auxiliary maintenance has its own bounded slice and never prevents capture.
	repairs, repairErr := p.local.ListingRepairs(32)
	if repairErr != nil {
		p.result.Errors["listing-maintenance"] = repairErr
	}
	for id, repair := range repairs {
		reg, found, err := p.local.LoadRegistration(id)
		if err != nil {
			p.result.Errors["listing-maintenance"] = err
			continue
		}
		if !found || reg.DestinationID != repair.DestinationID {
			_ = p.local.RemoveListingRepair(id)
			continue
		}
		expected, keyErr := archive.MetadataObjectKey(reg.Harness.Name, id)
		if keyErr != nil || repair.MetadataKey != expected {
			p.result.Errors["listing-maintenance"] = errors.New("invalid listing repair identity")
			continue
		}
		if p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		getter, ok := p.remote.(storage.VersionedGetter)
		if !ok {
			continue
		}
		data, _, err := getter.GetVersioned(p.ctx, repair.MetadataKey)
		if errors.Is(err, storage.ErrNotFound) {
			_ = p.local.RemoveListingRepair(id)
			continue
		}
		if err == nil {
			err = listingindex.PublishRevision(p.ctx, p.remote, repair.MetadataKey, data)
		}
		if err == nil {
			err = p.local.RemoveListingRepair(id)
		}
		if err != nil {
			p.result.Errors["listing-maintenance"] = err
		}
	}
}
