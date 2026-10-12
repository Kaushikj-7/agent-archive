package collector

import (
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/state"
)

// revisionPlan is private preparation for the existing frozen journal. It does
// not authorize upload. Stages are made durable by the next lifecycle checkpoint.
type revisionPlan struct {
	Current      string
	ObservedAt   time.Time
	Preserved    []archive.RevisionReference
	Sources      []revisionStage
	MeaningfulAt time.Time
}

type revisionStage struct {
	retainedBytes int
	Bundle        archive.SourceBundle
	Reference     archive.SourceReference
	Bytes         []byte
}

func (s *sessionScan) reconcileRevisions(read sourceRead, active archive.SourceBundle) (*revisionPlan, error) {
	if !s.canReconcileRevisions(read, active) {
		return nil, nil
	}
	r, ok := newSourceReader(s.reg, s.opts)
	if !ok {
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("revision source unavailable"))
	}
	provider, ok := r.(providerReader)
	if !ok {
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("revision source port unavailable"))
	}
	current := revisionID(active)
	if current == "" {
		current = read.observed.binding.PhysicalRolloutID
	}
	plan := &revisionPlan{Current: current, ObservedAt: s.now}
	// A transition must have complete supported authority. Force the ordinary
	// migration lane through full remote validation, keeping its newer candidate.
	previous, _, havePrevious := s.published.LastPublished()
	cached, _, _, haveCached := s.published.Cached()
	// A missing-source block records state without a retained bundle. That
	// bookkeeping is not an outgoing revision and cannot enter the planner.
	haveCached = haveCached && (cached.SchemaVersion != 0 || cached.ArchiveSessionID != "" || len(cached.NativeRecords) != 0 || len(cached.NativeText) != 0)
	if havePrevious && revisionID(previous) != current {
		if err := s.restoreReferenceAuthority(); err != nil {
			return nil, err
		}
		previous, _, havePrevious = s.published.LastPublished()
	}
	metadata, found, err := s.published.LastPublishedMetadata()
	if err != nil {
		return nil, err
	}
	if havePrevious && !found {
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("transition lacks complete reference authority"))
	}
	if found && metadata.History != nil {
		for _, ref := range metadata.History.Preserved {
			if ref.RevisionID != current {
				plan.Preserved = append(plan.Preserved, ref)
			}
		}
	}
	planner := revisionPlanner{budget: s.readBudget(), plan: plan, active: active, adapter: read.adapter, bytesLeft: 128 << 20, compress: s.compressSource}
	if err := s.addPendingRevision(&planner); err != nil {
		return nil, err
	}
	// Latest locally verified candidate precedes native candidates. It survives
	// missing outgoing files; a freshly opened extension can replace it below.
	if haveCached && revisionID(cached) != current {
		if err := planner.add(s.ctx, cached); err != nil {
			return nil, err
		}
	}
	if havePrevious && revisionID(previous) != current {
		if err := planner.add(s.ctx, previous); err != nil {
			return nil, err
		}
	}
	if read.observed.observation.Signature.Provider == "import-stage" {
		return s.reconcileStagedRevisions(read, planner, plan)
	}
	err = provider.revisions(s.ctx, read.adapter, read.observed, s.opts.maxTranscriptBytes(), active.History != nil, func(filtered archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
		return s.addNativeRevision(&planner, filtered, binding, metadata, cached, previous)
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func revisionID(b archive.SourceBundle) string {
	if b.History != nil {
		return b.History.ActiveRolloutID
	}
	// Source2 is an ordinary physical thread. Related/paginated inputs always
	// carry source3 history; no locator or copied prefix invents identity here.
	return b.NativeSessionID
}

type revisionPlanner struct {
	budget    *agentapi.NativeReadBudget
	compress  func(archive.SourceBundle) (archive.CompressedSource, error)
	plan      *revisionPlan
	active    archive.SourceBundle
	adapter   agentapi.TranscriptFilter
	bytesLeft int
}

func (p *revisionPlanner) add(ctx context.Context, bundle archive.SourceBundle) error {
	if err := bundle.ValidateHistory(); err != nil {
		return err
	}
	id := revisionID(bundle)
	if bundle.NativeSessionID != p.active.NativeSessionID || bundle.ArchiveSessionID != p.active.ArchiveSessionID || bundle.ProjectID != p.active.ProjectID {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("revision belongs to another registration"))
	}
	if id == p.plan.Current {
		return nil
	}
	if ownedEvidenceCovered(p.adapter, bundle, p.active) {
		p.dropCoveredLegacyStage(bundle)
		return nil
	}
	// Different privacy/adapter versions cannot prove native coverage. Maintenance
	// must reconcile them sequentially using retained inputs, never discard them.
	for i, stage := range p.plan.Sources {
		if revisionID(stage.Bundle) != id {
			continue
		}
		if ownedEvidenceCovered(p.adapter, bundle, stage.Bundle) {
			return nil
		}
		if !ownedEvidenceCovered(p.adapter, stage.Bundle, bundle) {
			return agentapi.Wrap(agentapi.Changed, errors.New("physical revision no longer extends verified evidence"))
		}
		p.bytesLeft += len(stage.Bytes)
		p.plan.Sources = append(p.plan.Sources[:i], p.plan.Sources[i+1:]...)
		break
	}
	if len(p.plan.Sources) >= archive.MaxHistorySpans {
		return agentapi.Wrap(agentapi.Limit, errors.New("revision stage limit exceeded"))
	}
	retainedBytes, err := p.checkStageEvidence(ctx, bundle)
	if err != nil {
		return err
	}
	compress := p.compress
	if compress == nil {
		compress = archive.BuildCompressedSource
	}
	compressed, err := compress(bundle)
	if err != nil {
		return err
	}
	if len(compressed.Bytes) > p.bytesLeft {
		return agentapi.Wrap(agentapi.Limit, errors.New("revision stage byte limit exceeded"))
	}
	key, err := archive.SourceObjectKey(bundle, compressed.SHA256)
	if err != nil {
		return err
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	replacement := archive.RevisionReference{RevisionID: id, CapturedAt: bundle.Capture.CapturedAt, Source: ref, SourceSchemaVersion: bundle.SchemaVersion, FilterVersion: bundle.Capture.FilterVersion}
	replaced := false
	for i, prior := range p.plan.Preserved {
		if prior.RevisionID == id {
			if prior.Source == ref {
				return nil
			}
			p.plan.Preserved[i] = replacement
			replaced = true
			break
		}
	}
	if !replaced {
		if len(p.plan.Preserved) >= archive.MaxHistorySpans {
			return agentapi.Wrap(agentapi.Limit, errors.New("preserved revision limit exceeded"))
		}
		p.plan.Preserved = append(p.plan.Preserved, replacement)
	}
	p.bytesLeft -= len(compressed.Bytes)
	p.plan.Sources = append(p.plan.Sources, revisionStage{retainedBytes: retainedBytes, Bundle: bundle, Reference: ref, Bytes: compressed.Bytes})
	if bundle.Capture.CapturedAt.After(p.plan.MeaningfulAt) {
		p.plan.MeaningfulAt = bundle.Capture.CapturedAt
	}
	return nil
}

// ownedEvidenceCovered compares same-thread logical ordinals after both owned
// boundaries, then delegates record semantics to the native integration. Physical
// lengths, inherited records and copied headers are never coverage evidence.
func ownedEvidenceCovered(adapter agentapi.TranscriptFilter, previous, candidate archive.SourceBundle) bool {
	evidence, ok := adapter.(agentapi.RevisionEvidence)
	if !ok || previous.Capture.FilterVersion != candidate.Capture.FilterVersion || previous.Capture.AdapterVersion != candidate.Capture.AdapterVersion || previous.Capture.SourceFormat != candidate.Capture.SourceFormat {
		return false
	}
	if previous.History == nil || candidate.History == nil {
		if revisionID(previous) != revisionID(candidate) {
			return false
		}
		return adapter.EvidenceExtends(ownedRevisionProjection(evidence, previous), ownedRevisionProjection(evidence, candidate))
	}
	j := 0
	for i := range previous.NativeRecords {
		if !previous.OwnRecord(i) || !evidence.MeaningfulRevisionRecord(previous, i) {
			continue
		}
		ordinal := revisionOrdinal(previous, i)
		for j < len(candidate.NativeRecords) && (revisionOrdinal(candidate, j) < ordinal || !candidate.OwnRecord(j) || !evidence.MeaningfulRevisionRecord(candidate, j)) {
			j++
		}
		if j == len(candidate.NativeRecords) || revisionOrdinal(candidate, j) != ordinal {
			return false
		}
		left, right := previous, candidate
		left.History, right.History = nil, nil
		left.Ordinals, right.Ordinals = nil, nil
		left.NativeRecords, right.NativeRecords = previous.NativeRecords[i:i+1], candidate.NativeRecords[j:j+1]
		left.NativeText, right.NativeText = nil, nil
		if !adapter.EvidenceExtends(left, right) {
			return false
		}
	}
	return true
}

func revisionOrdinal(bundle archive.SourceBundle, index int) uint64 {
	if bundle.History != nil {
		return bundle.Ordinals[index]
	}
	if index < 0 {
		return 0
	}
	return uint64(index)
}

// historyInputs supplies the existing frozen preparation engine's input shape.
func (p *revisionPlan) historyInputs() []state.HistoryInput {
	inputs := make([]state.HistoryInput, 0, len(p.Preserved))
	for _, ref := range p.Preserved {
		inputs = append(inputs, state.HistoryInput{Reference: ref.Source, RevisionID: ref.RevisionID, CapturedAt: ref.CapturedAt, FilterVersion: ref.FilterVersion})
	}
	return inputs
}

func (s *sessionScan) addNativeRevision(planner *revisionPlanner, filtered archive.FilteredTranscript, binding *archive.CodexSourceBinding, metadata archive.Metadata, cached, previous archive.SourceBundle) error {
	bundle, err := s.newSourceBundle(s.reg, planner.adapter, filtered, s.now, nil)
	if err != nil {
		return err
	}
	id := revisionID(bundle)
	if id == "" {
		id = binding.PhysicalRolloutID
	}
	if id == planner.plan.Current {
		return nil
	}
	if bundle.History == nil && id != bundle.NativeSessionID {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("historical ordinals unavailable"))
	}
	// Preserve old capture provenance unless this physical revision adds new
	// owned evidence. The one observation time is frozen for this whole plan.
	for _, prior := range []archive.SourceBundle{cached, previous} {
		if revisionID(prior) == id && ownedEvidenceCovered(planner.adapter, bundle, prior) {
			bundle.Capture.CapturedAt = prior.Capture.CapturedAt
			break
		}
	}

	if metadata.History != nil {
		for _, ref := range metadata.History.Preserved {
			if ref.RevisionID != id {
				continue
			}
			if ref.Source.CompressedBytes <= 0 || int64(ref.Source.CompressedBytes) > historyCompressedLimit {
				return agentapi.Wrap(agentapi.Limit, errors.New("preserved revision exceeds read budget"))
			}
			data, err := s.historyGet(ref.Source.Key, int64(ref.Source.CompressedBytes))
			if err != nil {
				return err
			}
			prior, err := s.decodeRevision(metadata, id, data)
			if err != nil {
				return err
			}
			if ownedEvidenceCovered(planner.adapter, bundle, prior) {
				return nil
			}
			if !ownedEvidenceCovered(planner.adapter, prior, bundle) {
				return agentapi.Wrap(agentapi.Changed, errors.New("preserved physical revision changed"))
			}
			break
		}
	}
	return planner.add(s.ctx, bundle)
}

func (s *sessionScan) addPendingRevision(planner *revisionPlanner) error {
	if pending, found, err := s.local.LoadPending(s.id()); err != nil {
		return err
	} else if found {
		var document archive.Metadata
		if err := s.unmarshalRetained(pending.MetadataBytes, &document); err != nil {
			return err
		}
		if err := s.validateAuthorityIdentity(document); err != nil {
			return err
		}
		if document.SourceBundle != pending.SourceReference() {
			return errors.New("pending candidate reference disagrees")
		}
		bundle, err := s.decodeReferenced(document, pending.SourceBytes)
		if err != nil {
			return err
		}
		if revisionID(bundle) != planner.plan.Current {
			if err := planner.add(s.ctx, bundle); err != nil {
				return err
			}
		}
	}

	return nil
}

// checkStageEvidence reserves retained records and encoded bytes independently of
// compressed disk size, including cached/pending inputs absent from native reads.
func (p *revisionPlanner) checkStageEvidence(ctx context.Context, bundle archive.SourceBundle) (int, error) {
	const countScratch = 32 << 10
	if !p.budget.Reserve(countScratch) {
		return 0, errRetainedBudget
	}
	defer p.budget.Release(countScratch)
	recordsLeft, bytesLeft := archive.MaxHistoryRecords, 128<<20
	for _, stage := range p.plan.Sources {
		recordsLeft -= len(stage.Bundle.NativeRecords)
		bytesLeft -= stage.retainedBytes
	}
	if len(bundle.NativeRecords) > recordsLeft || len(bundle.NativeText) != 0 {
		return 0, agentapi.Wrap(agentapi.Limit, errors.New("revision retained record budget exceeded"))
	}
	size := 0
	for _, record := range bundle.NativeRecords {
		// Count a conservative wire bound without allocating an encoded row.
		// Native records already belong to the retained input lease.
		n, err := jsonwire.Bound(ctx, record, int64(bytesLeft-size))
		if err != nil {
			if errors.Is(err, jsonwire.ErrLimit) {
				return 0, agentapi.Wrap(agentapi.Limit, errors.Join(errors.New("revision retained byte budget exceeded"), err))
			}
			return 0, err
		}
		size += int(n)
	}
	return size, nil
}

// Legacy source2 has no raw ordinal proof. Its ordered native evidence can only
// verify an extension of that same physical ordinary source, never another tip.
func ownedRevisionProjection(evidence agentapi.RevisionEvidence, bundle archive.SourceBundle) archive.SourceBundle {
	records := make([]map[string]any, 0, len(bundle.NativeRecords))
	for i, record := range bundle.NativeRecords {
		if bundle.OwnRecord(i) && evidence.MeaningfulRevisionRecord(bundle, i) {
			records = append(records, record)
		}
	}
	bundle.NativeRecords, bundle.History, bundle.Ordinals = records, nil, nil
	bundle.NativeText = nil
	return bundle
}

func (p *revisionPlanner) dropCoveredLegacyStage(bundle archive.SourceBundle) {
	if bundle.History == nil {
		return
	}
	for i, stage := range p.plan.Sources {
		if stage.Bundle.History != nil || revisionID(stage.Bundle) != revisionID(bundle) || !ownedEvidenceCovered(p.adapter, stage.Bundle, bundle) {
			continue
		}
		p.bytesLeft += len(stage.Bytes)
		p.plan.Sources = append(p.plan.Sources[:i], p.plan.Sources[i+1:]...)
		for j, ref := range p.plan.Preserved {
			if ref.Source == stage.Reference {
				p.plan.Preserved = append(p.plan.Preserved[:j], p.plan.Preserved[j+1:]...)
				break
			}
		}
		p.plan.MeaningfulAt = time.Time{}
		for _, remaining := range p.plan.Sources {
			if remaining.Bundle.Capture.CapturedAt.After(p.plan.MeaningfulAt) {
				p.plan.MeaningfulAt = remaining.Bundle.Capture.CapturedAt
			}
		}
		return
	}
}

func (s *sessionScan) reconcileStagedRevisions(read sourceRead, planner revisionPlanner, plan *revisionPlan) (*revisionPlan, error) {
	for _, staged := range read.stagedRevisions {
		filtered, err := s.refilterRetained(read.adapter, staged)
		if err != nil {
			return nil, err
		}
		if err := planner.add(s.ctx, filtered); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func (s *sessionScan) canReconcileRevisions(read sourceRead, active archive.SourceBundle) bool {
	return s.reg.Harness.Name == archive.HarnessCodex && read.observed.binding != nil && (active.History != nil || s.opts.CodexRollouts != nil)
}
