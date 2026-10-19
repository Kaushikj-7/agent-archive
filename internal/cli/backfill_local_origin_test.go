package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
)

// Exercise the real CLI composition and Git observer with temporary homes,
// native stores and configuration. The dry runs must preserve committed state.
func TestBackfillLocalOriginsUseConsistentCLIRecoveryEvidence(t *testing.T) {
	f, _, cfg, _ := firstRunImportPlan(t)
	t.Setenv("HOME", f.userHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.root, "xdg"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(root string, args ...string) {
		t.Helper()
		if out, err := gitremote.ExecRunner(t.Context(), root, append([]string{"-C", root}, args...)...); err != nil {
			t.Fatalf("synthetic Git %v: %s: %v", args, out, err)
		}
	}
	root := filepath.Join(f.userHome, "agent-archive")
	clone := filepath.Join(f.userHome, "personal-website")
	target := filepath.Join(f.userHome, "mirror")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-archive", "levenshtein", "personal-website", "mirror"} {
		path := filepath.Join(f.userHome, name)
		git(path, "init", "--quiet")
		git(path, "config", "remote.origin.url", "https://example.test/acme/"+name)
	}
	git(root, "config", "remote.origin.url", "https://example.test/acme/repo")
	git(clone, "config", "remote.origin.url", target)
	for _, configured := range []bool{false, true} {
		cfg.Archive.Projects = nil
		if configured {
			cfg.Archive.Projects = []archive.ProjectActivation{{Root: root, Included: true, ProjectID: archive.ProjectID(root)}, {Root: clone, Included: true, ProjectID: archive.ProjectID(clone)}}
		}
		if err := config.Save(f.data, cfg); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(f.data, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		out, stderr, code := f.run(t, "--dry-run", "--json", "--harness", "codex")
		if code != 0 {
			t.Fatalf("configured=%v: %d %s", configured, code, stderr)
		}
		var result struct {
			Projects []struct {
				Root     string         `json:"root"`
				Sessions map[string]int `json:"sessions"`
			} `json:"projects"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		recovered := 0
		for _, p := range result.Projects {
			if p.Root == root {
				recovered = p.Sessions["codex"]
			}
		}
		if recovered != 2 {
			t.Fatalf("configured=%v: wanted both live and recovered Codex sessions: %s", configured, out)
		}
		after, err := os.ReadFile(filepath.Join(f.data, "config.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("dry run changed configuration", err)
		}
	}
	git(clone, "config", "remote.origin.url", root)
	out, stderr, code := f.run(t, "--dry-run", "--json", "--harness", "codex")
	if code != 0 {
		t.Fatal(code, stderr)
	}
	var ambiguous struct {
		Diagnostics []struct {
			Detail string `json:"detail"`
			Count  int    `json:"count"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(out), &ambiguous); err != nil {
		t.Fatal(err)
	}
	for _, d := range ambiguous.Diagnostics {
		if d.Detail == "project_ambiguous" && d.Count == 1 {
			return
		}
	}
	t.Fatalf("a second physical clone must remain ambiguous: %s", out)
}
