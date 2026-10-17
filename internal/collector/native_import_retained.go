package collector

// importStageActive distinguishes selected payload from retained attribution.
// A historical stage digest survives release. Every retained entrypoint must
// independently validate its durable release and selecting lineage below.
func (s *sessionScan) importStageActive() bool {
	return s.reg.StagedImportSHA256 != "" && !s.releasedHistoricalImport
}

func (s *sessionScan) validateNativeImportRetained() error {
	s.releasedHistoricalImport = false
	if s.reg.NativeImport != nil && s.hasRetainedPublication() {
		active, err := s.published.NativeImportStageActive(s.reg)
		if err != nil {
			return err
		}
		s.releasedHistoricalImport = !active
		return nil
	}
	return nil
}
