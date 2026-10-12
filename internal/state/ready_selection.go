package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// ReadyPredecessor records the frozen remote predecessor observation.
type ReadyPredecessor string

// ReadyPredecessorAbsent, ReadyPredecessorPresent, and ReadyPredecessorUnknown
// retain the exact predecessor wire states.
const (
	ReadyPredecessorAbsent  ReadyPredecessor = "absent"
	ReadyPredecessorPresent ReadyPredecessor = "present"
	ReadyPredecessorUnknown ReadyPredecessor = "unknown"
)

type readyPolicyContext struct {
	Filter  string
	Adapter string
	Version string
	Skill   string
}

func readyPolicy(p PendingPublication) readyPolicyContext {
	return readyPolicyContext{Filter: p.Bundle.Capture.FilterVersion, Adapter: p.Bundle.Capture.AdapterName, Version: p.Bundle.Capture.AdapterVersion, Skill: p.SkillEvidence}
}

// ReadySelection freezes one ordinary ready source without catalog ownership.
// JournalVersion2 is mandatory so prior decoders cannot ignore this authority.
type ReadySelection struct {
	Version           int              `json:"version"`
	BundleSHA256      string           `json:"bundle_sha256"`
	MetadataSHA256    string           `json:"metadata_sha256"`
	SourceSetSHA256   string           `json:"source_set_sha256"`
	BindingSHA256     string           `json:"binding_sha256"`
	DestinationID     string           `json:"destination_id"`
	AdmissionContext  string           `json:"admission_context"`
	PolicyContext     string           `json:"policy_context"`
	Predecessor       ReadyPredecessor `json:"predecessor"`
	PredecessorSHA256 string           `json:"predecessor_sha256,omitempty"`
	Purpose           string           `json:"purpose"`
	SHA256            string           `json:"sha256"`
}

// ErrReadySelectionFrozen keeps unsupported successors and their evidence pending.
var ErrReadySelectionFrozen = errors.Join(ErrDurableStorageRecovery, errors.New("ready selecting authority is immutable; complete exact publication or retain evidence"))

func readySHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (s *Store) readyDigest(ctx context.Context, value any) (string, error) {
	reservation := s.resourceBudget.Reservation()
	const scratch = 32 << 10
	if !reservation.Reserve(scratch) {
		return "", errStateBudget
	}
	available := min(int64(maxPendingHistoryBytes)*2, reservation.Available())
	limit := available / 2
	n, err := agentmeta.JSONWireBound(ctx, value, limit)
	if errors.Is(err, agentmeta.ErrJSONWireLimit) {
		reservation.Refuse(available + 2)
	}
	reservation.Release(scratch)
	if err != nil {
		return "", errors.Join(errStateBudget, err)
	}
	if !reservation.Reserve(n + n) {
		return "", errStateBudget
	}
	defer reservation.Release(n + n)
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return readySHA(raw), nil
}

func readyDigestValid(h string) bool {
	data, err := hex.DecodeString(h)
	return err == nil && len(data) == sha256.Size && hex.EncodeToString(data) == h
}

func (p PendingPublication) validateReadySelectionShape() error {
	if p.StagedSelection != nil {
		return p.validateStagedShape()
	}
	if p.JournalVersion == 0 && p.Selection == nil {
		return nil
	}
	if p.JournalVersion != 2 || p.Selection == nil {
		return ErrDurableStorageRecovery
	}
	q := p.Selection
	if q.Version != 2 || q.Purpose != "capture" || q.DestinationID == "" || len(q.DestinationID) > 128 || p.MetadataOnly || p.History != nil || p.Bundle.History != nil || p.ImportExpectedMetadataSHA256 != "" || len(p.ImportRetired) != 0 {
		return ErrReadySelectionFrozen
	}
	for _, h := range []string{q.BundleSHA256, q.MetadataSHA256, q.SourceSetSHA256, q.BindingSHA256, q.AdmissionContext, q.PolicyContext, q.SHA256} {
		if !readyDigestValid(h) {
			return ErrDurableStorageRecovery
		}
	}
	switch q.Predecessor {
	case ReadyPredecessorAbsent, ReadyPredecessorUnknown:
		if q.PredecessorSHA256 != "" {
			return ErrDurableStorageRecovery
		}
	case ReadyPredecessorPresent:
		if !readyDigestValid(q.PredecessorSHA256) {
			return ErrDurableStorageRecovery
		}
	default:
		return ErrDurableStorageRecovery
	}
	return nil
}

// FreezeReadySelection creates the proof only from current protected ownership.
// Remote predecessor observation belongs to the caller; publication rechecks it
// before any source effect. Present is checksum-bound to retained exact bytes.
func (s *Store) FreezeReadySelection(ctx context.Context, id string, p PendingPublication, predecessor ReadyPredecessor, previous []byte) (PendingPublication, error) {
	if p.Selection != nil || p.JournalVersion != 0 || p.Catalog != nil {
		return p, ErrReadySelectionFrozen
	}
	if err := s.validateReadablePendingContext(ctx, p); err != nil {
		return p, err
	}
	cfg, err := s.inspectDurableReadRoots()
	if err != nil || !cfg.SupportsOrdinarySelection() {
		return p, errors.Join(ErrDurableStorageRecovery, err)
	}
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found || reg.CaptureFrozen || reg.StagedImportSHA256 != "" || reg.HistoricalProject != nil || !cfg.AcceptSession(reg) {
		return p, errors.Join(ErrReadySelectionFrozen, err)
	}
	if !readySelectionOwnerMatches(reg, p.Bundle) {
		return p, ErrDurableStorageRecovery
	}
	admission, err := s.readyDigest(ctx, reg)
	if err != nil {
		return p, err
	}
	policy, err := s.readyDigest(ctx, readyPolicy(p))
	if err != nil {
		return p, err
	}
	p.JournalVersion = 2
	p.Selection = &ReadySelection{Version: 2, MetadataSHA256: readySHA(p.MetadataBytes), DestinationID: cfg.DestinationID(), AdmissionContext: admission, PolicyContext: policy, Predecessor: predecessor, Purpose: "capture"}
	if err := s.freezeReadyPredecessor(ctx, id, &p, previous); err != nil {
		return p, err
	}
	var metadata archive.Metadata
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeScratch()
	if err := scratch.unmarshalOwned(p.MetadataBytes, &metadata); err != nil {
		return p, err
	}
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 1 || refs[0] != p.SourceReference() {
		return p, errors.Join(ErrDurableStorageRecovery, err)
	}
	p.Selection.BundleSHA256, err = s.readyDigest(ctx, p.Bundle)
	if err != nil {
		return p, err
	}
	p.Selection.SourceSetSHA256, err = s.readyDigest(ctx, refs)
	if err != nil {
		return p, err
	}
	p.Selection.BindingSHA256, err = s.readyBinding(ctx, p)
	if err != nil {
		return p, err
	}
	proof := *p.Selection
	proof.SHA256 = ""
	p.Selection.SHA256, err = s.readyDigest(ctx, proof)
	if err != nil {
		return p, err
	}
	return p, s.validateReadySelection(ctx, p)
}

func (s *Store) freezeReadyPredecessor(ctx context.Context, id string, p *PendingPublication, previous []byte) error {
	if p.Selection.Predecessor == ReadyPredecessorPresent {
		if len(previous) == 0 {
			return ErrDurableStorageRecovery
		}
		read, closeRead := s.WithReadBudget(ctx, s.resourceBudget)
		defer closeRead()
		published, err := read.LoadPublishedStateContext(ctx, id)
		if err != nil {
			return err
		}
		if readySHA(published.Metadata()) != readySHA(previous) {
			return ErrDurableStorageRecovery
		}
		p.Selection.PredecessorSHA256 = readySHA(previous)
	} else if len(previous) != 0 {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (s *Store) readyBinding(ctx context.Context, p PendingPublication) (string, error) {
	p.Selection = nil
	p.StagedSelection = nil
	p.Catalog = nil
	p.JournalVersion = 0
	p.Attempted = false
	p.SourceSize = len(p.SourceBytes)
	p.SourceBytes = nil
	return s.readyDigest(ctx, p)
}

func (s *Store) validateReadySelection(ctx context.Context, p PendingPublication) error {
	if p.StagedSelection != nil {
		return s.validateStagedSelection(ctx, p)
	}
	if p.Selection == nil {
		return p.validateReadySelectionShape()
	}
	if err := p.validateReadySelectionShape(); err != nil {
		return err
	}
	if err := validateReadySourceKeys(p); err != nil {
		return err
	}
	if err := s.validateReadyOwner(ctx, p); err != nil {
		return err
	}
	if err := s.validateReadyDigests(ctx, p); err != nil {
		return err
	}
	return s.validateReadyReference(ctx, p)
}

func validateReadySourceKeys(p PendingPublication) error {
	if len(p.SourceBytes) == 0 || len(p.SourceBytes) > maxPendingHistoryBytes || readySHA(p.SourceBytes) != p.SourceSHA256 {
		return ErrDurableStorageRecovery
	}
	sourceKey, err := archive.SourceObjectKey(p.Bundle, p.SourceSHA256)
	if err != nil || sourceKey != p.SourceKey {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	metadataKey, err := archive.MetadataObjectKey(p.Bundle.Capture.Harness.Name, p.Bundle.ArchiveSessionID)
	if err != nil || metadataKey != p.MetadataKey {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (s *Store) validateReadyOwner(ctx context.Context, p PendingPublication) error {
	cfg, err := s.inspectDurableReadRoots()
	if err != nil || !cfg.SupportsOrdinarySelection() || cfg.DestinationID() != p.Selection.DestinationID {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	reg, found, err := s.LoadRegistration(p.Bundle.ArchiveSessionID)
	if err != nil || !found || reg.CaptureFrozen || reg.StagedImportSHA256 != "" || reg.HistoricalProject != nil || !cfg.AcceptSession(reg) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if !readySelectionOwnerMatches(reg, p.Bundle) {
		return ErrDurableStorageRecovery
	}
	admission, err := s.readyDigest(ctx, reg)
	if err != nil || admission != p.Selection.AdmissionContext {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if p.SkillEvidence != string(cfg.EffectiveSkillEvidence()) {
		return ErrReadySelectionFrozen
	}
	return nil
}

func (s *Store) validateReadyDigests(ctx context.Context, p PendingPublication) error {
	policy, err := s.readyDigest(ctx, readyPolicy(p))
	if err != nil || policy != p.Selection.PolicyContext {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	bundleDigest, err := s.readyDigest(ctx, p.Bundle)
	if err != nil || bundleDigest != p.Selection.BundleSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	binding, err := s.readyBinding(ctx, p)
	if err != nil || binding != p.Selection.BindingSHA256 || readySHA(p.MetadataBytes) != p.Selection.MetadataSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	proof := *p.Selection
	proof.SHA256 = ""
	digest, err := s.readyDigest(ctx, proof)
	if err != nil || digest != p.Selection.SHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (s *Store) validateReadyReference(ctx context.Context, p PendingPublication) error {
	var metadata archive.Metadata
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeScratch()
	if err := scratch.unmarshalOwned(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	if metadata.SessionID != p.Bundle.ArchiveSessionID || metadata.NativeSessionID != p.Bundle.NativeSessionID || metadata.ProjectID != p.Bundle.ProjectID || metadata.Harness.Name != p.Bundle.Capture.Harness.Name || metadata.ParentSessionID != p.Bundle.ParentSessionID || metadata.NativeChild != p.Bundle.NativeChild || !metadata.CapturedAt.Equal(p.Bundle.Capture.CapturedAt) || metadata.FilterVersion != p.Bundle.Capture.FilterVersion || metadata.Adapter.Name != p.Bundle.Capture.AdapterName || metadata.Adapter.Version != p.Bundle.Capture.AdapterVersion {
		return ErrDurableStorageRecovery
	}
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 1 || refs[0] != p.SourceReference() {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	sourceSet, err := s.readyDigest(ctx, refs)
	if err != nil || sourceSet != p.Selection.SourceSetSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

// ValidateReadySelection enforces the current configuration and immutable journal.
func (s *Store) ValidateReadySelection(ctx context.Context, p PendingPublication) error {
	return s.validateReadySelection(ctx, p)
}

// UnmarshalJSON decodes a bounded descriptor and refuses unknown or duplicate fields.
func (q *ReadySelection) UnmarshalJSON(raw []byte) error {
	if len(raw) > 16<<10 {
		return ErrDurableStorageRecovery
	}
	if err := rejectDuplicateAuthorityKeys(raw, []string{"version", "bundle_sha256", "metadata_sha256", "source_set_sha256", "binding_sha256", "destination_id", "admission_context", "policy_context", "predecessor", "predecessor_sha256", "purpose", "sha256"}); err != nil {
		return err
	}
	type plain ReadySelection
	var next plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return ErrDurableStorageRecovery
	}
	*q = ReadySelection(next)
	return nil
}

func (q *ReadySelection) validateProofDigest() error {
	if q == nil || q.Version != 2 {
		return ErrDurableStorageRecovery
	}
	proof := *q
	proof.SHA256 = ""
	raw, err := json.Marshal(proof)
	if err != nil || readySHA(raw) != q.SHA256 {
		return ErrDurableStorageRecovery
	}
	return nil
}

// SaveReadyPublication records selecting proof together with the cache before
// request acknowledgment and pending removal. No source body is duplicated.
func (p *Published) SaveReadyPublication(pending PendingPublication, at time.Time) error {
	if pending.StagedSelection != nil {
		return p.saveStagedPublication(pending, at)
	}
	if err := pending.ValidateReadySlot(p.id); err != nil {
		return err
	}
	if pending.Selection == nil {
		return p.SavePublication(pending.Bundle, at, pending.SourceReference(), pending.MetadataBytes)
	}
	if err := p.store.validateReadySelection(p.store.durableContext(), pending); err != nil {
		return err
	}
	source := pending.SourceReference()
	next, err := p.nextBudgeted(pending.Bundle, at, CacheStatusPublished, "", pending.MetadataBytes, nil, &source)
	if err != nil {
		return err
	}
	proof := *pending.Selection
	next.LastPublished.Selection = &proof
	next.PublicationVersion = 2
	return p.write(next)
}

func (p *Published) preserveReadyPublished(ctx context.Context, next *publishedState) error {
	prior := p.state.LastPublished
	if prior != nil && prior.Selection != nil {
		if next.LastPublished == nil {
			return ErrReadySelectionFrozen
		}
		if next.LastPublished.Selection == nil {
			if readySHA(next.MetadataBytes) != prior.Selection.MetadataSHA256 {
				return ErrReadySelectionFrozen
			}
			proof := *prior.Selection
			next.LastPublished.Selection = &proof
		}
	}
	if next.LastPublished != nil && next.LastPublished.Selection != nil {
		next.PublicationVersion = 2
		cfg, err := p.store.inspectDurableReadRoots()
		if err != nil || !cfg.SupportsOrdinarySelection() {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
		if next.LastPublished.Selection.MetadataSHA256 != readySHA(next.MetadataBytes) {
			return ErrReadySelectionFrozen
		}
		return p.validateReadyPublished(ctx, next)
	}
	return nil
}

func (s *Store) checkSelectingPendingWrite(ctx context.Context, id string, p PendingPublication) error {
	cfg, err := s.inspectDurableReadRoots()
	if err != nil {
		return err
	}
	if !cfg.PublicationCompositionProtection {
		return nil
	}
	read, closeRead := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeRead()
	old, found, err := read.LoadPendingContext(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		if p.Selection != nil || p.StagedSelection != nil {
			return nil
		}
		return ErrReadySelectionFrozen
	}
	if old.StagedSelection != nil && (p.StagedSelection == nil || *old.StagedSelection != *p.StagedSelection) {
		return ErrReadySelectionFrozen
	}
	if old.Selection != nil && (p.Selection == nil || *old.Selection != *p.Selection) {
		return ErrReadySelectionFrozen
	}
	before, err := read.readyBinding(ctx, old)
	if err != nil {
		return err
	}
	after, err := s.readyBinding(ctx, p)
	if err != nil || before != after {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	return nil
}

// Ordinary selecting cleanup requires the acknowledged exact publication proof.
// Catalog work still uses its stronger joined removal/tombstone path.
func (s *Store) checkSelectingPendingRemoval(id string) error {
	read, closeRead := s.WithReadBudget(s.durableContext(), s.resourceBudget)
	defer closeRead()
	pending, found, err := read.catalogPendingForFence(id)
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if !found {
		return nil
	}
	if pending.StagedSelection != nil {
		return s.checkStagedPendingRemoval(pending)
	}
	if pending.Selection == nil {
		return nil
	}
	if err = read.validateReadySelection(s.durableContext(), pending); err != nil {
		return err
	}
	published, err := read.LoadPublishedState(id)
	if err != nil {
		return err
	}
	if published.state.LastPublished == nil || published.state.LastPublished.Selection == nil || *published.state.LastPublished.Selection != *pending.Selection {
		return ErrReadySelectionFrozen
	}
	return nil
}

// validateReadyPublished applies the existing acknowledged metadata/snapshot
// checks to the candidate state, then binds the full retained bundle and set.
func (p *Published) validateReadyPublished(ctx context.Context, next *publishedState) error {
	q := next.LastPublished.Selection
	if err := (PendingPublication{JournalVersion: 2, Selection: q}).validateReadySelectionShape(); err != nil {
		return err
	}
	if err := q.validateProofDigest(); err != nil {
		return err
	}
	read, closeRead := p.store.WithReadBudget(ctx, p.store.resourceBudget)
	defer closeRead()
	view := &Published{store: read, id: p.id, state: *next, found: true}
	metadata, found, err := view.LastPublishedMetadata()
	if err != nil || !found {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 1 || next.LastPublished.Source == nil || *next.LastPublished.Source != refs[0] {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	set, err := read.readyDigest(ctx, refs)
	if err != nil || set != q.SourceSetSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	bundle, _, found := view.LastPublished()
	if !found {
		return ErrDurableStorageRecovery
	}
	digest, err := read.readyDigest(ctx, bundle)
	if err != nil || digest != q.BundleSHA256 || metadata.NativeChild != bundle.NativeChild {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	cfg, err := read.inspectDurableReadRoots()
	if err != nil || cfg.DestinationID() != q.DestinationID {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

// The proof is recomputable wire data. Both construction and every subsequent
// validator must independently match the retained registration's owner.
func readySelectionOwnerMatches(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return reg.ArchiveSessionID == bundle.ArchiveSessionID && reg.NativeSessionID == bundle.NativeSessionID && reg.ProjectID == bundle.ProjectID && reg.ParentSessionID == bundle.ParentSessionID && reg.NativeChild == bundle.NativeChild && reg.Harness.Name == bundle.Capture.Harness.Name
}

// ValidateReadySlot binds selecting authority to the requested journal/session
// slot independently of the owner recorded inside its recomputable wire data.
// Existing legacy publication wire keeps its current identity rules.
func (p PendingPublication) ValidateReadySlot(id string) error {
	if (p.Selection != nil || p.StagedSelection != nil || p.JournalVersion != 0) && id != p.Bundle.ArchiveSessionID {
		return ErrDurableStorageRecovery
	}
	return nil
}
