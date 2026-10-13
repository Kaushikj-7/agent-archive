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
