package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestNativeInventoryPrivateSchemaCompiles(t *testing.T) {
	t.Parallel()
	compiler := jsonschema.NewCompiler()
	var id string
	for _, name := range []string{"native-source-authority.schema.json", "native-inventory.schema.json"} {
		raw, err := os.ReadFile(filepath.Join(schemaDir, name))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		id = doc.(map[string]any)["$id"].(string)
		if !strings.HasSuffix(id, "/schemas/"+name) {
			t.Fatal(id)
		}
		if err = compiler.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := compiler.Compile(id); err != nil {
		t.Fatal(err)
	}
}
