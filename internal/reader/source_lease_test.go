package reader

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestRetainedLeasePressureReleaseCancellationAndCorruption(t *testing.T) {
	metadata, want, store := fixture(t)
	data, err := store.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(256 << 10)
	reservation := budget.Reservation()
	pressure := int64(224 << 10)
	if !reservation.Reserve(pressure) {
		t.Fatal("reserve")
	}
	_, release, err := DecodeReferencedSourceLeased(t.Context(), metadata, data, Limits{}, budget)
	if !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("pressure refusal %v", err)
	}
	release()
	used, _ := budget.Charged()
	if used != pressure {
		t.Fatalf("refusal leaked %d", used)
	}
	reservation.Release(pressure)
	bundle, release, err := DecodeReferencedSourceLeased(t.Context(), metadata, data, Limits{}, budget)
	if err != nil || bundle.NativeSessionID != want.NativeSessionID {
		t.Fatalf("small useful progress %#v %v", bundle, err)
	}
	used, peak := budget.Charged()
	if used <= 0 || peak > 256<<10 {
		t.Fatalf("lease charges %d/%d", used, peak)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(release)
	}
	wg.Wait()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("release leaked %d", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, release, err = DecodeReferencedSourceLeased(ctx, metadata, data, Limits{}, budget)
	release()
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, release, err = DecodeReferencedSourceLeased(t.Context(), metadata, []byte("corrupt"), Limits{}, budget)
	release()
	if err == nil {
		t.Fatal("accepted corruption")
	}
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("failure leaked %d", used)
	}
}

// Decoded output can outlive the session that created it. Releasing its lease
// while another session is full must leave that session's ceiling unchanged.
func TestRetainedSourcePriorLeaseReleasePreservesCurrentScope(t *testing.T) {
	metadata, _, store := fixture(t)
	data, err := store.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(256 << 10)
	_, release, err := DecodeReferencedSourceLeased(t.Context(), metadata, data, Limits{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	priorUsed, _ := budget.Charged()
	if priorUsed <= 0 {
		t.Fatal("fixture holds no decoded lease")
	}
	end := budget.BeginScope(10)
	defer end()
	current := budget.Reservation()
	if !current.Reserve(10) {
		t.Fatal("current reserve")
	}
	release()
	release()
	if current.Reserve(1) || !budget.ScopeExceeded() {
		t.Fatal("prior decoded lease freed the current owner's ceiling")
	}
	current.Release(10)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("leases leaked", used)
	}
}
