package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type gcTestScenario string

type gcClockMode string

const (
	gcTestBeforeLease      gcTestScenario = "before-lease"
	gcTestAfterLease       gcTestScenario = "after-lease"
	gcTestBeforeRelease    gcTestScenario = "before-release"
	gcTestAfterRelease     gcTestScenario = "after-release"
	gcTestAfterHoldRelease gcTestScenario = "after-hold-release"
	gcTestGeneration       gcTestScenario = "generation"
	gcTestInventory        gcTestScenario = "inventory"
	gcTestReleasedBytes    gcTestScenario = "released-bytes"
	gcTestForeignHead      gcTestScenario = "foreign-head"
	gcTestUnknownField     gcTestScenario = "unknown-field"
	gcTestUncertain        gcClockMode    = "uncertain"
	gcTestRegressing       gcClockMode    = "regressing"
	gcTestPrecision        gcClockMode    = "precision"
)

type gcCrashStore struct {
	*qualifiedStore
	fault gcTestScenario
	fired bool
}

func (s *gcCrashStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	fail := false
	after := false
	if !s.fired && s.fault != "" {
		if key == HeadKey {
			var head CatalogHead
			if err := json.Unmarshal(raw, &head); err != nil {
				return "", err
			}
			if head.GCLease != "" && (s.fault == gcTestBeforeLease || s.fault == gcTestAfterLease) {
				fail = true
				after = s.fault == gcTestAfterLease
			}
			if head.GCLease == "" && (s.fault == gcTestBeforeRelease || s.fault == gcTestAfterRelease) {
				fail = true
				after = s.fault == gcTestAfterRelease
			}
		}
		if key == CoordinatorKey && s.fault == gcTestAfterHoldRelease {
			var state admissions
			if err := json.Unmarshal(raw, &state); err != nil {
				return "", err
			}
			if state.GCReceipt != nil && state.GCLink == nil {
				fail = true
				after = true
			}
		}
	}
	if fail {
		s.fired = true
		if !after {
			return "", errors.New("private injected crash")
		}
	}
	etag, err := s.qualifiedStore.PutConditional(ctx, key, raw, condition)
	if fail && err == nil {
		return "", errors.New("private lost acknowledgement")
	}
	return etag, err
}

func TestGCRecoveryPhasesKeepExactDurableAuthority(t *testing.T) {
	for _, phase := range []gcTestScenario{gcTestBeforeLease, gcTestAfterLease, gcTestBeforeRelease, gcTestAfterRelease, gcTestAfterHoldRelease} {
		t.Run(string(phase), func(t *testing.T) {
			raw := &gcCrashStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
			w, err := New(raw)
			if err != nil {
				t.Fatal(err)
			}
			m := mutation(t, w, "crash-"+string(phase))
			if _, err = w.Commit(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			_, before, err := w.Head(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			raw.fault = phase
			if err = w.Collect(t.Context(), heldBarrier{}); err == nil {
				t.Fatal("injected crash ignored")
			}
			state, _, err := w.Coordinator().read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			owner := ""
			if state.GCLink != nil {
				owner = state.GCLink.Owner
			} else if state.GCReceipt != nil {
				owner = state.GCReceipt.Owner
			}
			if owner == "" {
				t.Fatal("lost durable GC owner")
			}
			store, err := Wrap(raw)
			if err != nil {
				t.Fatal(err)
			}
			beforeCoordinator, err := raw.Get(t.Context(), CoordinatorKey)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CatalogRecoveryBarrier(t.Context(), "wrong-owner"); err == nil {
				t.Fatal("wrong owner accepted")
			}
			afterCoordinator, err := raw.Get(t.Context(), CoordinatorKey)
			if err != nil || string(beforeCoordinator) != string(afterCoordinator) {
				t.Fatal("wrong owner mutated coordinator", err)
			}
			barrier, err := store.CatalogRecoveryBarrier(t.Context(), owner)
			if err != nil {
				t.Fatal(err)
			}
			if err = w.Collect(t.Context(), barrier); err == nil {
				t.Fatal("recovery marker collected")
			}
			if err = w.RecoverGC(t.Context(), barrier, owner); err != nil {
				t.Fatal("exact recovery refused", err)
			}
			h, after, err := w.Head(t.Context())
			if err != nil || h.GCLease != "" || before == after {
				t.Fatal("head release/fence proof", err)
			}
			state, _, err = w.Coordinator().read(t.Context())
			if err != nil || state.Seal != "" || state.Hold != "" || state.GCLink != nil {
				t.Fatal("durable hold not released", err)
			}
			if _, err = raw.Get(t.Context(), m.Next.Summary.SourceBundle.Key); err != nil {
				t.Fatal("live source removed", err)
			}
			repeated, err := store.CatalogRecoveryBarrier(t.Context(), owner)
			if err != nil {
				t.Fatal(err)
			}
			if err = w.RecoverGC(t.Context(), repeated, owner); err != nil {
				t.Fatal("known completed recovery not idempotent", err)
			}
		})
	}
}

func TestUnleasedSealRecoveryRequiresExactOwnerGeneration(t *testing.T) {
	w, _ := fixture(t)
	owner, err := w.Coordinator().Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := w.Coordinator().read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Coordinator().RecoverUnleasedSeal(t.Context(), "wrong", state.Generation); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = w.Coordinator().RecoverUnleasedSeal(t.Context(), owner, state.Generation+1); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = w.Coordinator().RecoverUnleasedSeal(t.Context(), owner, state.Generation); err != nil {
		t.Fatal(err)
	}
}

func TestGCRecoveryRefusesForeignHeadAndDamagedDescriptor(t *testing.T) {
	for _, damage := range []gcTestScenario{gcTestForeignHead, gcTestGeneration, gcTestInventory, gcTestReleasedBytes, gcTestUnknownField} {
		t.Run(string(damage), func(t *testing.T) {
			raw := &gcCrashStore{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
			w, err := New(raw)
			if err != nil {
				t.Fatal(err)
			}
			m := mutation(t, w, "damage")
			if _, err = w.Commit(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			raw.fault = gcTestBeforeRelease
			if err = w.Collect(t.Context(), heldBarrier{}); err == nil {
				t.Fatal("crash ignored")
			}
			state, etag, err := w.Coordinator().read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			owner := state.GCLink.Owner
			if damage == gcTestForeignHead {
				head, headETag, err := w.Head(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				head.Generation++
				body, err := json.Marshal(head)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = raw.PutConditional(t.Context(), HeadKey, body, storage.PutCondition{MatchETag: headETag}); err != nil {
					t.Fatal(err)
				}
			} else {
				if damage == gcTestGeneration {
					state.Generation++
				}
				if damage == gcTestInventory {
					state.GCLink.Inventory = append(state.GCLink.Inventory, m.Next.Metadata)
				}
				if damage == gcTestReleasedBytes {
					state.GCLink.ReleasedHead = []byte(`{"foreign":true}`)
				}
				body, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if damage == gcTestUnknownField {
					var fields map[string]any
					if err = json.Unmarshal(body, &fields); err != nil {
						t.Fatal(err)
					}
					fields["unknown"] = "damaged"
					body, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err = raw.PutConditional(t.Context(), CoordinatorKey, body, storage.PutCondition{MatchETag: etag}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := raw.Get(t.Context(), CoordinatorKey)
			if err != nil {
				t.Fatal(err)
			}
			if err = w.RecoverGC(t.Context(), heldBarrier{}, owner); err == nil {
				t.Fatal("damaged authority recovered")
			}
			after, err := raw.Get(t.Context(), CoordinatorKey)
			if err != nil || string(before) != string(after) {
				t.Fatal("recovery changed damaged authority", err)
			}
		})
	}
}

func TestCoordinatorMaintenanceClockRefusalPerformsNoWrites(t *testing.T) {
	for _, mode := range []gcClockMode{gcTestUncertain, gcTestRegressing, gcTestPrecision} {
		t.Run(string(mode), func(t *testing.T) {
			raw := &clockFixture{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}}
			w, err := New(raw)
			if err != nil {
				t.Fatal(err)
			}
			m := mutation(t, w, "clock-preflight")
			if _, err = w.Commit(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case gcTestUncertain:
				raw.override = true
				raw.clock = storage.CatalogTime{Earliest: time.Now().Add(-time.Hour), Latest: time.Now().Add(time.Hour)}
			case gcTestRegressing:
				raw.override = true
				raw.clock = storage.CatalogTime{Earliest: time.Now().Add(-time.Hour), Latest: time.Now().Add(-time.Hour)}
			case gcTestPrecision:
				raw.missingPrecision = true
			}
			store, err := Wrap(raw)
			if err != nil {
				t.Fatal(err)
			}
			before, err := raw.Get(t.Context(), CoordinatorKey)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CatalogBarrier(t.Context()); err == nil {
				t.Fatal("invalid clock acquired seal")
			}
			if err = w.Collect(t.Context(), heldBarrier{}); err == nil {
				t.Fatal("invalid clock collected")
			}
			after, err := raw.Get(t.Context(), CoordinatorKey)
			if err != nil || string(before) != string(after) {
				t.Fatal("clock refusal changed coordinator", err)
			}
		})
	}
}
