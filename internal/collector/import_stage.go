package collector

import (
	"bytes"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (s *sessionScan) readImportStage() (read sourceRead, ok bool, err error) {
	stage, found, err := s.local.LoadImportStage(s.ctx, s.reg)
	if err != nil {
		return read, false, err
	}
	if !found {
		return read, false, state.ErrDurableStorageRecovery
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return read, false, err
	}
	// Reapply current privacy rules; staged evidence is never raw native input.
	bounded, ok := adapter.(agentapi.BoundedTranscriptRefilter)
	if !ok {
		return read, false, errors.New("staged import requires bounded privacy refilter")
	}
	b := s.readBudget()
	largest := int64(0)
	for _, row := range stage.Bundle.NativeRecords {
		n, err := jsonwire.Bound(s.ctx, row, b.Available())
		if err != nil {
			return read, false, err
		}
		largest = max(largest, n)
	}
	for _, text := range stage.Bundle.NativeText {
		largest = max(largest, int64(len(text.Content)))
	}
	scratch := 3*largest + (64 << 10)
	if !b.Reserve(scratch) {
		return read, false, errRetainedBudget
	}
	defer b.Release(scratch)
	allowance := min(s.opts.maxTranscriptBytes(), b.Available()/2)
	if allowance <= 0 || !b.Reserve(allowance) {
		return read, false, errRetainedBudget
	}
	mark := len(s.retainedReleases)
	filtered, err := bounded.RefilterBounded(s.ctx, stage.Bundle, s.reg.SessionStartedAt, agentapi.ReadLimits{FilteredBytes: allowance, RecordBytes: archive.MaxRecordBytes})
	if err != nil {
		b.Release(allowance)
		return read, false, err
	}
	// Refiltering retained safe records cannot rediscover omissions in the native
	// input. Keep that capture proof, and append only newly observed gaps.
	filtered.Gaps = mergeCaptureGaps(importFilterGaps(stage.Bundle.Capture.Gaps), filtered.Gaps)
	filtered.ObservedHarness = stage.Bundle.Capture.Harness
	filtered.Format = stage.Bundle.Capture.SourceFormat
	if stage.Registration.CodexBinding != nil {
		filtered.NativeStartAt = stage.Registration.CodexBinding.FirstNativeTaskAt
	}
	n := int64(filtered.Boundary.RetainedBytes)
	if n < 0 || n > allowance {
		b.Release(allowance)
		return read, false, errRetainedBudget
	}
	b.Release(allowance - n)
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	read.filterLease = &mark
	if err != nil {
		return read, false, err
	}
	read.adapter, read.filtered = adapter, filtered
	read.stagedRevisions = stage.Preserved
	read.observed = observe(s.reg.SourceKind, agentapi.SourceObservation{Present: true, Size: 1, Activity: stage.Bundle.Capture.CapturedAt, Signature: agentapi.SourceSignature{Version: 1, Provider: "import-stage", Token: stage.SHA256}})
	read.observed.binding = s.reg.CodexBinding
	return read, true, nil
}

func (s *sessionScan) resumeStagedPolicy(p state.PendingPublication) (state.PendingPublication, error) {
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return p, err
	}
	if p.Bundle.Capture.FilterVersion == archive.FilterVersion && p.Bundle.Capture.AdapterVersion == adapter.Version() && pendingSkillMode(p.SkillEvidence) == s.opts.skillEvidence() {
		return p, nil
	}
	current, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	expected := ""
	if errors.Is(err, storage.ErrNotFound) && p.ImportExpectedMetadataSHA256 != "" {
		return p, errHistoryMetadataConflict
	}
	if !errors.Is(err, storage.ErrNotFound) {
		if err != nil {
			return p, err
		}
		expected = metadataSHA(current)
		if expected != metadataSHA(p.MetadataBytes) && expected != p.ImportExpectedMetadataSHA256 {
			return p, errHistoryMetadataConflict
		}
	}
	bundle, err := s.refilterRetained(adapter, p.Bundle)
	if err != nil {
		return p, err
	}
	bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, pendingSkillMode(p.SkillEvidence))
	bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
	rendered, err := renderPublication(s.ctx, s.resolveParser(), s.parserVersion(), bundle, s.reg, s.now, s.opts, s.priorRepoKey)
	if err != nil {
		return p, err
	}
	if rendered.declined {
		return p, errors.New("staged import no longer satisfies the skill-use policy; retain pending evidence for review")
	}
	next := p
	next.Bundle = bundle
	next.SourceKey, next.SourceSHA256, next.SourceBytes = rendered.source.Key, rendered.source.SHA256, rendered.sourceBytes
	next.MetadataBytes = rendered.metadata
	next.SkillEvidence = string(s.opts.skillEvidence())
	next.ImportExpectedMetadataSHA256 = expected
	next.Attempted = false
	next.ImportRetired = append([]archive.SourceReference(nil), p.ImportRetired...)
	if p.SourceKey != next.SourceKey {
		if len(next.ImportRetired) >= 64 {
			return p, errors.New("staged privacy replacement limit reached; recover pending publication")
		}
		next.ImportRetired = append(next.ImportRetired, p.SourceReference())
	}
	if err := s.local.SavePending(s.id(), next); err != nil {
		return p, err
	}
	return next, nil
}

func (s *sessionScan) verifyImportPublication(p state.PendingPublication) error {
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, p.MetadataBytes) {
		return errHistoryMetadataConflict
	}
	var m archive.Metadata
	if err := s.unmarshalRetained(raw, &m); err != nil {
		return err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.verifyHistorySource(s.ctx, ref.Key, ref.SHA256, ref.CompressedBytes); err != nil {
			return err
		}
		if m.History == nil {
			mark := len(s.retainedReleases)
			data, err := s.historyGet(ref.Key, int64(ref.CompressedBytes))
			if err != nil {
				return err
			}
			_, err = s.decodeReferenced(m, data)
			s.releaseRetainedAfter(mark)
			if err != nil {
				return err
			}
		}
	}
	current, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, p.MetadataBytes) {
		return errHistoryMetadataConflict
	}
	return nil
}

// Registration-derived relationship gaps are rebuilt from current ownership.
// Retained native omissions remain proof of what the filter could not retain.
type importRelationshipGap string

const (
	importParentLinkPending   importRelationshipGap = "native_parent_link_pending"
	importRecoveredGeneration importRelationshipGap = "recovered_generation"
)

func importFilterGaps(gaps []archive.CaptureGap) []archive.CaptureGap {
	out := make([]archive.CaptureGap, 0, len(gaps))
	for _, gap := range gaps {
		if code := importRelationshipGap(gap.Code); code != importParentLinkPending && code != importRecoveredGeneration {
			out = append(out, gap)
		}
	}
	return out
}
