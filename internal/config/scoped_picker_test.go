package config

import (
	"encoding/json"
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
