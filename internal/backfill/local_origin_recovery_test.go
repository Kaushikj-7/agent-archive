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
	"github.com/wangjohn/agent-archive/internal/state"
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
			tr, env, cfg, root, goneID, clone, target := localOriginRecoveryFixture(t)
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
			data := t.TempDir()
			if err := config.Save(data, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(data)
			if err != nil {
				t.Fatal(err)
			}
			registration := Registration{Context: t.Context(), Home: data, Store: store, AdmittedAt: fixedNow}
			first, err := registration.Run(p.Imported())
			if err != nil || len(first.Sessions) != len(p.Imported()) {
				t.Fatal(first, err)
			}
			// A distinct deleted session is recoverable after the first import
			// commits the formerly proposed local-origin clone to configuration.
			const secondID = "00000000-0000-0000-0000-000000000098"
			addRepoRollout(tr, secondID, tr.path("home/.codex/worktrees/deleted/repo"))
			second := plan(t, env, nil, cfg, Filters{})
			c = candidate(t, second, secondID)
			if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("second import: %+v %+v", c, c.Diagnostic)
			}
			temporaryRecoveryGit(t, target, "config", "remote.origin.url", "https://example.test/acme/repo")
			if err := second.CheckRecovery(t.Context()); err == nil {
				t.Fatal("changed chain admitted a now-ambiguous recovery")
			}
			stale, err := registration.Run([]Candidate{c})
			if err != nil || len(stale.Sessions) != 0 || stale.NotAdmitted != 1 {
				t.Fatal("changed chain reached registration", stale, err)
			}
		})
	}
}

type localRecoveryCase string

const (
	localRecoverySameKey           localRecoveryCase = "same_key"
	localRecoveryExcludedClone     localRecoveryCase = "excluded_clone"
	localRecoveryNestedExclusion   localRecoveryCase = "nested_exclusion"
	localRecoveryOriginalExclusion localRecoveryCase = "original_exclusion"
	localRecoveryCycle             localRecoveryCase = "cycle"
	localRecoveryMissing           localRecoveryCase = "missing"
	localRecoveryUnreadable        localRecoveryCase = "unreadable"
	localRecoveryConflict          localRecoveryCase = "conflict"
	localRecoveryUnresolved        localRecoveryCase = "unresolved"
)

func TestLocalOriginRecoveryKeepsCompetingClonesAndExclusions(t *testing.T) {
	for _, kind := range []localRecoveryCase{localRecoverySameKey, localRecoveryExcludedClone, localRecoveryNestedExclusion, localRecoveryOriginalExclusion, localRecoveryCycle, localRecoveryMissing, localRecoveryUnreadable, localRecoveryConflict, localRecoveryUnresolved} {
		for _, configured := range []bool{false, true} {
			t.Run(string(kind)+map[bool]string{false: "/proposed", true: "/configured"}[configured], func(t *testing.T) {
				tr, env, cfg, root, goneID, clone, target := localOriginRecoveryFixture(t)
				if configured {
					cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(clone, true)}
				}
				want := sourcefacts.RecoveryAmbiguous
				switch kind {
				case localRecoverySameKey, localRecoveryExcludedClone:
					// Reaching an already configured clone must retain clone as a
					// distinct physical competitor rather than coalescing it into root.
					temporaryRecoveryGit(t, clone, "config", "remote.origin.url", root)
					if kind == localRecoveryExcludedClone {
						cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(clone, false)}
					}
				case localRecoveryNestedExclusion:
					cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(tr.mkdir("home/repo/private"), false)}
					want = sourcefacts.RecoverySubtreeUnavailable
				case localRecoveryOriginalExclusion:
					cfg.Archive.Projects = []archive.ProjectActivation{project(root, true), project(filepath.Dir(tr.path("home/.codex/worktrees/deleted/repo")), false)}
					want = sourcefacts.RecoveryExcluded
				case localRecoveryCycle:
					temporaryRecoveryGit(t, target, "config", "remote.origin.url", clone)
					want = sourcefacts.RecoveryOriginCycle
				case localRecoveryMissing:
					temporaryRecoveryGit(t, clone, "config", "remote.origin.url", tr.path("missing-target"))
					want = sourcefacts.RecoveryOriginMissing
				case localRecoveryUnreadable:
					path := filepath.Join(target, ".git", "config")
					if err := os.Chmod(path, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(path, 0600) })
					want = sourcefacts.RecoveryOriginUnreadable
				case localRecoveryConflict:
					temporaryRecoveryGit(t, clone, "config", "--add", "remote.origin.url", "https://example.test/acme/repo")
					want = sourcefacts.RecoveryOriginConflict
				case localRecoveryUnresolved:
					temporaryRecoveryGit(t, target, "config", "--unset-all", "remote.origin.url")
					want = sourcefacts.RecoveryOriginUnresolved
				}
				p := plan(t, env, nil, cfg, Filters{})
				c := candidate(t, p, goneID)
				if kind == localRecoveryOriginalExclusion {
					if c.Skip != SkipExcludedProject {
						t.Fatalf("exclusion bypassed: %+v", c)
					}
					return
				}
				if c.Skip != SkipWorktreeUnresolved || c.ProjectResolution != nil || c.Diagnostic == nil || c.Diagnostic.Detail != DiagnosticDetail(want) {
					t.Fatalf("want %s: %+v %+v", want, c, c.Diagnostic)
				}
				if kind == localRecoveryNestedExclusion {
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
			tr, env, cfg, root, _, _, _ := localOriginRecoveryFixture(t)
			p := plan(t, env, nil, cfg, Filters{})
			clone := tr.mkdir("home/new-clone")
			temporaryRecoveryGit(t, clone, "init", "--quiet")
			target := tr.path("missing")
			if same {
				target = root
			}
			temporaryRecoveryGit(t, clone, "config", "remote.origin.url", target)
			addRepoRollout(tr, "00000000-0000-0000-0000-0000000000a1", clone)
			if err := p.CheckRecovery(context.Background()); err == nil {
				t.Fatal("new local-origin clone retained an unproven unique recovery")
			}
		})
	}
}
