package state

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// nativeImportStagedAdmissionDigest normalizes only a closed membership detach
// after proving the exact current persisted selecting ACK. It grants no new
// selection, source, first-owner or generic registration CAS permission.
func (s *Store) nativeImportStagedAdmissionDigest(ctx context.Context, reg archive.SessionRegistration, q *StagedSelection) (string, error) {
	if reg.NativeImport != nil && reg.ValidateNativeImport() != nil {
		return "", archive.ErrUnsupportedNativeImport
	}
	if reg.NativeImport == nil || reg.NativeImport.Membership.State == archive.NativeImportAttached {
		return s.readyDigest(ctx, reg)
	}
	return s.nativeImportExactACKAdmissionDigest(ctx, reg, q)
}

// nativeImportExactACKAdmissionDigest proves the current initial selecting ACK
// and either its held stage or exact local-complete readback receipt. It never
// treats local-complete progress as released maintenance authority.
func (s *Store) nativeImportExactACKAdmissionDigest(ctx context.Context, reg archive.SessionRegistration, q *StagedSelection) (string, error) {
	if reg.NativeImport == nil || (reg.NativeImport.Membership.State != archive.NativeImportDetached && reg.NativeImport.Membership.State != archive.NativeImportAttached) {
		return "", archive.ErrUnsupportedNativeImport
	}
	if reg.ValidateNativeImport() != nil || q == nil || q.Version != 3 || q.validateDigest() != nil || validateNativeImportSelectionAuthority(reg, q.Core) != nil {
		return "", ErrReadySelectionFrozen
	}
	read, closeRead := s.WithImportResultScope(ctx)
	defer closeRead()
	var actual publishedState
	found, err := read.readPublishedOwned(read.publishedPath(reg.ArchiveSessionID), &actual)
	if err != nil || !found || actual.PublicationVersion != 3 || actual.LastPublished == nil || actual.LastPublished.StagedProof() == nil || !sameStagedSelection(*q, *actual.LastPublished.StagedProof()) || readySHA(actual.MetadataBytes) != q.Core.MetadataSHA256 {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	current, found, err := read.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || current.ValidateNativeImport() != nil || validateNativeImportSelectionAuthority(current, q.Core) != nil {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	digest, err := read.nativeImportOriginalStageOwnerDigest(ctx, reg)
	if err != nil || digest != q.Core.AdmissionContext {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	currentDigest, err := read.nativeImportOriginalStageOwnerDigest(ctx, current)
	if err != nil || currentDigest != digest {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	retained, _, found := actual.resolveLastPublished()
	if !found || !readySelectionOwnerMatches(current, retained) {
		return "", ErrReadySelectionFrozen
	}
	bundle, err := read.readyDigest(ctx, retained)
	if err != nil || bundle != q.Core.BundleSHA256 {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	stage, staged, err := read.LoadImportStage(ctx, current)
	if err != nil {
		return "", err
	}
	if !staged {
		// Stage absence is not evidence: only the exact existing closed readback
		// receipt and local-complete/released progress can retain the original proof.
		if err := read.requireStagedReleaseProgress(current.ArchiveSessionID, q); err != nil {
			return "", err
		}
		return digest, nil
	}
	if !stage.TypedSelection || stage.Version != 2 || stage.SHA256 != q.StageSHA256 || stage.Validate(ctx, current) != nil {
		return "", ErrReadySelectionFrozen
	}
	source, err := read.readyDigest(ctx, stage.Bundle)
	if err != nil || source != q.Core.BundleSHA256 {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	roster, err := read.readyDigest(ctx, stage.Children)
	if err != nil || roster != q.RosterSHA256 {
		return "", errors.Join(ErrReadySelectionFrozen, err)
	}
	return digest, nil
}

func (s *Store) nativeImportOriginalStageOwnerDigest(ctx context.Context, reg archive.SessionRegistration) (string, error) {
	if reg.ValidateNativeImport() != nil {
		return "", archive.ErrUnsupportedNativeImport
	}
	receipt := *reg.NativeImport
	receipt.Membership.State = archive.NativeImportAttached
	reg.NativeImport = &receipt
	return s.readyDigest(ctx, reg)
}
