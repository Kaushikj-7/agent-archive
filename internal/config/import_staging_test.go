package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportStagingFencePreservesRetentionAndPermissionFloors(t *testing.T) {
	cfg, _ := discoveryConfig(t)
	cfg.RetentionDays, cfg.RetentionProtection = 0, true
	cfg.ImportStagingProtection, cfg.DurableStorageProtection = true, true
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"version":9,"writer":"import-staging-v9"`)) {
		t.Fatalf("staging fence did not dominate retention %s", raw)
	}
	var loaded Config
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != 9 || !loaded.RetentionProtection || loaded.RetentionDays != 0 {
		t.Fatalf("lower retention contract lost %+v", loaded)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	auth := wire["discovery"].(map[string]any)["authorizations"].([]any)[0].(map[string]any)
	delete(auth, "native_start_floor")
	corrupt, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var rejected Config
	if err := json.Unmarshal(corrupt, &rejected); err == nil {
		t.Fatal("staging fence reopened legacy permission migration")
	}
}

func TestImportStagingGuardPersistsStickyCapabilityBeforeWrite(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	if err := WithImportStaging(home, func(g DurableStorageGuard) error {
		cfg, found, err := Load(home)
		if err != nil || !found || cfg.SchemaVersion != 9 || !cfg.ImportStagingProtection || !cfg.DurableStorageProtection {
			t.Fatalf("evidence callback preceded full durable fence %+v %v", cfg, err)
		}
		return g.CheckHome(home)
	}); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(home)
	if err != nil || !cfg.ImportStagingProtection || cfg.SchemaVersion != 9 {
		t.Fatalf("ordinary save removed staging protection %+v %v", cfg, err)
	}
	cfg.SchemaVersion = 10
	if err := Save(home, cfg); err == nil {
		t.Fatal("save downgraded unknown future capability")
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNumericImportStagingRequiresCapability(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"schema_version":9,"import_staging_protection":true,"retention_days":90}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != 9 || !cfg.ImportStagingProtection {
		t.Fatal("numeric staging capability lost")
	}
}
