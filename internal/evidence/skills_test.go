package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// The recorded hash is of the redacted text, never the original: a hash of
// the original, uploaded beside the redacted snapshot, would let whoever
// holds the bucket confirm a guess at the redacted secret.
//
// Regression: 2026-09 review of filter 12.
func TestObserveSkillsHashesRedactedTextAndFiltersSnapshot(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	dir := filepath.Join(project, ".agents", "skills", "folder-name")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: reviewed-name\ndescription: test\n---\nuse token=synthetic-secret-value\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", ProjectRoot: project, UserHome: home, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[2].Kind != archive.EvidenceKindSkillInventory || got[3].Kind != archive.EvidenceKindSkillSnapshot {
		t.Fatalf("evidence=%#v", got)
	}
	if got[2].Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) || got[2].Payload["root_status"] != "present" {
		t.Fatalf("inventory=%#v", got[2])
	}
	snapshot := got[3].Payload
	if snapshot["name"] != "reviewed-name" || len(snapshot["sha256"].(string)) != 64 || snapshot["redacted"] != true {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	redacted := strings.Replace(content, "synthetic-secret-value", "[REDACTED]", 1)
	want, original := sha256.Sum256([]byte(redacted)), sha256.Sum256([]byte(content))
	if snapshot["sha256"] != hex.EncodeToString(want[:]) || snapshot["sha256"] == hex.EncodeToString(original[:]) {
		t.Fatalf("snapshot hash=%v, want the hash of %q", snapshot["sha256"], redacted)
	}
	inventory := got[2].Payload["skills"].([]any)[0].(map[string]any)
	if inventory["sha256"] != snapshot["sha256"] {
		t.Fatalf("inventory hash=%v, snapshot hash=%v", inventory["sha256"], snapshot["sha256"])
	}
	if text := snapshot["snapshot"].(string); strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("snapshot text=%q", text)
	}
	if _, truncated := snapshot["truncated"]; truncated || fmt.Sprint(snapshot["gaps"]) != "[sensitive_content_redacted]" {
		t.Fatalf("gap labels=%#v", snapshot)
	}
}

func TestObserveSkillsLabelsTruncatedSnapshotWithoutClaimingRedaction(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".agents", "skills", "long")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\nname: long\n---\n" + strings.Repeat("plain instruction text\n", 4096)) // ~96 KiB, over the 16 KiB snapshot cap
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	for _, item := range got {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			snapshot = item.Payload
		}
	}
	digest := sha256.Sum256(original)
	if snapshot == nil || snapshot["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatalf("snapshot hash=%v", snapshot["sha256"])
	}
	// The body is capped at 16 KiB; the hash still covers the whole file and
	// the original size is recorded, so a reader knows what it is missing.
	if body := snapshot["snapshot"].(string); len(body) != maxSnapshotBodyBytes {
		t.Fatalf("snapshot body = %d bytes, want the %d byte cap", len(body), maxSnapshotBodyBytes)
	}
	if snapshot["original_bytes"] != float64(len(original)) {
		t.Fatalf("original_bytes=%v, want %d", snapshot["original_bytes"], len(original))
	}
	// The cap is applied before filtering, so this is our own truncation, not
	// a filter gap: "gaps" reports only what the privacy filter changed.
	if snapshot["truncated"] != true || snapshot["redacted"] != false {
		t.Fatalf("truncation mislabeled: truncated=%v redacted=%v", snapshot["truncated"], snapshot["redacted"])
	}
	if _, reported := snapshot["gaps"]; reported {
		t.Fatalf("filter gaps reported for an unfiltered body: %v", snapshot["gaps"])
	}
}

func TestObserveSkillsUnreadableRootIsCoverageGapNotFailure(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A regular file where the skills directory is expected: ENOTDIR, not ENOENT.
	if err := os.WriteFile(filepath.Join(home, ".agents", "skills"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("unreadable root failed observation: %v", err)
	}
	if len(got) != 2 || got[0].Kind != archive.EvidenceKindSkillInventory || got[0].Payload["scope"] != "user_agents" {
		t.Fatalf("evidence=%#v", got)
	}
	if got[0].Payload["root_status"] != "unreadable" || got[0].Payload["inventory_complete"] != false || len(got[0].Payload["skills"].([]any)) != 0 {
		t.Fatalf("unreadable root=%#v", got[0])
	}
	if got[1].Payload["root_status"] != "absent" {
		t.Fatalf("other roots were affected: %#v", got[1])
	}
}

func TestObserveSkillsUnreadableSkillFileIsUninspected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for root")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	for _, name := range []string{"locked", "readable"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(root, "locked", "SKILL.md")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("unreadable SKILL.md failed observation: %v", err)
	}
	inventory := got[0].Payload
	if inventory["root_status"] != "present" || inventory["inventory_complete"] != false || inventory["omitted_count"] != float64(1) {
		t.Fatalf("inventory=%#v", inventory)
	}
	names := []string{}
	for _, item := range got {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			names = append(names, item.Payload["name"].(string))
		}
	}
	if len(names) != 1 || names[0] != "readable" {
		t.Fatalf("snapshots=%v", names)
	}
}

func TestObserveSkillsAbsentRootsAreScopedAndLeaveUseKnowledgeUnknown(t *testing.T) {
	now := time.Now().UTC()
	got, err := observeBuiltinSkills(SkillOptions{Harness: "claude", ProjectRoot: t.TempDir(), UserHome: t.TempDir(), ObservedAt: time.Now()})
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	for _, item := range got {
		if item.Kind != archive.EvidenceKindSkillInventory || item.Payload["root_status"] != "absent" || item.Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) || len(item.Payload["skills"].([]any)) != 0 {
			t.Fatalf("observation=%#v", item)
		}
	}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: archive.FilterVersion, CapturedAt: now}, SupplementalEvidence: got}
	analysis, parseErr := nativecodec.ParseClaude(context.Background(), bundle)
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, "machine", now, now, archive.SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64)}, archive.ParserInfo{})
	if err != nil || metadata.SkillDetection != archive.SkillDetectionUnavailable || len(metadata.SkillsUsed) != 0 {
		t.Fatalf("metadata=%#v err=%v", metadata, err)
	}
}

func TestObserveSkillsDoesNotWalkUnrelatedProjectDirectories(t *testing.T) {
	project := t.TempDir()
	unrelated := filepath.Join(project, "vendor", ".agents", "skills", "hidden")
	if err := os.MkdirAll(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "SKILL.md"), []byte("---\nname: hidden\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", ProjectRoot: project, UserHome: t.TempDir(), ObservedAt: time.Now()})
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	for _, item := range got {
		if item.Kind != archive.EvidenceKindSkillInventory || item.Payload["root_status"] != "absent" {
			t.Fatalf("unexpected evidence=%#v", got)
		}
	}
}

func TestObserveSkillsRecordsRemovalAndDeduplicatesRepeatedAbsence(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	dir := filepath.Join(root, "review")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	present, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	absent, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: first.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	merged := archive.MergeSupplementalEvidence(present, absent)
	if len(merged) != len(present)+1 {
		t.Fatalf("removal was not recorded exactly once: present=%#v absent=%#v merged=%#v", present, absent, merged)
	}
	repeated, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: first.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if again := archive.MergeSupplementalEvidence(merged, repeated); len(again) != len(merged) {
		t.Fatalf("repeated absence changed history: before=%#v after=%#v", merged, again)
	}
}

func TestObserveSkillsRecordsExistingEmptyRoot(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Payload["root_status"] != "empty" || got[0].Payload["inventory_complete"] != true || len(got[0].Payload["skills"].([]any)) != 0 {
		t.Fatalf("empty root=%#v", got[0])
	}
}

func TestObserveSkillsUsesAggregateSnapshotBudgetAndDistinctCodexScopes(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	items := []struct {
		root string
		name string
	}{
		{filepath.Join(home, ".agents", "skills"), "current"},
		{filepath.Join(home, ".agents", "skills"), "extra-one"},
		{filepath.Join(home, ".agents", "skills"), "extra-two"},
		{filepath.Join(home, ".codex", "skills"), "legacy"},
		{filepath.Join(project, ".agents", "skills"), "project"},
	}
	for _, item := range items {
		dir := filepath.Join(item.root, item.name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + item.name + "\n---\n" + strings.Repeat("x", maxSkillBytes-64)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", ProjectRoot: project, UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]bool{}
	snapshotBytes, omitted := 0, false
	for _, item := range got {
		if item.Kind == archive.EvidenceKindSkillInventory {
			scopes[item.Payload["scope"].(string)] = true
			if count, _ := item.Payload["snapshot_omitted_count"].(float64); count > 0 {
				omitted = true
			}
		}
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			snapshotBytes += len(item.Payload["snapshot"].(string))
		}
	}
	for _, scope := range []string{"user_agents", "user_codex_legacy", "project_agents"} {
		if !scopes[scope] {
			t.Fatalf("missing scope %q in %#v", scope, scopes)
		}
	}
	if snapshotBytes > maxSnapshotBytes || !omitted {
		t.Fatalf("snapshotBytes=%d omitted=%v", snapshotBytes, omitted)
	}
}

func TestObserveSkillsRefusesOverflowInventory(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	for i := range maxSkillsPerRoot + 1 {
		dir := filepath.Join(root, fmt.Sprintf("skill-%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: small\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Payload["root_status"] != "unreadable" || got[0].Payload["inventory_complete"] != false || len(got[0].Payload["skills"].([]any)) != 0 {
		t.Fatalf("overflow root retained prefix: %#v", got)
	}
	if _, ok := got[0].Payload["omitted_count"]; ok {
		t.Fatal("overflow root claimed an exact omitted total")
	}
	if _, ok := got[0].Payload["truncated"]; ok {
		t.Fatalf("inventory=%#v", got)
	}
}

func TestObserveSkillsLabelsUnscannedLegacySubtrees(t *testing.T) {
	home := t.TempDir()
	nested := filepath.Join(home, ".codex", "skills", ".system", "imagegen")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "SKILL.md"), []byte("---\nname: imagegen\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := observeBuiltinSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Payload["scope"] != "user_codex_legacy" || got[1].Payload["inventory_complete"] != false || got[1].Payload["omitted_count"] != float64(1) {
		t.Fatalf("evidence=%#v", got)
	}
}
