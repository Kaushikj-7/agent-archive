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
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestMuseSkillsSourceBundleUsesSharedFilteredInventoryWithoutUseClaims(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	raw := museNativeFixtureBytes(t, "synthetic-own-tools.jsonl", museSyntheticID, workspace)
	writeMuseNative(t, data, museSyntheticID, raw)
	var selected archive.FilteredTranscript
	counter.after = func(out archive.FilteredTranscript) { selected = out }
	_, stderr, code := runHandoff(t, env, "--latest", "--source", "local", "--harness", "muse", "--format", "json")
	if code != 0 {
		t.Fatalf("fixture handoff %d %s", code, stderr)
	}
	userHome, xdg, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	calls := map[string]int{}
	env.LookupEnv = func(key string) (string, bool) {
		calls[key]++
		switch skillEnvironmentKey(key) {
		case skillXDGConfigHome:
			return xdg, true
		case skillCodexHome:
			return codexHome, true
		}
		t.Fatalf("unexpected environment read %s", key)
		return "", false
	}
	roots := []string{filepath.Join(xdg, "muse", "skills"), filepath.Join(userHome, ".agents", "skills"), filepath.Join(userHome, ".claude", "skills"), filepath.Join(codexHome, "skills"), filepath.Join(workspace, ".agents", "skills"), filepath.Join(workspace, ".codex", "skills"), filepath.Join(workspace, ".claude", "skills")}
	body := []byte("---\nname: synthetic-review\n---\nuse token=synthetic-secret-value\n")
	for _, root := range roots {
		dir := filepath.Join(root, "synthetic-review")
		must(t, os.MkdirAll(dir, 0700))
		must(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), body, 0600))
	}
	// A symlink cannot bring a foreign file into a user inventory.
	outside := filepath.Join(t.TempDir(), "SKILL.md")
	must(t, os.WriteFile(outside, []byte("PRIVATE_FOREIGN_FILE_SENTINEL"), 0600))
	escape := filepath.Join(roots[1], "escape")
	must(t, os.MkdirAll(escape, 0700))
	must(t, os.Symlink(outside, filepath.Join(escape, "SKILL.md")))
	reg := archive.SessionRegistration{ArchiveSessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", NativeSessionID: museSyntheticID, ProjectID: "local", ProjectRoot: workspace, Harness: archive.Harness{Name: "muse"}, SessionStartedAt: selected.NativeStartAt}
	observe := skillObserver(env, config.SkillEvidenceBody)
	evidence, err := observe(reg, env.Now())
	must(t, err)
	// The user/project queries and later sessions reuse one immutable env snapshot.
	_, err = observe(reg, env.Now())
	must(t, err)
	if len(calls) != 2 || calls["XDG_CONFIG_HOME"] != 1 || calls["CODEX_HOME"] != 1 {
		t.Fatalf("environment snapshot reads: %v", calls)
	}
	bundle, err := archive.NewSourceBundle(reg, counter.TranscriptFilter, selected, env.Now(), evidence)
	must(t, err)
	encoded, err := json.Marshal(bundle)
	must(t, err)
	if bundle.Capture.FilterVersion != "31" || bundle.Capture.AdapterVersion != "0.31.0" {
		t.Fatal("wrong privacy provenance")
	}
	for _, secret := range []string{"synthetic-secret-value", "PRIVATE_FOREIGN_FILE_SENTINEL", userHome, xdg, codexHome, outside} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("private skill value uploaded: %q", secret)
		}
	}
	filteredHash := sha256.Sum256([]byte(archive.RedactText(string(body))))
	snapshots := 0
	for _, item := range bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			snapshots++
			if item.Payload["sha256"] != hex.EncodeToString(filteredHash[:]) {
				t.Fatal("snapshot hash is not filtered content hash")
			}
			if item.Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) || !strings.Contains(item.Payload["snapshot"].(string), "[REDACTED]") {
				t.Fatal("snapshot lost installed-only/redaction")
			}
		}
	}
	if snapshots != 7 {
		t.Fatalf("snapshot count %d", snapshots)
	}
	analysis, err := analyzeSource(t.Context(), env.Agents, bundle)
	must(t, err)
	parser, _ := env.agentRegistry().LookupParser("muse")
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic-machine", selected.NativeStartAt, env.Now(), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, archive.ParserInfo{Name: "muse", Version: parser.Version()})
	must(t, err)
	if len(metadata.SkillsAvailable) == 0 || len(metadata.SkillsUsed) != 0 || metadata.SkillDetection != archive.SkillDetectionUnavailable {
		t.Fatal("inventory became eligibility or use")
	}
	for _, skill := range metadata.SkillsAvailable {
		if skill.Coverage != archive.SkillCoverageInstalledOnly || skill.SHA256 == "" {
			t.Fatal("inventory coverage/hash changed")
		}
	}
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceMetadata} {
		observer := skillObserver(env, mode)
		items, err := observer(reg, env.Now())
		must(t, err)
		raw, err := json.Marshal(items)
		must(t, err)
		if mode == config.SkillEvidenceNone && len(items) != 0 {
			t.Fatal("none mode inventoried skills")
		}
		if bytes.Contains(raw, []byte("use token=")) {
			t.Fatal("non-body mode retained body")
		}
	}
	reg.Origin = archive.SessionOriginImport
	imported, err := observe(reg, env.Now())
	must(t, err)
	if len(imported) != 0 {
		t.Fatal("import received today's skills")
	}
}

func TestMuseSkillEnvironmentSnapshotBoundsAndAlias(t *testing.T) {
	t.Parallel()
	home, project, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	dir := filepath.Join(xdg, "muse", "skills", "synthetic-review")
	must(t, os.MkdirAll(dir, 0700))
	must(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: synthetic-review\n---\nsynthetic"), 0600))
	reads := 0
	env := testEnv(t, t.TempDir(), time.Now())
	env.UserHomeDir = func() (string, error) { return home, nil }
	env.LookupEnv = func(key string) (string, bool) {
		reads++
		switch skillEnvironmentKey(key) {
		case skillXDGConfigHome:
			return xdg, true
		case skillCodexHome:
			return strings.Repeat("x", 4097), true
		}
		t.Fatalf("unexpected key %s", key)
		return "", false
	}
	observe := skillObserver(env, config.SkillEvidenceMetadata)
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: "muse-code"}, ProjectRoot: project}
	items, err := observe(reg, env.Now())
	must(t, err)
	if reads != 2 {
		t.Fatalf("fixed snapshot reads %d", reads)
	}
	found := false
	for _, item := range items {
		if item.Payload["scope"] == "user_muse_config" && item.Payload["root_status"] == "present" {
			found = true
		}
		if item.Payload["scope"] == "user_codex_foreign" {
			t.Fatal("oversized configured root became fallback")
		}
	}
	if !found {
		t.Fatal("alias lost exact config root")
	}
	reg.Harness.Name = "muse"
	_, err = observe(reg, env.Now())
	must(t, err)
	if reads != 2 {
		t.Fatal("alias re-read environment")
	}
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceBody} {
		reg.Origin = archive.SessionOriginImport
		_, err = skillObserver(env, mode)(reg, env.Now())
		must(t, err)
	}
	if reads != 2 {
		t.Fatal("none/import read skill environment")
	}
}
