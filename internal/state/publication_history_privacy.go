package state

// SettleHistoryPublicationPrivacy releases original evidence only after a
// complete history privacy successor is selected in durable published state.
// The collector has recorded retirement and consumed exact source-set readback;
// shared settlement rechecks the selected commit and original-evidence link.
func (s *Store) SettleHistoryPublicationPrivacy(id string, p PendingPublication) error {
	if p.JournalVersion != 5 || p.Phase != PublicationReady || p.History == nil || p.History.Preparing || p.History.MaintenanceOwed || p.Commit == nil || p.Commit.Purpose != PublicationPrivacyRewrite {
		return ErrDurableStorageRecovery
	}
	if err := p.ValidateHistoryBudgeted(id, s.resourceBudget); err != nil {
		return err
	}
	return s.settlePublicationEvidence(id, p, true)
}
