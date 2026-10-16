package agentapi

import "testing"

func TestCapturedReadBudgetKeepsOriginalScopeAndSeparateOwners(t *testing.T) {
	b := NewNativeReadBudget(100)
	end := b.BeginScope(20)
	original := b.Reservation()
	captured, err := original.CapturedBudget()
	if err != nil {
		t.Fatal(err)
	}
	end()
	endOther := b.BeginScope(90)
	defer endOther()
	a, c := captured.Reservation(), captured.Reservation()
	if !a.Reserve(8) || !c.Reserve(9) {
		t.Fatal("original scope refused available work")
	}
	if captured.Available() != 3 {
		t.Fatal("scope changed", captured.Available())
	}
	if c.Reserve(4) || !captured.ScopeExceeded() || b.ScopeExceeded() {
		t.Fatal("refusal escaped captured owner")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("one helper released another helper's charge")
			}
		}()
		a.Release(9)
	}()
	if used, _ := b.Charged(); used != 17 {
		t.Fatal("failed release changed ledger", used)
	}
	a.Release(8)
	c.Release(9)
	if used, peak := captured.Charged(); used != 0 || peak != 17 {
		t.Fatal("captured facade invented a ledger", used, peak)
	}
	captured.ForgetScopeExceeded()
	if !captured.ScopeExceeded() {
		t.Fatal("facade cleared original refusal")
	}
}

func TestCapturedReadBudgetCannotEscapeThroughUnscopedOrNewScope(t *testing.T) {
	b := NewNativeReadBudget(100)
	end := b.BeginScope(1)
	defer end()
	captured, err := b.Reservation().CapturedBudget()
	if err != nil {
		t.Fatal(err)
	}
	if captured.ReserveUnscoped(2) || captured.ReserveUnscopedRequired(2) {
		t.Fatal("captured facade obtained unscoped credit")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("captured facade opened a new scope")
			}
		}()
		captured.BeginScope(100)
	}()
	if captured.Reservation().Reserve(2) {
		t.Fatal("scope limit increased")
	}
	if used, _ := b.Charged(); used != 0 {
		t.Fatal("unsupported operation changed charges", used)
	}
}

func TestCapturedReadBudgetAttributesSharedPressureToOriginalScope(t *testing.T) {
	b := NewNativeReadBudget(20)
	end := b.BeginScope(100)
	captured, err := b.Reservation().CapturedBudget()
	if err != nil {
		t.Fatal(err)
	}
	end()
	endOther := b.BeginScope(100)
	defer endOther()
	if !b.ReserveUnscoped(19) {
		t.Fatal("fixture shared charge")
	}
	defer b.ReleaseUnscoped(19)
	if captured.Reservation().Reserve(2) || !captured.ScopePressured() || captured.ScopeExceeded() || b.ScopePressured() {
		t.Fatal("shared pressure attributed to wrong scope")
	}
}

func TestCapturedReadBudgetRefusesUnownedFacade(t *testing.T) {
	for _, reservation := range []*NativeReadReservation{nil, {}, (*NativeReadBudget)(nil).Reservation()} {
		if b, err := reservation.CapturedBudget(); err == nil || b != nil {
			t.Fatal("unowned facade obtained unlimited budget")
		}
	}
}
