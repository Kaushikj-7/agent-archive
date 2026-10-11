package collector

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// subagentTranscriptGrace is how long a candidate a SubagentStop hook left
// may wait for its transcript to appear, or to hold a native record. Claude
// Code fires SubagentStop for some background agents with a transcript path
// it never writes; without a limit the candidate would be retried, and
// reported, on every pass for good, and its parent's link would stay pending.
// A real subagent's transcript is written well within this.
const subagentTranscriptGrace = 30 * time.Minute

// errSubagentWaiting marks a candidate whose transcript is missing or empty
// but still inside subagentTranscriptGrace. It is kept for the next pass and
// is not a failure.
var errSubagentWaiting = errors.New("subagent transcript is not written yet")

// errSubagentRunning marks a subagent whose transcript has records after the
// last SubagentStop that vouched for it, the latest of them within
// subagentTranscriptGrace: it was resumed (SendMessage continues a stopped
// agent in the same transcript) and is still at work. Its next stop, or
// subagentTranscriptGrace without a new record, moves the bound forward (see
// subagentEndBound); until then the collector waits, keeping any published
// snapshot. It is not a failure.
var errSubagentRunning = errors.New("subagent is running again since its last stop")

// errSubagentFutureRecord marks a subagent transcript whose last record is
// dated more than subagentTranscriptGrace after the pass's clock. No wait
// ends that: the record is never quiet (see subagentEndBound), so it would
// look like a subagent still running for as long as the gap lasts.
var errSubagentFutureRecord = errors.New("subagent transcript has records dated in the future")

// subagentFutureDated reports whether filtered's last record is dated more
// than subagentTranscriptGrace after now, the same allowance
// awaitSubagentTranscript gives a stop observed in the future.
func subagentFutureDated(filtered archive.FilteredTranscript, now time.Time) bool {
	return filtered.NativeEndAt.Sub(now) > subagentTranscriptGrace
}

// subagentEndBound is the latest native time a subagent transcript's records
// may carry: the SubagentStop observed at observedAt, or, once the transcript
// has written nothing for subagentTranscriptGrace, its last record. A
// subagent resumed after its stop may never stop again (its session was
// closed while it worked); without the second bound the records it wrote
// after that stop would never be archived. A last record dated after now
// (a clock that jumped back) is never quiet.
func subagentEndBound(filtered archive.FilteredTranscript, observedAt, now time.Time) time.Time {
	if end := filtered.NativeEndAt; end.After(observedAt) && !end.After(now) && now.Sub(end) >= subagentTranscriptGrace {
		return end
	}
	return observedAt
}

// subagentRejectedError is a candidate the collector decided not to register:
// it is acknowledged, and its parent told the link is unavailable, so it is
// never retried.
type subagentRejectedError struct{ code string }

func (e subagentRejectedError) Error() string { return e.code }

// Is reports whether target is ErrSubagentNotCaptured or ErrSubagentCandidate.
func (subagentRejectedError) Is(target error) bool {
	return target == ErrSubagentNotCaptured || target == ErrSubagentCandidate
}

// ErrSubagentNotCaptured is matched, with errors.Is, by every rejection of a
// subagent candidate: it is acknowledged and will not be retried. Only a
// rejection that lost a real subagent reaches Result.Errors (see
// expectedSubagentRejections); a caller that sees every rejection filters out
// the expected ones itself.
var ErrSubagentNotCaptured = errors.New("subagent was not captured")

// ErrSubagentCandidate is matched, with errors.Is, by every error
// Result.Errors holds for a subagent candidate (one a SubagentStop hook left,
// not yet registered), so callers can count it as a subagent though no
// registration says so.
var ErrSubagentCandidate = errors.New("subagent candidate")

// subagentCandidateError is a subagent candidate's failure, as its error
// reads, marked as a candidate's (see ErrSubagentCandidate).
type subagentCandidateError struct{ err error }

func (e subagentCandidateError) Error() string { return e.err.Error() }

func (e subagentCandidateError) Unwrap() error { return e.err }

// Is reports whether target is ErrSubagentCandidate.
func (subagentCandidateError) Is(target error) bool { return target == ErrSubagentCandidate }

// candidateFailure marks err as a subagent candidate's failure, unless it
// already is one.
func candidateFailure(err error) error {
	if errors.Is(err, ErrSubagentCandidate) {
		return err
	}
	return subagentCandidateError{err: err}
}

// expectedSubagentRejections are the rejection codes that lose nothing: a
// transcript that was never written, or that backfill found empty or gone; a
// parent that is not, or no longer, archived here; a subagent that started
// outside what the archive admits. They are decisions, counted but not
// reported as failures. Every other code (unknown format, provenance that
// does not check out, a conflicting registration, a transcript that exists
// but cannot be read) means a real subagent was lost, which the pass reports
// as a failed session once.
var expectedSubagentRejections = map[string]bool{
	subagentNeverWritten:                    true,
	"subagent_transcript_unavailable":       true,
	"subagent_parent_ownership_unavailable": true,
	"subagent_start_ineligible":             true,
}

// subagentOutcome is what materializing the pending candidates did.
type subagentOutcome struct {
	// errors holds the candidates that failed, keyed by archive session ID,
	// including those rejected for an unexpected reason.
	errors map[string]error
	// waiting lists the candidates kept for their transcripts (see
	// errSubagentWaiting).
	waiting []string
	// rejected maps each candidate rejected this pass to its code.
	rejected map[string]string
	// expired lists the candidates rejected this pass because their
	// transcripts were never written, for status to carry forward.
	expired []state.ExpiredSubagent
}

// materializeSubagentCandidates registers each candidate a hook left, rejects
// it, or keeps it waiting for its transcript, judged at now, the pass's
// clock. One unreadable candidate fails only itself.
func materializeSubagentCandidates(ctx context.Context, local *state.Store, opts Options, now time.Time) subagentOutcome {
	candidates, issues, err := local.ScanSubagentCandidates()
	if err != nil {
		return subagentOutcome{errors: map[string]error{"subagent-candidates": candidateFailure(err)}}
	}
	outcome := subagentOutcome{errors: map[string]error{}, rejected: map[string]string{}}
	for id, issue := range issues {
		outcome.errors[id] = candidateFailure(issue)
	}
	for _, candidate := range candidates {
		err := materializeSubagentCandidate(ctx, local, candidate, opts, now)
		var rejected subagentRejectedError
		switch {
		case err == nil:
		case errors.Is(err, errSubagentWaiting):
			outcome.waiting = append(outcome.waiting, candidate.ArchiveSessionID)
		case errors.As(err, &rejected):
			outcome.rejected[candidate.ArchiveSessionID] = rejected.code
			if rejected.code == subagentNeverWritten {
				outcome.expired = append(outcome.expired, state.ExpiredSubagent{ArchiveSessionID: candidate.ArchiveSessionID, AgentType: archive.SanitizeSubagentType(candidate.AgentType), ExpiredAt: now.UTC()})
			}
			if !expectedSubagentRejections[rejected.code] {
				outcome.errors[candidate.ArchiveSessionID] = err
			}
		default:
			outcome.errors[candidate.ArchiveSessionID] = candidateFailure(err)
		}
	}
	return outcome
}

func materializeSubagentCandidate(ctx context.Context, local *state.Store, candidate state.SubagentCandidate, opts Options, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkDurableSessionRead(ctx, local, candidate.ArchiveSessionID, opts); err != nil {
		return err
	}
	parent, err := admittedSubagentParent(local, candidate, opts)
	if err != nil {
		return err
	}
	reg := assembleSubagentRegistration(parent, candidate)
	if candidate.StagedImportSHA256 != "" {
		reg.StagedImportSHA256 = candidate.StagedImportSHA256
		stage, found, loadErr := local.LoadImportStage(ctx, reg)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return state.ErrDurableStorageRecovery
		}
		reg = stage.Registration
		reg.StagedImportSHA256 = candidate.StagedImportSHA256
		if reg.ParentSessionID != parent.ArchiveSessionID || reg.ParentNativeSessionID != parent.NativeSessionID {
			return state.ErrDurableStorageRecovery
		}
	} else {
		reg, err = validateCandidateTranscript(ctx, local, candidate, parent, reg, opts, now)
	}
	if err != nil {
		return err
	}
	if err := checkSubagentRegistrationConflict(local, candidate, reg); err != nil {
		return err
	}
	return persistSubagentCandidate(local, candidate, reg)
}

func admittedSubagentParent(local *state.Store, candidate state.SubagentCandidate, opts Options) (archive.SessionRegistration, error) {
	parent, found, err := local.LoadRegistration(candidate.ParentArchiveSessionID)
	if err != nil {
		return archive.SessionRegistration{}, err
	}
	if !found || parent.NativeSessionID != candidate.ParentNativeSessionID || parent.ProjectID != candidate.ProjectID || parent.ProjectRoot != candidate.ProjectRoot || archive.CanonicalHarness(parent.Harness.Name) != archive.CanonicalHarness(candidate.Harness.Name) || (opts.AcceptSession != nil && !opts.AcceptSession(parent)) {
		return archive.SessionRegistration{}, rejectSubagentCandidate(local, candidate, "subagent_parent_ownership_unavailable")
	}
	return parent, nil
}

func assembleSubagentRegistration(parent archive.SessionRegistration, candidate state.SubagentCandidate) archive.SessionRegistration {
	var startedAtSource archive.StartedAtSource
	if parent.Imported() || parent.Origin == archive.SessionOriginDiscovery {
		// Its start is set below from the earliest native record.
		startedAtSource = archive.StartedAtSourceTranscript
	}
	var proof *archive.CodexAdmissionProof
	if parent.CodexAdmission != nil {
		proofCopy := *parent.CodexAdmission
		proof = &proofCopy
	}
	return archive.SessionRegistration{CodexAdmission: proof, DiscoveryCwd: parent.DiscoveryCwd,
		ArchiveSessionID: candidate.ArchiveSessionID, NativeSessionID: candidate.NativeSessionID,
		ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, RepoKey: parent.RepoKey, Replay: parent.Replay, Harness: parent.Harness,
		TranscriptPath: candidate.TranscriptPath, RegisteredAt: candidate.ObservedAt,
		ParentSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID,
		SubagentID: candidate.AgentID, SubagentObservedAt: candidate.ObservedAt,
		// The child is admitted with its parent, into the same destination,
		// and by the same import when the parent was imported.
		AdmittedAt: parent.AdmittedAt, Origin: parent.Origin, ImportBatch: parent.ImportBatch,
		DestinationID:   parent.DestinationID,
		StartedAtSource: startedAtSource,
	}
}

func validateCandidateTranscript(ctx context.Context, local *state.Store, candidate state.SubagentCandidate, parent, reg archive.SessionRegistration, opts Options, now time.Time) (archive.SessionRegistration, error) {
	adapter, err := sourceAdapter(opts.Sources, reg.Harness.Name)
	if err != nil {
		return reg, rejectSubagentCandidate(local, candidate, "subagent_format_unavailable")
	}
	source, _ := newSourceReader(reg, opts)
	filtered, _, err := source.Filter(ctx, adapter, opts.maxTranscriptBytes())
	if ctx.Err() != nil {
		return reg, errors.Join(ctx.Err(), err)
	}
	// Missing and incomplete native records may wait for the writer. Retryable
	// failures, including cleanup joined with missing or a stable refusal,
	// retain the candidate and report the original cause.
	if err != nil && (agentapi.HasFailure(err, agentapi.Unavailable) || agentapi.HasFailure(err, agentapi.Changed) || agentapi.HasFailure(err, agentapi.Cleanup) || (!errors.Is(err, os.ErrNotExist) && !agentapi.Deterministic(err))) {
		return reg, err
	}
	// A transcript with lines but no recognized record yet is treated like an
	// empty one: its first record may still be on its way.
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, archive.ErrUnsafeSourceFormat) {
		// It exists but cannot be read: too large, a record too large, not a
		// regular file, or another deterministic refusal.
		return reg, errors.Join(rejectSubagentCandidate(local, candidate, "subagent_transcript_unreadable"), err)
	}
	if err != nil || subagentTranscriptEmpty(filtered) {
		return reg, awaitSubagentTranscript(local, candidate, now)
	}
	reg.SessionStartedAt = filtered.NativeStartAt
	bound := subagentEndBound(filtered, candidate.ObservedAt, now)
	if filtered.NativeEndAt.After(bound) && !subagentFutureDated(filtered, now) {
		// Resumed after this stop and still at work. It is registered all
		// the same, with this stop as its bound, which is what a later stop
		// of a registered subagent must record; the scan holds its records
		// to that bound (see errSubagentRunning). Rejecting it would report a
		// failure and tell the parent the link is unavailable, only for the
		// next stop to register it after all.
		bound = filtered.NativeEndAt
	}
	if code := checkNewSubagent(filtered, reg.ParentNativeSessionID, reg.SubagentID, parent.SessionStartedAt, candidate.ObservedAt, bound); code != "" {
		return reg, rejectSubagentCandidate(local, candidate, code)
	}
	if opts.AcceptSession != nil && !opts.AcceptSession(reg) {
		return reg, rejectSubagentCandidate(local, candidate, "subagent_start_ineligible")
	}
	return reg, nil
}

// awaitSubagentTranscript decides for a candidate whose transcript is missing
// or holds no native record yet. A candidate from a hook waits out
// subagentTranscriptGrace from its SubagentStop, then is rejected as never
// written. So is one observed more than the grace in the future: a clock
// that has jumped back would otherwise keep it waiting that much longer. A
// transcript backfill found is history and will not grow, so it is
// rejected at once.
func awaitSubagentTranscript(local *state.Store, candidate state.SubagentCandidate, now time.Time) error {
	if candidate.Origin == archive.SessionOriginImport {
		return rejectSubagentCandidate(local, candidate, "subagent_transcript_unavailable")
	}
	if age := now.Sub(candidate.ObservedAt); age < subagentTranscriptGrace && age >= -subagentTranscriptGrace {
		return errSubagentWaiting
	}
	return rejectSubagentCandidate(local, candidate, subagentNeverWritten)
}

func checkSubagentRegistrationConflict(local *state.Store, candidate state.SubagentCandidate, reg archive.SessionRegistration) error {
	if existing, found, err := local.LoadRegistration(reg.ArchiveSessionID); err != nil {
		return err
	} else if found && (existing.NativeSessionID != reg.NativeSessionID || existing.ParentSessionID != reg.ParentSessionID || existing.ParentNativeSessionID != reg.ParentNativeSessionID || existing.ProjectID != reg.ProjectID || existing.ProjectRoot != reg.ProjectRoot || archive.CanonicalHarness(existing.Harness.Name) != archive.CanonicalHarness(reg.Harness.Name) || existing.SubagentID != reg.SubagentID || existing.TranscriptPath != reg.TranscriptPath || !existing.SessionStartedAt.Equal(reg.SessionStartedAt)) {
		return rejectSubagentCandidate(local, candidate, "subagent_registration_conflict")
	}
	return nil
}

// Registration is durable before hook evidence, and the candidate remains
// retryable until both writes complete. Replaying either write is safe.
func persistSubagentCandidate(local *state.Store, candidate state.SubagentCandidate, reg archive.SessionRegistration) error {
	if err := local.SaveRegistration(reg); err != nil {
		return err
	}
	if candidate.Origin == archive.SessionOriginImport {
		// No SubagentStop fired for a subagent backfill found.
		return local.AcknowledgeSubagentCandidate(candidate)
	}
	lifecycle, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindLifecycleHook, ObservedAt: candidate.ObservedAt,
		Provenance: "hook:" + strings.ToLower(reg.Harness.Name) + ":subagentstop",
		Payload:    map[string]any{"event_name": "SubagentStop", "agent_id": candidate.AgentID},
	}})
	if err != nil {
		return err
	}
	if len(lifecycle) > 0 {
		if err := local.SaveRequest(reg.ArchiveSessionID, "subagentstop", candidate.ObservedAt, lifecycle[0]); err != nil {
			return err
		}
	}
	return local.AcknowledgeSubagentCandidate(candidate)
}

// subagentTranscriptEmpty reports a subagent transcript with no native
// records yet.
func subagentTranscriptEmpty(filtered archive.FilteredTranscript) bool {
	return filtered.NativeStartAt.IsZero() && len(filtered.Records) == 0
}

// checkNewSubagent applies the checks a subagent transcript with records must
// pass before it is first registered, its records ending by bound (see
// subagentEndBound), and returns the code it is rejected with, or "".
func checkNewSubagent(filtered archive.FilteredTranscript, parentNativeSessionID, agentID string, parentStartedAt, observedAt, bound time.Time) string {
	if checkSubagentProvenance(filtered, parentNativeSessionID, agentID, filtered.NativeStartAt, bound) != nil {
		return "subagent_provenance_unavailable"
	}
	if filtered.NativeStartAt.Before(parentStartedAt) || filtered.NativeStartAt.After(observedAt) {
		return "subagent_start_ineligible"
	}
	return ""
}

// CheckImportedSubagent reports why the collector would refuse to register a
// subagent transcript backfill found, observed at observedAt, or nil when it
// would register it: the transcript is empty, its native timestamps are
// incomplete or end after observedAt, it names another parent session or
// agent, or it starts before its parent. Backfill's plan runs it so the
// subagents it counts are the ones the collector registers.
func CheckImportedSubagent(filtered archive.FilteredTranscript, parentNativeSessionID, agentID string, parentStartedAt, observedAt time.Time) error {
	if subagentTranscriptEmpty(filtered) {
		return errors.New("subagent transcript is empty")
	}
	if code := checkNewSubagent(filtered, parentNativeSessionID, agentID, parentStartedAt, observedAt, observedAt); code != "" {
		return errors.New(code)
	}
	return nil
}

// validateSubagentTranscript is called before registration and on every later
// scan, judged at now, because the hook-provided path is mutable local state.
func validateSubagentTranscript(reg archive.SessionRegistration, filtered archive.FilteredTranscript, now time.Time) error {
	if reg.NativeChild || reg.ParentSessionID == "" {
		// Native providers validate their own stable identity, source snapshot and ownership.
		// They remain capturable while running and have no synthetic stop bound.
		return nil
	}
	err := checkSubagentProvenance(filtered, reg.ParentNativeSessionID, reg.SubagentID, reg.SessionStartedAt, subagentEndBound(filtered, reg.SubagentObservedAt, now))
	if errors.Is(err, errSubagentRunning) && subagentFutureDated(filtered, now) {
		return errSubagentFutureRecord
	}
	return err
}

// checkSubagentProvenance checks that a subagent transcript's native
// timestamps are complete, start at startedAt, and end no later than
// observedAt, and that its records name only the parent session and the
// agent. A transcript that ends after observedAt is errSubagentRunning.
func checkSubagentProvenance(filtered archive.FilteredTranscript, parentNativeSessionID, agentID string, startedAt, observedAt time.Time) error {
	if !filtered.NativeStartComplete || filtered.NativeStartAt.IsZero() || filtered.NativeEndAt.IsZero() || observedAt.IsZero() {
		return errors.New("subagent transcript has incomplete native timestamp provenance")
	}
	if filtered.NativeEndAt.After(observedAt) {
		return errSubagentRunning
	}
	if !filtered.NativeStartAt.Equal(startedAt) {
		return errors.New("subagent native start changed after registration")
	}
	if len(filtered.AgentIDs) == 0 {
		return errors.New("subagent transcript has no child agent identity")
	}
	if len(filtered.SessionIDs) == 0 {
		return errors.New("subagent transcript has no parent session identity")
	}
	for _, id := range filtered.SessionIDs {
		if id != parentNativeSessionID {
			return errors.New("subagent transcript parent session identity mismatch")
		}
	}
	for _, id := range filtered.AgentIDs {
		if id != agentID {
			return errors.New("subagent transcript agent identity mismatch")
		}
	}
	return nil
}

// subagentNeverWritten is the rejection code, and the parent's capture gap
// code, of a candidate whose transcript Claude Code never wrote.
const subagentNeverWritten = "subagent_transcript_never_written"

// subagentExpiryProvenance marks the capture gap the collector records on a
// parent whose subagent's transcript was never written.
const subagentExpiryProvenance = "collector:subagent-expiry"

// subagentNeverWrittenDetail is the capture gap detail for a subagent whose
// transcript was never written. It is fixed, archive-authored text: the
// subagent's type stays on this machine (the candidate, status), because a name
// the sanitizer admits can still be a secret redaction does not recognize.
const subagentNeverWrittenDetail = "Claude Code reported a subagent but never wrote its transcript"

// rejectSubagentCandidate acknowledges candidate and tells its parent the
// link is unavailable, then returns a subagentRejectedError with code. A
// transcript that was never written also leaves the parent a capture gap
// saying why the subagent is missing. Any other error means one of those
// writes failed and the candidate stays.
func rejectSubagentCandidate(local *state.Store, candidate state.SubagentCandidate, code string) error {
	link, err := archive.NewLinkedSessionEvidence(candidate.ArchiveSessionID, archive.LinkedSessionUnavailable, candidate.ObservedAt)
	if err != nil {
		return err
	}
	evidence := []archive.SupplementalEvidence{link}
	if code == subagentNeverWritten {
		// Fixed text observed at the stop, like the link, so a retry after a
		// failed write saves the same item, which the request keeps once.
		gap, err := archive.NewCaptureGapEvidence(code, subagentNeverWrittenDetail, subagentExpiryProvenance, candidate.ObservedAt)
		if err != nil {
			return err
		}
		evidence = append(evidence, gap)
	}
	// A parent retention has forgotten has nobody left to notify. The
	// candidate is still acknowledged: retrying it would report the same
	// permanent condition on every pass.
	if err := local.SaveRequest(candidate.ParentArchiveSessionID, code, candidate.ObservedAt, evidence...); err != nil && !errors.Is(err, state.ErrSessionNotRegistered) {
		return err
	}
	if err := local.AcknowledgeSubagentCandidate(candidate); err != nil {
		return err
	}
	return subagentRejectedError{code: code}
}

// announceSubagent tells a published subagent's parent that it is published,
// from the subagent's durable publication state, even when it has no new
// content or its live transcript has gone away. The notification stays
// urgent so a repaired link reaches the parent on the next pass rather than
// waiting out the upload debounce; SaveRequest drops evidence the pending
// request already carries, so retrying a parent that never publishes is a
// no-op instead of an unbounded append. The parent is read only as far as
// its summary: decoding its whole bundle once for each of its subagents, on
// every pass, was most of the cost of a pass.
func announceSubagent(local *state.Store, reg archive.SessionRegistration, child *state.Published) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	_, publishedAt, published := child.LastPublished()
	if !published {
		return nil
	}
	if _, found, err := local.LoadRegistration(reg.ParentSessionID); err != nil || !found {
		return err
	}
	parent, found, err := local.LoadPublishedSummary(reg.ParentSessionID)
	if err != nil {
		return err
	}
	if found && parent.Status == state.CacheStatusBlocked {
		// A blocked parent cannot republish, and blocking acknowledges its
		// request, so a notification written now would be written and
		// discarded again on every pass for as long as the gap lasts. The link
		// is announced once the parent is unblocked and its cached bundle
		// still lacks it.
		return nil
	}
	if found && parent.LinksPublished(reg.ArchiveSessionID) {
		return nil
	}
	evidence, err := archive.NewLinkedSessionEvidence(reg.ArchiveSessionID, archive.LinkedSessionPublished, publishedAt)
	if reg.NativeChild {
		evidence.Provenance = "native:relationship"
	}
	if err != nil {
		return err
	}
	return local.SaveRequest(reg.ParentSessionID, "subagent-published", publishedAt, evidence)
}

// ReadImportSubagentBundle validates the child on its filtered snapshot before
// its parent import admits the durable source selection.
func ReadImportSubagentBundle(ctx context.Context, parent archive.SessionRegistration, candidate state.SubagentCandidate, opts Options) (archive.SessionRegistration, archive.SourceBundle, error) {
	reg := assembleSubagentRegistration(parent, candidate)
	adapter, err := sourceAdapter(opts.Sources, reg.Harness.Name)
	if err != nil {
		return reg, archive.SourceBundle{}, err
	}
	source, _ := newSourceReader(reg, opts)
	filtered, _, err := source.Filter(ctx, adapter, min(opts.maxTranscriptBytes(), state.MaxImportStageBytes))
	if err != nil {
		return reg, archive.SourceBundle{}, err
	}
	if err := CheckImportedSubagent(filtered, parent.NativeSessionID, candidate.AgentID, parent.SessionStartedAt, candidate.ObservedAt); err != nil {
		return reg, archive.SourceBundle{}, err
	}
	reg.SessionStartedAt = filtered.NativeStartAt
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, candidate.ObservedAt, nil)
	return reg, bundle, err
}
