package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// The raw protocol is usable by the prior decoder too: its rejection is a
// missing feature, never a compile-failure oracle.
func TestStagedSelectionRawWriterFencePreservesOrdinary(t *testing.T) {
	const raw = `{"schema_version":{"version":13,"writer":"staged-selection-v13"},"machine_id":"synthetic","retention_days":90,"durable_storage_protection":true,"publication_composition_protection":true,"import_staging_protection":true,"staged_selection_protection":true}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != 13 || !cfg.PublicationCompositionProtection || !cfg.DurableStorageProtection || !cfg.ImportStagingProtection {
		t.Fatal("supported distinct protocols lost")
	}
	round, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(round), `"writer":"staged-selection-v13"`) || !strings.Contains(string(round), `"staged_selection_protection":true`) {
		t.Fatal("writer downgrade")
	}
	for _, mutate := range []func(string) string{
		func(s string) string {
			return strings.Replace(s, `"staged_selection_protection":true`, `"staged_selection_protection":false`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"import_staging_protection":true`, `"import_staging_protection":false`, 1)
		},
		func(s string) string { return strings.Replace(s, `"version":13`, `"version":12`, 1) },
		func(s string) string {
			return strings.Replace(s, `"staged-selection-v13"`, `"publication-composition-v12"`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"machine_id":"synthetic"`, `"machine_id":"synthetic","storage":{"archive_format":"catalog-v4"}`, 1)
		},
	} {
		var rejected Config
		if err := json.Unmarshal([]byte(mutate(raw)), &rejected); err == nil {
			t.Fatal("unsupported staging configuration accepted")
		}
	}
}
