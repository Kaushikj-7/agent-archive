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
