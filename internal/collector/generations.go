package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// PrepareGenerationRecovery filters the current admitted file without reading
// or rewriting retained history. The returned builder renders a fixed snapshot
// with the latest locked registration and its explicit successor identity.
// The caller closes the returned data lease
// after the preview and any returned publication have finished their consumers.
func PrepareGenerationRecovery(ctx context.Context, reg archive.SessionRegistration, at time.Time, opts Options) (builder func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error), release func(), resultErr error) {
	if reg.HistoricalProject != nil {
		return nil, func() {}, errors.New("historical imports cannot recover native continuation; preview a new explicit import")
	}
	if reg.IsChild() || !reg.ReadsTranscriptFile() {
		return nil, func() {}, errors.New("recovery supports top-level transcript files only; start a fresh parent session for subagent recovery")
	}
	semantics, err := sourceSemantics(opts.Sources, reg)
	if err != nil {
		return nil, func() {}, err
	}
	if semantics.Mutation != agentapi.AppendOnly {
		return nil, func() {}, errors.New("recovery requires an append-only native transcript")
	}
	adapter, err := sourceAdapter(opts.Sources, reg.Harness.Name)
	if err != nil {
		return nil, func() {}, err
	}
	owner := newSessionScan(ctx, nil, nil, reg, state.Request{}, nil, at, opts)
	closePass := openCursorPass(nil, &owner.opts)
	keep := false
	defer func() {
		resultErr = errors.Join(resultErr, closePass())
		if !keep || resultErr != nil {
			owner.releaseRetained()
			builder, release = nil, func() {}
		}
	}()
	opts = owner.opts
	reader, available := newSourceReader(reg, opts)
	if !available {
		return nil, func() {}, errors.New("current native transcript is unavailable")
	}
	filtered, _, err := reader.Filter(ctx, adapter, opts.maxTranscriptBytes())
	if err != nil {
		return nil, func() {}, fmt.Errorf("filter current native transcript: %w", err)
	}
	// Recovery may replace a compacted transcript, but cannot reassign evidence
	// from another native session. An authoritative retained metadata identity
	// wins; copied/resumed records may contain several ownership IDs provided the
	// admitted session is among them. Missing identity facts retain hook authority.
	if filtered.LocalIdentity.ID != "" && filtered.LocalIdentity.ID != reg.NativeSessionID || len(filtered.SessionIDs) > 0 && !slices.Contains(filtered.SessionIDs, reg.NativeSessionID) {
		return nil, func() {}, errors.New("current transcript identity differs from the registered session; recovery cannot attach another native session")
	}
	// Verify the candidate before presenting confirmation; no journal or archive ID
	// is allocated for empty/unsafe input or a publication excluded by skill policy.
	previewOwner := len(owner.retainedReleases)
	preview, err := owner.newSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
		return nil, func() {}, err
	}
	if err := preview.ValidateHistory(); err != nil {
		return nil, func() {}, err
	}
	if opts.RequireSkillUse {
		mark := len(owner.retainedReleases)
		rendered, err := renderPublication(ctx, opts.parserFor(reg.Harness.Name), opts.parserVersionFor(reg.Harness.Name), preview, reg, at, opts, func() string { return reg.RepoKey })
		if err != nil {
			return nil, func() {}, err
		}
		owner.releaseRetainedAfter(mark)
		if rendered.declined {
			return nil, func() {}, errors.New("current transcript does not meet configured skill-use policy")
		}
	}
	if len(preview.NativeRecords) == 0 && len(preview.NativeText) == 0 {
		return nil, func() {}, errors.New("current transcript retains no evidence")
	}
	owner.releaseRetainedIndex(previewOwner)
	keep = true
	builder, release = generationRecoveryBuilder(ctx, owner, filtered, adapter)
	return builder, release, nil
}

// maintainFrozen runs only from retained evidence. Privacy and parser upgrades
// may republish it with its original capture age; no native source or current
// skill inventory is observed, even if the live transcript grew or vanished.
func (s *sessionScan) maintainFrozen() (sessionOutcome, error) {
	if err := s.retainedOnlyAuthority(); err != nil {
		return outcomeSkipped, err
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, err
	}
	if pending, found, err := s.local.LoadPending(s.id()); err != nil {
		return outcomeSkipped, err
	} else if found {
		if pending.History != nil {
			return s.resumeHistory(pending)
		}
		if pending.Bundle.Capture.FilterVersion != archive.FilterVersion || pending.Bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(pending.Bundle.SupplementalEvidence, s.opts.skillEvidence()) {
			// Stronger privacy supersedes a retained-history maintenance retry.
			// Rebuild below from the last acknowledged publication, without native
			// input and without carrying the discarded retry's newer age forward.
			if err := s.local.RemovePending(s.id()); err != nil {
				return outcomeSkipped, err
			}
		} else if _, err := s.publishPending(pending); err != nil {
			return outcomeSkipped, err
		}
	}
	bundle, _, found := s.published.LastPublished()
	if !found {
		return outcomeSkipped, errors.New("frozen generation has no retained publication")
	}
	var retained []archive.SupplementalEvidence
	for _, evidence := range s.req.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession || evidence.Kind == archive.EvidenceKindExplicitFeedback {
			retained = append(retained, evidence)
		}
	}
	updated := mergeSupplementalEvidence(bundle.SupplementalEvidence, retained)
	sameLinks, err := s.jsonEncodingsEqual(bundle.SupplementalEvidence, updated)
	if err != nil {
		return outcomeSkipped, err
	}
	if !sameLinks || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		bundle.SupplementalEvidence = updated
		bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
		filtered, err := s.refilterRetained(adapter, bundle)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("refilter frozen retained history: %w", err)
		}
		// Never fall back to live input when retained refilter fails.
		maintenanceOptions := s.opts
		maintenanceOptions.RequireSkillUse = false // Existing history still requires current privacy maintenance.
		rendered, err := renderPublication(s.ctx, s.resolveParser(), s.parserVersion(), filtered, s.reg, s.now, maintenanceOptions, s.priorRepoKey)
		if err != nil {
			return outcomeSkipped, err
		}
		if rendered.declined {
			return outcomeSkipped, errors.New("frozen history maintenance cannot decline an existing publication")
		}
		pending := state.PendingPublication{ImportExpectedMetadataSHA256: func() string {
			if s.reg.StagedImportSHA256 != "" {
				return metadataSHA(s.published.Metadata())
			}
			return ""
		}(), SkillEvidence: string(s.opts.skillEvidence()), Bundle: filtered, SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata, ReadyAt: s.now, Attempted: true}
		if err := s.local.SavePending(s.id(), pending); err != nil {
			return outcomeSkipped, err
		}
		outcome, err := s.publishPending(pending)
		if err != nil {
			return outcome, err
		}
		return outcome, s.completeRequest("complete frozen generation request")
	}
	outcome, handled, err := regenerateMetadata(s)
	if err != nil || handled {
		return outcome, err
	}
	return outcomeSkipped, s.completeRequest("complete frozen generation request")
}

// Frozen maintenance is settled by derivation and privacy policy, never by a
// stat of the live file. Validate bounded lineage authority before trusting
// its token; requests and publication/scan journals still force maintenance.
func (p *pass) unchangedFrozenSinceLastScan(reg archive.SessionRegistration) (bool, state.ScanSignature, error) {
	var signature state.ScanSignature
	if err := p.retainedSignatureAuthority(reg); err != nil {
		return false, signature, err
	}
	if owed, err := p.linkOwed(reg); err != nil || owed {
		return false, signature, err
	}
	signature, found, err := p.local.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found || !signature.Frozen || signature.SourceSetVersion != sourceSetVersion(reg) || signature.Failed {
		return false, signature, err
	}
	if reg.HistoricalProject != nil && signature.PublishedProjectName != p.opts.historicalProjectName(reg) {
		return false, signature, nil
	}
	adapterVersion, known := harnessAdapterVersion(p.opts.Sources, reg.Harness.Name)
	if !known || signature.ParserVersion != p.opts.parserVersionFor(reg.Harness.Name) || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapterVersion || pendingSkillMode(signature.SkillEvidence) != p.opts.skillEvidence() || signature.PublishedLastHead != headFingerprint(reg.LastHead) {
		return false, signature, nil
	}
	unchanged, _, err := p.owesNothing(reg.ArchiveSessionID, false)
	return unchanged, signature, err
}

func (p *pass) retainedSignatureAuthority(reg archive.SessionRegistration) error {
	if reg.HistoricalProject == nil {
		return p.local.FrozenGeneration(reg)
	}
	s := sessionScan{reg: reg}
	return s.validateHistoricalSnapshot()
}

func (s *sessionScan) recordFrozenSignature() error {
	_, requested, err := s.local.LoadRequest(s.id())
	if err != nil {
		return err
	}
	// This scan is finishing; its own journal does not prevent recording the
	// signature, but queued evidence, uploads and rate-limited work still do.
	if outstanding, err := s.local.Outstanding(s.reg, requested); err != nil || outstanding.OwedAfterScan() {
		return err
	}
	adapterVersion, known := harnessAdapterVersion(s.opts.Sources, s.reg.Harness.Name)
	if !known {
		return nil
	}
	projectName := ""
	if s.reg.HistoricalProject != nil {
		var metadata archive.Metadata
		if err := s.unmarshalRetained(s.published.Metadata(), &metadata); err != nil {
			return err
		}
		projectName = metadata.ProjectName
	}
	summary := s.published.Summary()
	return s.local.SaveScanSignature(s.id(), state.ScanSignature{
		SourceSetDigest: summary.SourceSetDigest, CurrentRevision: summary.CurrentRevision,
		SourceSchemaVersion: summary.SourceSchemaVersion, MetadataSchemaVersion: summary.MetadataSchemaVersion,
		SourceSetComplete: summary.SourceSetComplete, MeaningfulCapturedAt: summary.MeaningfulCapturedAt,
		SourceSetVersion:     sourceSetVersion(s.reg),
		PublishedProjectName: projectName,
		Frozen:               true, SkillEvidence: string(s.opts.skillEvidence()),
		ParserVersion: s.parserVersion(), FilterVersion: archive.FilterVersion,
		AdapterVersion: adapterVersion, PublishedLastHead: s.publishedLastHead(),
	})
}

// generationRecoveryBuilder retains immutable preview data until every returned
// publication finishes its consumer, including durable confirmation.
func generationRecoveryBuilder(ctx context.Context, owner *sessionScan, filtered archive.FilteredTranscript, adapter archive.Adapter) (func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error), func()) {
	reg, at, opts := owner.reg, owner.now, owner.opts
	closed := false
	closeData := func() { closed = true; owner.releaseRetained() }
	return func(latest archive.SessionRegistration, id string) (archive.SessionRegistration, state.PendingPublication, error) {
		if closed {
			return latest, state.PendingPublication{}, agentapi.ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return latest, state.PendingPublication{}, err
		}
		if latest.NativeSessionID != reg.NativeSessionID || latest.ProjectRoot != reg.ProjectRoot || latest.TranscriptPath != reg.TranscriptPath || latest.DestinationID != reg.DestinationID {
			return latest, state.PendingPublication{}, errors.New("registration changed during recovery; preview again")
		}
		mark := len(owner.retainedReleases)
		returned := false
		defer func() {
			if !returned {
				owner.releaseRetainedAfter(mark)
			}
		}()
		latest.ArchiveSessionID = id
		latest.PreviousGenerationID = reg.ArchiveSessionID
		bundle, err := owner.newSourceBundle(latest, adapter, filtered, at, nil)
		if err != nil {
			return latest, state.PendingPublication{}, err
		}
		rendered, err := renderPublication(ctx, opts.parserFor(latest.Harness.Name), opts.parserVersionFor(latest.Harness.Name), bundle, latest, at, opts, func() string { return latest.RepoKey })
		if err != nil {
			return latest, state.PendingPublication{}, err
		}
		if rendered.declined {
			return latest, state.PendingPublication{}, errors.New("current transcript does not meet configured skill-use policy")
		}
		var history *state.PendingHistory
		if bundle.History != nil {
			// The original generation retains all alternatives under its prefix.
			history = &state.PendingHistory{Version: 1, FilterVersion: bundle.Capture.FilterVersion, AdapterVersion: bundle.Capture.AdapterVersion, PreparedAt: at}
		}
		pending := state.PendingPublication{History: history, SkillEvidence: string(opts.skillEvidence()), Bundle: bundle, SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata, ReadyAt: at, Attempted: true}
		if err := pending.ValidateHistoryBudgeted(id, owner.readBudget()); err != nil {
			return latest, state.PendingPublication{}, err
		}
		returned = true
		return latest, pending, nil
	}, closeData
}

func (s *sessionScan) retainedOnlyAuthority() error {
	if s.reg.HistoricalProject != nil {
		return s.reg.ValidateHistoricalProject()
	}
	return s.local.FrozenGeneration(s.reg)
}
