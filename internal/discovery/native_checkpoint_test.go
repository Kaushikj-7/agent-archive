package discovery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestNativeCatalogMemberViewsValidateBoundaries(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, ` { "version":6,"nested":{"comma,":"escaped\\\" }","array":[1,{"x":"y"}]},"native_inventory":null } `} {
		got, ok := nativeCatalogMembers([]byte(raw))
		var want map[string]json.RawMessage
		if !ok || json.Unmarshal([]byte(raw), &want) != nil {
			t.Fatal("valid catalog refused", raw)
		}
		for key, value := range got {
			var a, b any
			if json.Unmarshal(value, &a) != nil || json.Unmarshal(want[key], &b) != nil || !reflect.DeepEqual(a, b) {
				t.Fatal("member boundary changed", key)
			}
		}
		if len(got) != len(want) {
			t.Fatal("member omitted")
		}
	}
	for _, raw := range []string{`[]`, `{"version":6`, `{"v":1,"v":2}`, `{"v":1,"\u0076":2}`, `{"x":"unterminated}`, `{"x":1,}`} {
		if _, ok := nativeCatalogMembers([]byte(raw)); ok {
			t.Fatal("invalid catalog accepted", raw)
		}
	}
}

func TestOrdinaryCatalogReplacementDropsAbsentOwnedFields(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "discovery-catalog.json")
	before := []byte(`{"version":6,"priority":{"removed":1},"retries":[{"locator":"removed"}],"classifier":99,"native_inventory":{"future":true},"future_member":{"keep":true},"native_coverage":{"retired":true}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := persistNativeCatalog(t.Context(), nil, path, catalog{Version: 6}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if json.Unmarshal(raw, &after) != nil {
		t.Fatal("invalid catalog")
	}
	for _, key := range []string{"priority", "retries", "classifier", "native_coverage"} {
		if after[key] != nil {
			t.Fatal("retired field survived", key)
		}
	}
	for _, key := range []string{"native_inventory", "future_member"} {
		if after[key] == nil {
			t.Fatal("unrelated member dropped", key)
		}
	}
}

func TestOrdinaryCatalogRebuildsCorruptCheckpoint(t *testing.T) {
	t.Parallel()
	for _, before := range []string{`{"version":`, `{"version":5,"future_member":true}`, `{"version":6,"version":6}`} {
		path := filepath.Join(t.TempDir(), "discovery-catalog.json")
		if err := os.WriteFile(path, []byte(before), 0600); err != nil {
			t.Fatal(err)
		}
		if err := persistNativeCatalog(t.Context(), nil, path, catalog{Version: 6}); err != nil {
			t.Fatal("rebuild refused", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		object, ok := nativeCatalogMembers(raw)
		if !ok || string(object["version"]) != "6" || object["future_member"] != nil {
			t.Fatal("invalid rebuild", string(raw))
		}
	}
}

func TestNativeCatalogReadRefusesGrowthBeyondReservedInput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "discovery-catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":6}`), 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	release, limit, err := nativeCatalogScratch(path, 0, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.WriteFile(path, []byte(`{"version":6,"growth":"larger"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := boundedCatalogBytes(path, limit); !errors.Is(err, errCatalogChanged) {
		t.Fatal("unreserved growth read", err)
	}
}

func FuzzNativeCatalogMembers(f *testing.F) {
	for _, seed := range []string{`{}`, `{"version":6,"native_inventory":{"x":["a,b",null,{}]}}`, `{"a":"\\\"","b":-1.25e+3}`, `{"v":1,"\u0076":2}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			return
		}
		members, ok := nativeCatalogMembers(raw)
		if !ok {
			return
		}
		var decoded map[string]json.RawMessage
		if json.Unmarshal(raw, &decoded) != nil || len(members) != len(decoded) {
			t.Fatal("accepted invalid object")
		}
		for key, value := range members {
			var a, b any
			if json.Unmarshal(value, &a) != nil || json.Unmarshal(decoded[key], &b) != nil || !reflect.DeepEqual(a, b) {
				t.Fatal("changed member", key)
			}
		}
	})
}
