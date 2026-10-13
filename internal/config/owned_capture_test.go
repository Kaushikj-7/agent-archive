package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

func ownedCaptureConfigFixture(t *testing.T) (string, []byte) {
	t.Helper()
	home := t.TempDir()
	cfg := Config{MachineID: "synthetic", DurableStorageProtection: true, PublicationCompositionProtection: true, ImportStagingProtection: true, StagedSelectionProtection: true, SettledCursorSelectionProtection: true}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return home, raw
}

func TestOwnedCaptureConfigRetainsOnlyEncodedEquivalentDecodedLease(t *testing.T) {
	home, raw := ownedCaptureConfigFixture(t)
	held, err := local.OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	budget := agentapi.NewNativeReadBudget(128 << 20)
	cfg, found, closeCfg, err := LoadRootedOwned(t.Context(), held, budget.Reservation())
	if err != nil || !found || !cfg.SupportsSettledCursorSelection() {
		t.Fatal(cfg, found, err)
	}
	if budget.Available() != 128<<20-int64(len(raw)) {
		t.Fatal("encoded equivalent cfg lease not retained")
	}
	closeCfg()
	closeCfg()
	if budget.Available() != 128<<20 {
		t.Fatal("cfg close lost charge")
	}
	ordinary, found, err := LoadRooted(held)
	if err != nil || !found || ordinary.SchemaVersion != cfg.SchemaVersion || ordinary.DestinationID() != cfg.DestinationID() {
		t.Fatal("default decoding diverged", ordinary, found, err)
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("read changed config", err)
	}
}

func TestOwnedCaptureConfigRefusesPressureWithoutEffects(t *testing.T) {
	home, raw := ownedCaptureConfigFixture(t)
	held, err := local.OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	limit := 3*int64(len(raw)) + (32 << 10) - 1
	budget := agentapi.NewNativeReadBudget(limit)
	_, _, closeCfg, err := LoadRootedOwned(t.Context(), held, budget.Reservation())
	closeCfg()
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("prospective cfg pressure accepted", err)
	}
	if budget.Available() != limit {
		t.Fatal("failed cfg read leaked")
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("pressure changed config", err)
	}
}

func TestOwnedCaptureConfigGrowthPreservesCompleteBody(t *testing.T) {
	body := `{"complete":"` + strings.Repeat("x", 8192) + `"}`
	budget := agentapi.NewNativeReadBudget(128 << 20)
	owner := budget.Reservation()
	const initial int64 = 4
	if !owner.Reserve(3*initial + (32 << 10)) {
		t.Fatal("adequate fixture owner")
	}
	raw, allocated, err := readOwnedConfigBody(t.Context(), strings.NewReader(body), initial, owner)
	if err != nil || string(raw) != body || allocated < int64(len(body)) {
		t.Fatal("grown body truncated", allocated, err)
	}
	owner.Release(3*allocated + (32 << 10))
	if budget.Available() != 128<<20 {
		t.Fatal("growth owner leaked")
	}
}

func TestOwnedImportGuardRefusesUnsupportedPromotion(t *testing.T) {
	home, raw := ownedCaptureConfigFixture(t)
	cfg, _, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SettledCursorSelectionProtection = false
	cfg.StagedSelectionProtection = false
	if err = Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	called := false
	err = WithImportStagingOwned(t.Context(), home, func() ConfigReadReservation { return budget.Reservation() }, func(DurableStorageGuard) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("unsupported config promoted", err)
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("promotion refusal changed config", err)
	}
	if budget.Available() != 128<<20 {
		t.Fatal("guard refusal leaked config")
	}
}
