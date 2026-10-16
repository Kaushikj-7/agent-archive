package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestNativeInventorySharedCacheCompositionAndRelease(t *testing.T) {
	t.Parallel()
	home, root, _ := inventoryFixture(t)
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	caches := int64(PassCacheBytes + codex.PassCacheBytes)
	queries := make([]agentapi.LocatorRequest, maxCatalog)
	for i := range queries {
		queries[i] = agentapi.LocatorRequest{Root: root, NativeID: inventoryMeasurementID(i)}
	}
	// Mirror the actual collector reservation, including its existing config's
	// maximum sixteen committed roots and all pending ordering vector/map storage.
	queryCharge := int64(len(queries))*(int64(reflect.TypeFor[agentapi.LocatorRequest]().Size())+128) + 16*int64(reflect.TypeFor[archive.NativeRoot]().Size())
	if caches != 84<<20 || caches+8<<20+queryCharge > agentapi.PassCacheReserve {
		t.Fatal("existing cache composition exceeded", caches, queryCharge)
	}
	if !budget.ReserveUnscoped(caches + queryCharge) {
		t.Fatal("baseline cache charge")
	}
	l, err := NewNativeInventory(t.Context(), home, []archive.NativeRoot{root}, queries, budget, muse.ValidateNativeLocator)
	if err != nil {
		t.Fatal(err)
	}
	end := budget.BeginScope(collector.SessionReadCeiling)
	session := budget.Reservation()
	if !session.Reserve(collector.SessionReadCeiling) {
		t.Fatal("native inventory stole session ceiling")
	}
	session.Release(collector.SessionReadCeiling)
	end()
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	if l.progress.Owners != nil || l.eligible != nil || l.expected != nil || l.tokens != nil || l.validate != nil {
		t.Fatal("references outlived inventory charge")
	}
	used, peak := budget.Charged()
	if used != caches+queryCharge || peak > agentapi.PassReadBudget {
		t.Fatal(used, peak)
	}
	budget.ReleaseUnscoped(caches + queryCharge)
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatal("release", used)
	}
	t.Logf("existing_caches=%d native=%d actual_max_query_rank=%d cache_total=%d reserve=%d peak_shared=%d", caches, 8<<20, queryCharge, caches+(8<<20)+queryCharge, agentapi.PassCacheReserve, peak)
}

func TestNativeInventoryActualPressureOversizedUnreadableAndCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"pressure", "oversized", "unreadable", "canceled-before", "canceled-after"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home, root, _ := inventoryFixture(t)
			path := filepath.Join(home, "discovery-catalog.json")
			before := []byte(`{"version":6,"unrelated":{"preserve":true}}`)
			if mode == "oversized" {
				before = append([]byte(`{"version":6,"native_inventory":"`), bytes.Repeat([]byte("x"), maxCatalogBytes/32+1)...)
				before = append(before, []byte(`","unrelated":true}`)...)
			}
			if mode == "unreadable" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			limit := agentapi.PassReadBudget
			if mode == "pressure" {
				limit = (8 << 20) - 1
			}
			budget := agentapi.NewNativeReadBudget(limit)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled-before" {
				cancel()
			}
			q := agentapi.LocatorRequest{NativeID: inventoryOwner, Root: root}
			l, err := NewNativeInventory(ctx, home, []archive.NativeRoot{root}, []agentapi.LocatorRequest{q}, budget, muse.ValidateNativeLocator)
			if mode == "canceled-after" {
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				if _, err = l.Locate(ctx, q); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if l.Counts() != (NativeInventoryCounts{}) && l.Counts().Names != 0 {
					t.Fatal("native IO after cancellation", l.Counts())
				}
				if err = l.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || l != nil {
				t.Fatal("actual failing constructor accepted", mode, err)
			}
			used, _ := budget.Charged()
			if used != 0 {
				t.Fatal("failed constructor leak", mode, used)
			}
			if mode != "unreadable" {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("failed/canceled inventory changed prior state", err)
				}
			}
		})
	}
}

func TestNativeInventoryTypedVectorBound(t *testing.T) {
	t.Parallel()
	// Couple each directory cell with its parent's short child-edge cell. Short
	// edge strings alone have more typed overhead than wire bytes. UUID owner/
	// match strings and page cells have their own independent wire lower bounds.
	for _, sample := range []any{nativeDirectory{Path: ".", Pages: []nativePage{}, Children: []string{}, Matches: []string{}}, nativePage{Kind: "EOF"}, inventoryOwner} {
		typ := reflect.TypeOf(sample)
		wire, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		payload := 0
		switch sample.(type) {
		case nativeDirectory:
			wire = append(wire, []byte(`"."`)...)
			// Parent string vectors are independently bounded by <=3 cells/name
			// below; both path payload references are conservatively counted.
			payload = 3*int(reflect.TypeFor[string]().Size()) + 2
		case nativePage:
			payload = len("EOF")
		case string:
			payload = len(inventoryOwner)
		}
		vector := reflect.New(reflect.SliceOf(typ)).Elem()
		vector.Set(reflect.MakeSlice(reflect.SliceOf(typ), 0, 0))
		for n := 1; n <= maxCatalog; n++ {
			vector.Grow(1)
			vector.SetLen(n)
			if typ.Kind() == reflect.String && vector.Cap() > 3*n {
				t.Fatal("child string capacity bound", n, vector.Cap())
			}
			if uint64(vector.Cap())*uint64(typ.Size())+uint64(payload*n) > 3*uint64(n)*uint64(len(wire)) {
				t.Fatal("pinned typed/wire charge proof", typ, n, vector.Cap(), len(wire))
			}
		}
	}
}

// An injected descriptor observes whether canceled operations reach native I/O.
type inventoryCanceledFile struct {
	transcriptio.File
	calls int
}

func (f *inventoryCanceledFile) Stat() (os.FileInfo, error) {
	f.calls++
	return nil, errors.New("unexpected stat")
}
func (f *inventoryCanceledFile) ReadAt([]byte, int64) (int, error) {
	f.calls++
	return 0, errors.New("unexpected read")
}
func TestNativeInventoryCanceledDescriptorDoesNoLaterIO(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	owner := &NativeInventory{ctx: ctx, remaining: Budget}
	descriptor := &inventoryCanceledFile{}
	counted := &nativeCountedFile{File: descriptor, owner: owner}
	if _, err := counted.Stat(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := counted.ReadAt(make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if descriptor.calls != 0 || owner.Counts() != (NativeInventoryCounts{}) {
		t.Fatal("canceled native IO", descriptor.calls, owner.Counts())
	}
}
