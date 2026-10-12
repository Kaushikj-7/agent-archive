package agentapi

import "testing"

// A prior owner's release must not free the current session's ceiling.
func TestScopePriorReleasePreservesCurrentCharges(t *testing.T) {
	b := NewNativeReadBudget(128)
	prior := b.Reservation()
	if !prior.Reserve(8) {
		t.Fatal("prior owner reservation refused")
	}
	end := b.BeginScope(10)
	defer end()
	current := b.Reservation()
	if !current.Reserve(10) {
		t.Fatal("current owner's ceiling refused")
	}
	prior.Release(8)
	if current.Reserve(1) {
		t.Fatal("prior owner release let the current session exceed its ceiling")
	}
	if !b.ScopeExceeded() {
		t.Fatal("own ceiling refusal was not recorded")
	}
	current.Release(10)
	used, _ := b.Charged()
	if used != 0 {
		t.Fatalf("reservation leaked: %d", used)
	}
}

func TestReservationReleaseFreesOnlyItsOwnerAcrossNestedScopes(t *testing.T) {
	b := NewNativeReadBudget(128)
	outer := b.BeginScope(10)
	earlier := b.Reservation()
	if !earlier.Reserve(7) {
		t.Fatal("outer reserve")
	}
	inner := b.BeginScope(10)
	current := b.Reservation()
	if !current.Reserve(10) {
		t.Fatal("inner reserve")
	}
	earlier.Release(4)
	if b.Available() != 0 || current.Reserve(1) {
		t.Fatal("outer release lowered inner charges")
	}
	current.Release(3)
	if b.Available() != 3 || !current.Reserve(3) {
		t.Fatal("current release did not free current owner")
	}
	inner()
	if b.Available() != 7 {
		t.Fatal("outer charges not preserved", b.Available())
	}
	// Cleanup of an ended scope cannot lower the restored outer scope.
	current.Release(10)
	if b.Available() != 7 || !earlier.Reserve(7) || earlier.Reserve(1) {
		t.Fatal("ended inner scope released outer charges")
	}
	outer()
	laterEnd := b.BeginScope(10)
	later := b.Reservation()
	if !later.Reserve(10) {
		t.Fatal("later reserve")
	}
	earlier.Release(10)
	if b.Available() != 0 || later.Reserve(1) {
		t.Fatal("ended outer scope released later charges")
	}
	later.Release(10)
	laterEnd()
	if used, _ := b.Charged(); used != 0 {
		t.Fatal("leaked", used)
	}
}

func TestReservationGrowthKeepsOriginalOwner(t *testing.T) {
	b := NewNativeReadBudget(30)
	outer := b.BeginScope(10)
	original := b.Reservation()
	if !original.Reserve(6) {
		t.Fatal("original reserve")
	}
	inner := b.BeginScope(5)
	current := b.Reservation()
	if !current.Reserve(5) || !original.Reserve(4) {
		t.Fatal("growth used another owner's ceiling")
	}
	if original.Reserve(1) {
		t.Fatal("growth exceeded original owner's ceiling")
	}
	if b.ScopeExceeded() {
		t.Fatal("original refusal attributed to inner scope")
	}
	original.Release(10)
	if b.Available() != 0 {
		t.Fatal("growth release lowered current charges")
	}
	current.Release(5)
	inner()
	if !b.ScopeExceeded() {
		t.Fatal("original refusal lost")
	}
	outer()
	if used, _ := b.Charged(); used != 0 {
		t.Fatal("leaked", used)
	}
}

func TestReservationOverReleaseCannotConsumeAnotherOwner(t *testing.T) {
	b := NewNativeReadBudget(30)
	first, other := b.Reservation(), b.Reservation()
	if !first.Reserve(8) || !other.Reserve(10) {
		t.Fatal("reserve")
	}
	copied := *first // Copies share the private owner; they cannot mint charges.
	first.Release(8)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("copied handle over-release did not panic")
			}
		}()
		copied.Release(1)
	}()
	if used, _ := b.Charged(); used != 10 {
		t.Fatal("panic changed other owner's charges", used)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("unscoped release consumed scoped charges")
			}
		}()
		b.ReleaseUnscoped(1)
	}()
	other.Release(10)
	if used, _ := b.Charged(); used != 0 {
		t.Fatal("leaked", used)
	}
}

func TestReservationPreflightUsesOriginalScope(t *testing.T) {
	b := NewNativeReadBudget(30)
	outer := b.BeginScope(10)
	original := b.Reservation()
	if !original.Reserve(6) {
		t.Fatal("original reserve")
	}
	inner := b.BeginScope(5)
	current := b.Reservation()
	if !current.Reserve(5) {
		t.Fatal("current reserve")
	}
	if original.Available() != 4 || current.Available() != 0 {
		t.Fatal("preflight used the wrong scope")
	}
	original.Refuse(5)
	if b.ScopeExceeded() || b.ScopePressured() {
		t.Fatal("original preflight marked current scope")
	}
	current.Release(5)
	inner()
	if !b.ScopeExceeded() || b.ScopePressured() {
		t.Fatal("original preflight refusal classification lost")
	}
	original.Release(6)
	outer()
	if used, _ := b.Charged(); used != 0 {
		t.Fatal("leaked", used)
	}
}
