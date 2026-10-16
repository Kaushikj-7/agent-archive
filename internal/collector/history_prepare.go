package collector

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// advanceHistoryPreparation filters one frozen historical input per slice.
// Until the last input is staged, no remote write or request acknowledgement
// is allowed. Each completed input checkpoints only immutable private work.
func (s *sessionScan) advanceHistoryPreparation(p *state.PendingPublication) error {
	if p.History == nil || !p.History.Preparing {
		return nil
	}
	if p.Attempted {
		return errors.New("attempted history publication cannot be prepared again")
	}
	if err := p.ValidateHistoryBudgeted(s.id(), s.readBudget()); err != nil {
		return err
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return err
	}
	if pendingSkillMode(p.SkillEvidence) != s.opts.skillEvidence() {
		return errors.New("history preparation skill policy changed; frozen inputs remain pending")
	}
	if p.History.FilterVersion != "" && (p.History.FilterVersion != archive.FilterVersion || p.History.AdapterVersion != adapter.Version()) {
		return errors.New("history preparation policy changed; frozen inputs remain pending")
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	// Missing per-input provenance belongs to legacy journals. Decode their
	// checksum-bound originals before applying the output's resolved parent.
	if p.History.PrivacyCursor < len(p.History.Inputs) {
		input := p.History.Inputs[p.History.PrivacyCursor]
		if err := s.prepareHistoryInput(p, &metadata, input); err != nil {
			return err
		}
		p.History.PrivacyCursor++
		encoded, err := s.marshalRetained(metadata)
		if err != nil {
			return err
		}
		p.MetadataBytes = encoded
	}
	p.History.Preparing = p.History.PrivacyCursor < len(p.History.Inputs)
	refs, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	protected := map[archive.SourceReference]bool{}
	for _, ref := range refs {
		protected[ref] = true
	}
	stages := p.History.Sources[:0]
	for _, stage := range p.History.Sources {
		if protected[stage.Reference] {
			stages = append(stages, stage)
		}
	}
	p.History.Sources = stages
	if !p.History.Preparing {
		if err := s.derivePreparedHistory(p, metadata); err != nil {
			return err
		}
		p.History.PreparedAt = s.now
		for i := range p.History.Retired {
			if p.History.Retired[i].RetiredAt.IsZero() {
				p.History.Retired[i].RetiredAt = s.now
			}
		}
	}
	return s.local.SavePending(s.id(), *p)
}

// historyInputBundle is input's bundle. The current source of a publication
// this scan froze from its own candidate (frozenSource) is that candidate,
// the bundle the publication carries (p.Bundle), compressed in this scan:
// decoding the staged copy of those bytes again would hold a second copy of
// a long thread's whole chain, so the bundle is used as it is. Any other
// input is decoded from its retained bytes, as is the current source of a
// publication an earlier scan froze, whose bundle may since have gained
// observations its staged source lacks.
func (s *sessionScan) historyInputBundle(p *state.PendingPublication, identity archive.Metadata, input state.HistoryInput) (archive.SourceBundle, error) {
	own := s.frozenSource != (archive.SourceReference{}) && input.Reference == s.frozenSource &&
		identity.History != nil && input.RevisionID == identity.History.CurrentRevision && input.Reference == p.SourceReference() && !p.CarriesNoSource() &&
		revisionID(p.Bundle) == input.RevisionID && input.ParentSessionID != nil && *input.ParentSessionID == p.Bundle.ParentSessionID &&
		(input.SourceSchemaVersion == 0 || input.SourceSchemaVersion == p.Bundle.SchemaVersion)
	if own {
		return p.Bundle, nil
	}
	if identity.History != nil && input.RevisionID == identity.History.CurrentRevision {
		currentInputDecodes.Add(1)
	}
	return s.loadHistoryInput(identity, input)
}

// currentInputDecodes counts history preparations that decoded a
// publication's current source from its retained bytes.
var currentInputDecodes atomic.Int64

func (s *sessionScan) loadHistoryInput(identity archive.Metadata, input state.HistoryInput) (archive.SourceBundle, error) {
	selected := identity
	if input.ParentSessionID != nil {
		selected.ParentSessionID = *input.ParentSessionID
	}
	selected.SchemaVersion = archive.HistoryMetadataSchemaVersion
	selected.History = &archive.RevisionHistory{CurrentRevision: input.RevisionID}
	selected.SourceBundle = input.Reference
	selected.CapturedAt = input.CapturedAt
	selected.FilterVersion = input.FilterVersion
	// Original inputs remain live even after their stage leaves final Sources.
	mark := len(s.retainedReleases)
	data, err := s.readRetainedInputBytes(input.Reference)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	preserved := identity.History != nil && input.RevisionID != identity.History.CurrentRevision
	if preserved {
		selected.History = &archive.RevisionHistory{CurrentRevision: identity.History.CurrentRevision, Preserved: []archive.RevisionReference{{RevisionID: input.RevisionID, CapturedAt: input.CapturedAt, Source: input.Reference, FilterVersion: input.FilterVersion, SourceSchemaVersion: input.SourceSchemaVersion}}}
		// Keep the original active pointer separate from the frozen preserved
		// input so the reader uses that physical revision's producer observations.
		selected.SourceBundle = identity.SourceBundle
		selected.CapturedAt = identity.CapturedAt
	}
	decode := func() (archive.SourceBundle, error) {
		if preserved {
			return s.decodeRevision(selected, input.RevisionID, data)
		}
		return s.decodeReferenced(selected, data)
	}
	bundle, err := decode()
	// Old completed repair journals did not record input parents. Their exact
	// checksum input can still prove the one allowed unresolved-to-known native
	// child transition; a different known parent never becomes a candidate.
	if err != nil && input.ParentSessionID == nil && identity.NativeChild && s.reg.NativeChild && identity.ParentSessionID != "" {
		selected.ParentSessionID = ""
		bundle, err = decode()
		if err == nil && !bundle.NativeChild {
			err = errors.New("unresolved legacy input is not a native child")
		}
	}
	// Decode owns independent records; compressed input is no longer used.
	s.releaseRetainedIndex(mark)
	if err == nil && input.SourceSchemaVersion != 0 && bundle.SchemaVersion != input.SourceSchemaVersion {
		return archive.SourceBundle{}, errors.New("frozen input source schema differs from retained bytes")
	}
	return bundle, err
}

func replacePreparedReference(p *state.PendingPublication, metadata *archive.Metadata, input state.HistoryInput, filtered archive.SourceBundle, next archive.SourceReference, data []byte) error {
	found := false
	for i := range metadata.History.Preserved {
		revision := &metadata.History.Preserved[i]
		if revision.RevisionID == input.RevisionID && revision.Source == input.Reference {
			revision.Source, revision.SourceSchemaVersion, revision.FilterVersion = next, filtered.SchemaVersion, filtered.Capture.FilterVersion
			found = true
		}
	}
	if metadata.SourceBundle == input.Reference && metadata.History.CurrentRevision == input.RevisionID {
		metadata.SourceBundle, metadata.FilterVersion = next, filtered.Capture.FilterVersion
		p.Bundle = filtered
		p.SourceKey, p.SourceSHA256, p.SourceBytes = next.Key, next.SHA256, data
		found = true
	}
	if !found {
		return errors.New("history preparation input no longer belongs to its frozen set")
	}
	return nil
}

// readRetainedInputBytes prefers immutable owned stages, including originals
// removed from Sources. Remote recovery must later pass exact input decoding.
func (s *sessionScan) readRetainedInputBytes(ref archive.SourceReference) ([]byte, error) {
	stage := state.PendingSource{Reference: ref, Name: ref.SHA256 + ".gz"}
	data, err := s.historyStage(stage)
	if err == nil {
		return data, nil
	}
	return s.historyGet(ref.Key, int64(ref.CompressedBytes))
}

func (s *sessionScan) derivePreparedHistory(p *state.PendingPublication, metadata archive.Metadata) error {
	maintenance := s.opts
	maintenance.RequireSkillUse = false
	maintenance.repoKeys = nil
	maintenance.RepoKey = func(string) string { return metadata.RepoKey }
	if maintenance.MachineID == "" {
		maintenance.MachineID = metadata.MachineID
	}
	rendered, err := renderPublication(s.ctx, s.resolveParser(), s.parserVersion(), p.Bundle, s.reg, s.now, maintenance, func() string { return metadata.RepoKey })
	if err != nil {
		return err
	}
	var derived archive.Metadata
	if err := s.unmarshalRetained(rendered.metadata, &derived); err != nil {
		return err
	}
	derived.SchemaVersion, derived.History = archive.HistoryMetadataSchemaVersion, metadata.History
	derived.SourceBundle = metadata.SourceBundle
	p.MetadataBytes, err = s.marshalRetained(derived)
	if err != nil {
		return err
	}
	return nil
}

func (s *sessionScan) prepareHistoryInput(p *state.PendingPublication, metadata *archive.Metadata, input state.HistoryInput) error {
	// An in-flight journal keeps one parent target across all preparation slices.
	// A later link remains requested and is applied by a complete successor.
	reg := s.reg
	if nativeChildOwned(reg, p.Bundle) {
		if reg.ParentSessionID != "" && p.Bundle.ParentSessionID != "" && reg.ParentSessionID != p.Bundle.ParentSessionID {
			return errors.New("frozen native child parent conflicts")
		}
		reg.ParentSessionID = p.Bundle.ParentSessionID
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return err
	}
	bundle, err := s.historyInputBundle(p, *metadata, input)
	if err != nil {
		return err
	}
	privacyChanged := bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence())
	observationsChanged := false
	if input.RevisionID == metadata.History.CurrentRevision {
		observations := mergeSupplementalEvidence(bundle.SupplementalEvidence, p.Bundle.SupplementalEvidence)
		same, err := s.jsonEncodingsEqual(observations, bundle.SupplementalEvidence)
		if err != nil {
			return err
		}
		observationsChanged = !same
		bundle.SupplementalEvidence = observations
	}
	if nativeParentResolved(reg, bundle) || nativeChildMarkerPending(reg, bundle) || observationsChanged || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
		filtered, err := s.refilterRetainedFor(reg, adapter, bundle)
		if err != nil {
			return fmt.Errorf("filter preserved revision: %w", err)
		}
		compressed, err := s.compressSource(filtered)
		if err != nil {
			return err
		}
		if int64(len(compressed.Bytes)) > historyCompressedLimit {
			return storage.ErrObjectTooLarge
		}
		key, err := archive.SourceObjectKey(filtered, compressed.SHA256)
		if err != nil {
			return err
		}
		next := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
		stage, err := s.local.StagePendingSource(s.id(), next, compressed.Bytes)
		if err != nil {
			return err
		}
		if err := replacePreparedReference(p, metadata, input, filtered, next, compressed.Bytes); err != nil {
			return err
		}
		if next != input.Reference {
			p.History.Sources = append(p.History.Sources, stage)
			p.History.Retired = append(p.History.Retired, state.RetiredSource{Reference: input.Reference, PrivacySensitive: privacyChanged})
		}
	}
	return nil
}
