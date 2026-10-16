package catalog

import (
	"bytes"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReservedPickerSchemaMatchesHeadRefusalDescriptor(t *testing.T) {
	raw, err := os.ReadFile("../../schemas/catalog-picker-format.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	id := "https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/catalog-picker-format.schema.json"
	if err = compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		raw   string
		valid bool
	}{
		{`{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"]}`, true},
		{`{"Version":1,"RowSchema":2,"Capabilities":["scoped-picker-v1"]}`, false},
		{`{"Version":1,"RowSchema":1,"Capabilities":[]}`, false},
		{`{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1","unknown"]}`, false},
		{`{"Version":1,"RowSchema":1,"Capabilities":["scoped-picker-v1"],"Complete":true}`, false},
	} {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(fixture.raw)))
		if err != nil {
			t.Fatal(err)
		}
		if valid := schema.Validate(value) == nil; valid != fixture.valid {
			t.Fatalf("valid=%v for %s", valid, fixture.raw)
		}
	}
}
