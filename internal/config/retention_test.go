package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRetentionWireAndStickyComposition(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{`{"schema_version":1}`, 90}, {`{"schema_version":1,"retention_days":14}`, 14}, {`{"schema_version":1,"retention_days":0}`, 0}} {
		var cfg Config
		if err := json.Unmarshal([]byte(tc.raw), &cfg); err != nil || cfg.RetentionDays != tc.want {
			t.Fatalf("decode %s: %+v %v", tc.raw, cfg, err)
		}
		home := t.TempDir()
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, _, err := Load(home)
		if err != nil || loaded.RetentionDays != tc.want {
			t.Fatalf("load: %+v %v", loaded, err)
		}
		if tc.want == 0 {
			raw, err := json.Marshal(loaded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte(`retention-forever-v8`)) {
				t.Fatalf("missing fence %s", raw)
			}
			var old struct {
				SchemaVersion int `json:"schema_version"`
			}
			if json.Unmarshal(raw, &old) == nil {
				t.Fatal("published integer decoder accepted forever")
			}
			loaded.RetentionDays = 90
			loaded.RetentionProtection = false
			if err := Save(home, loaded); err != nil {
				t.Fatal(err)
			}
			loaded, _, err = Load(home)
			if err != nil || !loaded.RetentionProtection || loaded.RetentionDays != 90 {
				t.Fatalf("lost sticky protection %+v %v", loaded, err)
			}
		}
	}
	for _, days := range []int{-1, 36501} {
		var cfg Config
		if json.Unmarshal([]byte(`{"retention_days":`+jsonInt(days)+`}`), &cfg) == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	cfg := Config{RetentionDays: 0, GenerationProtection: true, CodexHistoryProtection: true, DurableStorageProtection: true}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Config
	if err := json.Unmarshal(raw, &loaded); err != nil || loaded.SchemaVersion != 8 || !loaded.RetentionProtection || !loaded.DurableStorageProtection {
		t.Fatalf("composition %+v %v", loaded, err)
	}
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRetentionOrdering(t *testing.T) {
	for _, tc := range []struct {
		next    int
		old     int
		shorter bool
	}{{0, 90, false}, {90, 0, true}, {0, 0, false}, {30, 90, true}, {90, 30, false}} {
		if RetentionShorter(tc.next, tc.old) != tc.shorter {
			t.Fatalf("ordering %+v", tc)
		}
	}
}

func TestRetentionRejectsNullInsteadOfDefaulting(t *testing.T) {
	for _, raw := range []string{`{"retention_days":null}`, `{"schema_version":{"version":8,"writer":"retention-forever-v8"},"retention_protection":true,"retention_days":null}`} {
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
			t.Fatalf("accepted a noninteger retention policy: %s", raw)
		}
	}
}

func TestForeverFenceKeepsDiscoveryFloorValidation(t *testing.T) {
	{
		cfg, _ := discoveryConfig(t)
		cfg.RetentionDays, cfg.RetentionProtection = 0, true
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var valid Config
		if err := json.Unmarshal(raw, &valid); err != nil {
			t.Fatal(err)
		}
		// A dominating fence must not turn the lower discovery authorization
		// into a legacy floor migration and reopen admissions from the past.
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
			t.Fatal("fence discarded discovery floor validation")
		}
	}
}

func TestStagingWriterRejectsMissingOrFutureCapability(t *testing.T) {
	for _, raw := range []string{`{"schema_version":{"version":10,"writer":"future-v10"},"import_staging_protection":true,"retention_days":0,"retention_protection":true}`, `{"schema_version":1,"import_staging_protection":true,"retention_days":90}`, `{"schema_version":9,"retention_days":90}`} {
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
			t.Fatalf("accepted unsupported staging configuration: %s", raw)
		}
	}
}
