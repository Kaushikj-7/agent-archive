package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

type confirmationFailureCase string

const (
	confirmationStale      confirmationFailureCase = "stale"
	confirmationCancelled  confirmationFailureCase = "cancelled"
	confirmationBatchSaved confirmationFailureCase = "batch_saved"
)

func firstRunImportPlan(t *testing.T) (*backfillFixture, backfill.Plan, config.Config, *bool) {
	t.Helper()
	f := newBackfillFixture(t)
	// This transaction fixture reviews file evidence only. The general fixture's
	// deliberately unknown database ownership must not certify uniqueness.
	if err := os.Remove(macCursorDatabase(f.userHome)); err != nil {
		t.Fatal(err)
	}
	// Its deliberately unresolved Cursor locator also belongs in the separate
	// conservative-inventory tests, rather than this successful transaction.
	unknown := filepath.Join(f.userHome, ".cursor", "projects", cursorSlugFor(filepath.Join(f.userHome, "no-such-folder")), "agent-transcripts", "k-lost", "k-lost.jsonl")
	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects = nil
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	const liveID = "0a9b3c4d-0000-4000-8000-0000000000aa"
	const goneID = "0a9b3c4d-0000-4000-8000-0000000000bb"
	relative := filepath.Join(".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+liveID+".jsonl")
	body, err := os.ReadFile(filepath.Join(f.userHome, relative))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.userHome, "agent-archive")
	gone := filepath.Join(f.userHome, ".codex", "worktrees", "deleted", "repo")
	bodyCopy := strings.ReplaceAll(string(body), liveID, goneID)
	bodyCopy = strings.Replace(bodyCopy, root, gone, 1)
	bodyCopy = strings.Replace(bodyCopy, `"source":"cli"`, `"source":"cli","git":{"repository_url":"https://example.test/acme/repo"}`, 1)
	f.write(t, strings.Replace(relative, liveID, goneID, 1), bodyCopy)
	env := f.env.backfillEnvironment(f.userHome, cfg)
	current := true
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		key := archive.RepoKey("https://example.test/acme/" + filepath.Base(path))
		if path == root {
			key = archive.RepoKey("https://example.test/acme/repo")
		}
		return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return current }
	p, err := backfill.BuildPlan(t.Context(), env, newArchiveState(f.data, cfg), cfg, backfill.Filters{Harnesses: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Imported()) != 2 {
		t.Fatalf("%+v", p.Candidates)
	}
	return f, p, cfg, &current
}

func TestFirstRunImportConfirmationCommitsBeforeBackgroundAdmission(t *testing.T) {
	f, p, cfg, _ := firstRunImportPlan(t)
	var out, stderr bytes.Buffer
	code := importPlanLocked(f.env, &out, &stderr, f.data, p, configFingerprint(cfg), true)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	confirmed, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed.Archive.Projects) != 1 {
		t.Fatalf("unexpected capture expansion: %+v", confirmed.Archive.Projects)
	}
	regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	imports := 0
	for _, reg := range regs {
		if reg.Imported() {
			imports++
		}
	}
	if imports != 2 {
		t.Fatalf("%d imported registrations", imports)
	}
}

func TestFirstRunImportConfirmationFailureDoesNotCommitProposedProjects(t *testing.T) {
	for _, failure := range []confirmationFailureCase{confirmationStale, confirmationCancelled, confirmationBatchSaved} {
		t.Run(string(failure), func(t *testing.T) {
			f, p, cfg, current := firstRunImportPlan(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case confirmationStale:
				// Every session the import would admit is stale: the live
				// session is left to the partial-admission test.
				*current = false
				for i := range p.Candidates {
					if p.Candidates[i].ProjectResolution == nil {
						p.Candidates[i].Skip = backfill.SkipFilteredOut
					}
				}
			case confirmationCancelled:
				cancel()
			case confirmationBatchSaved:
				f.env.backfillCheckpoint = func(step string) error {
					if step == "batch saved" {
						return errors.New("synthetic config transition failure")
					}
					return nil
				}
			}
			if _, err := commitImport(ctx, f.env, f.data, p, configFingerprint(cfg)); err == nil {
				t.Fatal("confirmation unexpectedly succeeded")
			}
			after, _, err := config.Load(f.data)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Archive.Projects) != 0 {
				t.Fatal("failed confirmation committed capture projects")
			}
			regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			for _, reg := range regs {
				if reg.Imported() {
					t.Fatal("failed confirmation admitted session")
				}
			}
		})
	}
}

func TestImportConfirmationValidationDeadlineStartsAfterCollectorWait(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "recovered"}[recovered], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, p, cfg, _ := firstRunImportPlan(t)
				if !recovered {
					for i := range p.Candidates {
						p.Candidates[i].ProjectResolution = nil
					}
				}
				release, err := lockCollector(f.data, "synthetic collector", f.env.now())
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					time.Sleep(35 * time.Second)
					release()
				}()
				// Stop after the configuration commit, before registration. The
				// real collector lock outlives the validation allowance but
				// stays within the supported wait.
				committed := false
				f.env.backfillCheckpoint = func(step string) error {
					if step == "committed" {
						committed = true
						return errors.New("synthetic stop after confirmation")
					}
					return nil
				}
				var out, stderr bytes.Buffer
				code := importPlanLocked(f.env, &out, &stderr, f.data, p, configFingerprint(cfg), true)
				if code != 1 || !committed {
					t.Fatalf("collector wait consumed validation deadline: %s", &stderr)
				}
				after, _, err := config.Load(f.data)
				if err != nil || len(after.Archive.Projects) != 1 {
					t.Fatal("confirmation did not commit the reviewed project", after.Archive.Projects, err)
				}
			})
		})
	}
}

// A recovered session whose evidence changed after the plan was confirmed is
// left out and reported; the rest of the confirmed plan is imported.
func TestFirstRunImportLeavesOutChangedRecoveryAndImportsTheRest(t *testing.T) {
	f, p, cfg, current := firstRunImportPlan(t)
	*current = false
	var out, stderr bytes.Buffer
	if code := importPlanLocked(f.env, &out, &stderr, f.data, p, configFingerprint(cfg), true); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	if !strings.Contains(out.String(), "Registered 1 session") || !strings.Contains(out.String(), "Not imported: 1 session whose project or source evidence changed after the plan was made. Run backfill again with the same options to import it.") {
		t.Fatalf("partial admission not reported:\n%s", out.String())
	}
	after, _, err := config.Load(f.data)
	if err != nil || len(after.Archive.Projects) != 1 {
		t.Fatal("live session's project was not added", after.Archive.Projects, err)
	}
	regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	imported := 0
	for _, reg := range regs {
		if reg.Imported() {
			imported++
			if reg.ProjectResolution != nil {
				t.Fatal("changed recovery was registered", reg.ProjectResolution)
			}
		}
	}
	if imported != 1 {
		t.Fatalf("%d imported registrations", imported)
	}
}
