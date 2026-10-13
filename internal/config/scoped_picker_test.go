package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScopedPickerFenceCannotBeFlattenedIntoOlderConfiguration(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":14,"retention_days":90}`,
		`{"schema_version":{"version":14,"writer":"scoped-picker-v1"},"retention_days":90}`,
		`{"schema_version":{"version":12,"writer":"scoped-picker-v1"},"retention_days":90}`,
		`{"schema_version":{"version":14,"writer":"publication-composition-v12"},"retention_days":90}`,
		`{"schema_version":1,"retention_days":90,"scoped_picker_protection":true}`,
		`{"schema_version":1,"retention_days":90,"scoped_picker_protection":false}`,
		`{"schema_version":1,"retention_days":90,"scoped_picker_protection":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			c := Config{MachineID: "unchanged"}
			if err := json.Unmarshal([]byte(raw), &c); err == nil {
				t.Fatal("accepted unimplemented scoped fence")
			}
			if c.MachineID != "unchanged" {
				t.Fatal("refusal changed receiver")
			}
		})
	}
}

func TestScopedPickerReservationRefusesLayeredStagedConfiguration(t *testing.T) {
	cfg := Config{MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true, PublicationCompositionProtection: true, ImportStagingProtection: true, HistoricalProjectProtection: true, StagedSelectionProtection: true}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var staged Config
	if err := json.Unmarshal(raw, &staged); err != nil {
		t.Fatal("valid staged configuration refused", err)
	}
	if staged.SchemaVersion != 13 || !staged.StagedSelectionProtection || !staged.PublicationCompositionProtection || !staged.DurableStorageProtection || !staged.ImportStagingProtection || !staged.HistoricalProjectProtection {
		t.Fatal("lost layered staged protections")
	}
	for _, mutation := range []string{
		strings.Replace(string(raw), `"version":13,"writer":"staged-selection-v13"`, `"version":14,"writer":"scoped-picker-v1"`, 1),
		strings.Replace(string(raw), `"writer":"staged-selection-v13"`, `"writer":"scoped-picker-v1"`, 1),
		strings.Replace(string(raw), `"machine_id":"synthetic"`, `"machine_id":"synthetic","scoped_picker_protection":true`, 1),
	} {
		if mutation == string(raw) {
			t.Fatal("probe did not alter selecting authority")
		}
		next := cfg
		if err := json.Unmarshal([]byte(mutation), &next); err == nil {
			t.Fatal("reserved picker flattened layered staged configuration")
		}
		after, err := json.Marshal(next)
		if err != nil || string(after) != string(raw) {
			t.Fatal("refusal changed layered configuration", err)
		}
	}
}

func TestScopedPickerConfigurationComposesAndPreservesProtections(t *testing.T) {
	t.Parallel()
	c := Config{RetentionDays: 90, ScopedPickerProtection: true, StagedSelectionProtection: true, PublicationCompositionProtection: true, DurableStorageProtection: true, RetentionProtection: true, ImportStagingProtection: true, HistoricalProjectProtection: true, GenerationProtection: true, CodexHistoryProtection: true}
	c.Storage.ArchiveFormat = "catalog-v4"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 14 || !decoded.ScopedPickerProtection || !decoded.PublicationCompositionProtection || !decoded.DurableStorageProtection || !decoded.HistoricalProjectProtection || !decoded.StagedSelectionProtection || decoded.SupportsStagedSelection() {
		t.Fatal(decoded)
	}
	next := Config{RetentionDays: 90}
	next.Storage = c.Storage
	PreserveWriterFence(&next, decoded)
	raw, err = json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &decoded); err != nil || !decoded.ScopedPickerProtection {
		t.Fatal(err)
	}
}

func TestScopedPickerLayeredConfigurationRejectsMissingLowerCapabilities(t *testing.T) {
	t.Parallel()
	c := Config{RetentionDays: 90, ScopedPickerProtection: true, StagedSelectionProtection: true, PublicationCompositionProtection: true, DurableStorageProtection: true, ImportStagingProtection: true}
	c.Storage.ArchiveFormat = "catalog-v4"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"durable_storage_protection", "import_staging_protection", "publication_composition_protection", "scoped_picker_protection"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			probe := strings.Replace(string(raw), `"`+field+`":true`, `"`+field+`":false`, 1)
			var next Config
			if err := json.Unmarshal([]byte(probe), &next); err == nil {
				t.Fatal("accepted missing lower fence")
			}
		})
	}
	for _, probe := range []string{
		strings.Replace(string(raw), `"version":14`, `"version":13`, 1),
		strings.Replace(string(raw), `"writer":"scoped-picker-v1"`, `"writer":"staged-selection-v13"`, 1),
		strings.Replace(string(raw), `"writer":"scoped-picker-v1"`, `"writer":"scoped-picker-v1","future":true`, 1),
		strings.Replace(string(raw), `"scoped_picker_protection":true`, `"scoped_picker_protection":true,"scoped_picker_protection":true`, 1),
	} {
		var next Config
		if err = json.Unmarshal([]byte(probe), &next); err == nil {
			t.Fatal("accepted mixed or ambiguous scoped configuration", probe)
		}
	}
	next := Config{RetentionDays: 90}
	next.Storage = c.Storage
	PreserveWriterFence(&next, c)
	if !next.StagedSelectionProtection || !next.ScopedPickerProtection {
		t.Fatal("removed sticky protections")
	}
	if next.SupportsStagedSelection() {
		t.Fatal("enabled catalog staging")
	}
}
