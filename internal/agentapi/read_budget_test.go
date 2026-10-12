package agentapi

import (
	"errors"
	"testing"
)

func TestReadScopeCeilingIsTheOwnersOwn(t *testing.T) {
	t.Parallel()
	b := NewNativeReadBudget(100)
	end := b.BeginScope(40)
	if !b.Reserve(30) || b.ScopeExceeded() {
		t.Fatal("within the scope")
	}
	if got := b.Available(); got != 10 {
		t.Fatalf("available %d, want the scope's 10", got)
	}
	if b.Reserve(11) || !b.ScopeExceeded() {
		t.Fatal("over the scope's ceiling must refuse and say so")
	}
	if used, _ := b.Charged(); used != 30 {
		t.Fatalf("a refusal charged: %d", used)
	}
	b.Release(30)
	end()
	if b.ScopeExceeded() {
		t.Fatal("an ended scope is still reported")
	}
	if !b.Reserve(90) {
		t.Fatal("without a scope the shared limit alone applies")
	}
	b.Release(90)
}

func TestReadScopeTellsSharedPressureFromItsOwn(t *testing.T) {
	t.Parallel()
	b := NewNativeReadBudget(100)
	if !b.Reserve(80) { // held by the pass, before the scope began
		t.Fatal("reserve")
	}
	end := b.BeginScope(50)
	defer end()
	if b.Reserve(30) || b.ScopeExceeded() {
		t.Fatal("a refusal by the shared limit is not the scope's own")
	}
	if got := b.Available(); got != 20 {
		t.Fatalf("available %d, want the shared 20", got)
	}
	b.Refuse(21)
	if b.ScopeExceeded() {
		t.Fatal("a refused request within the scope's ceiling marked it")
	}
	// The pass's earlier charge ending inside the scope never makes the
	// scope's own count negative.
	b.Release(80)
	if !b.Reserve(50) || b.Reserve(1) || !b.ScopeExceeded() {
		t.Fatal("scope ceiling after an earlier owner released")
	}
	b.Release(50)
}

func TestReadScopeRefuseMarksOnlyPastTheCeiling(t *testing.T) {
	t.Parallel()
	b := NewNativeReadBudget(100)
	end := b.BeginScope(40)
	defer end()
	b.Refuse(40)
	if b.ScopeExceeded() {
		t.Fatal("a request that fits was marked")
	}
	b.Refuse(41)
	if !b.ScopeExceeded() {
		t.Fatal("a request past the ceiling was not marked")
	}
}

func TestNestedReadScopeRestores(t *testing.T) {
	t.Parallel()
	b := NewNativeReadBudget(100)
	outer := b.BeginScope(60)
	inner := b.BeginScope(10)
	if b.Reserve(11) || !b.ScopeExceeded() {
		t.Fatal("inner ceiling")
	}
	inner()
	if b.ScopeExceeded() || !b.Reserve(50) {
		t.Fatal("outer scope not restored")
	}
	b.Release(50)
	outer()
}

func TestRawLimitErrorCarriesSizes(t *testing.T) {
	t.Parallel()
	o := SourceObservation{Present: true, Size: 7}
	err := RawLimit(7, 5, &o)
	var raw *RawLimitError
	if !errors.Is(err, ErrRawLimit) || !errors.As(err, &raw) || raw.Size != 7 || raw.Limit != 5 || Failure(err) != Limit {
		t.Fatalf("%v", err)
	}
	if got, ok := ErrorObservation(err); !ok || got.Size != 7 {
		t.Fatal("observation lost")
	}
}

func TestUnscopedCacheIsNoSessionsCharge(t *testing.T) {
	t.Parallel()
	b := NewNativeReadBudget(100)
	end := b.BeginScope(40)
	defer end()
	if !b.ReserveUnscoped(50) || b.Available() != 40 {
		t.Fatal("a pass cache counted toward the scope", b.Available())
	}
	if !b.Reserve(30) {
		t.Fatal("reserve")
	}
	// Releasing the cache does not lower the scope's own count.
	b.ReleaseUnscoped(50)
	if b.Reserve(11) || !b.ScopeExceeded() {
		t.Fatal("the cache release lowered the session's count")
	}
	b.Release(30)
	other := NewNativeReadBudget(100)
	endOther := other.BeginScope(40)
	defer endOther()
	if other.ReserveUnscoped(101) || other.ScopeExceeded() || other.ScopePressured() {
		t.Fatal("a refused cache was charged to the session")
	}
}

func TestScopePressuredIsSharedRefusalOnly(t *testing.T) {
	t.Parallel()
	b := NewNativeReadBudget(100)
	if !b.ReserveUnscoped(80) {
		t.Fatal("reserve")
	}
	end := b.BeginScope(50)
	defer end()
	if b.ScopePressured() {
		t.Fatal("pressured before any refusal")
	}
	if b.Reserve(21) || !b.ScopePressured() || b.ScopeExceeded() {
		t.Fatal("a shared refusal was not pressure")
	}
	b.ReleaseUnscoped(80)
}
