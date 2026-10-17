//go:build unix

package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func installedSkillFixture(t *testing.T) (SkillOptions, string, string) {
	t.Helper()
	data := t.TempDir()
	boundary := filepath.Join(data, "muse")
	directory := "plugins/cache/local/synthetic/" + strings.Repeat("a", 64) + "/package"
	manifest := []byte(`{"schemaVersion":1,"name":"synthetic","version":"0.1.0","compat":{"source":"native","manifestDir":".muse-plugin"},"capabilities":{"skills":[{"id":"first","path":"skills/body.md","enabledDefault":false},{"id":"second","path":"skills/body.md","enabledDefault":true}],"commands":[],"hooks":[],"mcpServers":[],"reminders":[]}}`)
	digest := sha256.Sum256(manifest)
	registry := map[string]any{"version": 1, "plugins": map[string]any{"synthetic": map[string]any{"id": "synthetic", "version": "0.1.0", "manifest_family": "muse", "enabled": false, "trust": "user-local", "source": map[string]any{"provenance": "native-local", "path": filepath.Join(data, "PRIVATE_SOURCE_DECOY")}, "manifest_sha256": "sha256:" + hex.EncodeToString(digest[:]), "package_sha256": "sha256:" + strings.Repeat("a", 64), "cache_path": directory}}}
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	for relative, value := range map[string][]byte{"plugins/installed.json": raw, directory + "/.muse-plugin/plugin.json": manifest, directory + "/skills/body.md": []byte("---\nname: shared-name\n---\napi_key=synthetic-secret-value\n"), directory + "/undeclared/SKILL.md": []byte("UNDECLARED_PRIVATE_SENTINEL")} {
		path := filepath.Join(boundary, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	provider := muse.Skills().(agentapi.SkillInventoryInterpreter)
	input, ok := provider.InventoryInput(agentapi.SkillLocations{DataHome: data})
	if !ok {
		t.Fatal("no input")
	}
	return SkillOptions{Harness: "muse", ObservedAt: time.Now(), Mode: config.SkillEvidenceBody, Inventory: provider, InventoryInput: input}, boundary, directory
}

func TestInstalledSkillsShareCatalogManifestAndBodyBudget(t *testing.T) {
	t.Parallel()
	options, boundary, directory := installedSkillFixture(t)
	budget := skillReadBudget{remaining: maxSnapshotBytes, catalogRemaining: 1 << 20, declarations: maxSkillsPerRoot}
	got := observeInstalledSkills(options, &budget)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := 0
	for _, item := range got {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			snapshots++
			if item.Payload["scope"] != "muse-plugin:synthetic" {
				t.Fatal("unstable logical identity")
			}
		}
	}
	if snapshots != 2 || len(budget.cache) != 3 {
		t.Fatalf("expected one physical registry+manifest+body read, got %d/%d", snapshots, len(budget.cache))
	}
	var charged int64
	var metadata int64
	for _, relative := range []string{"plugins/installed.json", directory + "/.muse-plugin/plugin.json", directory + "/skills/body.md"} {
		info, statErr := os.Stat(filepath.Join(boundary, relative))
		if statErr != nil {
			t.Fatal(statErr)
		}
		charged += info.Size()
		if !strings.HasSuffix(relative, "body.md") {
			metadata += info.Size()
		}
	}
	if budget.remaining != maxSnapshotBytes-charged || budget.catalogRemaining != (1<<20)-metadata || budget.declarations != 254 {
		t.Fatal("independent allowance or duplicate physical read")
	}
	for _, private := range []string{boundary, "PRIVATE_SOURCE_DECOY", "UNDECLARED_PRIVATE_SENTINEL", "synthetic-secret-value", strings.Repeat("a", 64), directory} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatalf("private metadata/body escaped: %q", private)
		}
	}
	if !bytes.Contains(raw, []byte("[REDACTED]")) {
		t.Fatal("declared body was not redacted")
	}
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceMetadata, config.SkillEvidenceNone} {
		options.Mode = mode
		items, observeErr := ObserveSkills(options)
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		for _, item := range items {
			if item.Kind == archive.EvidenceKindSkillSnapshot {
				t.Fatal("nonbody mode retained body")
			}
		}
		if mode == config.SkillEvidenceNone && len(items) != 0 {
			t.Fatal("none observed current installation")
		}
	}
}

func TestInstalledSkillsRefuseChangedUnconfinedAndOversizedFiles(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"manifest-hash", "registry-symlink", "manifest-symlink", "body-symlink", "ancestor-symlink", "nonregular", "public-mode", "oversize-registry", "oversize-manifest", "catalog-budget", "body-budget"} {
		t.Run(name, func(t *testing.T) {
			options, boundary, directory := installedSkillFixture(t)
			registry := filepath.Join(boundary, "plugins/installed.json")
			manifest := filepath.Join(boundary, directory, ".muse-plugin/plugin.json")
			body := filepath.Join(boundary, directory, "skills/body.md")
			budget := skillReadBudget{remaining: maxSnapshotBytes, catalogRemaining: 1 << 20, declarations: maxSkillsPerRoot}
			switch name {
			case "manifest-hash":
				if err := os.WriteFile(manifest, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "registry-symlink", "manifest-symlink", "body-symlink":
				path := registry
				if name == "manifest-symlink" {
					path = manifest
				}
				if name == "body-symlink" {
					path = body
				}
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("FOREIGN_SENTINEL"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "ancestor-symlink":
				skills := filepath.Dir(body)
				moved := skills + "-old"
				if err := os.Rename(skills, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, skills); err != nil {
					t.Fatal(err)
				}
			case "nonregular":
				if err := os.Remove(body); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(body, 0700); err != nil {
					t.Fatal(err)
				}
			case "public-mode":
				if err := os.Chmod(manifest, 0644); err != nil {
					t.Fatal(err)
				}
			case "oversize-registry":
				if err := os.WriteFile(registry, bytes.Repeat([]byte(" "), maxSkillInventoryBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize-manifest":
				if err := os.WriteFile(manifest, bytes.Repeat([]byte(" "), maxSkillInventoryBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "catalog-budget":
				budget.catalogRemaining = 1
			case "body-budget":
				a, _ := os.Stat(registry)
				b, _ := os.Stat(manifest)
				budget.remaining = a.Size() + b.Size()
			}
			got := observeInstalledSkills(options, &budget)
			raw, _ := json.Marshal(got)
			for _, item := range got {
				if item.Kind == archive.EvidenceKindSkillSnapshot {
					t.Fatalf("refused case produced body: %s", raw)
				}
			}
			if bytes.Contains(raw, []byte("FOREIGN_SENTINEL")) {
				t.Fatal("foreign data retained")
			}
			gap := false
			for _, item := range got {
				if item.Payload["inventory_complete"] == false {
					gap = true
				}
			}
			if !gap {
				t.Fatal("refusal became complete empty inventory")
			}
		})
	}
}

func TestInstalledSkillReadCacheRevalidatesLogicalBoundaryAndDrift(t *testing.T) {
	t.Parallel()
	_, boundary, directory := installedSkillFixture(t)
	root, err := os.OpenRoot(boundary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	relative := directory + "/skills/body.md"
	inspection, err := inspectConfinedSkill(root, relative)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inspection.read(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(boundary, relative)
	if err = os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if inspection.stable() {
		t.Fatal("atomic replacement retained cached identity")
	}
	if _, err = inspection.read(); err == nil {
		t.Fatal("stale identity read accepted")
	}
	registry, err := inspectConfinedSkill(root, "plugins/installed.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(boundary, "plugins"), filepath.Join(boundary, "plugins-old")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("plugins-old", filepath.Join(boundary, "plugins")); err != nil {
		t.Fatal(err)
	}
	if registry.stable() {
		t.Fatal("substituted ancestor passed registry generation check")
	}
}

func TestInstalledSkillMetadataUsesExistingAllowanceAndFailedReadReservations(t *testing.T) {
	t.Parallel()
	_, boundary, _ := installedSkillFixture(t)
	root, err := os.OpenRoot(boundary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	inspection, err := inspectConfinedSkill(root, "plugins/installed.json")
	if err != nil {
		t.Fatal(err)
	}
	size := inspection.info.Size()
	budget := skillReadBudget{remaining: size, catalogRemaining: size, declarations: 256}
	if _, err = budget.readUsing(inspection.info, true, inspection.read); err != nil {
		t.Fatal(err)
	}
	if budget.remaining != 0 || budget.catalogRemaining != 0 {
		t.Fatal("metadata used separate allowance")
	}
	if _, err = budget.readUsing(inspection.info, true, inspection.read); err != nil || budget.remaining != 0 || budget.catalogRemaining != 0 {
		t.Fatal("same physical metadata charged twice")
	}
	failed := skillReadBudget{remaining: 2 * size, catalogRemaining: 2 * size, declarations: 256}
	for i := 0; i < 2; i++ {
		if _, err = failed.readUsing(inspection.info, true, func() ([]byte, error) { return nil, errSkillInventoryFile }); err == nil {
			t.Fatal("failed read succeeded")
		}
	}
	if failed.remaining != 0 || failed.catalogRemaining != 0 || len(failed.cache) != 0 {
		t.Fatal("failed read replenished allowance or cached unknown bytes")
	}
	if _, err = failed.readUsing(inspection.info, true, inspection.read); err == nil {
		t.Fatal("exhausted budget reset")
	}
}
