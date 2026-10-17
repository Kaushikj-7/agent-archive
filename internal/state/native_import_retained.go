package state

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// NativeImportRetainedAuthority validates the existing release and selecting
// lineage. It opens no native input and needs no creating batch or root config.
func (s *Store) NativeImportRetainedAuthority(ctx context.Context, reg archive.SessionRegistration) error {
	published, err := s.LoadPublishedStateContext(ctx, reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	return published.ValidateNativeImportAuthority(reg)
}

// NativeImportStageActive reads the same closed state used by collector scans.
func (s *Store) NativeImportStageActive(ctx context.Context, reg archive.SessionRegistration) (bool, error) {
	published, err := s.LoadPublishedStateContext(ctx, reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	return published.NativeImportStageActive(reg)
}

// NativeImportStageActive reports payload selection independently of the
// permanent stage attribution. Released historical state is verified here.
func (p *Published) NativeImportStageActive(reg archive.SessionRegistration) (bool, error) {
	if reg.NativeImport != nil {
		if err := reg.ValidateNativeImport(); err != nil {
			return false, err
		}
	}
	if reg.StagedImportSHA256 == "" {
		return false, nil
	}
	if reg.NativeImport == nil || !p.Summary().Published {
		return true, nil
	}
	if p.TypedStagedPublished() {
		release, err := p.store.loadStagedReleaseState(p.store.durableContext(), reg.ArchiveSessionID)
		if err != nil {
			return false, err
		}
		return !release.released, nil
	}
	return false, p.ValidateNativeImportAuthority(reg)
}

// ValidateNativeImportReadAuthority accepts only the exact initial typed ACK
// with its matching durable stage, or the verified released selecting lineage.
// It grants no publication, maintenance, native-source or cleanup permission.
func (p *Published) ValidateNativeImportReadAuthority(reg archive.SessionRegistration) error {
	if p == nil || p.store == nil || p.id != reg.ArchiveSessionID || reg.ValidateNativeImport() != nil {
		return ErrDurableStorageRecovery
	}
	if !p.TypedStagedPublished() {
		return p.ValidateNativeImportAuthority(reg)
	}
	ctx := p.store.durableContext()
	read, closeRead := p.store.WithReadBudget(ctx, p.store.resourceBudget)
	defer closeRead()
	current, err := read.LoadPublishedStateContext(ctx, p.id)
	if err != nil {
		return err
	}
	q := p.state.LastPublished.StagedProof()
	if current.state.PublicationVersion != 3 || q.Version != 3 || !current.TypedStagedPublished() || !sameStagedSelection(*q, *current.state.LastPublished.StagedProof()) || !bytes.Equal(p.state.MetadataBytes, current.state.MetadataBytes) {
		return ErrDurableStorageRecovery
	}
	released, err := read.nativeImportReadReleaseState(p.id, q)
	if err != nil {
		return err
	}
	if released {
		return current.ValidateNativeImportAuthority(reg)
	}
	stage, found, err := read.LoadImportStage(ctx, reg)
	if err != nil || !found || stage.Version != 2 || !stage.TypedSelection || stage.SHA256 != q.StageSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	owner, err := read.readyDigest(ctx, reg)
	if err != nil || owner != q.Core.AdmissionContext {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	bundle, err := read.readyDigest(ctx, stage.Bundle)
	if err != nil || bundle != q.Core.BundleSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

// This read-only coherence check uses the existing closed codecs and exact
// selection equality. A pending publication's mutation policy is irrelevant to
// reading the already acknowledged source. Present controls never fall back.
func (s *Store) nativeImportReadReleaseState(id string, q *StagedSelection) (bool, error) {
	var receipt stagedReleaseReceipt
	err := s.readStagedControl(id, stagedReleaseReceiptName, &receipt)
	receiptFound := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if receiptFound && !sameStagedSelection(receipt.Selection, *q) {
		return false, ErrDurableStorageRecovery
	}
	var progress stagedReleaseProgress
	if err := s.readStagedControl(id, stagedReleaseProgressName, &progress); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !receiptFound || progress.Version != q.Version || progress.ReceiptSHA256 != receipt.SHA256 {
		return false, ErrDurableStorageRecovery
	}
	return progress.Phase == stagedReleaseReleased, nil
}

// ValidateNativeImportAuthority is read-only: a receipt never substitutes for
// an acknowledged selecting publication and its exact original release proof.
func (p *Published) ValidateNativeImportAuthority(reg archive.SessionRegistration) error {
	original, err := p.store.nativeImportReleasedSelection(p.store.durableContext(), reg)
	if err != nil {
		return err
	}
	if p.id != reg.ArchiveSessionID || p.state.LastPublished == nil {
		return ErrDurableStorageRecovery
	}
	retained, _, found := p.state.resolveLastPublished()
	if !found || !readySelectionOwnerMatches(reg, retained) {
		return ErrDurableStorageRecovery
	}
	if current := p.state.LastPublished.StagedProof(); current != nil {
		if !sameStagedSelection(*current, original) {
			return ErrDurableStorageRecovery
		}
		return nil
	}
	admission, err := archive.NativeImportAdmissionContext(reg)
	if err != nil {
		return err
	}
	if p.state.PublicationVersion == 5 && p.state.Commit != nil {
		if p.state.Commit.DestinationID != reg.DestinationID || p.state.Commit.AdmissionContext != admission {
			return ErrDurableStorageRecovery
		}
		return p.store.validateSelectingPublishedBudgeted(p.state)
	}
	if current := p.state.LastPublished.Selection; current != nil {
		if validateNativeImportSelectionAuthority(reg, *current) != nil || current.AdmissionContext != admission || current.DestinationID != reg.DestinationID {
			return ErrDurableStorageRecovery
		}
		return p.validateReadyPublished(p.store.durableContext(), &p.state)
	}
	return ErrDurableStorageRecovery
}

func (s *Store) nativeImportReleasedSelection(ctx context.Context, reg archive.SessionRegistration) (StagedSelection, error) {
	var receipt stagedReleaseReceipt
	if err := reg.ValidateNativeImport(); err != nil {
		return receipt.Selection, err
	}
	if err := ctx.Err(); err != nil {
		return receipt.Selection, err
	}
	id := reg.ArchiveSessionID
	if err := s.readStagedControl(id, stagedReleaseReceiptName, &receipt); err != nil {
		return receipt.Selection, errors.Join(ErrDurableStorageRecovery, err)
	}
	var progress stagedReleaseProgress
	if err := s.readStagedControl(id, stagedReleaseProgressName, &progress); err != nil {
		return receipt.Selection, errors.Join(ErrDurableStorageRecovery, err)
	}
	q := receipt.Selection
	if _, present, err := s.loadPreparedImportStage(ctx, id); err != nil || present {
		return q, errors.Join(ErrReadySelectionFrozen, err)
	}
	if q.Version != 3 || progress.Version != q.Version || progress.Phase != stagedReleaseReleased || progress.ReceiptSHA256 != receipt.SHA256 || q.StageSHA256 != reg.StagedImportSHA256 || q.Core.DestinationID != reg.DestinationID || validateNativeImportSelectionAuthority(reg, q.Core) != nil {
		return q, ErrDurableStorageRecovery
	}
	// Only the supported membership state can differ from the original owner.
	original := reg
	proof := *reg.NativeImport
	proof.Membership.State = "attached"
	original.NativeImport = &proof
	current, found, err := s.LoadRegistration(id)
	if err != nil || !found || current.ValidateNativeImport() != nil || current.StagedImportSHA256 != q.StageSHA256 || validateNativeImportSelectionAuthority(current, q.Core) != nil {
		return q, errors.Join(ErrDurableStorageRecovery, err)
	}
	currentProof := *current.NativeImport
	currentProof.Membership.State = "attached"
	current.NativeImport = &currentProof
	currentDigest, err := s.readyDigest(ctx, current)
	if err != nil || currentDigest != q.Core.AdmissionContext {
		return q, errors.Join(ErrDurableStorageRecovery, err)
	}
	digest, err := s.readyDigest(ctx, original)
	if err != nil || digest != q.Core.AdmissionContext {
		return q, errors.Join(ErrDurableStorageRecovery, err)
	}
	return q, nil
}

// A historical selecting successor must be normally prepared against this
// exact predecessor. Ordinary typed/ready guards retain their existing policy.
func (p *Published) nativeImportSelectingSuccessor(ctx context.Context, next *publishedState) (bool, error) {
	reg, found, err := p.store.LoadRegistration(p.id)
	if err != nil || !found {
		return false, err
	}
	if reg.NativeImport == nil {
		return false, nil
	}
	if err := p.ValidateNativeImportAuthority(reg); err != nil {
		return false, err
	}
	if next.LastPublished == nil {
		return false, ErrReadySelectionFrozen
	}
	bundle, _, found := next.resolveLastPublished()
	if !found || !readySelectionOwnerMatches(reg, bundle) {
		return false, ErrReadySelectionFrozen
	}
	admission, err := archive.NativeImportAdmissionContext(reg)
	if err != nil {
		return false, err
	}
	predecessor := readySHA(p.state.MetadataBytes)
	if next.PublicationVersion == 5 && next.Commit != nil && next.Preparation != nil {
		a := next.Preparation
		if a.DestinationID != reg.DestinationID || a.AdmissionContext != admission || a.Predecessor != PredecessorPresent || a.PredecessorSHA256 != predecessor || next.Commit.AdmissionContext != admission || next.Commit.DestinationID != reg.DestinationID {
			return false, ErrReadySelectionFrozen
		}
		candidate := *next
		summary, err := p.store.summaryBudgeted(candidate)
		if err != nil {
			return false, err
		}
		candidate.Summary = &summary
		return true, p.store.validateSelectingPublishedBudgeted(candidate)
	}
	if q := next.LastPublished.Selection; q != nil {
		if validateNativeImportSelectionAuthority(reg, *q) != nil || q.AdmissionContext != admission || q.DestinationID != reg.DestinationID || q.Predecessor != ReadyPredecessorPresent || q.PredecessorSHA256 != predecessor {
			return false, ErrReadySelectionFrozen
		}
		return true, p.validateReadyPublished(ctx, next)
	}
	return false, ErrReadySelectionFrozen
}
