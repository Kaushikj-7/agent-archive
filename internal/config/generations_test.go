package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestGenerationFencePreservesPoliciesAndRollback(t *testing.T) {
	t.Parallel()
	protected, _ := blanketConfig(t)
	for _, initial := range []Config{{RetentionDays: 90, MachineID: "m", Archive: archive.Config{Enabled: true}}, protected} {
		home := t.TempDir()
		initial.GenerationProtection = true
		if err := Save(home, initial); err != nil {
			t.Fatal(err)
		}
		loaded, found, err := Load(home)
		if err != nil || !found || !loaded.GenerationProtection || loaded.SchemaVersion != 4 {
			t.Fatalf("load protected %#v %v", loaded, err)
		}
		originalDiscovery := loaded.Discovery
		originalCodex := loaded.CodexCapture
		encoded, err := json.Marshal(loaded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte(`"writer":"archive-generations-v4"`)) {
			t.Fatalf("missing writer fence: %s", encoded)
		}
		// A setup rollback restoring legacy policy keeps the irreversible writer
		// capability floor, without reopening blanket permissions.
		rollback := Config{RetentionDays: 90, MachineID: "m", Archive: archive.Config{Enabled: false}}
		PreserveWriterFence(&rollback, loaded)
		if err := Save(home, rollback); err != nil {
			t.Fatal(err)
		}
		rolled, _, err := Load(home)
		if err != nil || !rolled.GenerationProtection || rolled.SchemaVersion != 4 || rolled.Archive.Enabled {
			t.Fatalf("rollback lost floor: %#v %v", rolled, err)
		}
		if originalCodex != nil && rolled.EffectiveCodexCaptureScope() != CodexIncludedProjects {
			t.Fatal("rollback resurrected blanket scope")
		}
		_ = originalDiscovery
		// Even an attempted plain Save cannot strip the floor.
		if err := Save(home, Config{RetentionDays: 90, MachineID: "m"}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil || !bytes.Contains(raw, []byte(`archive-generations-v4`)) {
			t.Fatalf("save stripped protection: %s %v", raw, err)
		}
	}
}

func TestGenerationFenceRejectsMissingPolicyFloors(t *testing.T) {
	t.Parallel()
	cfg, _ := blanketConfig(t)
	cfg.GenerationProtection = true
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["generation_protection"] = json.RawMessage(`false`)
	broken, _ := json.Marshal(doc)
	var out Config
	if err := json.Unmarshal(broken, &out); err == nil {
		t.Fatal("v4 fence accepted without generation protection")
	}
	doc["generation_protection"] = json.RawMessage(`true`)
	doc["codex_capture"] = json.RawMessage(`{"scope":"all-projects","authorization":{"generation":"g","agent":"codex","destination_id":"d","intervals":[{"start":"2026-10-01T00:00:00Z"}]}}`)
	broken, _ = json.Marshal(doc)
	if err := json.Unmarshal(broken, &out); err == nil {
		t.Fatal("v4 fence accepted missing native start floor")
	}
}

// This acceptance executes an explicitly supplied binary built from the PR's
// recorded pre-generation base. It uses the existing disposable native writer
// fixture; absence of that binary is a skip, never claimed downgrade evidence.
func TestPriorGenerationWriterRefusesV4(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_PRIOR_GENERATION_BINARY")
	if binary == "" {
		t.Skip("requires recorded prior-main writer binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("prior writer executable must be absolute")
	}
	root, pause := publishedPauseFixture(t, binary)
	control := filepath.Join(root, "control")
	protected := filepath.Join(root, "protected")
	cfg, _ := blanketConfig(t)
	if err := Save(control, cfg); err != nil {
		t.Fatal(err)
	}
	if out, err := pause(control); err != nil {
		t.Fatalf("prior v3 writer control failed: %v %s", err, out)
	}
	cfg.GenerationProtection = true
	if err := Save(protected, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(protected, "config.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := pause(protected); err == nil {
		t.Fatalf("prior writer accepted v4: %s", out)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("prior writer mutated protected state: %v", err)
	}
}
