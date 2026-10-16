package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

type pendingOrderLookup struct {
	order   []string
	locates int
	closes  int
}

func (l *pendingOrderLookup) PendingOrder() []string { return l.order }

func (l *pendingOrderLookup) Locate(context.Context, agentapi.LocatorRequest) (agentapi.LocatorResult, error) {
	l.locates++
	return agentapi.LocatorResult{Status: agentapi.LocatorPending}, nil
}

func (*pendingOrderLookup) Check(context.Context, string, string) error {
	return errors.New("no selection")
}

func nativePendingRegistration(i int) archive.SessionRegistration {
	return archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("pending-%03d", i), NativeSessionID: fmt.Sprintf("00000000-0000-7000-8000-%012x", i+1), Harness: archive.Harness{Name: "muse"}}
}

func TestNativePendingOrderPreservesOtherSlotsAndBorrowsNoAuthority(t *testing.T) {
	t.Parallel()
	root := archive.NativeRoot{Agent: "muse", Configured: "/synthetic", Canonical: "/synthetic", Revision: "one"}
	lookup := &pendingOrderLookup{}
	for i := 256; i >= 0; i-- {
		lookup.order = append(lookup.order, nativePendingRegistration(i).NativeSessionID)
	}
	opts := Options{NativeRoots: []archive.NativeRoot{root}, NativeLocators: lookup, NativeLocatorFactory: func(context.Context, NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		panic("scheduling never invokes factory")
	}}
	p := pass{opts: opts}
	registrations := []archive.SessionRegistration{}
	for i := range 257 {
		registrations = append(registrations, nativePendingRegistration(i))
	}
	bound := nativePendingRegistration(500)
	bound.TranscriptPath = "/synthetic/bound"
	bound.NativeBinding = &archive.NativeSourceBinding{Path: bound.TranscriptPath}
	frozen := nativePendingRegistration(501)
	frozen.CaptureFrozen = true
	imported := nativePendingRegistration(502)
	imported.Origin = archive.SessionOriginImport
	registrations = append(registrations[:100], append([]archive.SessionRegistration{bound, frozen, imported}, registrations[100:]...)...)
	before := append([]archive.SessionRegistration(nil), registrations...)
	p.orderPendingNative(registrations)
	for _, index := range []int{100, 101, 102} {
		if registrations[index].ArchiveSessionID != before[index].ArchiveSessionID {
			t.Fatal("nonpending slot moved", index)
		}
	}
	expected := 256
	for _, reg := range registrations {
		if !pendingNativeEligible(reg, opts) {
			continue
		}
		if reg.NativeSessionID != nativePendingRegistration(expected).NativeSessionID {
			t.Fatal("pending order", reg.NativeSessionID, expected)
		}
		expected--
	}
	reader, ok := newSourceReader(registrations[0], opts)
	if !ok {
		t.Fatal("pending unavailable")
	}
	if lookup.locates != 0 {
		t.Fatal("constructor did I/O")
	}
	if _, err := reader.Signature(t.Context()); agentapi.Failure(err) != agentapi.Unavailable || lookup.locates != 0 {
		t.Fatal("signature minted authority", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.Signature(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNativeLocatorFactorySnapshotLedgerAndOwnership(t *testing.T) {
	t.Parallel()
	root := archive.NativeRoot{Agent: "muse", Configured: "/synthetic", Canonical: "/synthetic", Revision: "one"}
	lookup := &pendingOrderLookup{}
	opts := Options{NativeRoots: []archive.NativeRoot{root}}
	closeSources := openCursorPass(nil, &opts)
	defer func() {
		if err := closeSources(); err != nil {
			t.Error(err)
		}
	}()
	p := pass{ctx: t.Context(), local: state.OpenReadOnly("/synthetic-state"), opts: opts, registrations: []archive.SessionRegistration{nativePendingRegistration(1), nativePendingRegistration(0)}}
	called := 0
	p.opts.NativeLocatorFactory = func(ctx context.Context, scope NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		called++
		if ctx != p.ctx || scope.ReadBudget != p.opts.sourcePasses.env.ReadBudget || len(scope.Queries) != 2 || scope.Queries[0].NativeID != nativePendingRegistration(0).NativeSessionID {
			t.Fatal("wrong immutable factory scope")
		}
		scope.Roots[0].Revision = "changed-copy"
		return lookup, func() error { lookup.closes++; return nil }, nil
	}
	finalize, err := p.openNativeInventory()
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || p.opts.NativeLocators != lookup || p.opts.sourcePasses.env.NativeLocators != lookup || p.opts.NativeRoots[0].Revision != "one" {
		t.Fatal("factory/env mismatch")
	}
	if used, _ := p.opts.sourcePasses.env.ReadBudget.Charged(); used == 0 {
		t.Fatal("query snapshot uncharged")
	}
	if err = finalize(); err != nil {
		t.Fatal(err)
	}
	if err = finalize(); err != nil || lookup.closes != 1 {
		t.Fatal("owned finalizer repeated", err)
	}
	if used, _ := p.opts.sourcePasses.env.ReadBudget.Charged(); used != 0 {
		t.Fatal("query charge leaked", used)
	}
	// Borrowed lookups are not finalized. Simultaneous configuration refuses.
	p.opts.NativeLocatorFactory = nil
	if closeLookup, err := p.openNativeInventory(); err != nil || closeLookup != nil || lookup.closes != 1 {
		t.Fatal(err)
	}
	p.opts.NativeLocatorFactory = opts.NativeLocatorFactory
	p.opts.NativeLocatorFactory = func(context.Context, NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		t.Fatal("ambiguous factory invoked")
		return nil, nil, nil
	}
	if _, err = p.openNativeInventory(); err == nil {
		t.Fatal("borrowed+owned accepted")
	}
}

type failedInventorySources struct{ closed *int }

func (s failedInventorySources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	_, f, ok := testSources.LookupSources(name)
	return failedInventoryProvider{closed: s.closed}, f, ok
}

type failedInventoryProvider struct {
	sourceio.FileProvider
	closed *int
}

func (p failedInventoryProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.FileProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return failedInventoryPass{SourcePass: pass, closed: p.closed}, nil
}

type failedInventoryPass struct {
	agentapi.SourcePass
	closed *int
}

func (p failedInventoryPass) Close() error { *p.closed++; return p.SourcePass.Close() }

type inventoryFailureMode string

const (
	inventoryUnreadable inventoryFailureMode = "unreadable"
	inventoryOversized  inventoryFailureMode = "oversized"
	inventoryPressure   inventoryFailureMode = "pressure"
)

func TestFailedNativeInventoryKeepsPendingAndBoundWorkAndClosesInOrder(t *testing.T) {
	t.Parallel()
	for _, failure := range []inventoryFailureMode{inventoryUnreadable, inventoryOversized, inventoryPressure} {
		t.Run(string(failure), func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "bound.jsonl", codexTranscript)
			bound := registration(t, path)
			if err := store.SaveRegistration(bound); err != nil {
				t.Fatal(err)
			}
			pending := nativePendingRegistration(0)
			pending.ProjectID = bound.ProjectID
			pending.ProjectRoot = bound.ProjectRoot
			pending.SessionStartedAt = bound.SessionStartedAt
			pending.RegisteredAt = bound.RegisteredAt
			if err := store.SaveRegistration(pending); err != nil {
				t.Fatal(err)
			}
			root := archive.NativeRoot{Agent: "muse", Configured: filepath.Dir(path), Canonical: filepath.Dir(path), Revision: "one"}
			cfg, _, err := config.Load(store.Home())
			if err != nil {
				t.Fatal(err)
			}
			cfg.NativeRoots = []archive.NativeRoot{root}
			if err = config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			catalogPath := filepath.Join(store.Home(), "discovery-catalog.json")
			switch failure {
			case inventoryUnreadable:
				if err = os.Mkdir(catalogPath, 0700); err != nil {
					t.Fatal(err)
				}
			case inventoryPressure:
				// The factory installs transient shared-ledger pressure below.
			case inventoryOversized:
				raw := append([]byte(`{"version":6,"native_inventory":"`), bytes.Repeat([]byte("x"), (1<<20)+1)...)
				raw = append(raw, []byte(`","unrelated":true}`)...)
				if err = os.WriteFile(catalogPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			closed, finalized, calls := 0, 0, 0
			opts := Options{Sources: failedInventorySources{closed: &closed}, Parsers: testParsers, MachineID: "synthetic", NativeRoots: cfg.NativeRoots, RepoKey: func(string) string { return "" }, Now: func() time.Time { return time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC) }, NativeLocatorFactory: func(ctx context.Context, pass NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
				calls++
				if failure == inventoryPressure {
					charge := agentapi.PassReadBudget - (8 << 20) + 1
					if !pass.ReadBudget.ReserveUnscoped(charge) {
						t.Fatal("pressure setup")
					}
					defer pass.ReadBudget.ReleaseUnscoped(charge)
				}
				lookup, failureErr := discovery.NewNativeInventory(ctx, pass.Store.Home(), pass.Roots, pass.Queries, pass.ReadBudget, muse.ValidateNativeLocator)
				if lookup != nil || failureErr == nil {
					t.Fatal("actual constructor did not fail", failureErr)
				}
				return &pendingOrderLookup{}, func() error {
					finalized++
					if closed == 0 {
						t.Error("inventory closed before bound source pass")
					}
					return nil
				}, errors.Join(failureErr, errors.New("private/native/path must not reach diagnostics: "+string(failure)))
			}}
			remote := storagetest.NewMemoryStore()
			result, err := Run(t.Context(), store, remote, opts)
			defer result.CloseStagedResults()
			if err != nil || len(result.Published) != 1 || result.Published[0] != bound.ArchiveSessionID || len(result.Waiting) != 1 || result.Waiting[0] != pending.ArchiveSessionID || len(result.Skipped) != 0 || calls != 1 || finalized != 1 {
				t.Fatal(err, result, calls, finalized)
			}
			if got := result.Errors["native-inventory"]; got == nil || got.Error() != "native inventory unavailable; retry next pass" {
				t.Fatal("unbounded private diagnostic", got)
			}
			saved, found, err := store.LoadRegistration(pending.ArchiveSessionID)
			if err != nil || !found || saved.TranscriptPath != "" || saved.NativeBinding != nil {
				t.Fatal("pending owner mutated", saved, err)
			}
			result2, err := Run(t.Context(), store, remote, opts)
			defer result2.CloseStagedResults()
			if err != nil || calls != 2 || finalized != 2 || len(result2.Waiting) != 1 {
				t.Fatal("constructor not retried / pending lost", err, result2, calls, finalized)
			}
		})
	}
}
