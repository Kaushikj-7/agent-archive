package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/pairing"
)

type projectRequirementEdit struct {
	name    string
	answers string
}

// Pairing reviews consent before staging credentials and probing storage.
// Returning from an edit must retain that unchecked state too.
func TestPairingReviewDoesNotClaimUnprobedStorage(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"cancel\n", "edit\nretention\ncustom\n30\ncancel\n"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			home, userHome, root := t.TempDir(), t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			cfg := config.Config{Harnesses: []string{"claude"}, RetentionDays: 90, SkillEvidence: config.SkillEvidenceMetadata, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "unprobed"}, Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: true}}}}
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(input), &out)
			defer p.close()
			_, err := reviewPairingSettings(p, pairing.Payload{}, cfg, config.Config{}, false, userHome, setupOptions{}, env)
			if err == nil {
				t.Fatal("cancel unexpectedly accepted")
			}
			wantReviews := 1
			if strings.HasPrefix(input, "edit") {
				wantReviews = 2
			}
			if strings.Contains(out.String(), "✓ Storage connected") || strings.Count(out.String(), "Storage not checked yet") != wantReviews || !setupContainsText(out.String(), "Connection will be checked after you confirm these settings") {
				t.Fatalf("unprobed pairing review claims readiness:\n%s", &out)
			}
		})
	}
}

var projectRequirementEdits = []projectRequirementEdit{
	{"included-scope", "codex-scope\nincluded-projects\n"},
	{"add-claude", "apps\ny\ny\ny\nn\n"},
	{"add-cursor", "apps\ny\ny\nn\ny\n"},
}

func zeroProjectCodexSetup(t *testing.T) (Env, string, string) {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "all-projects")
	return env, home, userHome
}

func setupCaptureFiles(t *testing.T, env Env, home, userHome string, cfg config.Config) map[string][]byte {
	t.Helper()
	paths := []string{filepath.Join(home, "config.json"), filepath.Join(home, "setup-transaction.json")}
	for _, app := range []string{"codex", "claude", "cursor"} {
		paths = append(paths, env.hookFiles(userHome)[app])
	}
	for _, path := range cfg.HookFiles {
		paths = append(paths, path)
	}
	files := map[string][]byte{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			files[path] = nil
			continue
		}
		must(t, err)
		files[path] = data
	}
	return files
}

func TestSetupReviewRequiresProjectsAfterZeroProjectScopeOrAppEdit(t *testing.T) {
	t.Parallel()
	for _, edit := range projectRequirementEdits {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			env, home, userHome := zeroProjectCodexSetup(t)
			cfg := mustLoadConfig(t, home)
			before := setupCaptureFiles(t, env, home, userHome, cfg)
			// A save answer is rejected in the real review loop, then cancel.
			out := setupRun(t, env, "retention\n90-days\nedit\n"+edit.answers+"yes\nno\n", 0)
			if !strings.Contains(out, "Project required") || !strings.Contains(out, "Fix the blocking checks first") || strings.Contains(out, "Setup complete") {
				t.Fatalf("invalid review edit committed or lacked correction: %s", out)
			}
			if !reflect.DeepEqual(before, setupCaptureFiles(t, env, home, userHome, cfg)) {
				t.Fatal("rejected review changed active config, journal or hooks")
			}
		})
	}
}

func TestSetupReviewProjectEditRepairsZeroProjectScopeOrAppChange(t *testing.T) {
	t.Parallel()
	for _, edit := range projectRequirementEdits {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			env, home, _ := zeroProjectCodexSetup(t)
			project, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			out := setupRun(t, env, "retention\n90-days\nedit\n"+edit.answers+"edit\nprojects\n"+project+"\n\nyes\n", 0)
			cfg := mustLoadConfig(t, home)
			if !strings.Contains(out, "Setup complete") || includedProjects(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != project {
				t.Fatalf("project correction did not commit: %+v %s", cfg.Archive.Projects, out)
			}
			if edit.name == "included-scope" && cfg.EffectiveCodexCaptureScope() != config.CodexIncludedProjects {
				t.Fatal("project correction lost chosen included scope")
			}
			if edit.name == "add-claude" && !containsString(cfg.Harnesses, "claude") || edit.name == "add-cursor" && !containsString(cfg.Harnesses, "cursor") {
				t.Fatal("project correction lost selected app")
			}
		})
	}
}

func TestPairingReviewRequiresProjectsAfterZeroProjectScopeOrAppEdit(t *testing.T) {
	t.Parallel()
	for _, edit := range projectRequirementEdits {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			env, home, userHome := zeroProjectCodexSetup(t)
			cfg := mustLoadConfig(t, home)
			before := setupCaptureFiles(t, env, home, userHome, cfg)
			var out bytes.Buffer
			p := newPrompter(strings.NewReader("edit\n"+edit.answers+"save\ncancel\n"), &out)
			_, err := reviewPairingSettings(p, pairing.Payload{}, cfg, cfg, true, userHome, setupOptions{}, env)
			if err == nil || !strings.Contains(err.Error(), "pairing cancelled") || !strings.Contains(out.String(), "Include a project") {
				t.Fatalf("pairing accepted invalid edited scope/apps: %v %s", err, &out)
			}
			if !reflect.DeepEqual(before, setupCaptureFiles(t, env, home, userHome, cfg)) {
				t.Fatal("rejected pairing review changed active files")
			}
			project := t.TempDir()
			out.Reset()
			p = newPrompter(strings.NewReader("edit\n"+edit.answers+"edit\nprojects\n"+project+"\n\nsave\n"), &out)
			corrected, err := reviewPairingSettings(p, pairing.Payload{}, cfg, cfg, true, userHome, setupOptions{}, env)
			must(t, err)
			if includedProjects(corrected.Archive.Projects) != 1 {
				t.Fatal("pairing project correction lost its inclusion")
			}
		})
	}
}

func TestSetupTransactionRejectsProjectlessCaptureBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	for _, excludedOnly := range []bool{false, true} {
		for _, app := range []string{"codex", "claude", "cursor"} {
			t.Run(app+map[bool]string{false: "-empty", true: "-excluded"}[excludedOnly], func(t *testing.T) {
				t.Parallel()
				env, home, userHome := zeroProjectCodexSetup(t)
				old := mustLoadConfig(t, home)
				next := old
				next.Harnesses = []string{app}
				must(t, config.SetCodexCaptureScope(&next, config.CodexIncludedProjects))
				if excludedOnly {
					next.Archive.Projects = []archive.ProjectActivation{{Root: t.TempDir(), Included: false}}
				}
				before := snapshotSetupDirectories(t, home, userHome)
				if err := applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env); err == nil || !strings.Contains(err.Error(), "no project is included") {
					t.Fatalf("invalid setup transaction accepted: %v", err)
				}
				if !reflect.DeepEqual(before, snapshotSetupDirectories(t, home, userHome)) {
					t.Fatal("rejected transaction changed config, hooks, locks or other files")
				}
			})
		}
	}
}

func TestSetupTransactionAllowsNoSelectedAppsWithoutProjects(t *testing.T) {
	t.Parallel()
	env, home, userHome := zeroProjectCodexSetup(t)
	old := mustLoadConfig(t, home)
	next := old
	next.Harnesses = nil
	must(t, config.SetCodexCaptureScope(&next, config.CodexIncludedProjects))
	must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
	if got := mustLoadConfig(t, home); len(got.Harnesses) != 0 || includedProjects(got.Archive.Projects) != 0 {
		t.Fatal("setup changed the no-selected-app project choice")
	}
}

func snapshotSetupDirectories(t *testing.T, roots ...string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, root := range roots {
		must(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			files[path], err = os.ReadFile(path)
			return err
		}))
	}
	return files
}
