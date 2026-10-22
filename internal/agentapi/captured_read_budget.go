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

// CaptureFromReservation borrows an existing scoped owner only when it belongs
// to this actual ledger. A pinned facade additionally requires its exact scope;
// neither a foreign owner nor the receiver's later ambient scope supplies credit.
func (b *NativeReadBudget) CaptureFromReservation(reservation *NativeReadReservation) (*NativeReadBudget, error) {
	if b == nil || reservation == nil || reservation.owner == nil || reservation.owner.budget == nil || reservation.owner.scope == nil {
		return nil, ErrReadBudget
	}
	actual := b
	if b.captured != nil {
		actual = b.captured.budget
		if reservation.owner.scope != b.captured.scope {
			return nil, ErrReadBudget
		}
	}
	if reservation.owner.budget != actual {
		return nil, ErrReadBudget
	}
	return reservation.CapturedBudget()
}
