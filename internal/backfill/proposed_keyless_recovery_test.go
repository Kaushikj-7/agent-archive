package backfill

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// A proposed clone whose origin is not a shareable remote (for example a local
// path to a deleted worktree) has a checkout root but no repository key. It
// cannot own the recorded key, so it must not block recovery into a configured
// project; configured, unobserved and budget-exhausted roots stay strict.
func TestFirstRunRecoveryKeylessProposedCloneDoesNotBlockConfiguredMatch(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	for _, kind := range []string{"proposed_keyless", "configured_keyless", "proposed_budget", "proposed_unobserved", "proposed_gains_key"} {
		t.Run(kind, func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
			clone := tr.repo("work/clone")
			id := "00000000-0000-0000-0000-000000000033"
			tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, clone, fixedNow.Add(-time.Hour)))
			if kind == "configured_keyless" {
				cfg.Archive.Projects = append(cfg.Archive.Projects, project(clone, true))
			}
			known := func(path string) sourcefacts.RepositoryIdentity {
				return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true, Dependencies: []sourcefacts.RepositoryDependency{{Path: path, Stamp: "s"}}}
			}
			gained := false
			env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
				switch {
				case path != clone || gained:
					return known(path)
				case kind == "proposed_budget":
					return sourcefacts.RepositoryIdentity{BudgetExhausted: true}
				case kind == "proposed_unobserved":
					return sourcefacts.RepositoryIdentity{}
				}
				// gitremote.IdentityObserver's answer for a checkout whose origin has no repository key.
				return sourcefacts.RepositoryIdentity{Root: path}
			}
			// Like gitremote.ProjectIdentityCurrent, stamps never validate unknown identity.
			env.RepositoryIdentityCurrent = func(id sourcefacts.RepositoryIdentity) bool { return id.Known && len(id.Dependencies) > 0 }
			p := plan(t, env, nil, cfg, Filters{})
			c := candidate(t, p, goneID)
			want := map[string]sourcefacts.RecoveryOutcome{
				"configured_keyless":  sourcefacts.RecoveryInventoryUnavailable,
				"proposed_budget":     sourcefacts.RecoveryBudgetExhausted,
				"proposed_unobserved": sourcefacts.RecoveryInventoryUnavailable,
			}[kind]
			if want != "" {
				if c.Skip != SkipWorktreeUnresolved || c.Diagnostic == nil || c.Diagnostic.Detail != DiagnosticDetail(want) || c.ProjectResolution != nil {
					t.Fatalf("want %s: %+v diagnostic %+v", want, c, c.Diagnostic)
				}
				return
			}
			if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil || c.ProjectResolution.Method != "recorded_repository" {
				t.Fatalf("keyless proposed clone blocked recovery: %+v diagnostic %+v", c, c.Diagnostic)
			}
			gained = kind == "proposed_gains_key"
			err := p.CheckRecovery(t.Context())
			if gained && err == nil {
				t.Fatal("proposed clone that gained the recorded key kept the unique match")
			}
			if !gained && err != nil {
				t.Fatal(err)
			}
		})
	}
}
