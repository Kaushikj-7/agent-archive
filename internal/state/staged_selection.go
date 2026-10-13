package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// StagedSelection is a separately mandatory codec; ordinary selection2 never
// acquires staged meaning. Stage and request authority are derived by Store.
type StagedSelection struct {
	CursorFacts       *cursor.SettledImportFacts `json:"cursor_facts,omitempty"`
	CursorFactsSHA256 string                     `json:"cursor_facts_sha256,omitempty"`
	Version           int                        `json:"version"`
	StageSHA256       string                     `json:"stage_sha256"`
	RosterSHA256      string                     `json:"roster_sha256"`
	CoveredToken      string                     `json:"covered_token"`
	HadRequest        bool                       `json:"had_request"`
	Core              ReadySelection             `json:"proof"`
	SHA256            string                     `json:"sha256"`
}

// UnmarshalJSON enforces the bounded, closed staged-selection proof codec.
func (q *StagedSelection) UnmarshalJSON(raw []byte) error {
	if len(raw) > 16<<10 {
		return ErrDurableStorageRecovery
	}
	keys := []string{"version", "stage_sha256", "roster_sha256", "covered_token", "had_request", "proof", "sha256"}
	var version struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(raw, &version) != nil {
		return ErrDurableStorageRecovery
	}
	if version.Version == 4 {
		keys = append(keys, "cursor_facts_sha256", "cursor_facts")
	}
	if err := strictStagedKeys(raw, keys); err != nil {
		return err
	}
	type plain StagedSelection
	var v plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return ErrDurableStorageRecovery
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ErrDurableStorageRecovery
	}
	if err := rejectStagedNullAliases(fields["proof"], []string{"version", "bundle_sha256", "metadata_sha256", "source_set_sha256", "binding_sha256", "destination_id", "admission_context", "policy_context", "predecessor", "predecessor_sha256", "purpose", "sha256"}); err != nil {
		return err
	}
	*q = StagedSelection(v)
	return q.validateDigest()
}

func (q *StagedSelection) validateDigest() error {
	if err := q.validateProofShape(); err != nil {
		return err
	}
	proofCopy := *q
	proofCopy.SHA256 = ""
	digest, err := boundedStagedProofDigest(proofCopy)
	if err != nil || digest != q.SHA256 {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (q *StagedSelection) validateProofShape() error {
	if q == nil || !q.validStagedProtocol() || q.Core.SHA256 != "" || q.HadRequest != (q.CoveredToken != "") || len(q.CoveredToken) > 128 || len(q.Core.DestinationID) == 0 || len(q.Core.DestinationID) > 128 {
		return ErrDurableStorageRecovery
	}
	for _, digest := range []string{q.StageSHA256, q.RosterSHA256, q.SHA256, q.Core.BundleSHA256, q.Core.MetadataSHA256, q.Core.SourceSetSHA256, q.Core.BindingSHA256, q.Core.AdmissionContext, q.Core.PolicyContext} {
		if !readyDigestValid(digest) {
			return ErrDurableStorageRecovery
		}
	}
	switch q.Core.Predecessor {
	case ReadyPredecessorAbsent, ReadyPredecessorUnknown:
		if q.Core.PredecessorSHA256 != "" {
			return ErrDurableStorageRecovery
		}
	case ReadyPredecessorPresent:
		if !readyDigestValid(q.Core.PredecessorSHA256) {
			return ErrDurableStorageRecovery
		}
	default:
		return ErrDurableStorageRecovery
	}
	return nil
}

func (p PendingPublication) validateStagedShape() error {
	if p.StagedProof() == nil {
		return nil
	}
	q := p.StagedProof()
	if p.CursorSelection != nil && p.StagedSelection != nil || (q.Version == 4 && p.CursorSelection == nil) || (q.Version == 3 && p.StagedSelection == nil) {
		return ErrDurableStorageRecovery
	}
	if p.JournalVersion != q.Version || p.Selection != nil || p.Catalog != nil || p.MetadataOnly || p.History != nil || p.Bundle.History != nil || p.ImportExpectedMetadataSHA256 != "" || len(p.ImportRetired) != 0 || p.RequestToken != q.CoveredToken {
		return ErrReadySelectionFrozen
	}
	if err := q.validateDigest(); err != nil {
		return err
	}
	for _, h := range []string{q.Core.BundleSHA256, q.Core.MetadataSHA256, q.Core.SourceSetSHA256, q.Core.BindingSHA256, q.Core.AdmissionContext, q.Core.PolicyContext} {
		if !readyDigestValid(h) {
			return ErrDurableStorageRecovery
		}
	}
	switch q.Core.Predecessor {
	case ReadyPredecessorAbsent, ReadyPredecessorUnknown:
		if q.Core.PredecessorSHA256 != "" {
			return ErrDurableStorageRecovery
		}
	case ReadyPredecessorPresent:
		if !readyDigestValid(q.Core.PredecessorSHA256) {
			return ErrDurableStorageRecovery
		}
	default:
		return ErrDurableStorageRecovery
	}
	return nil
}

func (s *Store) stagedContext(ctx context.Context, id string) (config.Config, archive.SessionRegistration, ImportStage, error) {
	cfg, err := s.inspectDurableReadRoots()
	if err != nil || !cfg.SupportsStagedSelection() || cfg.Paused || !cfg.Archive.Enabled {
		return cfg, archive.SessionRegistration{}, ImportStage{}, errors.Join(ErrReadySelectionFrozen, err)
	}
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found || reg.StagedImportSHA256 == "" || reg.CaptureFrozen || reg.HistoricalProject != nil || !cfg.AcceptSession(reg) || reg.DestinationID != cfg.DestinationID() {
		return cfg, reg, ImportStage{}, errors.Join(ErrReadySelectionFrozen, err)
	}
	stage, found, err := s.LoadImportStage(ctx, reg)
	if err != nil || !found || (stage.Version != 2 && stage.Version != 3) || !stage.TypedSelection || len(stage.Preserved) != 0 || stage.Bundle.History != nil || len(stage.Children) > 64 {
		return cfg, reg, stage, errors.Join(ErrReadySelectionFrozen, err)
	}
	if reg.Harness.Name == archive.HarnessCursor {
		if !cfg.SupportsSettledCursorSelection() || stage.Version != 3 || stage.validateCursorFacts(reg) != nil || s.validateSettledIndex(reg) != nil {
			return cfg, reg, stage, ErrReadySelectionFrozen
		}
	} else if stage.Version != 2 {
		return cfg, reg, stage, ErrReadySelectionFrozen
	}
	if _, err := agentmeta.JSONWireBound(ctx, stage.Children, 64<<10); err != nil {
		return cfg, reg, stage, errors.Join(ErrDurableStorageCapacity, err)
	}
	return cfg, reg, stage, nil
}

// FreezeStagedSelection accepts only freshly prepared typed noncatalog stages.
// It never promotes a legacy stage or obtains evidence from native sources.
func (s *Store) FreezeStagedSelection(ctx context.Context, id string, p PendingPublication, predecessor ReadyPredecessor, previous []byte) (PendingPublication, error) {
	if p.StagedProof() != nil || p.Selection != nil || p.JournalVersion != 0 || p.Catalog != nil {
		return p, ErrReadySelectionFrozen
	}
	if err := s.validateReadablePendingContext(ctx, p); err != nil {
		return p, err
	}
	cfg, reg, stage, err := s.stagedContext(ctx, id)
	if err != nil {
		return p, err
	}
	if !readySelectionOwnerMatches(reg, p.Bundle) || p.Bundle.History != nil || p.History != nil || p.MetadataOnly {
		return p, ErrReadySelectionFrozen
	}
	request, found, err := s.LoadRequest(id)
	if err != nil {
		return p, err
	}
	if found && request.Token != p.RequestToken || !found && p.RequestToken != "" {
		return p, ErrDurableStorageRecovery
	}
	version := 3
	purpose := stagedImportPurpose
	var facts *cursor.SettledImportFacts
	var factsSHA256 string
	if stage.Version == 3 {
		version = 4
		purpose = settledCursorImportPurpose
		facts = stage.CursorFacts
		factsSHA256, err = s.readyDigest(ctx, stage.CursorFacts)
		if err != nil {
			return p, err
		}
	}
	q := &StagedSelection{Version: version, StageSHA256: stage.SHA256, CoveredToken: p.RequestToken, HadRequest: found, CursorFacts: facts, CursorFactsSHA256: factsSHA256, Core: ReadySelection{Version: version, Purpose: string(purpose), DestinationID: cfg.DestinationID(), Predecessor: predecessor, MetadataSHA256: readySHA(p.MetadataBytes)}}
	predecessorView := p
	predecessorView.Selection = &q.Core
	if err := s.freezeReadyPredecessor(ctx, id, &predecessorView, previous); err != nil {
		return p, err
	}
	q.RosterSHA256, err = s.readyDigest(ctx, stage.Children)
	if err != nil {
		return p, err
	}
	q.Core.AdmissionContext, err = s.readyDigest(ctx, reg)
	if err != nil {
		return p, err
	}
	q.Core.PolicyContext, err = s.readyDigest(ctx, readyPolicy(p))
	if err != nil {
		return p, err
	}
	sourceFact, err := s.validateStagedSourceForFactory(ctx, stage, p, nil)
	if err != nil {
		return p, err
	}
	q.Core.BundleSHA256, err = s.readyDigest(ctx, p.Bundle)
	if err != nil {
		return p, err
	}
	var metadata archive.Metadata
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeScratch()
	if err = scratch.unmarshalOwned(p.MetadataBytes, &metadata); err != nil {
		return p, err
	}
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 1 || refs[0] != p.SourceReference() || !stagedMetadataMatches(metadata, p.Bundle) {
		return p, ErrDurableStorageRecovery
	}
	q.Core.SourceSetSHA256, err = s.readyDigest(ctx, refs)
	if err != nil {
		return p, err
	}
	p.JournalVersion = q.Version
	setPendingStagedProof(&p, q)
	q.Core.BindingSHA256, err = s.readyBinding(ctx, p)
	if err != nil {
		return p, err
	}
	q.SHA256, err = s.readyDigest(ctx, *q)
	if err != nil {
		return p, err
	}
	return p, s.finishStagedSelectionFactory(ctx, p, sourceFact)
}

func (s *Store) finishStagedSelectionFactory(ctx context.Context, p PendingPublication, fact *validatedStagedSource) error {
	q := p.StagedProof()
	if fact == nil || q == nil {
		return ErrReadySelectionFrozen
	}
	request, found, err := s.LoadRequest(p.Bundle.ArchiveSessionID)
	if err != nil {
		return err
	}
	if found != q.HadRequest || found && request.Token != q.CoveredToken {
		return ErrReadySelectionFrozen
	}
	return s.validateStagedSelectionWithSource(ctx, p, fact)
}

func (s *Store) validateStagedSelection(ctx context.Context, p PendingPublication) error {
	return s.validateStagedSelectionWithSource(ctx, p, nil)
}

func (s *Store) validateStagedSelectionWithSource(ctx context.Context, p PendingPublication, fact *validatedStagedSource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.StagedProof() == nil {
		return nil
	}
	if err := p.validateStagedShape(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateReadySourceKeys(p); err != nil {
		return err
	}
	q := p.StagedProof()
	cfg, reg, stage, err := s.stagedContext(ctx, p.Bundle.ArchiveSessionID)
	if err != nil {
		return err
	}
	if stage.Version == 3 {
		facts, err := s.readyDigest(ctx, stage.CursorFacts)
		if err != nil || q.Version != 4 || q.CursorFacts == nil || stage.CursorFacts == nil || *q.CursorFacts != *stage.CursorFacts || facts != q.CursorFactsSHA256 {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	} else if q.Version != 3 {
		return ErrDurableStorageRecovery
	}
	if p.SkillEvidence != string(cfg.EffectiveSkillEvidence()) || stage.SkillEvidence != cfg.EffectiveSkillEvidence() || stage.Bundle.Capture.FilterVersion != archive.FilterVersion {
		return ErrReadySelectionFrozen
	}
	if cfg.DestinationID() != q.Core.DestinationID || stage.SHA256 != q.StageSHA256 || !readySelectionOwnerMatches(reg, p.Bundle) {
		return ErrDurableStorageRecovery
	}
	if err := s.validateStagedSourceWithFact(ctx, stage, p, fact); err != nil {
		return err
	}
	checks := []struct {
		value any
		want  string
	}{{reg, q.Core.AdmissionContext}, {stage.Children, q.RosterSHA256}, {p.Bundle, q.Core.BundleSHA256}, {readyPolicy(p), q.Core.PolicyContext}}
	for _, check := range checks {
		got, err := s.readyDigest(ctx, check.value)
		if err != nil || got != check.want {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	}
	binding, err := s.readyBinding(ctx, p)
	if err != nil || binding != q.Core.BindingSHA256 || readySHA(p.MetadataBytes) != q.Core.MetadataSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	var metadata archive.Metadata
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeScratch()
	if err = scratch.unmarshalOwned(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 1 || refs[0] != p.SourceReference() || !stagedMetadataMatches(metadata, p.Bundle) {
		return ErrDurableStorageRecovery
	}
	got, err := s.readyDigest(ctx, refs)
	if err != nil || got != q.Core.SourceSetSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (p *Published) saveStagedPublication(pending PendingPublication, at time.Time) error {
	if err := pending.ValidateReadySlot(p.id); err != nil {
		return err
	}
	if err := p.store.validateStagedSelection(p.store.durableContext(), pending); err != nil {
		return err
	}
	source := pending.SourceReference()
	next, err := p.nextBudgeted(pending.Bundle, at, CacheStatusPublished, "", pending.MetadataBytes, nil, &source)
	if err != nil {
		return err
	}
	proof := *pending.StagedProof()
	next.LastPublished.setStagedProof(&proof)
	next.PublicationVersion = proof.Version
	return p.write(next)
}

func (p *Published) preserveStagedPublished(ctx context.Context, next *publishedState) error {
	prior := p.state.LastPublished
	if prior != nil && prior.StagedProof() != nil {
		if next.LastPublished == nil {
			return ErrReadySelectionFrozen
		}
		if next.LastPublished.StagedProof() == nil {
			if readySHA(next.MetadataBytes) != prior.StagedProof().Core.MetadataSHA256 {
				return ErrReadySelectionFrozen
			}
			proof := *prior.StagedProof()
			next.LastPublished.setStagedProof(&proof)
		}
	}
	if next.LastPublished == nil || next.LastPublished.StagedProof() == nil {
		return nil
	}
	return p.validateStagedPublished(ctx, next)
}

func (p *Published) validateStagedPublished(ctx context.Context, next *publishedState) error {
	q := next.LastPublished.StagedProof()
	if q == nil || next.LastPublished.CursorSelection != nil && next.LastPublished.StagedSelection != nil || q.Version == 4 && next.LastPublished.CursorSelection == nil || q.Version == 3 && next.LastPublished.StagedSelection == nil {
		return ErrDurableStorageRecovery
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if next.LastPublished.Selection != nil || q.validateDigest() != nil || readySHA(next.MetadataBytes) != q.Core.MetadataSHA256 {
		return ErrDurableStorageRecovery
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := p.store.inspectDurableReadRoots()
	if err != nil || !cfg.SupportsStagedSelection() || (q.Version == 4 && !cfg.SupportsSettledCursorSelection()) || cfg.DestinationID() != q.Core.DestinationID {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	retained, _, found := next.resolveLastPublished()
	if !found {
		return ErrDurableStorageRecovery
	}
	bundle, err := p.store.readyDigest(ctx, retained)
	if err != nil || bundle != q.Core.BundleSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	var metadata archive.Metadata
	scratch, closeScratch := p.store.WithReadBudget(ctx, p.store.resourceBudget)
	defer closeScratch()
	if err := scratch.unmarshalOwned(next.MetadataBytes, &metadata); err != nil {
		return ErrDurableStorageRecovery
	}
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 1 || next.LastPublished.Source == nil || refs[0] != *next.LastPublished.Source || !stagedMetadataMatches(metadata, retained) {
		return ErrDurableStorageRecovery
	}
	set, err := p.store.readyDigest(ctx, refs)
	if err != nil || set != q.Core.SourceSetSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	reg, found, err := p.store.LoadRegistration(p.id)
	if err != nil || !found || reg.StagedImportSHA256 != q.StageSHA256 || !readySelectionOwnerMatches(reg, retained) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if q.Version == 4 {
		if e := p.store.validateSettledIndex(reg); e != nil {
			return e
		}
		factStage := ImportStage{Version: 3, TypedSelection: true, CursorFacts: q.CursorFacts}
		if factStage.validateCursorFacts(reg) != nil {
			return ErrDurableStorageRecovery
		}
		stage, present, e := p.store.LoadImportStage(ctx, reg)
		if e != nil {
			return e
		}
		if present {
			if stage.Version != 3 || stage.SHA256 != q.StageSHA256 || stage.CursorFacts == nil || *stage.CursorFacts != *q.CursorFacts {
				return ErrDurableStorageRecovery
			}
			stageBundle, e := p.store.readyDigest(ctx, stage.Bundle)
			if e != nil || stageBundle != q.Core.BundleSHA256 {
				return errors.Join(ErrDurableStorageRecovery, e)
			}
		} else {
			var receipt stagedReleaseReceipt
			if e := p.store.readStagedControl(p.id, stagedReleaseReceiptName, &receipt); e != nil || receipt.Version != 4 || !sameStagedSelection(receipt.Selection, *q) {
				return errors.Join(ErrDurableStorageRecovery, e)
			}
		}
		facts, err := p.store.readyDigest(ctx, q.CursorFacts)
		if err != nil || facts != q.CursorFactsSHA256 {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	}
	admission, err := p.store.readyDigest(ctx, reg)
	if err != nil || admission != q.Core.AdmissionContext {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	next.PublicationVersion = q.Version
	return nil
}

// Closed typed authority uses exact key spelling and rejects null/missing keys.
func strictStagedKeys(raw []byte, keys []string) error {
	if err := rejectDuplicateAuthorityKeys(raw, keys); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ErrDurableStorageRecovery
	}
	for _, key := range keys {
		value, present := fields[key]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrDurableStorageRecovery
		}
	}
	if len(fields) != len(keys) {
		return ErrDurableStorageRecovery
	}
	return nil
}

func rejectStagedNullAliases(raw []byte, names []string) error {
	if err := rejectDuplicateAuthorityKeys(raw, names); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ErrDurableStorageRecovery
	}
	for key, value := range fields {
		for _, name := range names {
			if strings.EqualFold(key, name) && (key != name || bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
				return ErrDurableStorageRecovery
			}
		}
	}
	return nil
}

// Metadata must describe the exact retained source, including its privacy and
// adapter epoch. A recomputed digest cannot authorize a different identity.
func stagedMetadataMatches(metadata archive.Metadata, bundle archive.SourceBundle) bool {
	return metadata.SchemaVersion == archive.MetadataSchemaVersion &&
		metadata.SessionID == bundle.ArchiveSessionID &&
		metadata.NativeSessionID == bundle.NativeSessionID &&
		metadata.ProjectID == bundle.ProjectID &&
		metadata.Harness == bundle.Capture.Harness &&
		metadata.ParentSessionID == bundle.ParentSessionID &&
		metadata.NativeChild == bundle.NativeChild &&
		metadata.CapturedAt.Equal(bundle.Capture.CapturedAt) &&
		metadata.FilterVersion == bundle.Capture.FilterVersion &&
		metadata.Adapter.Name == bundle.Capture.AdapterName &&
		metadata.Adapter.Version == bundle.Capture.AdapterVersion
}

func (q *StagedSelection) validStagedProtocol() bool {
	if q.Version == 3 {
		return q.Core.Version == 3 && q.Core.Purpose == "staged-import" && q.CursorFactsSHA256 == "" && q.CursorFacts == nil
	}
	return q.Version == 4 && q.Core.Version == 4 && q.Core.Purpose == "settled-cursor-import" && readyDigestValid(q.CursorFactsSHA256) && q.CursorFacts != nil && settledCursorFactShape(*q.CursorFacts)
}

func settledCursorFactShape(f cursor.SettledImportFacts) bool {
	c := f.Chat
	return readyDigestValid(f.GlobalSHA256) && len(c.ID) > 0 && len(c.ID) <= 255 && len(c.KeyID) > 0 && len(c.KeyID) <= 255 && len(c.Folder) > 0 && len(c.Folder) <= 1024 && len(c.WorkspaceID) <= 255 && !c.Malformed && !c.CreatedAt.IsZero() && c.CursorFacts.VersionPresent && c.CursorFacts.Version == 18 && (c.CursorFacts.Relationships == agentapi.CursorRelationshipsAbsent && c.CursorFacts.ChildIDsSHA256 == "" || c.CursorFacts.Relationships == agentapi.CursorRelationshipsValid && readyDigestValid(c.CursorFacts.ChildIDsSHA256))
}

func sameStagedSelection(a, b StagedSelection) bool {
	af, bf := a.CursorFacts, b.CursorFacts
	a.CursorFacts = nil
	b.CursorFacts = nil
	if a != b {
		return false
	}
	if af == nil || bf == nil {
		return af == nil && bf == nil
	}
	ac, bc := *af, *bf
	at, bt := ac.Chat.CreatedAt, bc.Chat.CreatedAt
	ac.Chat.CreatedAt = time.Time{}
	bc.Chat.CreatedAt = time.Time{}
	return ac == bc && at.Equal(bt)
}

// StagedProof selects one mandatory staged authority; validators reject mixed fields.
func (p PendingPublication) StagedProof() *StagedSelection {
	if p.CursorSelection != nil {
		return p.CursorSelection
	}
	return p.StagedSelection
}

func setPendingStagedProof(p *PendingPublication, q *StagedSelection) {
	p.StagedSelection = nil
	p.CursorSelection = nil
	if q == nil {
		return
	}
	if q.Version == 4 {
		p.CursorSelection = q
	} else {
		p.StagedSelection = q
	}
}

func (p *publishedSnapshot) StagedProof() *StagedSelection {
	if p == nil {
		return nil
	}
	if p.CursorSelection != nil {
		return p.CursorSelection
	}
	return p.StagedSelection
}

func (p *publishedSnapshot) setStagedProof(q *StagedSelection) {
	p.StagedSelection = nil
	p.CursorSelection = nil
	if q == nil {
		return
	}
	if q.Version == 4 {
		p.CursorSelection = q
	} else {
		p.StagedSelection = q
	}
}

type stagedSelectionPurpose string

const (
	stagedImportPurpose        stagedSelectionPurpose = "staged-import"
	settledCursorImportPurpose stagedSelectionPurpose = "settled-cursor-import"
)
