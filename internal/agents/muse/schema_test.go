package muse

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestRetainedSchemaMatchesSyntheticGoldens(t *testing.T) {
	t.Parallel()
	compiler := jsonschema.NewCompiler()
	var id string
	for _, name := range []string{"muse-retained-v1.schema.json", "muse-retained-marker-v2.schema.json", "muse-retained-record.schema.json"} {
		raw, e := os.ReadFile(filepath.Join("..", "..", "..", "schemas", name))
		if e != nil {
			t.Fatal(e)
		}
		doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if e != nil {
			t.Fatal(e)
		}
		id = doc.(map[string]any)["$id"].(string)
		if e = compiler.AddResource(id, doc); e != nil {
			t.Fatal(e)
		}
	}
	schema, e := compiler.Compile(id)
	if e != nil {
		t.Fatal(e)
	}
	files, e := filepath.Glob(filepath.Join("testdata", "synthetic-*.golden.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range files {
		raw, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte("\n")) {
			v, e := jsonschema.UnmarshalJSON(bytes.NewReader(line))
			if e != nil {
				t.Fatal(e)
			}
			if e = schema.Validate(v); e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			v.(map[string]any)["raw_record_json"] = "private"
			if schema.Validate(v) == nil {
				t.Fatal("schema admitted private extra field")
			}
		}
	}
}

func TestProductionAllowlistAndSchemaMatchClosedRetainedDispatch(t *testing.T) {
	t.Parallel()
	read := func(path string) map[string]any {
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		m, _, e := decodeObject(raw)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	policy := read(filepath.Join("..", "..", "..", "dev", "qualification", "muse", "retained-allowlist-local-v1.json"))
	schema := read(filepath.Join("..", "..", "..", "schemas", "muse-retained-v1.schema.json"))
	variants := policy["variants"].([]any)
	branches := schema["oneOf"].([]any)
	if len(variants) != len(branches) {
		t.Fatal("policy/schema variant drift")
	}
	for i, v := range variants {
		variant := object(v)
		branch := object(object(branches[i])["properties"])
		props := object(object(branch["fields"])["properties"])
		if str(variant["type"]) != str(object(branch["type"])["const"]) || num(variant["payload_schema"]) != num(object(branch["payload_schema"])["const"]) {
			t.Fatal("discriminator drift")
		}
		required := map[string]bool{}
		for _, name := range object(branch["fields"])["required"].([]any) {
			required[str(name)] = true
		}
		optional := map[string]bool{}
		if names, ok := variant["optional_fields"].([]any); ok {
			for _, name := range names {
				optional[str(name)] = true
			}
		}
		if len(props) != len(variant["fields"].([]any)) {
			t.Fatal("policy/schema field ceiling drift")
		}
		fields := map[string]any{}
		for _, name := range variant["fields"].([]any) {
			key := str(name)
			if required[key] == optional[key] {
				t.Fatal("policy/schema optional field drift")
			}
			prop := object(props[key])
			if prop == nil {
				t.Fatal("policy field absent from schema")
			}
			if values, ok := prop["enum"].([]any); ok {
				fields[key] = values[0]
			} else if value, ok := prop["const"]; ok {
				fields[key] = value
			} else {
				switch prop["type"] {
				case "integer":
					fields[key] = json.Number("1")
				case "array":
					if key == "tool_calls" {
						fields[key] = []any{map[string]any{"call_id": "synthetic_call"}}
					} else {
						fields[key] = []any{map[string]any{"tool_call_index": json.Number("0"), "tool_call_id": "synthetic_call"}}
					}
				case "boolean":
					fields[key] = false
				default:
					if key == "sha256" {
						fields[key] = strings.Repeat("a", 64)
					} else if _, hasPattern := prop["pattern"]; hasPattern {
						fields[key] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
					} else {
						fields[key] = "synthetic"
					}
				}
			}
		}
		r := retained{Type: payloadType(str(variant["type"])), PayloadSchema: num(variant["payload_schema"]), Fields: fields}
		if !validFields(r) {
			t.Fatalf("schema variant not supported by exact retained decoder: %s %+v", r.Type, fields)
		}
	}
	const unsupportedRole runtimeRole = "future_authority"
	if optionalRuntime(unsupportedRole, eventTerminal) {
		t.Fatal("unknown runtime kind inherited optional policy")
	}
}

func TestSkillReadSchemaRequiresNullCausation(t *testing.T) {
	t.Parallel()
	compiler := jsonschema.NewCompiler()
	var id string
	for _, name := range []string{"muse-retained-v1.schema.json", "muse-retained-marker-v2.schema.json", "muse-retained-record.schema.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		id = doc.(map[string]any)["$id"].(string)
		if err = compiler.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, hashed := range []bool{false, true} {
		raw := skillContentNative(t, "ordinary")
		out, e := filterBytes(t, raw, agentapi.ReadLimits{})
		if e != nil {
			t.Fatal(e)
		}
		if hashed {
			_, join, _ := skillContentOperation(t, raw, nil)
			if _, e = join.Restore(t.Context(), archive.SourceBundle{}); e != nil {
				t.Fatal(e)
			}
			h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
			out, e = join.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
			if e != nil {
				t.Fatal(e)
			}
		}
		for _, tc := range []struct {
			name    string
			value   any
			missing bool
			valid   bool
		}{
			{name: "null", valid: true},
			{name: "uuid", value: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
			{name: "missing", missing: true},
			{name: "integer", value: json.Number("1")},
		} {
			records := append([][]byte(nil), out.Records...)
			at := len(records) - 1
			doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(records[at]))
			if e != nil {
				t.Fatal(e)
			}
			record := doc.(map[string]any)
			if str(object(record["fields"])["event_kind"]) != "skill_read_observed" {
				t.Fatal("fixture lacks skill-read event")
			}
			if tc.missing {
				delete(record, "causation_id")
			} else {
				record["causation_id"] = tc.value
			}
			schemaErr := schema.Validate(record)
			if (schemaErr == nil) != tc.valid {
				t.Fatal("schema causation mismatch", hashed, tc.name, schemaErr)
			}
			records[at], e = json.Marshal(record)
			if e != nil {
				t.Fatal(e)
			}
			changed := out
			changed.Records = records
			b := bundleFor(t, changed)
			_, parseErr := (Parser{}).Parse(t.Context(), b)
			_, refilterErr := (Filter{}).Refilter(t.Context(), b, time.Time{})
			if (parseErr == nil) != tc.valid || (refilterErr == nil) != tc.valid {
				t.Fatal("Go/schema causation disagreement", hashed, tc.name, parseErr, refilterErr)
			}
		}
	}
}
