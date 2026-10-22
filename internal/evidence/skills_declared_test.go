package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestDeclaredSkillsConfineFilesAndPreserveLogicalIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "nested", "body.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: shared-name\n---\napi_key=synthetic-secret-value\n")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("UNDECLARED_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	declared := skillRoot{path: root, user: root, scope: "user_muse_test_package", layout: agentapi.SkillDeclaredFiles, files: []agentapi.SkillFile{{ID: "first", Path: "nested/body.md"}, {ID: "second", Path: "nested/body.md"}}}
	budget := skillReadBudget{remaining: maxSnapshotBytes, declarations: maxSkillsPerRoot}
	items, err := observeRoot("muse", declared, time.Now(), &budget, config.SkillEvidenceBody)
	if err != nil {
		t.Fatal(err)
	}
	if budget.remaining != maxSnapshotBytes-int64(len(body)) || len(budget.cache) != 1 {
		t.Fatal("physical alias consumed bytes twice")
	}
	if len(items) != 3 || items[0].Payload["inventory_complete"] != true {
		t.Fatalf("items %+v", items)
	}
	if items[1].Payload["scope"] != items[2].Payload["scope"] || items[1].Payload["id"] == items[2].Payload["id"] {
		t.Fatal("declarations lost stable package scope or distinct IDs")
	}
	merged := archive.MergeSupplementalEvidence(nil, items)
	if len(merged) != 3 {
		t.Fatal("logical declarations collapsed in history")
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{root, "nested/body.md", "UNDECLARED_SENTINEL", "synthetic-secret-value"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("private/undeclared value escaped: %q", secret)
		}
	}
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceMetadata, config.SkillEvidenceNone} {
		got, err := ObserveSkills(SkillOptions{Harness: "muse", ObservedAt: time.Now(), Mode: mode, Locations: []agentapi.SkillRoot{{Path: root, UserBoundary: root, Scope: declared.scope, Layout: agentapi.SkillDeclaredFiles, Files: declared.files}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range got {
			if item.Kind == archive.EvidenceKindSkillSnapshot {
				t.Fatal("nonbody mode retained declared snapshot")
			}
		}
	}
}

func TestDeclaredSkillsRejectEscapesMalformedAndUndeclaredFiles(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	root := filepath.Join(project, "package")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(project, "outside.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		id   string
	}{{"parent", "../outside.md", "ok"}, {"absolute", outside, "ok"}, {"backslash", "nested\\body.md", "ok"}, {"empty-component", "a//b.md", "ok"}, {"nul", "bad\x00.md", "ok"}, {"invalid-utf8", "bad\xff.md", "ok"}, {"id-path", "x.md", "../bad"}, {"directory", "directory", "ok"}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ObserveSkills(SkillOptions{Harness: "muse", ObservedAt: time.Now(), Mode: config.SkillEvidenceBody, Locations: []agentapi.SkillRoot{{Path: root, ProjectBoundary: project, Scope: "project_muse_test", Layout: agentapi.SkillDeclaredFiles, Files: []agentapi.SkillFile{{Path: test.path, ID: test.id}}}}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			if bytes.Contains(raw, []byte("OUTSIDE_SENTINEL")) || len(got) != 1 || got[0].Payload["inventory_complete"] != false {
				t.Fatalf("unsafe declaration accepted: %s", raw)
			}
		})
	}
	t.Run("package-symlink", func(t *testing.T) {
		t.Parallel()
		if err := os.Symlink(outside, filepath.Join(root, "link.md")); err != nil {
			t.Skip(err)
		}
		got, err := ObserveSkills(SkillOptions{Harness: "muse", ObservedAt: time.Now(), Locations: []agentapi.SkillRoot{{Path: root, ProjectBoundary: project, Scope: "project_muse_test", Layout: agentapi.SkillDeclaredFiles, Files: []agentapi.SkillFile{{ID: "link", Path: "link.md"}}}}})
		if err != nil || len(got) != 1 || got[0].Payload["inventory_complete"] != false {
			t.Fatal("package escape used project-wide directory bounds")
		}
	})
	t.Run("private-scope", func(t *testing.T) {
		t.Parallel()
		got, err := ObserveSkills(SkillOptions{Harness: "muse", ObservedAt: time.Now(), Locations: []agentapi.SkillRoot{{Path: root, Scope: root, Layout: agentapi.SkillDeclaredFiles}}})
		raw, _ := json.Marshal(got)
		if err != nil || bytes.Contains(raw, []byte(root)) || got[0].Payload["inventory_complete"] != false {
			t.Fatal("raw package scope retained")
		}
	})
}

func TestDeclaredSkillBudgetIsSharedAcrossRootsAndPhysicalFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "body.md")
	if err := os.WriteFile(path, []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	files := make([]agentapi.SkillFile, maxSkillsPerRoot+1)
	for i := range files {
		files[i] = agentapi.SkillFile{ID: fmt.Sprintf("skill-%03d", i), Path: "body.md"}
	}
	budget := skillReadBudget{remaining: maxSnapshotBytes, declarations: maxSkillsPerRoot}
	got, err := observeRoot("muse", skillRoot{path: root, user: root, scope: "test", layout: agentapi.SkillDeclaredFiles, files: files}, time.Now(), &budget, config.SkillEvidenceMetadata)
	if err != nil || len(got) != 1 || got[0].Payload["inventory_complete"] != false || budget.declarations != 0 || len(budget.cache) != 1 || budget.remaining != maxSnapshotBytes-5 {
		t.Fatal("declaration/physical byte budget reset", err)
	}
	later, err := observeRoot("muse", skillRoot{path: root, user: root, scope: "other", layout: agentapi.SkillDeclaredFiles, files: files[:1]}, time.Now(), &budget, config.SkillEvidenceMetadata)
	if err != nil || later[0].Payload["inventory_complete"] != false || len(budget.cache) != 1 {
		t.Fatal("second root replenished declaration allowance")
	}
	// Exact inspected length is the read ceiling; growth/shrink or inode swap fails.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 6)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(path, 5, info); err == nil {
		t.Fatal("growing file bypassed reserved read budget")
	}
}
