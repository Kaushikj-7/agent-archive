package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	for _, raw := range []string{`[]`, `{"version":6`, `{"v":1,"v":2}`, `{"v":1,"\u0076":2}`, `{"x":"unterminated}`, `{"x":1,}`, "{}\v", "\f{}"} {
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
		encoded, err := encodeNativeCatalogMembers(members)
		if err != nil {
			return
		}
		roundtrip, ok := nativeCatalogMembers(encoded)
		if !ok || len(roundtrip) != len(members) {
			t.Fatal("writer emitted an unreadable catalog")
		}
		for key, value := range roundtrip {
			var a, b any
			if json.Unmarshal(value, &a) != nil || json.Unmarshal(members[key], &b) != nil || !reflect.DeepEqual(a, b) {
				t.Fatal("writer changed a member", key)
			}
		}
	})
}

func TestNativeCatalogLargeUnrelatedMemberFitsChargedScratch(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "discovery-catalog.json")
	before := append([]byte(`{"version":6,"large_unrelated":"`), bytes.Repeat([]byte("x"), 2<<20)...)
	before = append(before, []byte(`"}`)...)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	replacement := []byte(`{"version":1,"progress":true}`)
	// Three old-input lengths plus member/output overhead and fixed scratch:
	// retained input/key maps + one exact output. CAS uses only32KiB scratch.
	membersBefore, ok := nativeCatalogMembers(before)
	if !ok {
		t.Fatal("invalid unrelated-member fixture")
	}
	membersBefore["native_inventory"] = replacement
	outputSize, err := nativeCatalogOutputSize(membersBefore)
	if err != nil {
		t.Fatal(err)
	}
	required := 3*int64(len(before)) + 3*int64(len(replacement)) + (512 << 10)
	required += max(int64(outputSize)-int64(len(before))-int64(len(replacement)), 0)
	refused := agentapi.NewNativeReadBudget(required - 1)
	if err := mergeNativeCatalog(t.Context(), path, nil, replacement, true, refused); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("tight scratch pressure accepted", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, before) {
		t.Fatal("refusal changed catalog", err)
	}
	budget := agentapi.NewNativeReadBudget(required)
	if err := mergeNativeCatalog(t.Context(), path, nil, replacement, true, budget); err != nil {
		t.Fatal("supported unrelated member cannot be preserved", err)
	}
	if used, peak := budget.Charged(); used != 0 || peak != required {
		t.Fatal("scratch charge/release", used, peak)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	members, ok := nativeCatalogMembers(raw)
	if !ok || !bytes.Equal(members["native_inventory"], replacement) || len(members["large_unrelated"]) != (2<<20)+2 {
		t.Fatal("unrelated member changed")
	}
}

func TestNativeCatalogMemberKeysStayBoundedAcrossEncoding(t *testing.T) {
	t.Parallel()
	if _, ok := nativeCatalogMembers([]byte{'{', '"', 0xff, '"', ':', '1', '}'}); ok {
		t.Fatal("invalid UTF-8 key accepted")
	}
	key := strings.Repeat("<&>", 128)
	input := []byte(`{"` + key + `":"retained","escaped\"key":"value"}`)
	members, ok := nativeCatalogMembers(input)
	if !ok {
		t.Fatal("bounded escaped keys refused")
	}
	output, err := encodeNativeCatalogMembers(members)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if json.Unmarshal(output, &decoded) != nil || decoded[key] != "retained" || decoded["escaped\"key"] != "value" {
		t.Fatal("escaped key changed")
	}
	oversized := map[string]json.RawMessage{"large": json.RawMessage(append(append([]byte{'"'}, bytes.Repeat([]byte("x"), maxCatalogBytes)...), '"'))}
	if raw, err := encodeNativeCatalogMembers(oversized); !errors.Is(err, errCatalogTooLarge) || raw != nil {
		t.Fatal("over-cap output allocated", err)
	}
}

func TestNativeCatalogChargesExpandedQuotedKeysBeforeAllocation(t *testing.T) {
	t.Parallel()
	var before bytes.Buffer
	before.WriteString(`{"version":6`)
	for i := range 1000 {
		fmt.Fprintf(&before, `,"%s%04d":1`, strings.Repeat("<&>", 128), i)
	}
	before.WriteByte('}')
	path := filepath.Join(t.TempDir(), "discovery-catalog.json")
	if err := os.WriteFile(path, before.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := []byte(`{"version":1}`)
	members, ok := nativeCatalogMembers(before.Bytes())
	if !ok {
		t.Fatal("valid adversarial key fixture refused")
	}
	members["native_inventory"] = replacement
	outputSize, err := nativeCatalogOutputSize(members)
	if err != nil {
		t.Fatal(err)
	}
	base := 3*int64(before.Len()) + 3*int64(len(replacement)) + (512 << 10)
	extra := int64(outputSize) - int64(before.Len()) - int64(len(replacement))
	if extra <= 0 {
		t.Fatal("fixture did not expand")
	}
	refused := agentapi.NewNativeReadBudget(base)
	if err := mergeNativeCatalog(t.Context(), path, nil, replacement, true, refused); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("unpaid expanded keys accepted", err)
	}
	if used, _ := refused.Charged(); used != 0 {
		t.Fatal("pressure leaked", used)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, before.Bytes()) {
		t.Fatal("pressure changed checkpoint", err)
	}
	budget := agentapi.NewNativeReadBudget(base + extra)
	if err := mergeNativeCatalog(t.Context(), path, nil, replacement, true, budget); err != nil {
		t.Fatal("paid output refused", err)
	}
	if used, peak := budget.Charged(); used != 0 || peak != base+extra {
		t.Fatal("output ownership", used, peak)
	}
}

func TestNativeCatalogRefusesUnsupportedOutputMemberCount(t *testing.T) {
	t.Parallel()
	beforeMembers := map[string]json.RawMessage{"version": json.RawMessage("6")}
	for i := range 1023 {
		beforeMembers[fmt.Sprintf("future_%04d", i)] = json.RawMessage("true")
	}
	before, err := json.Marshal(beforeMembers)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := nativeCatalogMembers(before); !ok {
		t.Fatal("supported maximum input refused")
	}
	for _, nativeOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("nativeOnly=%v", nativeOnly), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "discovery-catalog.json")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			replacement := []byte(`{"version":1}`)
			if !nativeOnly {
				replacement, err = json.Marshal(catalog{Version: 6})
				if err != nil {
					t.Fatal(err)
				}
			}
			budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
			if err := mergeNativeCatalog(t.Context(), path, nil, replacement, nativeOnly, budget); !errors.Is(err, errCatalogTooLarge) {
				t.Fatal("unsupported output committed", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, before) {
				t.Fatal("refusal changed prior members", err)
			}
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("refusal leaked scratch", used)
			}
		})
	}
}

func TestNativeCatalogRefusesUnsupportedEncodedKey(t *testing.T) {
	t.Parallel()
	key := strings.Repeat("<&>", 1024)
	before := []byte(`{"version":6,"` + key + `":true}`)
	if _, ok := nativeCatalogMembers(before); !ok {
		t.Fatal("bounded input key refused")
	}
	path := filepath.Join(t.TempDir(), "discovery-catalog.json")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	if err := mergeNativeCatalog(t.Context(), path, nil, []byte(`{"version":1}`), true, budget); !errors.Is(err, errCatalogTooLarge) {
		t.Fatal("unsupported encoded key committed", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, before) {
		t.Fatal("refusal changed previous bytes", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("scratch leak", used)
	}
}
