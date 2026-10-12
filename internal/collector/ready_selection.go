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
	if p.History != nil || p.Bundle.History != nil || p.MetadataOnly || s.reg.HistoricalProject != nil || s.reg.StagedImportSHA256 != "" {
		return state.ErrReadySelectionFrozen
	}
	_, found, err := s.published.LastPublishedMetadata()
	if err != nil {
		return err
	}
	predecessor := "unknown"
	var raw []byte
	if found {
		predecessor = "present"
		raw = s.published.Metadata()
	} else {
		_, e := s.historyGet(p.MetadataKey, historyMetadataLimit)
		if errors.Is(e, storage.ErrNotFound) {
			predecessor = "absent"
		} else if e != nil {
			return e
		}
	}
	next, err := s.local.FreezeReadySelection(ctx, s.id(), *p, predecessor, raw)
	if err != nil {
		return err
	}
	*p = next
	return nil
}

// checkReadySelectionRemote never converts an unknown predecessor to absence.
func (s *sessionScan) checkReadySelectionRemote(p state.PendingPublication) (bool, error) {
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		if p.Selection.Predecessor == "absent" {
			return false, nil
		}
		return false, errHistoryMetadataConflict
	}
	if err != nil {
		return false, err
	}
	digest := metadataSHA(raw)
	if digest == p.Selection.MetadataSHA256 {
		return true, nil
	}
	if p.Selection.Predecessor == "present" && digest == p.Selection.PredecessorSHA256 {
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
