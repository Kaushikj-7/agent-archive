package config

import (
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDurableStorageGuardExpiresAndPersistsActualFloor(t *testing.T) {
	home := durableTestHome(t)
	if e := Save(home, Config{RetentionDays: 90, MachineID: "synthetic", GenerationProtection: true, CodexHistoryProtection: true, CodexNameLookup: CodexNameLookupNative}); e != nil {
		t.Fatal(e)
	}
	var retained DurableStorageGuard
	if e := WithDurableStorage(home, func(g DurableStorageGuard) error {
		retained = g
		cfg, found, e := Load(home)
		if e != nil || !found || cfg.SchemaVersion != 7 || !cfg.DurableStorageProtection || !cfg.GenerationProtection || !cfg.CodexHistoryProtection || cfg.CodexNameLookup != CodexNameLookupNative {
			t.Fatalf("actual floor: %+v %v", cfg, e)
		}
		return g.CheckHome(home)
	}); e != nil {
		t.Fatal(e)
	}
	if e := retained.CheckHome(home); e == nil {
		t.Fatal("expired guard accepted")
	}
	if e := Save(home, Config{RetentionDays: 90, MachineID: "rollback"}); e != nil {
		t.Fatal(e)
	}
	cfg, _, e := Load(home)
	if e != nil || cfg.SchemaVersion != 7 || !cfg.DurableStorageProtection {
		t.Fatalf("rollback lost floor: %+v %v", cfg, e)
	}
}

func TestDurableStorageRefusesMissingUnknownAndSetupConfig(t *testing.T) {
	for _, raw := range []string{"", `{"schema_version":{"version":6,"writer":"history-lifecycle-v6"},"history_protection":true}`, `{"schema_version":{"version":5,"writer":"staged-imports-v5"},"durable_import_protection":true}`, `{"schema_version":{"version":7,"writer":"durable-storage-v7"}}`, `{"schema_version":{"version":8,"writer":"publication-composition-v8"},"publication_composition_protection":true}`} {
		t.Run(raw, func(t *testing.T) {
			home := durableTestHome(t)
			if raw != "" {
				if e := os.WriteFile(filepath.Join(home, "config.json"), []byte(raw), 0600); e != nil {
					t.Fatal(e)
				}
			}
			called := false
			if e := WithDurableStorage(home, func(DurableStorageGuard) error { called = true; return nil }); e == nil || called {
				t.Fatalf("gate accepted unsupported config: %v", e)
			}
			if _, e := os.Lstat(filepath.Join(home, "hooks.lock")); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("refusal allocated lock: %v", e)
			}
			if raw != "" {
				after, e := os.ReadFile(filepath.Join(home, "config.json"))
				if e != nil || string(after) != raw {
					t.Fatal("refusal changed config", e)
				}
			}
		})
	}
	home := durableTestHome(t)
	if e := Save(home, Config{RetentionDays: 90, MachineID: "synthetic"}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(home, "setup-transaction.json"), []byte("owed"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := WithDurableStorage(home, func(DurableStorageGuard) error { return nil }); e == nil {
		t.Fatal("setup transaction bypassed")
	}
	cfg, _, e := Load(home)
	if e != nil || cfg.DurableStorageProtection {
		t.Fatal("setup refusal changed floor", e)
	}
}

func TestDurableStorageSymlinkHomeRefusesBeforeLockAllocation(t *testing.T) {
	parent := durableTestHome(t)
	outside := durableTestHome(t)
	home := filepath.Join(parent, "archive")
	if e := os.Symlink(outside, home); e != nil {
		t.Fatal(e)
	}
	if e := WithDurableStorage(home, func(DurableStorageGuard) error { return errors.New("unexpected") }); e == nil {
		t.Fatal("symlink accepted")
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatalf("outside writes: %v %v", entries, e)
	}
}

func durableTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if e := os.Chmod(home, 0700); e != nil {
		t.Fatal(e)
	}
	return home
}

func TestDurableStorageGuardRejectsReplacedConfig(t *testing.T) {
	home := durableTestHome(t)
	if err := Save(home, Config{RetentionDays: 90, MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := WithDurableStorage(home, func(g DurableStorageGuard) error {
		raw, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil {
			return err
		}
		next := filepath.Join(home, "replacement.json")
		if err = os.WriteFile(next, raw, 0600); err != nil {
			return err
		}
		if err = os.Rename(next, filepath.Join(home, "config.json")); err != nil {
			return err
		}
		if err = g.CheckHome(home); err == nil {
			t.Fatal("replaced configuration retained authority")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertOldWriterRefusesDurableStorage(t *testing.T, binary string) {
	t.Helper()
	root, pause := publishedPauseFixture(t, binary)
	home := filepath.Join(root, "archive")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{RetentionDays: 90, MachineID: "synthetic-old-storage-writer", Archive: archive.Config{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if out, err := pause(home); err != nil {
		t.Fatalf("source/released scalar control failed: %v %s", err, out)
	}
	if err := WithDurableStorage(home, func(g DurableStorageGuard) error { return g.CheckHome(home) }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(home, "publication-evidence")
	if err := os.Mkdir(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(evidence, "original")
	if err := os.WriteFile(sentinel, []byte("retained-original"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := pause(home)
	if err == nil || (!strings.Contains(string(out), "schema_version") && !strings.Contains(string(out), "writer fence")) {
		t.Fatalf("old writer accepted storage7: %v %s", err, out)
	}
	t.Logf("old writer actual refusal: %s", strings.TrimSpace(string(out)))
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("old writer mutated protected config", err)
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "retained-original" {
		t.Fatal("old writer mutated retained evidence", err)
	}
}

func TestPublishedWriterRefusesDurableStorageConfiguration(t *testing.T) {
	assertOldWriterRefusesDurableStorage(t, publishedWriterBinary(t))
}

func TestSourceBuiltWriterRefusesDurableStorageConfiguration(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_SOURCE_OLD_BINARY")
	if binary == "" {
		t.Skip("requires isolated source-built prior writer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("source-built writer must be an absolute executable")
	}
	assertOldWriterRefusesDurableStorage(t, binary)
}

func TestDurableStorageBinaryRefusesFutureCompositionConfiguration(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_STORAGE_BINARY")
	if binary == "" {
		t.Skip("requires the isolated current storage writer executable")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("storage writer must be an absolute executable")
	}
	root, pause := publishedPauseFixture(t, binary)
	home := filepath.Join(root, "archive")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{RetentionDays: 90, MachineID: "synthetic-storage-writer", Archive: archive.Config{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if out, err := pause(home); err != nil {
		t.Fatalf("legacy control failed: %v %s", err, out)
	}
	if err := WithDurableStorage(home, func(g DurableStorageGuard) error { return g.CheckHome(home) }); err != nil {
		t.Fatal(err)
	}
	if out, err := pause(home); err != nil {
		t.Fatalf("supported storage7 control failed: %v %s", err, out)
	}
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var future map[string]json.RawMessage
	if err := json.Unmarshal(before, &future); err != nil {
		t.Fatal(err)
	}
	future["schema_version"] = json.RawMessage(`{"version":8,"writer":"publication-composition-v8"}`)
	future["publication_composition_protection"] = json.RawMessage(`true`)
	encoded, err := json.MarshalIndent(future, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(home, "publication-evidence", "original")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("retained-original"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := pause(home)
	if err == nil || (!strings.Contains(string(out), "schema_version") && !strings.Contains(string(out), "writer fence")) {
		t.Fatalf("storage7 accepted future8: %v %s", err, out)
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || string(after) != raw {
		t.Fatal("future refusal mutated config", err)
	}
	retained, err := os.ReadFile(sentinel)
	if err != nil || string(retained) != "retained-original" {
		t.Fatal("future refusal changed original evidence", err)
	}
	t.Logf("actual storage7 writer refused future8: %s", strings.TrimSpace(string(out)))
}
