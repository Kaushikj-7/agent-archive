package discovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func importInventoryOpen(t *testing.T, root archive.NativeRoot) *NativeInventory {
	t.Helper()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	l, err := NewNativeImportInventory(t.Context(), []archive.NativeRoot{root}, budget, muse.ValidateNativeLocator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
		used, _ := budget.Charged()
		if used != 0 {
			t.Errorf("import inventory leaked %d", used)
		}
	})
	return l
}

func TestNativeImportInventorySharesCensusAndExpiresTokens(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	path := inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
	catalog := filepath.Join(home, "discovery-catalog.json")
	before := []byte("hostile foreign catalog must remain untouched")
	if err := os.WriteFile(catalog, before, 0600); err != nil {
		t.Fatal(err)
	}
	l := importInventoryOpen(t, root)
	if l.Counts().Names != 0 {
		t.Fatal("constructor scanned native namespace")
	}
	candidates, err := l.Candidates(t.Context())
	if err != nil || !candidates.Complete || len(candidates.IDs) != 1 || candidates.IDs[0] != inventoryOwner {
		t.Fatal(candidates, err)
	}
	pages := l.Counts().PageReads
	located, err := l.Locate(t.Context(), agentapi.LocatorRequest{Purpose: agentapi.LocatorHistoricalImport, Root: root, NativeID: inventoryOwner})
	if err != nil || located.Status != agentapi.LocatorFound || located.Candidates[0].Path != path {
		t.Fatal(located, err)
	}
	if err := l.Check(t.Context(), inventoryOwner, located.Revision); err != nil {
		t.Fatal(err)
	}
	if l.Counts().PageReads != pages {
		t.Fatal("selected source started a second census")
	}
	for _, purpose := range []agentapi.LocatorPurpose{agentapi.LocatorAdmittedOwner, agentapi.LocatorStartupCandidate, "future"} {
		result, err := l.Locate(t.Context(), agentapi.LocatorRequest{Purpose: purpose, Root: root, NativeID: inventoryOwner})
		if err != nil || result.Status != agentapi.LocatorRefused {
			t.Fatal("historical mode accepted another purpose", purpose, result, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(t.Context(), inventoryOwner, located.Revision); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("expired token", err)
	}
	after, err := os.ReadFile(catalog)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("historical close changed catalog", err)
	}
}

func TestNativeImportInventoryRefusesDuplicatesAndChurn(t *testing.T) {
	t.Parallel()
	for _, duplicate := range []bool{true, false} {
		name := "churn"
		if duplicate {
			name = "duplicate"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, root, raw := inventoryFixture(t)
			inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			if duplicate {
				inventoryFile(t, root, "2026/10/16", inventoryOwner, raw)
			}
			l := importInventoryOpen(t, root)
			if _, err := l.Candidates(t.Context()); err != nil {
				t.Fatal(err)
			}
			located, err := l.Locate(t.Context(), agentapi.LocatorRequest{Purpose: agentapi.LocatorHistoricalImport, Root: root, NativeID: inventoryOwner})
			if duplicate {
				if err != nil || located.Status != agentapi.LocatorAmbiguous {
					t.Fatal(located, err)
				}
				return
			}
			if err != nil || located.Status != agentapi.LocatorFound {
				t.Fatal(located, err)
			}
			inventoryFile(t, root, "2026/10/16", inventoryOwner, raw)
			if err := l.Check(t.Context(), inventoryOwner, located.Revision); err == nil {
				t.Fatal("changed namespace retained uniqueness")
			}
			projection, err := l.Candidates(t.Context())
			if err == nil && projection.Complete {
				t.Fatal("changed pass silently rescanned into complete preview")
			}
		})
	}
}

func TestNativeImportInventoryPartialAndCancelledCloseDoNotPersist(t *testing.T) {
	t.Parallel()
	_, root, _ := inventoryFixture(t)
	for i := range 2050 {
		if err := os.MkdirAll(filepath.Join(root.Canonical, "2026", "10", "15", inventoryMeasurementID(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	l, err := NewNativeImportInventory(ctx, []archive.NativeRoot{root}, budget, muse.ValidateNativeLocator)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := l.Candidates(ctx)
	if err != nil || projection.Complete {
		t.Fatal("bounded partial namespace claimed complete", projection.Complete, err)
	}
	counts := l.Counts()
	if _, err := l.Candidates(ctx); err != nil || l.Counts().PageReads != counts.PageReads {
		t.Fatal("retry extended the census", err)
	}
	cancel()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatal("cancelled close leaked", used)
	}
	if _, err := os.Stat(filepath.Join(root.Canonical, "discovery-catalog.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled close persisted catalog", err)
	}
}

func TestNativeImportInventoryPurposeDoesNotEnterOrdinaryConstructor(t *testing.T) {
	t.Parallel()
	home, root, _ := inventoryFixture(t)
	for _, purpose := range []agentapi.LocatorPurpose{agentapi.LocatorHistoricalImport, "future"} {
		budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
		l, err := NewNativeInventory(t.Context(), home, []archive.NativeRoot{root}, []agentapi.LocatorRequest{{Purpose: purpose, NativeID: inventoryOwner, Root: root}}, budget, muse.ValidateNativeLocator)
		if err == nil || l != nil {
			t.Fatal("ordinary mode accepted historical/future purpose", purpose, err)
		}
		used, _ := budget.Charged()
		if used != 0 {
			t.Fatal("rejected constructor leaked", used)
		}
	}
}
