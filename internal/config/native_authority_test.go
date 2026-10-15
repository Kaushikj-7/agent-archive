package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/destination"
)

func TestNativeAuthorityComposesWithExactBaseFences(t *testing.T) {
	fixtures := []Config{
		{RetentionDays: 90, SchemaVersion: 1},
		{RetentionDays: 90, CodexHistoryProtection: true},
		{RetentionDays: 90, SettledCursorSelectionProtection: true, StagedSelectionProtection: true, ImportStagingProtection: true, PublicationCompositionProtection: true, DurableStorageProtection: true},
		{RetentionDays: 90, ScopedPickerProtection: true, SettledCursorSelectionProtection: true, StagedSelectionProtection: true, ImportStagingProtection: true, PublicationCompositionProtection: true, DurableStorageProtection: true, Storage: destination.Config{ArchiveFormat: destination.FormatCatalogV4}},
	}
	for _, cfg := range fixtures {
		base, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "r1"}}
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var loaded Config
		if err = json.Unmarshal(raw, &loaded); err != nil {
			t.Fatal(string(raw), err)
		}
		if !loaded.NativeAuthorityProtection || len(loaded.NativeRoots) != 1 {
			t.Fatal("lost native authority")
		}
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(base, &doc)
		lower := doc["schema_version"]
		var wrapped map[string]json.RawMessage
		_ = json.Unmarshal(raw, &wrapped)
		var outer nativeAuthorityVersion
		_ = json.Unmarshal(wrapped["schema_version"], &outer)
		if !bytes.Equal(outer.Base, lower) {
			t.Fatal("base fence changed", string(raw))
		}
		if loaded.SchemaVersion == 16 {
			t.Fatal("outer fence replaced internal lower semantics")
		}
		for _, mutation := range []string{
			strings.Replace(string(raw), `"native_authority_protection":true`, `"native_authority_protection":false`, 1),
			strings.Replace(string(raw), `"native_authority_protection":true`, `"native_authority_protection":true,"NATIVE_AUTHORITY_PROTECTION":true`, 1),
			strings.Replace(string(raw), `"version":16`, `"version":17`, 1),
			strings.Replace(string(raw), `"writer":"native-authority-v16"`, `"writer":"future"`, 1),
			strings.Replace(string(raw), `"base":`+string(lower), `"base":null`, 1),
			strings.Replace(string(raw), `"base":`+string(lower), `"base":{"version":16,"writer":"native-authority-v16","base":1}`, 1),
			strings.Replace(string(raw), `"base":`+string(lower), `"base":{"version":99,"writer":"future"}`, 1),
		} {
			var refused Config
			if json.Unmarshal([]byte(mutation), &refused) == nil {
				t.Fatal("accepted invalid fence", mutation)
			}
		}
	}
}

func TestNativeAuthoritySaveAndRollbackKeepFenceAfterRootRemoval(t *testing.T) {
	home := t.TempDir()
	cfg := publishedWriterFiniteControl()
	cfg.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, publishedWriterFiniteControl()); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := Load(home)
	if err != nil || !found || !loaded.NativeAuthorityProtection || len(loaded.NativeRoots) != 0 {
		t.Fatal(loaded, err)
	}
	raw, err := json.Marshal(struct {
		Before Config `json:"before"`
	}{loaded})
	if err != nil || !bytes.Contains(raw, []byte("native-authority-v16")) {
		t.Fatal("rollback snapshot lost protection", err)
	}
	raw, err = os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || !bytes.Contains(raw, []byte("native-authority-v16")) {
		t.Fatal("save dropped fence", err)
	}
}

func TestPriorWriterRefusesNativeAuthorityBeforeMutation(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_SOURCE_OLD_BINARY")
	if binary == "" {
		t.Skip("requires exact isolated source-built prior writer; not published acceptance")
	}
	root, pause := publishedPauseFixture(t, binary)
	home := filepath.Join(root, "archive")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := publishedWriterFiniteControl()
	cfg.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := pause(home); err == nil || !strings.Contains(string(out), "writer fence") {
		t.Fatal("prior writer did not refuse", string(out), err)
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("prior writer changed native authority", err)
	}
}

func TestNativeRootPathChangesRequireNewRevision(t *testing.T) {
	home := t.TempDir()
	cfg := publishedWriterFiniteControl()
	cfg.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.NativeRoots[0].Canonical = "/moved"
	if err := Save(home, cfg); err == nil {
		t.Fatal("root changed without invalidating lookup revision")
	}
	cfg.NativeRoots[0].Revision = "two"
	if err := Save(home, cfg); err != nil {
		t.Fatal("explicit reconciliation revision refused", err)
	}
}

func TestNativeAuthorityCannotBeIntroducedBesideOlderSetupJournal(t *testing.T) {
	home := t.TempDir()
	cfg := publishedWriterFiniteControl()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "setup-transaction.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
	if err := Save(home, cfg); err == nil {
		t.Fatal("native authority committed beside older rollback bytes")
	}
	loaded, _, err := Load(home)
	if err != nil || loaded.NativeAuthorityProtection || len(loaded.NativeRoots) != 0 {
		t.Fatal("failed introduction changed existing configuration", err)
	}
}

func TestNativeAuthorityRefusesUnsupportedBaseBeforeSave(t *testing.T) {
	home := t.TempDir()
	cfg := publishedWriterFiniteControl()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{-1, 2, 16, 99} {
		invalid := Config{SchemaVersion: version, RetentionDays: 90, NativeRoots: []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}}
		if _, err := json.Marshal(invalid); err == nil {
			t.Errorf("serialized unsupported native base %d", version)
		}
		if err := Save(home, invalid); err == nil {
			t.Errorf("saved unsupported native base %d", version)
		}
		after, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("invalid base %d changed settings: %v", version, err)
		}
	}
}
