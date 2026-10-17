package collector

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// importStageActive distinguishes selected payload from retained attribution.
// A historical stage digest survives release. Every retained entrypoint must
// independently validate its durable release and selecting lineage below.
func (s *sessionScan) importStageActive() bool {
	return s.reg.StagedImportSHA256 != "" && !s.releasedHistoricalImport
}

func (s *sessionScan) validateNativeImportRetained() error {
	s.releasedHistoricalImport = false
	if s.reg.NativeImport != nil && s.hasRetainedPublication() {
		active, err := s.published.NativeImportStageActive(s.ctx, s.reg)
		if err != nil {
			return err
		}
		s.releasedHistoricalImport = !active
		return nil
	}
	return nil
}

func (p *pass) activeNativeImportStage(reg archive.SessionRegistration) (bool, error) {
	var budget *agentapi.NativeReadBudget
	if p.opts.sourcePasses != nil {
		budget = p.opts.sourcePasses.env.ReadBudget
	}
	read, closeRead := p.local.WithReadBudget(p.ctx, budget)
	defer closeRead()
	return read.NativeImportStageActive(p.ctx, reg)
}
