package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoryProtectionIsPermanentAndIndependentOfConsent(t *testing.T) {
	c := Config{RetentionDays: 90, MachineID: "m", CodexHistoryProtection: true}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"writer":"codex-history-v5"`) {
		t.Fatalf("missing writer fence: %s", raw)
	}
	var old struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(raw, &old) == nil {
		t.Fatal("old integer writer accepted protected configuration")
	}
	var current Config
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	rollback := Config{RetentionDays: 90, MachineID: "m"}
	PreserveWriterFence(&rollback, current)
	raw, err = json.Marshal(rollback)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	if !current.CodexHistoryProtection || current.Discovery != nil || current.CodexCapture != nil {
		t.Fatal("rollback changed protection or granted consent")
	}
}
