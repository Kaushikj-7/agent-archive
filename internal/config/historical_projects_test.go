package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func historicalPolicyFixture() (Config, archive.SessionRegistration) {
	cfg := Config{MachineID: "test", RetentionDays: 90, Archive: archive.Config{Enabled: true}, HistoricalProjectProtection: true, ImportedHarnesses: []string{"claude"}}
	destination := cfg.DestinationID()
	const id = "1234567890abcdef1234567890abcdef"
	cfg.HistoricalProjects = []HistoricalProject{{ID: id, Label: "Retired service", Permissions: []HistoricalProjectPermission{{DestinationID: destination, Enabled: true}}}}
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: "claude"}, ProjectID: id, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("2026-10-12-1"), DestinationID: destination, HistoricalProject: &archive.HistoricalProjectProof{Version: 1, ProjectID: id, DestinationID: destination, OriginalCwd: "/private/old", CanonicalCwd: "/private/old", Label: "Retired service"}}
	return cfg, reg
}

func TestHistoricalImportPermissionAndExclusions(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name    string
		change  func(*Config, *archive.SessionRegistration)
		allowed bool
	}{
		{name: "enabled", allowed: true},
		{name: "last imported app removed from no-hooks config", change: func(c *Config, _ *archive.SessionRegistration) { c.ImportedHarnesses = nil }},
		{name: "rename", change: func(c *Config, _ *archive.SessionRegistration) { c.HistoricalProjects[0].Label = "New label" }, allowed: true},
		{name: "disabled", change: func(c *Config, _ *archive.SessionRegistration) {
			c.HistoricalProjects[0].Permissions[0].Enabled = false
		}},
		{name: "missing fence", change: func(c *Config, _ *archive.SessionRegistration) { c.HistoricalProjectProtection = false }},
		{name: "hook impersonation", change: func(_ *Config, r *archive.SessionRegistration) { r.Origin = archive.SessionOriginHook }},
		{name: "unknown identity", change: func(_ *Config, r *archive.SessionRegistration) { r.ProjectID = strings.Repeat("a", 32) }},
		{name: "destination switch", change: func(c *Config, _ *archive.SessionRegistration) { c.Storage.Bucket = "different" }},
		{name: "source exclusion", change: func(c *Config, _ *archive.SessionRegistration) {
			c.Archive.Projects = []archive.ProjectActivation{{Root: "/private", Included: false}}
		}},
		{name: "canonical exclusion", change: func(c *Config, r *archive.SessionRegistration) {
			r.HistoricalProject.CanonicalCwd = "/excluded/old"
			c.Archive.Projects = []archive.ProjectActivation{{Root: "/excluded", Included: false}}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			cfg, reg := historicalPolicyFixture()
			if scenario.change != nil {
				scenario.change(&cfg, &reg)
			}
			if cfg.AcceptSession(reg) != scenario.allowed {
				t.Fatalf("acceptance wanted %v", scenario.allowed)
			}
		})
	}
}

func TestHistoricalWriterFloorComposesAndStaysPermanent(t *testing.T) {
	t.Parallel()
	cfg, _ := historicalPolicyFixture()
	cfg.ImportStagingProtection, cfg.RetentionProtection, cfg.DurableStorageProtection, cfg.CodexHistoryProtection, cfg.GenerationProtection = true, true, true, true, true
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"writer":"historical-projects-v10"`) {
		t.Fatal(string(raw))
	}
	var decoded Config
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 10 || !decoded.ImportStagingProtection {
		t.Fatal("lower protections lost")
	}
	next := Config{RetentionDays: 90}
	PreserveWriterFence(&next, decoded)
	raw, err = json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"writer":"historical-projects-v10"`) {
		t.Fatal("clearing projects downgraded writer")
	}
	var predecessor struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(raw, &predecessor) == nil {
		t.Fatal("published scalar decoder accepted historical config")
	}
	for _, writer := range []string{"import-staging-v9", "retention-forever-v8", "durable-storage-v7", "codex-history-v5", "archive-generations-v4"} {
		prior := strings.Replace(string(raw), `"version":10`, `"version":9`, 1)
		prior = strings.Replace(prior, "historical-projects-v10", writer, 1)
		if json.Unmarshal([]byte(prior), new(Config)) == nil {
			t.Fatalf("inconsistent predecessor %s accepted", writer)
		}
	}
}

func TestHistoricalPairingPreservesLocalDisabledPolicyAndLabels(t *testing.T) {
	t.Parallel()
	cfg, _ := historicalPolicyFixture()
	local := cfg.HistoricalProjects[0]
	local.Label = "Local label"
	local.Permissions[0].Enabled = false
	incoming := cfg.HistoricalProjects[0]
	incoming.Label = "Remote label"
	incoming.Permissions = append(incoming.Permissions, HistoricalProjectPermission{DestinationID: strings.Repeat("a", 64), Enabled: true})
	merged := ReconcileHistoricalProjects([]HistoricalProject{local}, []HistoricalProject{incoming})
	if len(merged) != 1 || merged[0].Label != "Local label" || merged[0].Permissions[0].Enabled || len(merged[0].Permissions) != 2 {
		t.Fatalf("policy widened: %+v", merged)
	}
}
