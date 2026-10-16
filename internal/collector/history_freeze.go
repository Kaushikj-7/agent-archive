package collector

import (
	"context"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// freezeRevisionPublication stages reconciled evidence before persisting its
// descriptor. Only acknowledged complete authority supplies the predecessor.
func (s *sessionScan) freezeRevisionPublication(p *state.PendingPublication) error {
	var final archive.Metadata
	if err := s.unmarshalRetained(p.MetadataBytes, &final); err != nil {
		return err
	}
	final.SchemaVersion = archive.HistoryMetadataSchemaVersion
	final.History = &archive.RevisionHistory{CurrentRevision: s.revisions.Current, Preserved: append([]archive.RevisionReference(nil), s.revisions.Preserved...)}
	previous, found, err := s.published.LastPublishedMetadata()
	if err != nil {
		return err
	}
	predecessor := ""
	if found {
		if err := s.validateAuthorityIdentity(previous); err != nil {
			return err
		}
		predecessor = metadataSHA(s.published.Metadata())
	} else {
		// Absence must be observed, never inferred from missing local state.
		if _, err := s.historyGet(p.MetadataKey, historyMetadataLimit); !errors.Is(err, storage.ErrNotFound) {
			if err != nil {
				return err
			}
			return errHistoryMetadataConflict
		}
	}
	history := &state.PendingHistory{Version: 1, Preparing: true, FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, ExpectedMetadataSHA256: predecessor}
	if err := boundFrozenReferences(final); err != nil {
		return err
	}
	total := len(p.SourceBytes)
	if total > 128<<20 {
		return storage.ErrObjectTooLarge
	}
	for _, source := range s.revisions.Sources {
		if len(source.Bytes) > (128<<20)-total {
			return storage.ErrObjectTooLarge
		}
		total += len(source.Bytes)
	}
	// Sweep only before staging begins: live descriptor and input stages survive.
	if err := s.local.SweepPendingSources(s.id()); err != nil {
		return err
	}
	active, err := s.local.StagePublicationSource(s.id(), p.SourceReference(), p.SourceBytes)
	if err != nil {
		return err
	}
	history.Sources = append(history.Sources, active)
	for _, source := range s.revisions.Sources {
		stage, err := s.local.StagePublicationSource(s.id(), source.Reference, source.Bytes)
		if err != nil {
			return err
		}
		history.Sources = append(history.Sources, stage)
	}
	history.Inputs = append(history.Inputs, state.HistoryInput{Reference: p.SourceReference(), RevisionID: s.revisions.Current, CapturedAt: p.Bundle.Capture.CapturedAt, FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SourceSchemaVersion: p.Bundle.SchemaVersion})
	if err := s.freezePreservedHistoryInputs(&final, history); err != nil {
		return err
	}
	// Only the current staged source was built from the final header.
	parent := p.Bundle.ParentSessionID
	history.Inputs[0].ParentSessionID = &parent
	if found {
		if err := freezeRetiredHistorySources(previous, final, history); err != nil {
			return err
		}
	}
	p.MetadataBytes, err = s.marshalRetained(final)
	if err != nil {
		return err
	}
	p.History, p.Attempted = history, false
	if found && previous.History == nil && final.History != nil {
		priorBundle, _, ok := s.published.LastPublished()
		if !ok {
			return errors.New("ordinary migration prior bundle unavailable")
		}
		adapter, e := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
		if e != nil {
			return e
		}
		policy, e := s.publicationPolicy(p.Bundle)
		if e != nil {
			return e
		}
		proof, e := state.ValidateOrdinaryHistoryMigration(s.ctx, s.published.Metadata(), p.MetadataBytes, priorBundle, p.Bundle, s.reg, s.priorBinding, s.reg.CodexBinding, adapter, s.publicationAdmission(), policy, s.readBudget())
		if e != nil {
			return e
		}
		*p, e = p.WithOrdinaryMigration(proof)
		if e != nil {
			return e
		}
	}
	return p.ValidateHistoryBudgeted(s.id(), s.readBudget())
}

// resumeHistory never bypasses retained work for a newer request or policy.
// A policy mismatch stays pending until complete successor preparation can
// resolve the remote prior/final state without uploading broader bytes.
func (s *sessionScan) resumeHistory(ctx context.Context, p state.PendingPublication) (sessionOutcome, error) {
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, err
	}
	filterVersion, adapterVersion := p.Bundle.Capture.FilterVersion, p.Bundle.Capture.AdapterVersion
	if p.History.Preparing {
		filterVersion, adapterVersion = p.History.FilterVersion, p.History.AdapterVersion
	}
	if p.History.MaintenanceOwed || pendingSkillMode(p.SkillEvidence) != s.opts.skillEvidence() || filterVersion != archive.FilterVersion || adapterVersion != adapter.Version() {
		return s.resumeStricterHistory(p)
	}
	committed, err := s.checkFrozenHistoryMetadata(p)
	if err != nil {
		return outcomeSkipped, err
	}
	if !committed {
		if err := s.recoverHistoryStages(p); err != nil {
			return outcomeSkipped, err
		}
	}
	if err := s.advanceHistoryPreparation(&p); err != nil {
		return outcomeSkipped, err
	}
	if p.History.Preparing {
		return outcomeSkipped, archive.ErrHistoryMutationPending
	}
	if latest := s.now.Add(s.opts.minUploadInterval()); p.ReadyAt.After(latest) {
		if p.Catalog != nil {
			return outcomeSkipped, state.ErrCatalogJournalFrozen
		}
		p.ReadyAt = latest
		if err := s.savePending(&p); err != nil {
			return outcomeSkipped, err
		}
	}
	if s.now.Before(p.ReadyAt) {
		s.readyAt = p.ReadyAt
		return outcomeRateLimited, nil
	}
	return s.publishPending(ctx, p)
}

func boundFrozenReferences(final archive.Metadata) error {
	refs, err := final.SourceReferences()
	if err != nil {
		return err
	}
	total := 0
	for _, ref := range refs {
		if ref.CompressedBytes <= 0 || int64(ref.CompressedBytes) > historyCompressedLimit || ref.CompressedBytes > (128<<20)-total {
			return storage.ErrObjectTooLarge
		}
		total += ref.CompressedBytes
	}
	return nil
}

// guardRevisionCandidate retains same-physical extension and age semantics before
// the ordinary guard is bypassed for a proved physical transition.
func (s *sessionScan) guardRevisionCandidate(read sourceRead, candidate *archive.SourceBundle) error {
	prior, _, found := s.published.LastPublished()
	if cached, _, status, present := s.published.Cached(); present && status != state.CacheStatusBlocked {
		prior, found = cached, true
	}
	if !found || revisionID(prior) != revisionID(*candidate) {
		retained, present, err := s.reactivatedRevision(*candidate)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		prior = retained
	}
	if !ownedEvidenceCovered(read.adapter, prior, *candidate) {
		return errors.New("current physical revision no longer extends verified evidence; retain history pending reconciliation")
	}
	if ownedEvidenceCovered(read.adapter, *candidate, prior) {
		candidate.Capture.CapturedAt = prior.Capture.CapturedAt
	}
	return nil
}

// settleRevisionCandidate records a token only after the complete retained
// manifest and active evidence agree with the acknowledged publication.
func (s *sessionScan) settleRevisionCandidate(read sourceRead, candidate *archive.SourceBundle) (bool, error) {
	prior, found, err := s.published.LastPublishedMetadata()
	if err != nil || !found || prior.History == nil || prior.History.CurrentRevision != s.revisions.Current {
		return false, err
	}
	same, err := s.jsonEncodingsEqual(prior.History.Preserved, s.revisions.Preserved)
	if err != nil || !same {
		return false, err
	}
	return s.compare(read, candidate)
}

// reactivatedRevision preserves the capture of already retained meaningful
// evidence when its physical revision becomes current again.
func (s *sessionScan) reactivatedRevision(candidate archive.SourceBundle) (archive.SourceBundle, bool, error) {
	metadata, found, err := s.published.LastPublishedMetadata()
	if err != nil || !found || metadata.History == nil {
		return archive.SourceBundle{}, false, err
	}
	for _, revision := range metadata.History.Preserved {
		if revision.RevisionID == revisionID(candidate) {
			input := state.HistoryInput{Reference: revision.Source, RevisionID: revision.RevisionID, CapturedAt: revision.CapturedAt, FilterVersion: revision.FilterVersion, SourceSchemaVersion: revision.SourceSchemaVersion}
			if input.FilterVersion == "" || input.SourceSchemaVersion == 0 {
				inputs, err := s.retainedManifestInputs(metadata, nil)
				if err != nil {
					return archive.SourceBundle{}, false, err
				}
				for _, verified := range inputs {
					if verified.RevisionID == input.RevisionID {
						input = verified
						break
					}
				}
			}
			bundle, err := s.loadHistoryInput(metadata, input)
			return bundle, err == nil, err
		}
	}
	return archive.SourceBundle{}, false, nil
}

// freezePreservedHistoryInputs records each immutable source's original parent
// before preparation can apply the output envelope's resolved parent.
func (s *sessionScan) freezePreservedHistoryInputs(final *archive.Metadata, history *state.PendingHistory) error {
	inputs := s.revisions.historyInputs()
	for i := range final.History.Preserved {
		revision := &final.History.Preserved[i]
		inputs[i].FilterVersion, inputs[i].SourceSchemaVersion = revision.FilterVersion, revision.SourceSchemaVersion
		mark := len(s.retainedReleases)
		bundle, err := s.loadHistoryInput(*final, inputs[i])
		if err != nil {
			return err
		}
		keep := len(s.retainedReleases)
		if len(bundle.ParentSessionID) > 4096 {
			return state.ErrDurableStorageRecovery
		}
		if err := s.retainCharge(int64(len(bundle.ParentSessionID)+len(bundle.Capture.FilterVersion)+len(bundle.Capture.AdapterVersion)) + 32); err != nil {
			return err
		}
		parent, marker := strings.Clone(bundle.ParentSessionID), bundle.NativeChild
		inputs[i].ParentSessionID, inputs[i].NativeChild = &parent, &marker
		inputs[i].AdapterVersion = strings.Clone(bundle.Capture.AdapterVersion)
		if revision.FilterVersion == "" || revision.SourceSchemaVersion == 0 {
			revision.FilterVersion, revision.SourceSchemaVersion = strings.Clone(bundle.Capture.FilterVersion), bundle.SchemaVersion
		}
		inputs[i].FilterVersion, inputs[i].SourceSchemaVersion = revision.FilterVersion, revision.SourceSchemaVersion
		s.keepRetainedFrom(mark, keep)
		history.Inputs = append(history.Inputs, inputs[i])
	}
	return nil
}

func freezeRetiredHistorySources(previous, final archive.Metadata, history *state.PendingHistory) error {
	refs, err := previous.SourceReferences()
	if err != nil {
		return err
	}
	finalRefs, err := final.SourceReferences()
	if err != nil {
		return err
	}
	protected := map[archive.SourceReference]bool{}
	for _, ref := range finalRefs {
		protected[ref] = true
	}
	for _, ref := range refs {
		if !protected[ref] {
			history.Retired = append(history.Retired, state.RetiredSource{Reference: ref})
		}
	}
	return nil
}
