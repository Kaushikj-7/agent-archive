package collector

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// skipUnchanged ends a session's turn without reading anything when the
// scan signature shows nothing changed. done reports that it did. A hook
// request always means a read, except on a Cursor database chat whose last
// read failed at the state it is still in: reading it again would copy the
// database only to fail the same way.
func (p *pass) skipUnchanged(reg archive.SessionRegistration, req state.Request) (done bool) {
	id := reg.ArchiveSessionID
	var unchanged bool
	var signature state.ScanSignature
	var err error
	if reg.CaptureFrozen {
		unchanged, signature, err = p.unchangedFrozenSinceLastScan(reg)
	} else {
		unchanged, signature, err = p.unchangedSinceLastScan(reg)
	}
	if errors.Is(err, agentapi.ErrReadBudget) && !errors.Is(err, state.ErrDurableStorageRecovery) {
		// Too little read memory to check is not a change: the scan itself
		// meets the same refusal and records it as a gap or a deferral.
		return false
	}
	if err != nil {
		p.fail(id, fmt.Errorf("check transcript for changes: %w", err))
		return true
	}
	failed, failure := unchanged && signature.Failed, ""
	if failed {
		failure = signature.FailedError
	}
	switch {
	case !unchanged, req.Token != "" && !failed:
		return false
	case failure != "":
		// Not read again, but still a failure: reported as one on every
		// pass, as if the read had been repeated. A hook request stays
		// queued, as it does for a transcript file the filter refuses: its
		// evidence is the only copy, and the chat's next change reads the
		// chat again with it.
		p.fail(id, unchangedSinceFailureError{message: failure, cursor: reg.SourceKind == archive.SourceKindCursorSQLite})
		return true
	}
	if entry, ok := p.opts.labels[id]; ok && entry.Label.Fingerprint() != signature.PublishedLabel && (signature.Blocked == "" || signature.Blocked == state.BlockedReasonTranscriptMissing) {
		if p.labelStates[id] != nil {
			return false
		}
		// A warm context avoids conversation decoding until a changed label
		// needs publication. Apply the same aggregate cap to that decode.
		published, n, err := p.labelLocal.LoadLabelPublication(id, (16<<20)-p.labelBytes)
		p.labelBytes += n
		if err == nil {
			p.labelStates[id] = published
			return false
		}
		// Keep the durable observation owed, without blocking normal capture.
		delete(p.opts.labels, id)
	}
	// Settled, or a recorded gap at the same state. A request on the gap is
	// completed without a read, as a block completes it: the chat is still
	// over the limit, and the gap stands until the chat changes.
	if req.Token != "" {
		if _, err := p.local.CompleteRequest(id, req.Token); err != nil {
			p.fail(id, fmt.Errorf("complete a request on an unchanged gap: %w", err))
			return true
		}
	}
	p.noteGap(reg, signature.Blocked, signature.SizeLimit)
	// Nothing to read, nothing to compare, nothing to journal.
	p.result.Skipped = append(p.result.Skipped, id)
	p.opts.progress(id, false)
	return true
}

// unchangedSinceLastScan answers the spec's "transcript changed?" question
// without opening, reading, parsing, or journaling anything: one stat of the
// transcript plus a few tiny local files. It is the difference between a pass
// costing time proportional to every session this machine has ever registered
// and one costing time proportional to the sessions that actually moved.
// signature is the one it compared against.
//
// It says yes only when all of the following hold, because each of them is a
// way an unchanged file can still owe work:
//
//   - The last completed scan left a signature (see ScanSignature): settled,
//     or a recorded gap (Blocked) at the same source state. For a missing
//     transcript that state is its absence, so a transcript that comes back
//     is read at once.
//   - The transcript is a regular file whose size and nanosecond modification
//     time both still match that signature.
//   - The parser, filter, and adapter versions still match, so an upgrade
//     re-derives every session instead of skipping it; and a gap or failure
//     at a size limit stands only while the limits do.
//   - No publication is pending and no interrupted scan is journaled.
//   - A subagent does not owe its parent the notice that it is published
//     (see linkOwed), which is sent from its scan.
//
// The caller has already established that no hook request is pending.
//
// A registration with no transcript path yet has nothing to read, and is
// unchanged whenever it owes no publication either: a hook that reports the
// path changes the registration, which the next pass reads.
//
// The residual risk is a transcript rewritten in place to exactly its previous
// byte length within the same nanosecond. Nanosecond mtimes make that
// essentially unreachable for a real application, but it is not a proof, so a
// Cursor text transcript — which has no per-record timestamps and is compared
// by prefix, making a silent in-place edit hardest to detect downstream — is
// never skipped on a stat alone while it exists.
//
// A Cursor database chat is identified by its cursorstore.Signature instead
// of a stat (see sourceReader).
//
// A Cursor chat whose last read failed in a way reading it again can't fix
// (see rememberFailedRead) is also "unchanged" while its signature and the
// versions match, although that failed scan is still journaled; Run reports
// its failure again without reading it.
func (p *pass) unchangedSinceLastScan(reg archive.SessionRegistration) (unchanged bool, signature state.ScanSignature, err error) {
	id := reg.ArchiveSessionID
	if err := checkDurableSessionRead(p.ctx, p.local, id, p.opts); err != nil {
		return false, signature, err
	}
	if owed, err := p.linkOwed(reg); err != nil || owed {
		return false, signature, err
	}
	reader, ok := newSourceReader(reg, p.opts)
	if !ok {
		return p.owesNothing(id, false)
	}
	signature, found, err := p.local.LoadScanSignature(id)
	if err != nil || !found || signature.Frozen || signature.SourceSetVersion != sourceSetVersion(reg) {
		return false, signature, err
	}
	if signature.SourceFormat == cursorTextSourceFormat && signature.Blocked != state.BlockedReasonTranscriptMissing {
		return false, signature, nil
	}
	// Even past a remembered read failure: the HEAD-only publication needs
	// no read of the source.
	if headFingerprint(reg.LastHead) != "" && headFingerprint(reg.LastHead) != signature.PublishedLastHead {
		return false, signature, nil
	}
	adapterVersion, known := harnessAdapterVersion(p.opts.Sources, reg.Harness.Name)
	if !known {
		return false, signature, nil
	}
	if signature.ParserVersion != p.opts.parserVersionFor(reg.Harness.Name) || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapterVersion || pendingSkillMode(signature.SkillEvidence) != p.opts.skillEvidence() {
		return false, signature, nil
	}
	if (signature.Failed || sizeLimitGap(signature.Blocked)) && (signature.FailedMaxBytes != p.opts.maxTranscriptBytes() || signature.FailedRecordLimit != recordLimit || signature.FailedRawLimit != p.opts.maxRawBytes(reg.Harness.Name) || readCeilingChanged(signature)) {
		return false, signature, nil
	}
	if !sourceStillAt(p.ctx, reader, signature) {
		return false, signature, nil
	}
	unchanged, _, err = p.owesNothing(id, signature.Failed)
	return unchanged, signature, err
}

// sourceStillAt reports whether the source is still in the state signature
// records: the same stat (or Cursor chat signature), or, for a gap recorded
// at a missing transcript, still missing. A source that cannot be observed
// for any other reason is not.
func sourceStillAt(ctx context.Context, reader sourceReader, signature state.ScanSignature) bool {
	observed, err := reader.Signature(ctx)
	if signature.Blocked == state.BlockedReasonTranscriptMissing {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && observed.matches(signature)
}

// settledSignature returns the signature the session's last scan settled at
// (one recorded by recordScanSignature, not a gap or a failure), when a stat
// of the source can be trusted against it: exactly as far as
// unchangedSinceLastScan trusts one, so never for a Cursor text transcript.
func (s *sessionScan) settledSignature() (state.ScanSignature, bool, error) {
	signature, found, err := s.local.LoadScanSignature(s.id())
	if err != nil || !found || signature.Blocked != "" || signature.Failed || signature.SourceFormat == cursorTextSourceFormat {
		return state.ScanSignature{}, false, err
	}
	return signature, true, nil
}

// sourceSettled reports whether the session's source is still in the state
// its last scan settled at (see settledSignature), and that scan ran the
// filter and adapter this build runs: reading the source now would produce
// exactly the evidence that scan cached.
func (s *sessionScan) sourceSettled(reader sourceReader) bool {
	signature, settled, err := s.settledSignature()
	if err != nil || !settled {
		return false
	}
	adapterVersion, known := harnessAdapterVersion(s.opts.Sources, s.reg.Harness.Name)
	if !known || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapterVersion || pendingSkillMode(signature.SkillEvidence) != s.opts.skillEvidence() {
		return false
	}
	return sourceStillAt(s.ctx, reader, signature)
}

// owesNothing reports that a session has no publication pending and, unless
// failed (a remembered Cursor failure, whose scan stays journaled), no
// interrupted scan journaled.
func (p *pass) owesNothing(id string, failed bool) (bool, state.ScanSignature, error) {
	if !failed {
		if scanPending, err := p.local.ScanPending(id); err != nil || scanPending {
			return false, state.ScanSignature{}, err
		}
	}
	pending, err := p.local.HasPending(id)
	if err != nil || pending {
		return false, state.ScanSignature{}, err
	}
	return true, state.ScanSignature{}, nil
}

// sizeLimitGap reports a gap recorded at a size limit, which stands only
// while the limits do.
func sizeLimitGap(reason state.BlockedReason) bool {
	return reason == state.BlockedReasonTranscriptTooLarge || reason == state.BlockedReasonRecordTooLarge
}

// noteGap counts a session sitting in a size-limit gap, and the limit it
// stopped at when known, for status.
func (p *pass) noteGap(reg archive.SessionRegistration, reason state.BlockedReason, limit *state.SizeLimitGap) {
	if sizeLimitGap(reason) {
		p.sizeLimited = append(p.sizeLimited, reg.ArchiveSessionID)
		if limit != nil {
			p.sizeLimits = append(p.sizeLimits, SizeLimitLabel(reg.Harness.Name, *limit))
		}
	}
}

// linkOwed reports a subagent that is published while its parent, still
// registered and not blocked, does not link it as published yet: its scan
// sends the parent that notice (announceSubagent), so it is not skipped. A
// blocked parent drops the notice it acknowledges, and is announced to once
// it is unblocked. Only the two sessions' summaries are read, the parent's
// once a pass however many subagents it has.
func (p *pass) linkOwed(reg archive.SessionRegistration) (bool, error) {
	parent := reg.ParentSessionID
	if parent == "" || !p.registered[parent] {
		return false, nil
	}
	child, found, err := p.local.LoadPublishedSummary(reg.ArchiveSessionID)
	if err != nil || !found || !child.Published {
		return false, err
	}
	summary, seen := p.parents[parent]
	if !seen {
		if summary, _, err = p.local.LoadPublishedSummary(parent); err != nil {
			return false, err
		}
		p.parents[parent] = summary
	}
	return summary.Status != state.CacheStatusBlocked && !summary.LinksPublished(reg.ArchiveSessionID), nil
}

// recordScanSignature marks a session settled at the transcript bytes this
// scan consumed, so the next pass can skip it. It is written only at an exit
// that owes no further work.
func (s *sessionScan) recordScanSignature(observed sourceState, bundle archive.SourceBundle) error {
	return s.local.SaveScanSignature(s.id(), s.scanSignature(observed, bundle))
}

func (s *sessionScan) scanSignature(observed sourceState, bundle archive.SourceBundle) state.ScanSignature {
	summary := s.published.Summary()
	return state.ScanSignature{
		SourceSetDigest: summary.SourceSetDigest, CurrentRevision: summary.CurrentRevision,
		SourceSchemaVersion: summary.SourceSchemaVersion, MetadataSchemaVersion: summary.MetadataSchemaVersion,
		SourceSetComplete: summary.SourceSetComplete, MeaningfulCapturedAt: summary.MeaningfulCapturedAt,
		SourceSetVersion: sourceSetVersion(s.reg),
		SkillEvidence:    string(s.opts.skillEvidence()),
		TranscriptSize:   observed.size(), TranscriptMtime: observed.file.Mtime,
		ParserVersion: s.parserVersion(), FilterVersion: bundle.Capture.FilterVersion,
		AdapterVersion: bundle.Capture.AdapterVersion, SourceFormat: bundle.Capture.SourceFormat,
		SourceSignature: signaturePointer(observed), SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
		PublishedLastHead: s.publishedLastHead(),
		PublishedLabel:    labelFingerprint(bundle),
	}
}

// recordBlockedSignature marks a session sitting in a recorded gap at the
// source state observed, so the next pass skips it until that state changes
// instead of reading it again to reach the same gap. Without an observed
// state (nil) nothing can vouch for the gap, and the signature is removed:
// the session is scanned again.
func (s *sessionScan) recordBlockedSignature(reason state.BlockedReason, observed *sourceState) error {
	adapterVersion, known := harnessAdapterVersion(s.opts.Sources, s.reg.Harness.Name)
	if observed == nil || !known {
		return s.local.RemoveScanSignature(s.id())
	}
	return s.local.SaveScanSignature(s.id(), state.ScanSignature{
		SourceSetVersion: sourceSetVersion(s.reg),
		SkillEvidence:    string(s.opts.skillEvidence()),
		TranscriptSize:   observed.size(), TranscriptMtime: observed.file.Mtime,
		ParserVersion: s.parserVersion(), FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion,
		SourceSignature: signaturePointer(*observed), SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
		FailedMaxBytes: s.opts.maxTranscriptBytes(), FailedRecordLimit: recordLimit, FailedRawLimit: s.opts.maxRawBytes(s.reg.Harness.Name),
		SizeLimit: s.sizeLimit,
		Blocked:   reason, PublishedLastHead: s.publishedLastHead(),
		PublishedLabel: s.publishedLabel(),
	})
}

// missingSource is the observed state of a source that is gone: nothing but
// its kind, recorded as recordScanSignature records it (empty for a file).
func missingSource(reg archive.SessionRegistration) *sourceState {
	if reg.SourceKind == archive.SourceKindCursorSQLite {
		return &sourceState{kind: archive.SourceKindCursorSQLite}
	}
	return &sourceState{}
}

func sourceSetVersion(reg archive.SessionRegistration) int {
	if reg.Harness.Name == "codex" {
		if reg.NativeChild || reg.CodexBinding.RequiresOwnTask() {
			return 3 // Revalidate first-task creation evidence for related native owners.
		}
		return 2
	}
	return 0
}
