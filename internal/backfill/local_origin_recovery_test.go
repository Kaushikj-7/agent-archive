package backfill

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func localOriginRecoveryFixture(t *testing.T) (*tree, Environment, config.Config, string, string, string, string) {
	t.Helper()
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	t.Setenv("HOME", tr.home)
	t.Setenv("XDG_CONFIG_HOME", tr.path("isolated-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	clone, target := tr.mkdir("home/local-clone"), tr.mkdir("home/mirror")
	for _, repo := range []string{root, clone, target} {
		temporaryRecoveryGit(t, repo, "init", "--quiet")
	}
	temporaryRecoveryGit(t, root, "config", "remote.origin.url", "https://example.test/acme/repo")
	temporaryRecoveryGit(t, target, "config", "remote.origin.url", "https://example.test/acme/other")
	temporaryRecoveryGit(t, clone, "config", "remote.origin.url", target)
	id := "00000000-0000-0000-0000-000000000033"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, clone, fixedNow.Add(-time.Hour)))
	observer := &gitremote.IdentityObserver{}
	env.RepositoryIdentity = observer.Lookup
	env.RepositoryIdentityCurrent = gitremote.ProjectIdentityCurrent
	return tr, env, cfg, root, goneID, clone, target
}

func TestLocalOriginRecoveryIsConsistentBeforeAndAfterConfiguration(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "proposed", true: "configured"}[configured], func(t *testing.T) {
			_, env, cfg, root, goneID, clone, target := localOriginRecoveryFixture(t)
			if configured {
				cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(clone, true)}
			}
			p := plan(t, env, nil, cfg, Filters{})
			c := candidate(t, p, goneID)
			if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("%+v %+v", c, c.Diagnostic)
			}
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
				t.Fatal(err)
			}
			second := plan(t, env, nil, cfg, Filters{})
			if c := candidate(t, second, goneID); c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("second import: %+v %+v", c, c.Diagnostic)
			}
			temporaryRecoveryGit(t, target, "config", "remote.origin.url", "https://example.test/acme/repo")
			if err := second.CheckRecovery(t.Context()); err == nil {
				t.Fatal("changed chain admitted a now-ambiguous recovery")
			}
		})
	}
}

func TestLocalOriginRecoveryKeepsCompetingClonesAndExclusions(t *testing.T) {
	for _, kind := range []string{"same_key", "excluded_clone", "nested_exclusion", "original_exclusion", "cycle", "missing", "unreadable", "conflict", "unresolved"} {
		for _, configured := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/proposed", true: "/configured"}[configured], func(t *testing.T) {
				tr, env, cfg, root, goneID, clone, target := localOriginRecoveryFixture(t)
				if configured {
					cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(clone, true)}
				}
				want := sourcefacts.RecoveryAmbiguous
				switch kind {
				case "same_key", "excluded_clone":
					// Reaching an already configured clone must retain clone as a
					// distinct physical competitor rather than coalescing it into root.
					temporaryRecoveryGit(t, clone, "config", "remote.origin.url", root)
					if kind == "excluded_clone" {
						cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(clone, false)}
					}
				case "nested_exclusion":
					cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(tr.mkdir("home/repo/private"), false)}
					want = sourcefacts.RecoverySubtreeUnavailable
				case "original_exclusion":
					cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(filepath.Dir(tr.path("home/.codex/worktrees/deleted/repo")), false)}
					want = sourcefacts.RecoveryExcluded
				case "cycle":
					temporaryRecoveryGit(t, target, "config", "remote.origin.url", clone)
					want = sourcefacts.RecoveryOriginCycle
				case "missing":
					temporaryRecoveryGit(t, clone, "config", "remote.origin.url", tr.path("missing-target"))
					want = sourcefacts.RecoveryOriginMissing
				case "unreadable":
					path := filepath.Join(target, ".git", "config")
					if err := os.Chmod(path, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(path, 0600) })
					want = sourcefacts.RecoveryOriginUnreadable
				case "conflict":
					temporaryRecoveryGit(t, clone, "config", "--add", "remote.origin.url", "https://example.test/acme/repo")
					want = sourcefacts.RecoveryOriginConflict
				case "unresolved":
					temporaryRecoveryGit(t, target, "config", "--unset-all", "remote.origin.url")
					want = sourcefacts.RecoveryOriginUnresolved
				}
				p := plan(t, env, nil, cfg, Filters{})
				c := candidate(t, p, goneID)
				if kind == "original_exclusion" {
					if c.Skip != SkipExcludedProject {
						t.Fatalf("exclusion bypassed: %+v", c)
					}
					return
				}
				if c.Skip != SkipWorktreeUnresolved || c.ProjectResolution != nil || c.Diagnostic == nil || c.Diagnostic.Detail != DiagnosticDetail(want) {
					t.Fatalf("want %s: %+v %+v", want, c, c.Diagnostic)
				}
				if kind == "nested_exclusion" {
					return
				}
				// Reviewed exact assignment bypasses unrelated incomplete or
				// competing inventory while retaining target-key and policy checks.
				cfg.Archive.Projects = append(cfg.Archive.Projects, project(root, true))
				mapped := plan(t, env, nil, cfg, Filters{ProjectMappings: map[string]string{tr.path("home/.codex/worktrees/deleted/repo"): root}})
				if c := candidate(t, mapped, goneID); c.Skip != "" || c.ProjectResolution == nil || c.ProjectResolution.Method != "explicit_mapping" {
					t.Fatalf("reviewed mapping blocked: %+v %+v", c, c.Diagnostic)
				}
			})
		}
	}
}

func TestAddedLocalOriginCloneCannotPreserveFalseUniqueness(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "unresolved", true: "same_key"}[same], func(t *testing.T) {
			tr, env, cfg, root, _, _, target := localOriginRecoveryFixture(t)
			p := plan(t, env, nil, cfg, Filters{})
			clone := tr.mkdir("home/new-clone")
			temporaryRecoveryGit(t, clone, "init", "--quiet")
			if same {
				target = root
			} else {
				target = tr.path("missing")
			}
			temporaryRecoveryGit(t, clone, "config", "remote.origin.url", target)
			addRepoRollout(tr, "00000000-0000-0000-0000-0000000000a1", clone)
			if err := p.CheckRecovery(context.Background()); err == nil {
				t.Fatal("new local-origin clone retained an unproven unique recovery")
			}
		})
	}
}
