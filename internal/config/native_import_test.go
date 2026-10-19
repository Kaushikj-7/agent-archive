package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeImportFenceComposesWithoutLivePermission(t *testing.T) {
	t.Parallel()
	for _, admission := range []bool{false, true} {
		for _, deletion := range []bool{false, true} {
			cfg := Config{MachineID: "synthetic", RetentionDays: 90, NativeImportProtection: true, NativeAdmissionProtection: admission, DeletionProtection: deletion, DurableStorageProtection: deletion, SourceSetPublicationProtection: deletion}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Config
			if err := json.Unmarshal(raw, &loaded); err != nil {
				t.Fatal(err)
			}
			if !loaded.NativeImportProtection || !loaded.NativeAuthorityProtection || loaded.NativeAdmissionProtection != admission || loaded.DeletionProtection != deletion || loaded.MuseCapture != nil || loaded.Archive.Enabled {
				t.Fatal("fence changed an independent capability or granted capture")
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			native := doc["schema_version"]
			if deletion {
				var outer nativeAuthorityVersion
				if err := json.Unmarshal(native, &outer); err != nil || outer.Writer != deletionWriter || outer.Import != "" {
					t.Fatalf("import changed outer deletion fence: %v", err)
				}
				native = outer.Base
			}
			if err := validateDeletionGuardVersion(native); err != nil {
				t.Fatalf("independent deletion guard rejected import capability: %v", err)
			}
			var old struct {
				Version   int             `json:"version"`
				Writer    string          `json:"writer"`
				Base      json.RawMessage `json:"base"`
				Admission string          `json:"admission,omitempty"`
			}
			decoder := json.NewDecoder(bytes.NewReader(native))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&old); err == nil {
				t.Fatal("frozen native writer silently ignored import authority")
			}
		}
	}
}

func TestNativeImportFenceRefusesAmbiguousSelectors(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Config{RetentionDays: 90, NativeImportProtection: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"import":"muse-historical-import"`, `"import":"future"`},
		{`,"import":"muse-historical-import"`, ``},
		{`"native_import_protection":true`, `"native_import_protection":false`},
		{`"native_import_protection":true`, `"native_import_protection":null`},
		{`"native_import_protection":true`, `"native_import_protection":true,"NATIVE_IMPORT_PROTECTION":true`},
		{`"import":"muse-historical-import"`, `"Import":"muse-historical-import"`},
		{`"import":"muse-historical-import"`, `"import":"muse-historical-import","import":"muse-historical-import"`},
		{`"import":"muse-historical-import"`, `"import":null`},
	} {
		changed := bytes.Replace(raw, []byte(pair[0]), []byte(pair[1]), 1)
		if bytes.Equal(raw, changed) {
			t.Fatalf("mutation did not reach selector %s", pair[0])
		}
		var cfg Config
		if err := json.Unmarshal(changed, &cfg); err == nil {
			t.Fatal("accepted ambiguous historical capability")
		}
	}
}

func TestNativeImportFenceSurvivesSaveAndRollback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	initial := Config{RetentionDays: 90, NativeImportProtection: true}
	if err := Save(home, initial); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := Load(home)
	if err != nil || !found || !loaded.NativeImportProtection || !loaded.NativeAuthorityProtection {
		t.Fatalf("save lost permanent import capability: %v", err)
	}
	var rollback Config
	PreserveWriterFence(&rollback, loaded)
	raw, err := json.Marshal(rollback)
	if err != nil || !bytes.Contains(raw, []byte(`"import":"muse-historical-import"`)) {
		t.Fatalf("rollback lost exact import selector: %v", err)
	}
}

func TestNativeImportFenceCannotBeIntroducedDuringPendingSetup(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Config{RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "setup-transaction.json"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{RetentionDays: 90, NativeImportProtection: true}); err == nil {
		t.Fatal("historical authority introduced across pending setup")
	}
	cfg, found, err := Load(home)
	if err != nil || !found || cfg.NativeImportProtection {
		t.Fatalf("refused save changed authority: %v", err)
	}
}
