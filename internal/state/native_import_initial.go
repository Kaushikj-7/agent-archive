package state

import (
	"bytes"
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func nativeImportStageFlags(cfg config.Config, reg archive.SessionRegistration) error {
	if reg.NativeImport == nil {
		return nil
	}
	if !cfg.NativeImportProtection || !cfg.NativeAuthorityProtection {
		return ErrReadySelectionFrozen
	}
	return reg.ValidateNativeImport()
}

// Before the initial ACK, configuration identity must still match the frozen
// proof. Root identity is lexical/configured; no directory or native file opens.
func nativeImportConfiguredRoot(cfg config.Config, reg archive.SessionRegistration) error {
	if err := nativeImportStageFlags(cfg, reg); err != nil {
		return err
	}
	if reg.NativeImport == nil {
		return nil
	}
	for _, root := range cfg.NativeRoots {
		if root.Agent != "muse" || root.Revision != reg.NativeBinding.RootRevision {
			continue
		}
		fingerprint, err := archive.NativeRootFingerprint(root)
		if err != nil {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
		if fingerprint == reg.NativeImport.Confirmation.RootFingerprint {
			return nil
		}
	}
	return ErrReadySelectionFrozen
}

// A changed root cannot authorize a new selection. The one exception is the
// exact already-persisted selecting ACK, whose normal complete validator has
// checked current owner, original receipt, retained source and metadata proof.
func (s *Store) validateNativeImportStagedConfig(ctx context.Context, cfg config.Config, reg archive.SessionRegistration, candidate PendingPublication) error {
	if err := nativeImportStageFlags(cfg, reg); err != nil {
		return err
	}
	if reg.NativeImport == nil {
		return nil
	}
	published, err := s.LoadPublishedStateContext(ctx, reg.ArchiveSessionID)
	if err != nil {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if published.state.PublicationVersion == 3 && published.state.LastPublished != nil {
		selected := published.state.LastPublished.StagedProof()
		proof := candidate.StagedProof()
		if selected != nil && proof != nil && sameStagedSelection(*selected, *proof) && bytes.Equal(published.state.MetadataBytes, candidate.MetadataBytes) {
			return nil
		}
	}
	return errors.Join(nativeImportConfiguredRoot(cfg, reg), s.nativeImportCurrentBatch(ctx, reg))
}

// A historical stage never enters the legacy generic registration adapter.
// First ownership still requires the distinct prepared historical capability.
func (s *Store) resumeOwnedNativeImport(ctx context.Context, stage ImportStage, reg archive.SessionRegistration, cfg config.Config) error {
	current, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || current.StagedImportSHA256 != stage.SHA256 || !sameImportStageOwner(reg, current) {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if err := nativeImportStageFlags(cfg, current); err != nil {
		return err
	}
	selected, err := s.LoadPublishedStateContext(ctx, current.ArchiveSessionID)
	if err != nil {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if !selected.found {
		return errors.Join(nativeImportConfiguredRoot(cfg, current), s.nativeImportCurrentBatch(ctx, current))
	}
	if selected.state.PublicationVersion != 3 || selected.state.LastPublished == nil {
		return ErrReadySelectionFrozen
	}
	proof := selected.state.LastPublished.StagedProof()
	if proof == nil || proof.StageSHA256 != stage.SHA256 || validateNativeImportSelectionAuthority(current, proof.Core) != nil {
		return ErrReadySelectionFrozen
	}
	bundle, err := s.readyDigest(ctx, stage.Bundle)
	if err != nil || bundle != proof.Core.BundleSHA256 {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	pending, found, err := s.catalogPendingForFence(current.ArchiveSessionID)
	if err != nil {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if found && (pending.StagedProof() == nil || !sameStagedSelection(*pending.StagedProof(), *proof) || !bytes.Equal(pending.MetadataBytes, selected.state.MetadataBytes)) {
		return ErrReadySelectionFrozen
	}
	return nil
}
