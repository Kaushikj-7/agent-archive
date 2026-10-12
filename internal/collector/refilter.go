package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A new filter or adapter version changes what earlier records look like, so
// the rewrite guard cannot compare a snapshot filtered by an earlier release
// with one this release filters (see nativeEvidenceExtends). Most of the
// time that is harmless: the transcript still holds everything, and the
// new-filter candidate replaces the old snapshot with the same evidence
// filtered by the current rules. A transcript that was truncated or
// rewritten since it was captured does not: its candidate is poorer than the
// snapshot. Publishing it would lose the rest for good, and keeping the old
// snapshot would leave content an earlier, weaker filter kept as the
// session's current copy. Instead the snapshot itself is filtered again by
// the current rules and republished, and the rewrite is recorded as the gap
// it is.

// versionChanged reports whether a and b were filtered by different filter
// or adapter versions.
func versionChanged(a, b archive.SourceBundle) bool {
	return a.Capture.FilterVersion != b.Capture.FilterVersion || a.Capture.AdapterVersion != b.Capture.AdapterVersion
}

// rewrittenSinceCapture reports whether a transcript file no longer holds
// everything the retained snapshot was captured from: the session already
// stands in a transcript_rewritten gap, or the file is smaller than at the
// scan that last settled it. A JSONL transcript only grows, so a smaller
// file has lost records; a rewrite that does not shrink the file is not
// detected here. Cursor database chats are never reported: a rewritten chat
// is published as the chat now is (see guard).
func (s *sessionScan) rewrittenSinceCapture(read sourceRead) (bool, error) {
	semantics, err := sourceSemantics(s.opts.Sources, s.reg)
	if err != nil {
		return false, err
	}
	if semantics.Mutation != agentapi.AppendOnly {
		return false, nil
	}
	if reason, blocked := s.published.Blocked(); blocked && reason == state.BlockedReasonTranscriptRewritten {
		return true, nil
	}
	signature, found, err := s.local.LoadScanSignature(s.id())
	if err != nil {
		return false, fmt.Errorf("load scan signature: %w", err)
	}
	if !found || signature.Blocked != "" || signature.Failed || signature.SourceKind != read.observed.kind {
		return false, nil
	}
	return read.observed.size() < signature.TranscriptSize, nil
}

// refilterBundle filters a retained snapshot's records again with adapter
// (the current filter), keeping its capture time, its supplemental evidence
// (filtered again by NewSourceBundle), and its capture gaps. Filtered output
// is valid filter input and refilters unchanged under the same rules
// (FuzzFilterJSONL, FuzzCursorText), so under newer rules only what they
// now drop or redact changes. What the earlier filter already dropped stays
// dropped.
func refilterBundle(ctx context.Context, reg archive.SessionRegistration, adapter archive.Adapter, bundle archive.SourceBundle) (archive.SourceBundle, error) {
	return refilterBundleBounded(ctx, reg, adapter, bundle, agentapi.ReadLimits{})
}

func refilterBundleBounded(ctx context.Context, reg archive.SessionRegistration, adapter archive.Adapter, bundle archive.SourceBundle, limits agentapi.ReadLimits) (archive.SourceBundle, error) {
	if err := ctx.Err(); err != nil {
		return archive.SourceBundle{}, err
	}
	if bundle.SchemaVersion != archive.SourceSchemaVersion && bundle.SchemaVersion != archive.HistorySourceSchemaVersion {
		return archive.SourceBundle{}, errors.New("unsupported retained source schema")
	}
	if bundle.ArchiveSessionID != reg.ArchiveSessionID || bundle.NativeSessionID != reg.NativeSessionID || bundle.ProjectID != reg.ProjectID || bundle.Capture.Harness.Name != reg.Harness.Name || !retainedParentMatches(reg, bundle) {
		return archive.SourceBundle{}, errors.New("retained refilter identity mismatch")
	}
	if err := bundle.ValidateHistory(); err != nil {
		return archive.SourceBundle{}, err
	}
	var filtered archive.FilteredTranscript
	var err error
	if limits.FilteredBytes > 0 {
		f, ok := adapter.(agentapi.BoundedTranscriptRefilter)
		if !ok {
			return archive.SourceBundle{}, errRetainedBudget
		}
		filtered, err = f.RefilterBounded(ctx, bundle, reg.SessionStartedAt, limits)
	} else {
		filtered, err = refilterNative(ctx, reg, adapter, bundle)
	}
	if err != nil {
		return archive.SourceBundle{}, err
	}
	if reg.StagedImportSHA256 != "" {
		filtered.Format = bundle.Capture.SourceFormat
	}
	refiltered, err := archive.NewSourceBundle(reg, adapter, filtered, bundle.Capture.CapturedAt, bundle.SupplementalEvidence)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	// Retained maintenance cannot adopt current producer/version observations.
	refiltered.Capture.Harness = bundle.Capture.Harness
	if err := refiltered.ValidateHistory(); err != nil {
		return archive.SourceBundle{}, err
	}
	originalGaps := bundle.Capture.Gaps
	if nativeParentResolved(reg, bundle) {
		originalGaps = withoutNativeParentPendingGap(originalGaps)
	}
	refiltered.Capture.Gaps = mergeCaptureGaps(originalGaps, refiltered.Capture.Gaps)
	return refiltered, nil
}

// refilterNative runs a snapshot's native records, or its native text, back
// through the filter.
func refilterNative(ctx context.Context, reg archive.SessionRegistration, adapter archive.Adapter, bundle archive.SourceBundle) (archive.FilteredTranscript, error) {
	f, ok := adapter.(agentapi.TranscriptFilter)
	if !ok {
		return archive.FilteredTranscript{}, errors.New("native refilter port required")
	}
	return f.Refilter(ctx, bundle, reg.SessionStartedAt)
}

// mergeCaptureGaps is first followed by each gap of second not already in
// it: the snapshot's own gaps, which filtering it again cannot rediscover
// (they describe the raw transcript), plus any the new filter reports.
func mergeCaptureGaps(first, second []archive.CaptureGap) []archive.CaptureGap {
	out := append([]archive.CaptureGap(nil), first...)
	for _, gap := range second {
		if !slices.Contains(out, gap) {
			out = append(out, gap)
		}
	}
	return out
}

// refilterRewritten replaces candidate, a rewritten transcript's evidence
// filtered by a new filter or adapter version, with the retained snapshot
// filtered again by the current rules. replaced reports that it did. A
// snapshot that cannot be filtered again leaves candidate to replace it, as
// before this existed, with a warning: the new filter's output, though
// poorer, is still safer to publish than the old filter's.
func (s *sessionScan) refilterRewritten(_ context.Context, read sourceRead, snapshot, candidate archive.SourceBundle) (_ archive.SourceBundle, replaced bool, err error) {
	rewritten, err := s.rewrittenSinceCapture(read)
	if err != nil || !rewritten {
		return candidate, false, err
	}
	refiltered, err := s.refilterRetained(read.adapter, snapshot)
	if err != nil {
		if errors.Is(err, agentapi.ErrReadBudget) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return candidate, false, err
		}
		s.warn(fmt.Errorf("filter the retained snapshot of a rewritten transcript again (the rewritten transcript replaces it): %w", err))
		return candidate, false, nil
	}
	if nativeEvidenceExtends(read.adapter, refiltered, candidate) {
		// Filtered the same way now, the two compare record for record, and
		// the transcript holds everything the snapshot did after all (a
		// file restored since its rewrite gap was recorded).
		//
		// Known limitation: refiltering the snapshot starts from what the
		// earlier filter wrote, and the candidate from the raw transcript.
		// Where the new rules change a record's output (a new redaction
		// label, a field the new adapter keeps), a restored transcript does
		// not compare equal: the snapshot is republished, the gap stands,
		// and later passes compare the same way, so the transcript's new
		// records are not captured until the user explicitly starts a linked
		// generation with agent-archive recover. No retained evidence is lost. A looser
		// check (record counts, file size) cannot tell a restored file from
		// one compacted and then grown, and would publish the latter over
		// the richer snapshot, the loss this exists to prevent.
		// TestFilterUpgradeKeepsTheSnapshotOfARestoredTranscriptItCannotMatch
		// pins it.
		return candidate, false, nil
	}
	s.rewritten = &candidate
	return refiltered, true, nil
}

// Native parent links may resolve after a retained revision was captured. The
// stable child owner still identifies that earlier evidence; a different known
// parent remains a conflict.
func retainedParentMatches(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return bundle.ParentSessionID == reg.ParentSessionID || nativeChildOwned(reg, bundle) && bundle.ParentSessionID == ""
}

// Only positively identified native child evidence can acquire its first archive parent.
func nativeParentResolved(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return nativeChildOwned(reg, bundle) && bundle.ParentSessionID == "" && reg.ParentSessionID != ""
}

// A missing optional marker is unknown on older sources. Only the same already
// admitted owner and a supported persisted child binding may upgrade it; parent
// links and current native observations cannot supply that proof.
func nativeChildOwned(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	if !reg.NativeChild {
		return false
	}
	if bundle.NativeChild {
		return true
	}
	binding := reg.CodexBinding
	return binding != nil && binding.Child && binding.Validate() == nil &&
		reg.Harness.Name == archive.HarnessCodex && bundle.Capture.Harness.Name == archive.HarnessCodex &&
		binding.NativeThreadID == reg.NativeSessionID && bundle.NativeSessionID == reg.NativeSessionID &&
		bundle.ArchiveSessionID == reg.ArchiveSessionID && bundle.ProjectID == reg.ProjectID &&
		binding.Home != "" && nativeBindingRelationshipsMatch(reg, binding)
}

func nativeChildMarkerPending(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return !bundle.NativeChild && nativeChildOwned(reg, bundle)
}
