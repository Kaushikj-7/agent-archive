package muse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const maxSkillCatalogBytes = 256 << 10

var errSkillInventory = errors.New("unsupported or incomplete installed Muse skill inventory")

func (skillProvider) InventoryInput(l agentapi.SkillLocations) (agentapi.SkillInventoryInput, bool) {
	if !validSkillRoot(l.DataHome) {
		return agentapi.SkillInventoryInput{}, false
	}
	return agentapi.SkillInventoryInput{Boundary: filepath.Join(l.DataHome, "muse"), Registry: "plugins/installed.json"}, true
}

// decodeSkillCatalog reuses the ordinary-root exact JSON decoder with narrower
// per-container/node bounds. Defaults used by native transcript parsing stay
// unchanged. Candidate counts are refused before the next map allocation.
func decodeSkillCatalog(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > maxSkillCatalogBytes || !utf8.Valid(raw) || !validEscapes(raw) {
		return nil, errSkillInventory
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	r := jsonReader{decoder: d, nodeLimit: 8192, memberLimit: 256}
	v, _, err := r.value(0)
	if err != nil {
		return nil, errSkillInventory
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, errSkillInventory
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errSkillInventory
	}
	return m, nil
}

func skillAllowedKeys(m map[string]any, allowed ...string) bool {
	if m == nil {
		return false
	}
	for key := range m {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func skillPortableID(id string) bool {
	return (archive.SkillDeclarationIdentity{Scope: "skill", ID: id}).Valid()
}

func skillDigest(value string) (string, bool) {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return "", false
	}
	for _, c := range value[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return value[7:], true
}

func skillVersion(value any) (string, bool) {
	s, ok := value.(string)
	if !ok || s == "" || len(s) > 128 {
		return "", false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return "", false
		}
	}
	return s, true
}

func (skillProvider) InterpretInventory(raw []byte) (agentapi.SkillInstalledInventory, error) {
	var out agentapi.SkillInstalledInventory
	root, err := decodeSkillCatalog(raw)
	if err != nil || !keys(root, "version", "plugins") || num(root["version"]) != 1 {
		return out, errSkillInventory
	}
	records, ok := root["plugins"].(map[string]any)
	if !ok {
		return out, errSkillInventory
	}
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record := object(records[id])
		source := object(record["source"])
		version, versionOK := skillVersion(record["version"])
		packageSHA, packageOK := skillDigest(str(record["package_sha256"]))
		manifestSHA, manifestOK := skillDigest(str(record["manifest_sha256"]))
		_, enabledOK := record["enabled"].(bool)
		directory := "plugins/cache/local/" + id + "/" + packageSHA + "/package"
		if !skillPortableID(id) || str(record["id"]) != id || !versionOK || !packageOK || !manifestOK || !enabledOK || str(record["trust"]) != "user-local" || str(record["manifest_family"]) != "muse" || !skillAllowedKeys(record, "id", "display_name", "version", "description", "manifest_family", "enabled", "trust", "source", "installed_at", "updated_at", "manifest_sha256", "package_sha256", "cache_path", "agent_definition_inventory") || !skillAllowedKeys(source, "provenance", "path") || str(source["provenance"]) != "native-local" || str(record["cache_path"]) != directory {
			out.Incomplete = true
			continue
		}
		out.Packages = append(out.Packages, agentapi.SkillInstalledPackage{ID: id, Version: version, Directory: directory, ManifestSHA256: manifestSHA})
	}
	return out, nil
}

func (skillProvider) InterpretManifest(input agentapi.SkillInventoryInput, pkg agentapi.SkillInstalledPackage, raw []byte) (agentapi.SkillRoot, error) {
	var empty agentapi.SkillRoot
	if len(raw) == 0 || len(raw) > maxSkillCatalogBytes || len(pkg.Directory) > 4096 || !skillPortableID(pkg.ID) || !validSkillRoot(input.Boundary) || input.Registry != "plugins/installed.json" {
		return empty, errSkillInventory
	}
	prefix := "plugins/cache/local/" + pkg.ID + "/"
	if !strings.HasPrefix(pkg.Directory, prefix) || !strings.HasSuffix(pkg.Directory, "/package") {
		return empty, errSkillInventory
	}
	locator := strings.TrimSuffix(strings.TrimPrefix(pkg.Directory, prefix), "/package")
	if _, ok := skillDigest("sha256:" + locator); !ok {
		return empty, errSkillInventory
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != pkg.ManifestSHA256 {
		return empty, errSkillInventory
	}
	m, err := decodeSkillCatalog(raw)
	if err != nil {
		return empty, err
	}
	compat := object(m["compat"])
	caps := object(m["capabilities"])
	if !skillAllowedKeys(m, "schemaVersion", "name", "displayName", "version", "description", "compat", "capabilities") || num(m["schemaVersion"]) != 1 || str(m["name"]) != pkg.ID || str(m["version"]) != pkg.Version || !keys(compat, "source", "manifestDir") || str(compat["source"]) != "native" || str(compat["manifestDir"]) != ".muse-plugin" || !skillAllowedKeys(caps, "skills", "commands", "hooks", "mcpServers", "reminders") {
		return empty, errSkillInventory
	}
	for key, value := range caps {
		if key != "skills" {
			if _, ok := value.([]any); !ok {
				return empty, errSkillInventory
			}
		}
	}
	declarations, ok := caps["skills"].([]any)
	if !ok {
		return empty, errSkillInventory
	}
	root := agentapi.SkillRoot{Layout: agentapi.SkillDeclaredFiles, Path: filepath.Join(input.Boundary, filepath.FromSlash(pkg.Directory)), Scope: "muse-plugin:" + pkg.ID, UserBoundary: input.Boundary}
	seen := map[string]bool{}
	for _, value := range declarations {
		entry := object(value)
		id, path := str(entry["id"]), str(entry["path"])
		if !skillAllowedKeys(entry, "id", "path", "enabledDefault") || !skillPortableID(id) || seen[id] || !skillManifestPath(path) {
			return empty, errSkillInventory
		}
		if v, exists := entry["enabledDefault"]; exists {
			if _, ok := v.(bool); !ok {
				return empty, errSkillInventory
			}
		}
		seen[id] = true
		root.Files = append(root.Files, agentapi.SkillFile{ID: id, Path: path})
	}
	return root, nil
}

func skillManifestPath(path string) bool {
	if len(path) == 0 || len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsAny(path, "\\:\x00") || filepath.IsAbs(path) {
		return false
	}
	for _, c := range path {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
