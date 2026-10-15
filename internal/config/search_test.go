package config

import (
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/destination"
)

type searchLayer string

const (
	searchLayerLegacy      searchLayer = "legacy"
	searchLayerPublication searchLayer = "publication"
	searchLayerStaged      searchLayer = "staged"
	searchLayerSettled     searchLayer = "settled"
	searchLayerCatalog     searchLayer = "catalog"
	searchLayerScoped      searchLayer = "scoped"
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
	for _, kind := range []searchLayer{searchLayerLegacy, searchLayerPublication, searchLayerStaged, searchLayerSettled, searchLayerCatalog, searchLayerScoped} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			format := destination.FormatLegacy
			if kind == searchLayerCatalog || kind == searchLayerScoped {
				format = destination.FormatCatalogV4
			}
			cfg := Config{RetentionDays: 90, DurableStorageProtection: true, ImportStagingProtection: true, HistoricalProjectProtection: true,
				PublicationCompositionProtection: kind != searchLayerLegacy, StagedSelectionProtection: kind == searchLayerStaged || kind == searchLayerSettled || kind == searchLayerScoped,
				SettledCursorSelectionProtection: kind == searchLayerSettled || kind == searchLayerScoped, ScopedPickerProtection: kind == searchLayerScoped, Storage: destination.Config{ArchiveFormat: format}}

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

func TestSearchFenceRetainsPermissionFloorValidation(t *testing.T) {
	t.Parallel()
	cfg, _ := discoveryConfig(t)
	if err := SetSearchIndexing(&cfg, true); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var valid Config
	if err = json.Unmarshal(raw, &valid); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	auth := wire["discovery"].(map[string]any)["authorizations"].([]any)[0].(map[string]any)
	delete(auth, "native_start_floor")
	broken, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	loaded := Config{MachineID: "unchanged"}
	if err = json.Unmarshal(broken, &loaded); err == nil || loaded.MachineID != "unchanged" {
		t.Fatal("outer search fence bypassed permission floors or mutated receiver")
	}
}
