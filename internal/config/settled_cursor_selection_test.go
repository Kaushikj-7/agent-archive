package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// This raw feature protocol compiles under an older decoder too; old refusal
// is feature absence, not a preexisting bug or compilation-based oracle.
func TestSettledCursorRawFenceAndOlderProtocols(t *testing.T) {
	const raw = `{"schema_version":{"version":14,"writer":"settled-cursor-selection-v14"},"retention_days":90,"durable_storage_protection":true,"publication_composition_protection":true,"import_staging_protection":true,"staged_selection_protection":true,"settled_cursor_selection_protection":true}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != 14 {
		t.Fatal("Cursor floor lost")
	}
	round, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(round), `"settled-cursor-selection-v14"`) {
		t.Fatal("Cursor fence downgraded")
	}
	for _, mutate := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"version":14`, `"version":13`, 1) },
		func(s string) string {
			return strings.Replace(s, `"settled_cursor_selection_protection":true`, `"settled_cursor_selection_protection":false`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"staged_selection_protection":true`, `"staged_selection_protection":false`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"durable_storage_protection":true`, `"durable_storage_protection":false`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"settled-cursor-selection-v14"`, `"staged-selection-v13"`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"retention_days":90`, `"retention_days":90,"storage":{"ArchiveFormat":"catalog-v4"}`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"settled_cursor_selection_protection":true`, `"settled_cursor_selection_protection":true,"SETTLED_CURSOR_SELECTION_PROTECTION":true`, 1)
		},
	} {
		var rejected Config
		if json.Unmarshal([]byte(mutate(raw)), &rejected) == nil {
			t.Fatal("unsupported/mixed Cursor fence accepted")
		}
	}
}
