//go:build unix

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// writeMusePluginInventory uses the observed installed.json wire representation,
// not CLI catalog JSON or a manually injected qualified package root.
func writeMusePluginInventory(t *testing.T, data string) []string {
	t.Helper()
	records := map[string]any{}
	private := []string{data, "UNDECLARED_PLUGIN_SENTINEL", "PRIVATE_SOURCE_DECOY", "synthetic-plugin-secret"}
	body := []byte("---\nname: shared-plugin-name\n---\napi_key=synthetic-plugin-secret\n")
	for index, id := range []string{"first-package", "second-package"} {
		declarations := []any{map[string]any{"id": "first", "path": "skills/body.md", "enabledDefault": index == 1}}
		if index == 0 {
			declarations = append(declarations, map[string]any{"id": "second", "path": "skills/body.md", "enabledDefault": false})
		}
		manifest := map[string]any{"schemaVersion": 1, "name": id, "version": "0.1.0", "compat": map[string]any{"source": "native", "manifestDir": ".muse-plugin"}, "capabilities": map[string]any{"skills": declarations, "commands": []any{map[string]any{"command": "NEVER_RUN_PRIVATE_SOURCE_DECOY"}}, "hooks": []any{}, "mcpServers": []any{}, "reminders": []any{}}}
		raw, err := json.Marshal(manifest)
		must(t, err)
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		packageHash := strings.Repeat(string(rune('a'+index)), 64)
		relative := "plugins/cache/local/" + id + "/" + packageHash + "/package"
		records[id] = map[string]any{"id": id, "version": "0.1.0", "manifest_family": "muse", "enabled": index == 1, "trust": "user-local", "source": map[string]any{"provenance": "native-local", "path": filepath.Join(data, "PRIVATE_SOURCE_DECOY")}, "manifest_sha256": "sha256:" + digest, "package_sha256": "sha256:" + packageHash, "cache_path": relative}
		for suffix, value := range map[string][]byte{".muse-plugin/plugin.json": raw, "skills/body.md": body, "undeclared/SKILL.md": []byte("UNDECLARED_PLUGIN_SENTINEL")} {
			path := filepath.Join(data, "muse", filepath.FromSlash(relative), filepath.FromSlash(suffix))
			must(t, os.MkdirAll(filepath.Dir(path), 0700))
			must(t, os.WriteFile(path, value, 0600))
		}
		private = append(private, digest, packageHash, relative)
	}
	registry, err := json.Marshal(map[string]any{"version": 1, "plugins": records})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(data, "muse", "plugins", "installed.json"), registry, 0600))
	return private
}

func TestMuseInstalledPluginsFlowThroughSourceBundleMetadataAndPassCache(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	raw := museNativeFixtureBytes(t, "synthetic-own-tools.jsonl", museSyntheticID, workspace)
	writeMuseNative(t, data, museSyntheticID, raw)
	var selected archive.FilteredTranscript
	counter.after = func(out archive.FilteredTranscript) { selected = out }
	_, stderr, code := runHandoff(t, env, "--latest", "--source", "local", "--harness", "muse", "--format", "json")
	if code != 0 {
		t.Fatalf("source/filter %d %s", code, stderr)
	}
	home, skillData := t.TempDir(), t.TempDir()
	env.UserHomeDir = func() (string, error) { return home, nil }
	env.LookupEnv = func(key string) (string, bool) {
		switch skillEnvironmentKey(key) {
		case skillXDGDataHome:
			return skillData, true
		case skillXDGConfigHome:
			return filepath.Join(home, "config"), true
		case skillCodexHome:
			return filepath.Join(home, "codex"), true
		}
		t.Fatalf("unexpected env read %s", key)
		return "", false
	}
	private := writeMusePluginInventory(t, skillData)
	reg := archive.SessionRegistration{ArchiveSessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", NativeSessionID: museSyntheticID, ProjectID: "local", ProjectRoot: workspace, Harness: archive.Harness{Name: "muse"}, SessionStartedAt: selected.NativeStartAt}
	observer := skillObserver(env, config.SkillEvidenceBody)
	items, err := observer(reg, env.Now())
	must(t, err)
	bundle, err := archive.NewSourceBundle(reg, counter.TranscriptFilter, selected, env.Now(), items)
	must(t, err)
	encoded, err := json.Marshal(bundle)
	must(t, err)
	for _, value := range private {
		if bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("private plugin locator/body escaped: %q", value)
		}
	}
	count := 0
	for _, item := range items {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			count++
			if item.Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) || !strings.HasPrefix(item.Payload["scope"].(string), "muse-plugin:") {
				t.Fatal("unqualified plugin identity")
			}
		}
	}
	if count != 3 {
		t.Fatalf("production registry interpreter lost declarations: %d", count)
	}
	analysis, err := analyzeSource(t.Context(), env.Agents, bundle)
	must(t, err)
	parser, _ := env.agentRegistry().LookupParser("muse")
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic-machine", selected.NativeStartAt, env.Now(), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("f", 64), CompressedBytes: 1}, archive.ParserInfo{Name: "muse", Version: parser.Version()})
	must(t, err)
	if len(metadata.SkillsAvailable) != 1 || len(metadata.SkillsAvailable[0].InstalledIdentities) != 3 || len(metadata.SkillsUsed) != 0 || metadata.SkillDetection != archive.SkillDetectionUnavailable {
		t.Fatal("installed plugin declarations collapsed or became use")
	}
	want := []archive.SkillDeclarationIdentity{{Scope: "muse-plugin:first-package", ID: "first"}, {Scope: "muse-plugin:first-package", ID: "second"}, {Scope: "muse-plugin:second-package", ID: "first"}}
	for index, identity := range metadata.SkillsAvailable[0].InstalledIdentities {
		if identity != want[index] {
			t.Fatal("wrong logical identity metadata")
		}
	}
	// A later alias/project reuses FILTERED user evidence without reopening a
	// removed catalog or resnapshotting the environment in this pass.
	must(t, os.RemoveAll(filepath.Join(skillData, "muse", "plugins")))
	env.LookupEnv = func(string) (string, bool) { t.Fatal("cached pass reopened environment"); return "", false }
	reg.Harness.Name = "muse-code"
	reg.ProjectRoot = t.TempDir()
	again, err := observer(reg, env.Now())
	must(t, err)
	againCount := 0
	for _, item := range again {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			againCount++
		}
	}
	if againCount != 3 {
		t.Fatal("project/alias repeated user registry observation")
	}
	reg.Origin = archive.SessionOriginImport
	imported, err := observer(reg, env.Now())
	must(t, err)
	if len(imported) != 0 {
		t.Fatal("import received current plugin inventory")
	}
	none := skillObserver(env, config.SkillEvidenceNone)
	reg.Origin = ""
	nothing, err := none(reg, env.Now())
	must(t, err)
	if len(nothing) != 0 {
		t.Fatal("none inventoried plugins")
	}
}
