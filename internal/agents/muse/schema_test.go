package muse

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestRetainedSchemaMatchesSyntheticGoldens(t *testing.T) {
	t.Parallel()
	raw, e := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "muse-retained-v1.schema.json"))
	if e != nil {
		t.Fatal(e)
	}
	doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	id := doc.(map[string]any)["$id"].(string)
	compiler := jsonschema.NewCompiler()
	if e = compiler.AddResource(id, doc); e != nil {
		t.Fatal(e)
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
				case "boolean":
					fields[key] = false
				default:
					fields[key] = "synthetic"
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
