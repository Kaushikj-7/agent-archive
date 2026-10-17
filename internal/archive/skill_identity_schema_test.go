package archive

import (
	"bytes"
	"encoding/json"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"testing"
)

func TestInstalledSkillIdentityMetadataSchemaAndOldStrictRefusal(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../schemas/metadata.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemaDoc map[string]any
	if err := json.Unmarshal(raw, &schemaDoc); err != nil {
		t.Fatal(err)
	}
	compile := func(defs any) *jsonschema.Schema {
		t.Helper()
		doc := map[string]any{"$defs": defs, "$ref": "#/$defs/skill"}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("skill.json", doc); err != nil {
			t.Fatal(err)
		}
		compiled, err := c.Compile("skill.json")
		if err != nil {
			t.Fatal(err)
		}
		return compiled
	}
	current := compile(schemaDoc["$defs"])
	validate := func(schema *jsonschema.Schema, raw string) error {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(raw)))
		if err != nil {
			return err
		}
		return schema.Validate(value)
	}
	legacy := `{"name":"shared-name","sha256":"same-hash","coverage":"installed_only"}`
	next := `{"name":"shared-name","sha256":"same-hash","coverage":"installed_only","installed_identities":[{"scope":"package-one","id":"first"},{"scope":"package-one","id":"second"}]}`
	if err := validate(current, legacy); err != nil {
		t.Fatal(err)
	}
	if err := validate(current, next); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"name":"x","installed_identities":[{"scope":"/private","id":"ok"}]}`, `{"name":"x","installed_identities":[{"scope":"ok","id":"../bad"}]}`, `{"name":"x","installed_identities":[{"scope":"ok","id":"first","path":"/private"}]}`, `{"name":"x","installed_identities":[]}`} {
		if validate(current, bad) == nil {
			t.Fatal("closed identity schema accepted malformed value")
		}
	}
	defs := schemaDoc["$defs"].(map[string]any)
	delete(defs["skill"].(map[string]any)["properties"].(map[string]any), "installed_identities")
	oldStrict := compile(defs)
	if validate(oldStrict, legacy) != nil || validate(oldStrict, next) == nil {
		t.Fatal("old strict schema compatibility boundary changed")
	}
}
