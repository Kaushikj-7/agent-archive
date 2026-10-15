package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestSearchAndNativeFencesComposeInEitherConsentOrder(t *testing.T) {
	for _, nativeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "search-first", true: "native-first"}[nativeFirst], func(t *testing.T) {
			home := t.TempDir()
			cfg := Config{RetentionDays: 90}
			addNative := func() {
				cfg.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
			}
			if nativeFirst {
				addNative()
			} else if err := SetSearchIndexing(&cfg, true); err != nil {
				t.Fatal(err)
			}
			if err := Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			var err error
			cfg, _, err = Load(home)
			if err != nil {
				t.Fatal(err)
			}
			if nativeFirst {
				if err = SetSearchIndexing(&cfg, true); err != nil {
					t.Fatal(err)
				}
			} else {
				addNative()
			}
			if err = Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			cfg, _, err = Load(home)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.SearchIndexing || !cfg.SearchLifecycleProtection || !cfg.NativeAuthorityProtection || cfg.SchemaVersion != 16 || !bytes.Contains(raw, []byte(`"base":{"version":16,"writer":"search-lifecycle-v16"}`)) {
				t.Fatal("combined authority lost", string(raw))
			}
			// Disabling/removing capabilities and replacing settings retain both fences,
			// without granting the replacement snapshot search consent.
			if err = SetSearchIndexing(&cfg, false); err != nil {
				t.Fatal(err)
			}
			cfg.NativeRoots = nil
			if err = Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			if err = Save(home, Config{RetentionDays: 90}); err != nil {
				t.Fatal(err)
			}
			cfg, _, err = Load(home)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SearchIndexing || !cfg.SearchLifecycleProtection || !cfg.NativeAuthorityProtection || len(cfg.NativeRoots) != 0 {
				t.Fatal("replacement lost sticky fences or gained consent", cfg)
			}
			var snapshot struct {
				Before Config `json:"before"`
			}
			raw, err = json.Marshal(struct {
				Before Config `json:"before"`
			}{cfg})
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &snapshot); err != nil || !snapshot.Before.SearchLifecycleProtection || !snapshot.Before.NativeAuthorityProtection {
				t.Fatal("rollback snapshot lost composed authority", err)
			}
		})
	}
}

func TestCombinedSearchNativeConfigRejectsAmbiguousOrMissingAuthority(t *testing.T) {
	cfg := Config{RetentionDays: 90, NativeRoots: []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}}
	if err := SetSearchIndexing(&cfg, true); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	valid := string(raw)
	mutations := []string{
		strings.Replace(valid, `"writer":"search-lifecycle-v16"`, `"writer":"future"`, 1),
		strings.Replace(valid, `"base":{"version":16`, `"base":{"version":17`, 1),
		strings.Replace(valid, `"search_lifecycle_protection":true`, `"search_lifecycle_protection":false`, 1),
		strings.Replace(valid, `"search_lifecycle_protection":true,`, ``, 1),
		strings.Replace(valid, `"search_lifecycle_protection":true`, `"search_lifecycle_protection":true,"SEARCH_LIFECYCLE_PROTECTION":true`, 1),
		strings.Replace(valid, `"search_lifecycle_protection":true`, `"search_lifecycle_protection":false,"search_lifecycle_protection":true`, 1),
		strings.Replace(valid, `"native_authority_protection":true`, `"native_authority_protection":true,"NATIVE_AUTHORITY_PROTECTION":true`, 1),
		strings.Replace(valid, `"writer":"search-lifecycle-v16"`, `"writer":"search-lifecycle-v16","WRITER":"search-lifecycle-v16"`, 1),
		strings.Replace(valid, `"base":{"version":16,"writer":"search-lifecycle-v16"}`, `"base":null`, 1),
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema_version"] = json.RawMessage(`{"version":16,"writer":"search-lifecycle-v16"}`)
	missingOuter, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	mutations = append(mutations, string(missingOuter))
	for _, mutated := range mutations {
		if mutated == valid {
			t.Fatal("inert mutation", mutated)
		}
		var loaded Config
		if json.Unmarshal([]byte(mutated), &loaded) == nil {
			t.Fatal("accepted malformed combined authority", mutated)
		}
	}
}

func TestSearchBaseDoesNotAcceptUnwrappedNativeSelectors(t *testing.T) {
	cfg := Config{RetentionDays: 90}
	if err := SetSearchIndexing(&cfg, true); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{`"native_authority_protection":false,`, `"native_roots":[],`} {
		mutated := append([]byte("{"+selector), raw[1:]...)
		var loaded Config
		if json.Unmarshal(mutated, &loaded) == nil {
			t.Fatal("native selector accepted without outer fence", string(mutated))
		}
	}
}
