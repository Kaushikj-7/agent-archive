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
	rootErr := nativeImportConfiguredRoot(cfg, reg)
	if rootErr == nil {
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
	return rootErr
}
