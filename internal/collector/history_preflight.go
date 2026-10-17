package collector

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (s *sessionScan) checkHistoryPublication(p state.PendingPublication) error {
	if err := s.checkHistoryPublicationLocal(p); err != nil {
		return err
	}
	if p.History == nil && s.reg.Harness.Name != "codex" {
		return nil
	}
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		return s.checkHistoryPublicationBody(p, nil)
	}
	if err != nil {
		return err
	}
	return s.checkHistoryPublicationBody(p, raw)
}
