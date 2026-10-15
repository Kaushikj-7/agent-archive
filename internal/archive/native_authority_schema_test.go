package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestNativeAuthoritySchemaAcceptsOpaqueFactsAndRejectsControls(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(schemaDir, "native-source-authority.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	id := doc.(map[string]any)["$id"].(string)
	// Also check the repository's public schema URL convention.
	_ = compileSchema(t, "native-source-authority.schema.json")
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	for _, definition := range []string{"token", "path"} {
		schema, err := compiler.Compile(id + "#/$defs/" + definition)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{"/native/session.jsonl", "/opaque/λ-001"} {
			if err := schema.Validate(value); err != nil {
				t.Fatalf("%s rejected ordinary fact %q: %v", definition, value, err)
			}
		}
		for _, value := range []string{"/bad\x00value", "/bad\rvalue", "/bad\nvalue"} {
			if schema.Validate(value) == nil {
				t.Fatalf("%s accepted control in %q", definition, value)
			}
		}
	}
}

func TestNativeAuthoritySchemaAcceptsOnlyExactSearchLifecycleBase(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(schemaDir, "native-source-authority.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	id := doc.(map[string]any)["$id"].(string)
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id + "#/$defs/fence")
	if err != nil {
		t.Fatal(err)
	}
	for _, writer := range []string{"search-lifecycle-v16", "future"} {
		value := map[string]any{"version": 16, "writer": "native-authority-v16", "base": map[string]any{"version": 16, "writer": writer}}
		err = schema.Validate(value)
		if (err == nil) != (writer == "search-lifecycle-v16") {
			t.Fatal("search base schema authority differs", writer, err)
		}
	}
}
