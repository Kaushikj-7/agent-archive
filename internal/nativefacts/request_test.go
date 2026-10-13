package nativefacts

import (
	"context"
	"errors"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func requestMechanismFixture(t *testing.T, budget *agentapi.NativeReadBudget) *CaptureRequest {
	t.Helper()
	r, err := FreezeCaptureRequest(t.Context(), archive.SessionRegistration{ArchiveSessionID: "mechanism", NativeSessionID: "native"}, agentapi.DatabaseChat{}, "/qualified/state.vscdb", time.Unix(1, 0).UTC(), CapturePolicy{FilterVersion: archive.FilterVersion}, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestCaptureRequestCopiedLeaseClosesExactlyOnce(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	request := requestMechanismFixture(t, budget)
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	alias := *lease
	request.Close()
	lease.Close()
	alias.Close()
	if budget.Available() != 128<<20 {
		t.Fatal("copied lease lost accounting")
	}
	if _, err = request.Borrow(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("closed request borrowed", err)
	}
}

func TestCaptureRequestCannotReleaseAnotherHandlesCharge(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	request := requestMechanismFixture(t, budget)
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	first, second := lease.Reservation(), lease.Reservation()
	if !first.Reserve(64 << 10) {
		t.Fatal("adequate scoped reservation refused")
	}
	available := budget.Available()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("unreserved handle released foreign charge")
			}
		}()
		second.Release(1)
	}()
	if budget.Available() != available {
		t.Fatal("charge stolen")
	}
	alias := *first
	first.Release(64 << 10)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("copied scope released twice")
			}
		}()
		alias.Release(1)
	}()
}

func TestCaptureRequestClosePinsViewsUntilRelease(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	request := requestMechanismFixture(t, budget)
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	scope := lease.Reservation()
	if !scope.Reserve(64 << 10) {
		t.Fatal("reserve")
	}
	held := budget.Available()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); request.Close() }()
	wg.Wait()
	if budget.Available() != held {
		t.Fatal("request close released borrowed view")
	}
	lease.Close()
	if scope.Reserve(1) {
		t.Fatal("closed lease grew")
	}
	if budget.Available() != held {
		t.Fatal("lease close released live scoped view")
	}
	scope.Release(64 << 10)
	if budget.Available() != 128<<20 {
		t.Fatal("last view did not release descriptor")
	}
}

func TestCaptureRequestUsesOriginalScopeAfterBeginScope(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	end := budget.BeginScope(512 << 10)
	request := requestMechanismFixture(t, budget)
	end()
	endTight := budget.BeginScope(512)
	defer endTight()
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal("descriptor recaptured tight ambient scope", err)
	}
	defer lease.Close()
	scope := lease.Reservation()
	if !scope.Reserve(64 << 10) {
		t.Fatal("fixed scope recaptured")
	}
	scope.Release(64 << 10)
	fresh := budget.Reservation()
	if fresh.Reserve(64 << 10) {
		fresh.Release(64 << 10)
		t.Fatal("ambient pressure control invalid")
	}
}

func TestCaptureRequestCancelsActiveOriginalContextOnClose(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	request := requestMechanismFixture(t, budget)
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	request.Close()
	if !errors.Is(context.Cause(lease.Context()), agentapi.ErrClosed) {
		t.Fatal("close did not revoke capture context")
	}
	if !errors.Is(lease.Check(), agentapi.ErrClosed) {
		t.Fatal("closed descriptor remained usable")
	}
}

func TestCaptureRequestKeepsOwnerBoundWithoutCombinedDescriptorCap(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	reg := archive.SessionRegistration{ArchiveSessionID: "mechanism", NativeSessionID: "native", RepoKey: strings.Repeat("r", 63000)}
	reviewed := agentapi.DatabaseChat{Folder: strings.Repeat("f", 4000)}
	request, err := FreezeCaptureRequest(t.Context(), reg, reviewed, "/qualified/state.vscdb", time.Unix(1, 0).UTC(), CapturePolicy{FilterVersion: archive.FilterVersion}, budget)
	if err != nil {
		t.Fatal("qualified owner refused because complete descriptor exceeds64KiB", err)
	}
	lease, err := request.Borrow()
	if err != nil {
		request.Close()
		t.Fatal(err)
	}
	snapshot, err := lease.Registration()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RepoKey != reg.RepoKey {
		t.Fatal("owner snapshot changed")
	}
	lease.Close()
	request.Close()
	if budget.Available() != 128<<20 {
		t.Fatal("descriptor leaked")
	}
	reg.RepoKey = strings.Repeat("r", 66000)
	if request, err = FreezeCaptureRequest(t.Context(), reg, reviewed, "/qualified/state.vscdb", time.Unix(1, 0).UTC(), CapturePolicy{FilterVersion: archive.FilterVersion}, budget); err == nil {
		request.Close()
		t.Fatal("old owner bound widened")
	}
}

func TestCaptureRequestDetachesMutableRegistration(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	resolution := &archive.ProjectResolution{Root: "/before"}
	reg := archive.SessionRegistration{ArchiveSessionID: "mechanism", NativeSessionID: "native", ProjectResolution: resolution}
	request, err := FreezeCaptureRequest(t.Context(), reg, agentapi.DatabaseChat{}, "/qualified/state.vscdb", time.Unix(1, 0).UTC(), CapturePolicy{FilterVersion: archive.FilterVersion}, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer request.Close()
	resolution.Root = "/after"
	first, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstSnapshot, err := first.Registration()
	if err != nil {
		t.Fatal(err)
	}
	if firstSnapshot.ProjectResolution.Root != "/before" {
		t.Fatal("host mutation replaced frozen owner")
	}
	firstSnapshot.ProjectResolution.Root = "/borrower"
	again, err := first.Registration()
	if err != nil {
		t.Fatal(err)
	}
	if again.ProjectResolution.Root != "/before" {
		t.Fatal("returned getter mutated private lease snapshot")
	}
	second, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondSnapshot, err := second.Registration()
	if err != nil {
		t.Fatal(err)
	}
	if secondSnapshot.ProjectResolution.Root != "/before" {
		t.Fatal("borrower mutated sealed descriptor")
	}
}

func TestCaptureRequestCopiedLeaseGettersConcurrentClose(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(128 << 20)
	request := requestMechanismFixture(t, budget)
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	alias := *lease
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 256 {
			_, _ = alias.Registration()
			_ = alias.Reviewed()
			_ = alias.Locator()
			_ = alias.At()
			_ = alias.Policy()
		}
	}()
	go func() { defer wg.Done(); lease.Close() }()
	wg.Wait()
	request.Close()
	if budget.Available() != 128<<20 {
		t.Fatal("concurrent copied lease close leaked")
	}
}
