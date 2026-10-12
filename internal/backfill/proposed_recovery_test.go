package backfill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

func firstRunRecoveryFixture(t *testing.T, reverse bool) (*tree, Environment, config.Config, string, string) {
	t.Helper()
	tr := newTree(t)
	root := tr.repo("home/repo")
	gone := tr.path("home/.codex/worktrees/deleted/repo")
	liveID, goneID := "00000000-0000-0000-0000-000000000011", "00000000-0000-0000-0000-000000000099"
	if reverse {
		liveID, goneID = goneID, liveID
	}
	for id, cwd := range map[string]string{liveID: root, goneID: gone} {
		body := strings.Replace(codexTranscript(id, id, cwd, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`, 1)
		tr.write(filepath.Join("home", codexFile(id)), body)
	}
	env := tr.env()
	key := archive.RepoKey("https://example.test/acme/repo")
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return true }
	return tr, env, config.Config{Archive: archive.Config{Enabled: true}}, root, goneID
}

func TestFirstRunRecoveryReviewsLiveDestinationBeforeDeletedCandidates(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "live_first", true: "deleted_first"}[reverse], func(t *testing.T) {
			_, env, cfg, root, goneID := firstRunRecoveryFixture(t, reverse)
			p := plan(t, env, nil, cfg, Filters{})
			c := candidate(t, p, goneID)
			if len(p.Imported()) != 2 || c.ProjectRoot != root || c.ProjectIncluded || c.ProjectResolution == nil {
				t.Fatalf("%+v", p.Candidates)
			}
			if len(cfg.Archive.Projects) != 0 {
				t.Fatal("planning modified committed configuration")
			}
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
			changes, err := ApplyToConfig(&cfg, p, fixedNow)
			if err != nil || len(changes.ProjectIDs) != 1 {
				t.Fatal(changes, err)
			}
			want := sourcefacts.RecoveryContext(cfg.Archive.Projects, nil, filepath.Clean)
			if c.ProjectResolution.PolicyContext != want {
				t.Fatalf("policy did not bind confirmed projects: %+v", c.ProjectResolution)
			}
			home := t.TempDir()
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			result, err := (Registration{Context: t.Context(), Home: home, Store: store, AdmittedAt: fixedNow}).Run(p.Imported())
			if err != nil || len(result.Sessions) != 2 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestFirstRunRecoveryCannotHideCloneOrUnknownEvidence(t *testing.T) {
	for _, kind := range []string{"clone", "excluded", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			clone := tr.repo("home/clone")
			id := "00000000-0000-0000-0000-000000000022"
			tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, clone, fixedNow.Add(-48*time.Hour)))
			if kind == "excluded" {
				cfg.Archive.Projects = []archive.ProjectActivation{project(clone, false)}
			}
			original := env.RepositoryIdentity
			env.RepositoryIdentity = func(ctx context.Context, path string) sourcefacts.RepositoryIdentity {
				if path == clone && kind == "unknown" {
					return sourcefacts.RepositoryIdentity{}
				}
				return original(ctx, path)
			}
			p := plan(t, env, nil, cfg, Filters{Projects: []string{root}, Since: fixedNow.Add(-24 * time.Hour).Format(dateLayout)})
			if c := candidate(t, p, goneID); c.Skip == "" || c.ProjectResolution != nil || c.ProjectRoot != "" {
				t.Fatalf("hidden evidence regained uniqueness: %+v", c)
			}
			if c := candidate(t, p, id); c.Skip != SkipFilteredOut {
				t.Fatalf("clone wasn't hidden by output filters: %+v", c)
			}
		})
	}
}

func TestFirstRunRecoveryRenewalRejectsStaleHiddenCloneAndCancellation(t *testing.T) {
	_, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	current := true
	env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return current }
	p := plan(t, env, nil, cfg, Filters{})
	current = false
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("stale identity admitted")
	}
	if len(cfg.Archive.Projects) != 0 {
		t.Fatal("preflight changed policy")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.CheckRecovery(ctx); err == nil {
		t.Fatal("cancelled confirmation accepted")
	}
}

func TestFirstRunRecoveryPendingHeaderCannotProposeDestination(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	id := "00000000-0000-0000-0000-000000000011"
	path := filepath.Join("home", codexFile(id))
	body := strings.Replace(codexTranscript(id, id, root, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"source":"cli","forked_from_id":"00000000-0000-0000-0000-000000000055"`, 1)
	tr.write(path, body)
	p := plan(t, env, nil, cfg, Filters{})
	if c := candidate(t, p, goneID); c.Skip != SkipWorktreeUnresolved {
		t.Fatalf("pending history invented destination: %+v", c)
	}
	if slices.ContainsFunc(p.Imported(), func(c Candidate) bool { return c.ProjectRoot == root }) {
		t.Fatal("pending destination imported")
	}
}

func TestFirstRunRecoveryUsesValidatedCursorLiveWitnessBeforeOutputFilters(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	liveID := "00000000-0000-0000-0000-000000000011"
	if err := os.Remove(tr.path(filepath.Join("home", codexFile(liveID)))); err != nil {
		t.Fatal(err)
	}
	tr.write("home/Library/Application Support/Cursor/User/workspaceStorage/current/workspace.json", fmt.Sprintf(`{"folder":%q}`, "file://"+root))
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(root), "agent-transcripts", "live-cursor", "live-cursor.jsonl"), cursorTranscript)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	c := candidate(t, p, goneID)
	if c.Skip != "" || c.ProjectRoot != root || c.ProjectIncluded || c.ProjectResolution == nil {
		t.Fatalf("%+v", c)
	}
	if cursor := candidate(t, p, "live-cursor"); cursor.Skip != SkipFilteredOut {
		t.Fatal(cursor)
	}
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
	changes, err := ApplyToConfig(&cfg, p, fixedNow)
	if err != nil || len(changes.ProjectIDs) != 1 {
		t.Fatal(changes, err)
	}
}

func TestFirstRunRecoveryLiveWitnessCannotDisappearBeforeConfirmation(t *testing.T) {
	tr, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	p := plan(t, env, nil, cfg, Filters{})
	if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("lost live witness certified inventory")
	}
}

func TestFirstRunRecoveryRequiresCommittedPolicyAndCurrentAdmissionEvidence(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncommitted", true: "stale_after_commit"}[committed], func(t *testing.T) {
			_, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
			current := true
			env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return current }
			p := plan(t, env, nil, cfg, Filters{})
			if committed {
				if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
					t.Fatal(err)
				}
				current = false
			}
			home := t.TempDir()
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			result, err := (Registration{Context: t.Context(), Home: home, Store: store, AdmittedAt: fixedNow}).Run([]Candidate{candidate(t, p, goneID)})
			if err != nil || len(result.Sessions) != 0 || result.NotAdmitted != 1 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestFirstRunRecoveryMalformedCloneHeaderCannotCertifyUniqueness(t *testing.T) {
	tr, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
	clone := tr.repo("home/clone")
	id := "00000000-0000-0000-0000-000000000022"
	other := "00000000-0000-0000-0000-000000000033"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(other, other, clone, fixedNow.Add(-time.Hour)))
	p := plan(t, env, nil, cfg, Filters{})
	if c := candidate(t, p, goneID); c.Skip != SkipWorktreeUnresolved || c.ProjectResolution != nil {
		t.Fatalf("malformed header certified inventory: %+v", c)
	}
}

func TestFirstRunRecoveryFilteredMalformedCursorCannotProposeDestination(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
		t.Fatal(err)
	}
	tr.write("home/Library/Application Support/Cursor/User/workspaceStorage/current/workspace.json", fmt.Sprintf(`{"folder":%q}`, "file://"+root))
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(root), "agent-transcripts", "bad-cursor", "bad-cursor.jsonl"), "unrecognized native format\n")
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.Skip != SkipWorktreeUnresolved || c.ProjectResolution != nil {
		t.Fatalf("unvalidated Cursor format invented destination: %+v", c)
	}
	if len(p.Imported()) != 0 {
		t.Fatal(p.Imported())
	}
}

func TestFirstRunRecoveryPendingWitnessCannotReplaceLostEligibleSource(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	id := "00000000-0000-0000-0000-000000000022"
	body := strings.Replace(codexTranscript(id, id, root, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"source":"cli","forked_from_id":"00000000-0000-0000-0000-000000000055"`, 1)
	tr.write(filepath.Join("home", codexFile(id)), body)
	p := plan(t, env, nil, cfg, Filters{})
	if len(p.Imported()) != 2 {
		t.Fatal(p.Candidates)
	}
	if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("pending witness replaced vanished eligible destination source")
	}
}

func TestFirstRunRecoveryArchivedEmptyCodexCannotProposeDestination(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	id := "00000000-0000-0000-0000-000000000011"
	body := codexTranscript(id, id, root, fixedNow.Add(-48*time.Hour))
	// A supported header and no conversation is insufficient eligibility.
	tr.write(filepath.Join("home", codexFile(id)), strings.SplitN(body, "\n", 2)[0]+"\n")
	p := plan(t, env, states{id: SkipAlreadyArchived}, cfg, Filters{})
	if c := candidate(t, p, goneID); c.ProjectResolution != nil || c.Skip == "" {
		t.Fatalf("header-only hidden source invented destination: %+v", c)
	}
}

func TestFirstRunRecoveryNewCloneRequiresNewPlan(t *testing.T) {
	tr, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	p := plan(t, env, nil, cfg, Filters{})
	clone := tr.repo("home/clone")
	id := "00000000-0000-0000-0000-000000000022"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, clone, fixedNow.Add(-time.Hour)))
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("new live clone escaped inventory renewal")
	}
}

func TestFirstRunRecoveryFilteredClaudeLocatorCannotProposeDestination(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	tr.write(filepath.Join("home", claudeFile("repo", "locator")), fmt.Sprintf(`{"cwd":%q}`+"\n", root))
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectResolution != nil || c.Skip == "" {
		t.Fatalf("raw Claude locator invented destination: %+v", c)
	}
}

func TestFirstRunRecoveryUnknownFileOwnershipCannotCertifyUniqueness(t *testing.T) {
	tr, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
	// A Codex session whose folder is unknown is a gap in Codex's own
	// evidence, so it still blocks Codex recoveries.
	unknown := "00000000-0000-0000-0000-000000000033"
	tr.write(filepath.Join("home", codexFile(unknown)), codexTranscript(unknown, unknown, "", fixedNow))
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectResolution != nil || c.Skip == "" {
		t.Fatalf("unknown live ownership certified uniqueness: %+v", c)
	}
	if c := candidate(t, p, goneID); c.Diagnostic == nil || c.Diagnostic.Detail != CauseSessionFolderUnknown {
		t.Fatalf("cause not named: %+v", c.Diagnostic)
	}
}

func TestFirstRunRecoveryExactMappingRetainsUnionContext(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
	clone := tr.repo("home/hidden-clone")
	id := "00000000-0000-0000-0000-000000000022"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, clone, fixedNow))
	gone := filepath.Join(env.Home, ".codex", "worktrees", "deleted", "repo")
	filters := Filters{Projects: []string{root}, ProjectMappings: map[string]string{gone: root}}
	p := plan(t, env, nil, cfg, filters)
	c := candidate(t, p, goneID)
	projects := append(slices.Clone(cfg.Archive.Projects), project(clone, true))
	want := sourcefacts.RecoveryContext(projects, filters.ProjectMappings, env.resolved)
	if c.ProjectResolution == nil || c.ProjectResolution.Method != "explicit_mapping" || c.ProjectResolution.Context != want {
		t.Fatalf("mapping lost complete evidence context: %+v", c.ProjectResolution)
	}
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryPolicyIncludesFinalNestedDecisions(t *testing.T) {
	tr, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
	folder := tr.mkdir("home/notes")
	nested := tr.repo("home/notes/nested")
	id := "00000000-0000-0000-0000-000000000022"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, folder, fixedNow.Add(-time.Hour)))
	p := plan(t, env, nil, cfg, Filters{})
	c := candidate(t, p, goneID)
	if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cfg.Archive.Projects, func(p archive.ProjectActivation) bool { return p.Root == nested && !p.Included }) {
		t.Fatal("fixture did not commit its nested exclusion", cfg.Archive.Projects)
	}
	if c.ProjectResolution == nil || c.ProjectResolution.PolicyContext != sourcefacts.RecoveryContext(cfg.Archive.Projects, nil, filepath.Clean) {
		t.Fatal("policy omitted final nested decisions", c.ProjectResolution)
	}
}

func TestFirstRunRecoveryPlainFolderBecomingCloneRequiresNewPlan(t *testing.T) {
	tr, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	folder := tr.mkdir("home/future-clone")
	id := "00000000-0000-0000-0000-000000000022"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, folder, fixedNow.Add(-48*time.Hour)))
	p := plan(t, env, nil, cfg, Filters{Since: fixedNow.Add(-24 * time.Hour).Format(dateLayout)})
	tr.mkdir("home/future-clone/.git")
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("known plain folder acquired clone ownership without renewing union")
	}
}
