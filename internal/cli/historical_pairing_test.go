package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestHistoricalPairingTransfersOnlyPortableImportPolicy(t *testing.T) {
	t.Parallel()
	fixture, _, mapping := newHistoricalOnlyFixture(t)
	_, stderr, code := fixture.importRun(t, nil, false, "--yes", "--background", "--mapping-file", mapping)
	if code != 0 {
		t.Fatalf("import %d %s", code, stderr)
	}
	cfg := mustLoadConfig(t, fixture.data)
	portable := exportHistoricalPairing(cfg)
	if len(portable) != 1 || !portable[0].Enabled {
		t.Fatal("historical permission not portable")
	}
	raw, err := json.Marshal(portable)
	must(t, err)
	if bytes.Contains(raw, []byte(fixture.userHome)) || bytes.Contains(raw, []byte("original_cwd")) || bytes.Contains(raw, []byte("permissions")) {
		t.Fatal("pairing revealed private assignments")
	}
	cfg.HistoricalProjects[0].Permissions[0].Enabled = false
	cfg.HistoricalProjects[0].Label = "Local renamed label"
	incoming := []config.HistoricalProject{{ID: portable[0].ID, Label: portable[0].Label, Permissions: []config.HistoricalProjectPermission{{DestinationID: cfg.DestinationID(), Enabled: portable[0].Enabled}}}}
	merged := config.ReconcileHistoricalProjects(cfg.HistoricalProjects, incoming)
	if len(merged) != 1 || merged[0].Permissions[0].Enabled || merged[0].Label != "Local renamed label" {
		t.Fatal("pairing overrode local historical policy", merged)
	}
	// Strict older receivers reject the optional portable section before mutation.
	var older struct {
		Version int `json:"version"`
	}
	decoder := json.NewDecoder(strings.NewReader(`{"version":1,"historical_projects":` + string(raw) + `}`))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&older) == nil {
		t.Fatal("old receiver silently discarded historical policy")
	}
}
