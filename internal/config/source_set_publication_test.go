package config

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/destination"
	"testing"
)

func TestSourceSetPublicationScopePersistsAndDerivesStorage(t *testing.T) {
	t.Parallel()
	home := durableTestHome(t)
	if err := Save(home, Config{MachineID: "synthetic", GenerationProtection: true, CodexHistoryProtection: true}); err != nil {
		t.Fatal(err)
	}
	var retained PublicationCompositionGuard
	if err := WithPublicationComposition(home, func(g PublicationCompositionGuard) error {
		retained = g
		cfg, found, err := Load(home)
		if err != nil || !found || cfg.SchemaVersion != 17 || !cfg.DurableStorageProtection || !cfg.SourceSetPublicationProtection || !cfg.GenerationProtection || !cfg.CodexHistoryProtection {
			t.Fatalf("floor not durably composed: %+v %v", cfg, err)
		}
		storage, err := g.Storage(home)
		if err != nil {
			return err
		}
		return storage.CheckHome(home)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Storage(home); err == nil {
		t.Fatal("expired composition accepted")
	}
	if err := WithDurableStorage(home, func(g DurableStorageGuard) error { return g.CheckHome(home) }); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{MachineID: "rollback"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(home)
	if err != nil || cfg.SchemaVersion != 17 || !cfg.SourceSetPublicationProtection || !cfg.DurableStorageProtection {
		t.Fatalf("lower writer lost sticky floor: %+v %v", cfg, err)
	}
}

func TestSourceSetPublicationWriterRefusesDowngradeAndMissingProtection(t *testing.T) {
	t.Parallel()
	cfg := Config{MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true, SourceSetPublicationProtection: true}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range [][]byte{
		bytes.Replace(raw, []byte(`"version":17`), []byte(`"version":16`), 1),
		bytes.Replace(raw, []byte(`source-set-publication-v17`), []byte(`search-lifecycle-v16`), 1),
		bytes.Replace(raw, []byte(`"source_set_publication_protection":true`), []byte(`"source_set_publication_protection":false`), 1),
		bytes.Replace(raw, []byte(`"durable_storage_protection":true`), []byte(`"durable_storage_protection":false`), 1),
	} {
		var got Config
		if err := json.Unmarshal(broken, &got); err == nil {
			t.Fatalf("accepted unsupported source-set writer %s", broken)
		}
	}
}

type sourceSetLowerCapability string

const (
	lowerLegacy       sourceSetLowerCapability = "legacy"
	lowerOrdinary     sourceSetLowerCapability = "ordinary"
	lowerStaged       sourceSetLowerCapability = "staged"
	lowerSettled      sourceSetLowerCapability = "settled"
	lowerScoped       sourceSetLowerCapability = "scoped"
	lowerSearch       sourceSetLowerCapability = "search"
	lowerNativeSearch sourceSetLowerCapability = "native-search"
)

func TestSourceSetPublicationPreservesLowerCapabilities(t *testing.T) {
	t.Parallel()
	for _, kind := range []sourceSetLowerCapability{lowerLegacy, lowerOrdinary, lowerStaged, lowerSettled, lowerScoped, lowerSearch, lowerNativeSearch} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			var format destination.ArchiveFormat
			if kind == lowerScoped {
				format = destination.FormatCatalogV4
			}
			cfg := Config{
				MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true,
				PublicationCompositionProtection: kind != lowerLegacy,
				ImportStagingProtection:          kind == lowerStaged || kind == lowerSettled || kind == lowerScoped,
				StagedSelectionProtection:        kind == lowerStaged || kind == lowerSettled || kind == lowerScoped,
				SettledCursorSelectionProtection: kind == lowerSettled || kind == lowerScoped,
				ScopedPickerProtection:           kind == lowerScoped,
				Storage:                          destination.Config{ArchiveFormat: format},
				SearchLifecycleProtection:        kind == lowerSearch || kind == lowerNativeSearch,
				SearchIndexing:                   kind == lowerSearch || kind == lowerNativeSearch,
				NativeAuthorityProtection:        kind == lowerNativeSearch,
			}
			if err := prepareDiscoveryConfig(&cfg); err != nil {
				t.Fatal(err)
			}
			ordinary, staged, settled := cfg.SupportsOrdinarySelection(), cfg.SupportsStagedSelection(), cfg.SupportsSettledCursorSelection()
			cfg.SourceSetPublicationProtection = true
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Config
			if err = json.Unmarshal(raw, &loaded); err != nil {
				t.Fatal(err)
			}
			if loaded.SchemaVersion != 17 || !loaded.SourceSetPublicationProtection || loaded.SupportsOrdinarySelection() != ordinary || loaded.SupportsStagedSelection() != staged || loaded.SupportsSettledCursorSelection() != settled || loaded.SearchIndexing != cfg.SearchIndexing || loaded.NativeAuthorityProtection != cfg.NativeAuthorityProtection {
				t.Fatalf("lower capabilities changed: %+v", loaded)
			}
		})
	}
}
