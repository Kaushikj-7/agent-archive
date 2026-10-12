package archive

import (
	"bytes"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const schemaDir = "../../schemas"

// compileSchema compiles one published schema, asserting formats
// (date-time) as well as structure.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(schemaDir, name))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	id, _ := doc.(map[string]any)["$id"].(string)
	if !strings.HasPrefix(id, "https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/"+name) {
		t.Fatalf("%s: $id %q is not the file's raw URL", name, id)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return schema
}

func TestHistoricalSchemasCompileAndRejectControlLabels(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		name string
		data string
	}{
		{"backfill-mapping.schema.json", `{"schema_version":1,"historical_projects":[{"id":"11111111111111111111111111111111","label":"Retired"}],"mappings":[{"cwd":"/retired","historical_project_id":"11111111111111111111111111111111"}]}`},
		{"historical-project-proof.schema.json", `{"version":1,"project_id":"11111111111111111111111111111111","destination_id":"1111111111111111111111111111111111111111111111111111111111111111","original_cwd":"/retired","canonical_cwd":"/retired","label":"Retired"}`},
		{"historical-projects.schema.json", `{"schema_version":{"version":10,"writer":"historical-projects-v10"},"historical_project_protection":true,"historical_projects":[{"id":"11111111111111111111111111111111","label":"Retired","permissions":[]}]}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			schema := compileSchema(t, fixture.name)
			valid, err := jsonschema.UnmarshalJSON(strings.NewReader(fixture.data))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(valid); err != nil {
				t.Fatal(err)
			}
			invalid, err := jsonschema.UnmarshalJSON(strings.NewReader(strings.Replace(fixture.data, "Retired", `bad\nlabel`, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if schema.Validate(invalid) == nil {
				t.Fatal("control characters accepted in historical label")
			}
		})
	}
}
