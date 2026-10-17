package muse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func skillWire(t testing.TB) (map[string]any, map[string]any) {
	t.Helper()
	manifest := map[string]any{"schemaVersion": 1, "name": "synthetic", "version": "0.1.0", "compat": map[string]any{"source": "native", "manifestDir": ".muse-plugin"}, "capabilities": map[string]any{"skills": []any{map[string]any{"id": "first", "path": "skills/body.md", "enabledDefault": false}, map[string]any{"id": "second", "path": "skills/body.md", "enabledDefault": true}}, "commands": []any{map[string]any{"command": "NEVER_EXECUTE"}}, "hooks": []any{}, "mcpServers": []any{}, "reminders": []any{}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	record := map[string]any{"id": "synthetic", "version": "0.1.0", "manifest_family": "muse", "enabled": true, "trust": "user-local", "source": map[string]any{"provenance": "native-local", "path": "/PRIVATE_SOURCE_MUST_NOT_BE_READ"}, "manifest_sha256": "sha256:" + hex.EncodeToString(sum[:]), "package_sha256": "sha256:" + strings.Repeat("a", 64), "cache_path": "plugins/cache/local/synthetic/" + strings.Repeat("a", 64) + "/package"}
	return map[string]any{"version": 1, "plugins": map[string]any{"synthetic": record}}, manifest
}

func skillWireJSON(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInstalledSkillCatalogUsesOnlyQualifiedWireLocators(t *testing.T) {
	t.Parallel()
	registry, manifest := skillWire(t)
	provider := skillProvider{}
	input, ok := provider.InventoryInput(agentapi.SkillLocations{DataHome: t.TempDir()})
	if !ok {
		t.Fatal("qualified data home rejected")
	}
	inventory, err := provider.InterpretInventory(skillWireJSON(t, registry))
	if err != nil || inventory.Incomplete || len(inventory.Packages) != 1 {
		t.Fatalf("catalog: %+v %v", inventory, err)
	}
	root, err := provider.InterpretManifest(input, inventory.Packages[0], skillWireJSON(t, manifest))
	if err != nil || len(root.Files) != 2 || root.Scope != "muse-plugin:synthetic" || root.Files[0].Path != root.Files[1].Path || root.Files[0].ID == root.Files[1].ID {
		t.Fatalf("root: %+v %v", root, err)
	}
	if strings.Contains(root.Path, "PRIVATE_SOURCE") || strings.Contains(root.Scope, strings.Repeat("a", 64)) {
		t.Fatal("unqualified/private locator escaped")
	}
}

func TestInstalledSkillCatalogRejectsAmbiguousAndUnsupportedAuthority(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"version", "duplicate", "case-alias", "trailing", "absolute-cache", "other-id", "digest", "family", "provenance", "unknown-selector", "null-enabled", "depth", "candidate-cap", "bytes"} {
		t.Run(name, func(t *testing.T) {
			registry, _ := skillWire(t)
			records := object(registry["plugins"])
			record := object(records["synthetic"])
			var raw []byte
			switch name {
			case "version":
				registry["version"] = 2
			case "duplicate":
				raw = []byte(`{"version":1,"version":1,"plugins":{}}`)
			case "case-alias":
				registry["Version"] = 1
			case "trailing":
				raw = append(skillWireJSON(t, registry), []byte(` {}`)...)
			case "absolute-cache":
				record["cache_path"] = "/private/forbidden/package"
			case "other-id":
				record["id"] = "different"
			case "digest":
				record["package_sha256"] = "sha256:" + strings.Repeat("A", 64)
			case "family":
				record["manifest_family"] = "native"
			case "provenance":
				object(record["source"])["provenance"] = "remote"
			case "unknown-selector":
				record["Cache_Path"] = record["cache_path"]
			case "null-enabled":
				record["enabled"] = nil
			case "depth":
				record["description"] = json.RawMessage(strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34))
			case "candidate-cap":
				for i := 0; i < 257; i++ {
					records[fmt.Sprintf("candidate-%d", i)] = nil
				}
			case "bytes":
				raw = bytes.Repeat([]byte(" "), maxSkillCatalogBytes+1)
			}
			if raw == nil {
				raw = skillWireJSON(t, registry)
			}
			got, err := (skillProvider{}).InterpretInventory(raw)
			if err == nil && (!got.Incomplete || len(got.Packages) != 0) {
				t.Fatalf("unsupported authority became complete inventory: %+v", got)
			}
		})
	}
	registry, _ := skillWire(t)
	object(registry["plugins"])["unsupported"] = map[string]any{"id": "unsupported"}
	got, err := (skillProvider{}).InterpretInventory(skillWireJSON(t, registry))
	if err != nil || !got.Incomplete || len(got.Packages) != 1 {
		t.Fatal("unsupported neighbor erased qualified subset or became absent")
	}
	empty, err := (skillProvider{}).InterpretInventory([]byte(`{"version":1,"plugins":{}}`))
	if err != nil || empty.Incomplete || len(empty.Packages) != 0 {
		t.Fatal("complete empty catalog changed")
	}
}

func TestInstalledSkillManifestChecksHashIdentityAndFiniteDeclarations(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"hash", "name", "version", "compat", "schema", "duplicate-id", "parent", "absolute", "backslash", "case-alias", "unknown-capability", "defaults-type", "declaration-cap", "duplicate-json"} {
		t.Run(name, func(t *testing.T) {
			registry, manifest := skillWire(t)
			provider := skillProvider{}
			input, _ := provider.InventoryInput(agentapi.SkillLocations{DataHome: t.TempDir()})
			inventory, err := provider.InterpretInventory(skillWireJSON(t, registry))
			if err != nil {
				t.Fatal(err)
			}
			pkg := inventory.Packages[0]
			entries := object(manifest["capabilities"])["skills"].([]any)
			entry := object(entries[0])
			switch name {
			case "name":
				manifest["name"] = "other"
			case "version":
				manifest["version"] = "other"
			case "compat":
				object(manifest["compat"])["source"] = "foreign"
			case "schema":
				manifest["schemaVersion"] = 2
			case "duplicate-id":
				object(entries[1])["id"] = "first"
			case "parent":
				entry["path"] = "../outside.md"
			case "absolute":
				entry["path"] = "/outside.md"
			case "backslash":
				entry["path"] = `skills\body.md`
			case "case-alias":
				entry["Path"] = "other.md"
			case "unknown-capability":
				object(manifest["capabilities"])["unknown"] = []any{}
			case "defaults-type":
				entry["enabledDefault"] = "true"
			case "declaration-cap":
				object(manifest["capabilities"])["skills"] = make([]any, 257)
			}
			raw := skillWireJSON(t, manifest)
			if name == "duplicate-json" {
				raw = []byte(`{"schemaVersion":1,"schemaVersion":1}`)
			}
			if name != "hash" {
				sum := sha256.Sum256(raw)
				pkg.ManifestSHA256 = hex.EncodeToString(sum[:])
			} else {
				raw = append(raw, ' ')
			}
			if _, err := provider.InterpretManifest(input, pkg, raw); err == nil {
				t.Fatal("unsupported manifest accepted")
			}
		})
	}
}

func FuzzInstalledSkillCatalog(f *testing.F) {
	registry, _ := skillWire(f)
	f.Add(skillWireJSON(f, registry))
	f.Add([]byte(`{"version":1,"plugins":{}}`))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := (skillProvider{}).InterpretInventory(raw)
		if err != nil {
			return
		}
		if len(got.Packages) > 256 {
			t.Fatal("unbounded installed package output")
		}
		for _, pkg := range got.Packages {
			if !skillPortableID(pkg.ID) || !strings.HasPrefix(pkg.Directory, "plugins/cache/local/"+pkg.ID+"/") || strings.Contains(pkg.Directory, "..") {
				t.Fatal("unsafe package locator")
			}
		}
	})
}

func TestInstalledSkillCatalogAdmitsExactByteAndCandidateBounds(t *testing.T) {
	t.Parallel()
	registry, _ := skillWire(t)
	record := object(object(registry["plugins"])["synthetic"])
	record["description"] = ""
	base := skillWireJSON(t, registry)
	record["description"] = strings.Repeat("x", maxSkillCatalogBytes-len(base))
	raw := skillWireJSON(t, registry)
	if len(raw) != maxSkillCatalogBytes {
		t.Fatal("boundary fixture is not exact")
	}
	if got, err := (skillProvider{}).InterpretInventory(raw); err != nil || got.Incomplete || len(got.Packages) != 1 {
		t.Fatal("exact admitted byte bound refused")
	}
	if _, err := (skillProvider{}).InterpretInventory(append(raw, ' ')); err == nil {
		t.Fatal("over byte bound admitted")
	}
	records := map[string]any{}
	for i := 0; i < 256; i++ {
		records[fmt.Sprintf("candidate-%d", i)] = nil
	}
	if got, err := (skillProvider{}).InterpretInventory(skillWireJSON(t, map[string]any{"version": 1, "plugins": records})); err != nil || !got.Incomplete || len(got.Packages) != 0 {
		t.Fatal("exact rejected-candidate bound changed")
	}
	records["extra"] = nil
	if _, err := (skillProvider{}).InterpretInventory(skillWireJSON(t, map[string]any{"version": 1, "plugins": records})); err == nil {
		t.Fatal("candidate257 allocated/accepted")
	}
}

func FuzzInstalledSkillManifest(f *testing.F) {
	registry, manifest := skillWire(f)
	provider := skillProvider{}
	inventory, err := provider.InterpretInventory(skillWireJSON(f, registry))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(skillWireJSON(f, manifest))
	f.Add([]byte(`{"schemaVersion":1,"name":"synthetic"}`))
	f.Add([]byte(`{"schemaVersion":1,"schemaVersion":1}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		pkg := inventory.Packages[0]
		sum := sha256.Sum256(raw)
		pkg.ManifestSHA256 = hex.EncodeToString(sum[:])
		input := agentapi.SkillInventoryInput{Boundary: "/synthetic/muse", Registry: "plugins/installed.json"}
		root, err := provider.InterpretManifest(input, pkg, raw)
		if err != nil {
			return
		}
		if root.Scope != "muse-plugin:synthetic" || len(root.Files) > 256 {
			t.Fatal("invalid logical declaration inventory")
		}
		seen := map[string]bool{}
		for _, file := range root.Files {
			if seen[file.ID] || !skillPortableID(file.ID) || !skillManifestPath(file.Path) {
				t.Fatal("ambiguous/escaping declaration accepted")
			}
			seen[file.ID] = true
		}
	})
}
