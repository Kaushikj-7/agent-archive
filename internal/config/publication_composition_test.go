package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/destination"
)

func TestPublicationCompositionExactFenceAndLowerLayers(t *testing.T) {
	for _, format := range []destination.ArchiveFormat{"", destination.FormatCatalogV4} {
		c := Config{MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true, ImportStagingProtection: true, HistoricalProjectProtection: true, PublicationCompositionProtection: true, Storage: destination.Config{ArchiveFormat: format}}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"writer":"publication-composition-v12"`) {
			t.Fatalf("missing exact fence: %s", raw)
		}
		var loaded Config
		if err = json.Unmarshal(raw, &loaded); err != nil {
			t.Fatal(err)
		}
		if loaded.SchemaVersion != 12 || !loaded.ImportStagingProtection || !loaded.HistoricalProjectProtection || !loaded.DurableStorageProtection {
			t.Fatalf("lost lower authority: %+v", loaded)
		}
		for _, mutate := range []func(string) string{
			func(s string) string {
				return strings.Replace(s, `{"version":12,"writer":"publication-composition-v12"}`, `12`, 1)
			},
			func(s string) string { return strings.Replace(s, `publication-composition-v12`, `catalog-v4-v11`, 1) },
			func(s string) string {
				return strings.Replace(s, `"durable_storage_protection":true`, `"durable_storage_protection":false`, 1)
			},
			func(s string) string {
				return strings.Replace(s, `"publication_composition_protection":true`, `"publication_composition_protection":false`, 1)
			},
		} {
			if json.Unmarshal([]byte(mutate(string(raw))), &Config{}) == nil {
				t.Fatal("damaged selecting fence accepted")
			}
		}
	}
	c := Config{MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true, Storage: destination.Config{ArchiveFormat: destination.FormatCatalogV4}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"writer":"catalog-v4-v11"`) || strings.Contains(string(raw), `publication_composition`) {
		t.Fatalf("legacy catalog writer changed: %s", raw)
	}
}
