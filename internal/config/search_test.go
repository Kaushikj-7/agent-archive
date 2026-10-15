package config

import (
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/destination"
)

func TestSearchConsentDefaultsOffAndRetainsLifecycleFence(t *testing.T) {
	t.Parallel()
	cfg := Config{RetentionDays: 90}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Config
	if err = json.Unmarshal(raw, &loaded); err != nil || loaded.SearchIndexing || loaded.SearchLifecycleProtection {
		t.Fatal(loaded, err)
	}
	if err = SetSearchIndexing(&loaded, true); err != nil {
		t.Fatal(err)
	}
	if err = SetSearchIndexing(&loaded, false); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &loaded); err != nil || loaded.SearchIndexing || !loaded.SearchLifecycleProtection || loaded.SchemaVersion != 16 {
		t.Fatal(loaded, err)
	}
	var old struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err = json.Unmarshal(raw, &old); err == nil {
		t.Fatal("old numeric writer accepted lifecycle ownership")
	}
	preserved := Config{RetentionDays: 90}
	PreserveWriterFence(&preserved, loaded)
	if !preserved.SearchLifecycleProtection || preserved.SearchIndexing {
		t.Fatal("rollback lost fence or granted consent")
	}
}

func TestSearchFenceComposesWithEverySelectionLayer(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"legacy", "publication", "staged", "settled", "catalog", "scoped"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{RetentionDays: 90, DurableStorageProtection: true, ImportStagingProtection: true, HistoricalProjectProtection: true}
			if kind != "legacy" {
				cfg.PublicationCompositionProtection = true
			}
			if kind == "staged" || kind == "settled" || kind == "scoped" {
				cfg.StagedSelectionProtection = true
			}
			if kind == "settled" || kind == "scoped" {
				cfg.SettledCursorSelectionProtection = true
			}
			if kind == "catalog" || kind == "scoped" {
				cfg.Storage.ArchiveFormat = destination.FormatCatalogV4
			}
			if kind == "scoped" {
				cfg.ScopedPickerProtection = true
			}
			before, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(before, &cfg); err != nil {
				t.Fatal(err)
			}
			ordinary, staged, settled := cfg.SupportsOrdinarySelection(), cfg.SupportsStagedSelection(), cfg.SupportsSettledCursorSelection()
			if err = SetSearchIndexing(&cfg, true); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Config
			if err = json.Unmarshal(raw, &loaded); err != nil {
				t.Fatal(err)
			}
			if loaded.SchemaVersion != 16 || !loaded.SearchIndexing || ordinary != loaded.SupportsOrdinarySelection() || staged != loaded.SupportsStagedSelection() || settled != loaded.SupportsSettledCursorSelection() {
				t.Fatal("outer fence changed underlying capabilities", loaded)
			}
			lower := searchUnderlyingConfig(loaded)
			after, err := json.Marshal(lower)
			if err != nil || string(before) != string(after) {
				t.Fatalf("lower configuration changed: %s vs %s (%v)", before, after, err)
			}
		})
	}
}

func TestSearchFenceRefusesMalformedOrUnprotectedConsent(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"schema_version":1,"search_indexing":true}`,
		`{"schema_version":16,"search_lifecycle_protection":true}`,
		`{"schema_version":{"version":16,"writer":"search-lifecycle-v16"}}`,
		`{"schema_version":{"version":15,"writer":"search-lifecycle-v16"},"search_lifecycle_protection":true}`,
		`{"schema_version":{"version":16,"writer":"settled-cursor-selection-v15"},"search_lifecycle_protection":true}`,
	} {
		cfg := Config{MachineID: "unchanged"}
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil || cfg.MachineID != "unchanged" {
			t.Fatal("accepted malformed consent or mutated receiver", raw)
		}
	}
}
