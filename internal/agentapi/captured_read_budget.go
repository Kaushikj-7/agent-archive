package agentapi

// CapturedBudget lends existing bounded helpers this reservation's original
// ledger and bounded nonnil scope. Each Reservation owns independent charges on that same
// scope. The facade creates no limit or credit, cannot open another scope or
// charge unscoped work, and cannot clear the original refusal diagnostics.
func (reservation *NativeReadReservation) CapturedBudget() (*NativeReadBudget, error) {
	if reservation == nil || reservation.owner == nil || reservation.owner.budget == nil || reservation.owner.scope == nil {
		return nil, ErrReadBudget
	}
	return &NativeReadBudget{captured: reservation.owner}, nil
}
