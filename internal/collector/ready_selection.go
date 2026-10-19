package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// freezeOrdinaryReadySelection runs after rendering and before pending save or
// catalog admission. The existing default legacy writer stays unchanged.
func (s *sessionScan) freezeOrdinaryReadySelection(ctx context.Context, p *state.PendingPublication) error {
	cfg, found, err := config.Load(s.local.Home())
	if err != nil {
		return err
	}
	if !found || !cfg.PublicationCompositionProtection {
		return nil
	}
	staged := s.importStageActive() && cfg.StagedSelectionProtection
	if staged {
		if !cfg.SupportsStagedSelection() {
			return state.ErrReadySelectionFrozen
		}
		if authority, ok := s.remote.(interface{ CatalogMetadataAuthority() bool }); ok && authority.CatalogMetadataAuthority() {
			return state.ErrReadySelectionFrozen
		}
	}
	if p.History != nil || p.Bundle.History != nil || p.MetadataOnly || s.reg.HistoricalProject != nil || s.importStageActive() && !staged {
		return state.ErrReadySelectionFrozen
	}
	_, found, err = s.published.LastPublishedMetadata()
	if err != nil {
		return err
	}
	predecessor := state.ReadyPredecessorUnknown
	var raw []byte
	if found {
		predecessor = state.ReadyPredecessorPresent
		raw = s.published.Metadata()
	} else {
		_, e := s.historyGet(p.MetadataKey, historyMetadataLimit)
		if errors.Is(e, storage.ErrNotFound) {
			predecessor = state.ReadyPredecessorAbsent
		} else if e != nil {
			return e
		}
	}
	var next state.PendingPublication
	if staged {
		next, err = s.local.FreezeStagedSelection(ctx, s.id(), *p, predecessor, raw)
	} else {
		next, err = s.local.FreezeReadySelection(ctx, s.id(), *p, predecessor, raw)
	}
	if err != nil {
		return err
	}
	*p = next
	return nil
}

// checkReadySelectionRemote never converts an unknown predecessor to absence.
func (s *sessionScan) checkReadySelectionRemote(p state.PendingPublication) (bool, error) {
	proof := p.Selection
	if p.StagedProof() != nil {
		proof = &p.StagedProof().Core
	}
	if proof == nil {
		return false, state.ErrReadySelectionFrozen
	}
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		if proof.Predecessor == state.ReadyPredecessorAbsent {
			return false, nil
		}
		return false, errHistoryMetadataConflict
	}
	if err != nil {
		return false, err
	}
	digest := metadataSHA(raw)
	if digest == proof.MetadataSHA256 {
		return true, nil
	}
	if proof.Predecessor == state.ReadyPredecessorPresent && digest == proof.PredecessorSHA256 {
		return false, nil
	}
	return false, errHistoryMetadataConflict
}

func (s *sessionScan) uploadReadySelection(p state.PendingPublication) error {
	selected, err := s.checkReadySelectionRemote(p)
	if err != nil {
		return err
	}
	if selected {
		return s.verifyImportPublication(p)
	}
	if err := s.ensureHistorySource(p.SourceKey, p.SourceSHA256, p.SourceBytes); err != nil {
		return err
	}
	selected, err = s.checkReadySelectionRemote(p)
	if err != nil {
		return err
	}
	if selected {
		return s.verifyImportPublication(p)
	}
	return s.publicationRemote(p).Put(s.ctx, p.MetadataKey, p.MetadataBytes)
}

func (s *sessionScan) releaseTypedStage() error {
	remote, ok := s.remote.(state.StagedReadback)
	if !ok {
		return state.ErrReadySelectionFrozen
	}
	return s.local.VerifyAndBeginStagedRelease(s.ctx, s.id(), remote, s.opts.CollectorGuard)
}
