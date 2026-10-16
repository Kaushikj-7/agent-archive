package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func scratchStore(t *testing.T) (*Store, *agentapi.NativeReadBudget) {
	t.Helper()
	s := newTestStore(t)
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	scoped, closeScoped := s.WithReadBudget(t.Context(), budget)
	t.Cleanup(closeScoped)
	return scoped, budget
}

func TestCursorScratchReservesBeforeCreateAndRetainsQuotaUntilCleanup(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	r, err := s.NewCursorScratch(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := r.OpenWorkspace(); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created before OpenWorkspace", err)
	}
	root, err := r.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	f, err := root.OpenFile("native.db", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(f.Close(), root.Close()); err != nil {
		t.Fatal(err)
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { _ = q.Close() }()
		_, e = q.usage()
		return e
	})
	if !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("scratch was not charged conservatively", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { _ = q.Close() }()
		_, e = q.usage()
		return e
	})
	if err != nil {
		t.Fatal("cleanup retained charge", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorScratchCapacityRefusesBeforeReceiptOrWorkspace(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	sparseDurable(t, filepath.Join(s.home, "pending", "other.json"), durableStorageQuota/2-1024)
	r, err := s.NewCursorScratch(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = r.Reserve(4096); !errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatal(err)
	}
	for _, path := range []string{r.Root(), filepath.Join(s.home, r.receiptPath())} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused reservation created data", path, err)
		}
	}
}

func TestCursorScratchUnknownChildRetainsReceiptAndAllowsCleanupRetry(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	r, err := s.NewCursorScratch(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	root, err := r.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.Root(), "unexpected"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = root.Close(); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.home, r.receiptPath())); err != nil {
		t.Fatal("receipt lost on failed cleanup", err)
	}
	if err = os.Remove(filepath.Join(r.Root(), "unexpected")); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorScratchRecoveryRefusesActiveOwnerThenCleansAbandoned(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	r, err := s.NewCursorScratch(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCursorScratch(t.Context()); !errors.Is(err, local.ErrBusy) {
		t.Fatal("active owner removed", err)
	}
	// Model process loss: release its OS lock and close its held home without cleanup.
	r.unlock()
	r.unlock = nil
	if err = r.home.Close(); err != nil {
		t.Fatal(err)
	}
	r.reservation.Release(cursorScratchControl)
	r.closed = true
	if err = s.RecoverCursorScratch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.home, r.receiptPath())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestCursorScratchCancellationDoesNotPreventOwnedCleanup(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	r, err := s.NewCursorScratch(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	root, err := r.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = r.Err(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorScratchReceiptRefusesAmbiguousOrFutureControl(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"version":1,"version":1,"owner":"cursor-live-wal","key":"c","token":"x","bytes":4096}`,
		`{"version":1,"owner":"cursor-live-wal","key":"c","token":"x","bytes":4096,"future":true}`,
		`{"version":1,"owner":"cursor-live-wal","key":"c","token":"x","bytes":4096} {}`,
		`{"version":1,"owner":"cursor-live-wal","key":"c","token":"x"}`,
	} {
		if _, ok := decodeCursorScratchReceipt([]byte(raw)); ok {
			t.Fatal("ambiguous control accepted", raw)
		}
	}
}

func TestCursorScratchRecoveryPreservesUnrecognizedContents(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	r, err := s.NewCursorScratch(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	root, err := r.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(r.Root(), "native.db")); err != nil {
		t.Fatal(err)
	}
	r.unlock()
	r.unlock = nil
	if err = r.home.Close(); err != nil {
		t.Fatal(err)
	}
	r.reservation.Release(cursorScratchControl)
	r.closed = true
	if err = s.RecoverCursorScratch(t.Context()); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "preserve" {
		t.Fatal(string(raw), err)
	}
	if _, err = os.Stat(filepath.Join(s.home, r.receiptPath())); err != nil {
		t.Fatal("receipt lost", err)
	}
}

func TestCursorScratchReleasesCapturedScopeAcrossLaterSession(t *testing.T) {
	t.Parallel()
	s, budget := scratchStore(t)
	end := budget.BeginScope(cursorScratchControl)
	r, err := s.NewCursorScratch(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	end()
	laterEnd := budget.BeginScope(cursorScratchControl)
	defer laterEnd()
	later := budget.Reservation()
	if !later.Reserve(cursorScratchControl) {
		t.Fatal("later session could not reserve")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the earlier owner must not free the later session's full ceiling.
	if later.Reserve(1) {
		t.Fatal("earlier cleanup released a later session charge")
	}
	later.Release(cursorScratchControl)
}

func TestCursorScratchRecoveryPreservesNoncanonicalToken(t *testing.T) {
	t.Parallel()
	s, _ := scratchStore(t)
	token := "foreign-workspace"
	scratch := filepath.Join(s.home, string(scratchDurableRoot), token)
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(scratch, "native.db")
	if err := os.WriteFile(native, []byte("foreign data"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt := cursorScratchReceipt{Version: 1, Owner: "cursor-live-wal", Key: "synthetic", Token: token, Bytes: 4096}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, string(reservationDurableRoot), token+".json")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCursorScratch(t.Context()); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(native); err != nil || string(got) != "foreign data" {
		t.Fatal("unrelated workspace removed", string(got), err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatal("unknown receipt changed", string(got), err)
	}
}

func TestCursorScratchReceiptUsesExactMintedTokenLanguage(t *testing.T) {
	t.Parallel()
	good := "0123456789abcdef0123456789abcdef"
	if !canonicalCursorScratchToken(good) {
		t.Fatal("minted token refused")
	}
	for _, bad := range []string{"", good[:31], good + "0", "0123456789ABCDEF0123456789ABCDEF", "../" + good, good[:31] + "g"} {
		if canonicalCursorScratchToken(bad) {
			t.Fatal("noncanonical token", bad)
		}
	}
	if _, ok := decodeCursorScratchReceipt(make([]byte, 4097)); ok {
		t.Fatal("overcap control accepted")
	}
}
