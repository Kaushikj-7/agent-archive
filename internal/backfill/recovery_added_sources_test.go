package backfill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

const (
	addedRepoURL  = "https://example.test/acme/repo"
	addedOtherURL = "https://example.test/acme/other"
)

// addedRollout writes a Codex rollout for id run in cwd, recording origin
// (none when ""), in a day folder the plan did not list.
func addedRollout(tr *tree, id, cwd, origin string) string {
	body := codexTranscript(id, id, cwd, fixedNow.Add(-time.Hour))
	if origin != "" {
		body = strings.Replace(body, `"source":"cli"`, `"git":{"repository_url":"`+origin+`"},"source":"cli"`, 1)
	}
	return tr.write(filepath.Join("home", ".codex", "sessions", "2026", "10", "09", "rollout-2026-10-09T10-00-00-"+id+".jsonl"), body)
}

// addRepoRollout writes a rollout of the fixture's repository run in cwd.
func addRepoRollout(tr *tree, id, cwd string) {
	addedRollout(tr, id, cwd, addedRepoURL)
}

// addedSourcesFixture is firstRunRecoveryFixture whose repository identities
// follow the folder name: "other" is another repository, "keyless" has no
// origin, "unknown" cannot be read, and every other root is a clone of the
// fixture's repository.
func addedSourcesFixture(t *testing.T) (*tree, Environment, config.Config, string, string) {
	t.Helper()
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		switch filepath.Base(path) {
		case "other":
			return sourcefacts.RepositoryIdentity{Root: path, Key: archive.RepoKey(addedOtherURL), Known: true}
		case "keyless":
			return sourcefacts.RepositoryIdentity{Root: path}
		case "unknown":
			return sourcefacts.RepositoryIdentity{}
		}
		return sourcefacts.RepositoryIdentity{Root: path, Key: archive.RepoKey(addedRepoURL), Known: true}
	}
	return tr, env, cfg, root, goneID
}

// Transcripts added after planning block a recorded recovery only when they
// could name a competing clone of its repository; removed or replaced
// witnesses stay strict.
func TestAddedTranscriptsAfterPlanningBlockOnlyCompetingClones(t *testing.T) {
	const addedID = "00000000-0000-0000-0000-0000000000a1"
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, tr *tree, root string)
		current bool
	}{
		{name: "unchanged", change: func(*testing.T, *tree, string) {}, current: true},
		{name: "another repository", current: true, change: func(_ *testing.T, tr *tree, _ string) {
			addedRollout(tr, addedID, tr.repo("home/other"), addedOtherURL)
		}},
		{name: "plain folder", current: true, change: func(_ *testing.T, tr *tree, _ string) {
			addedRollout(tr, addedID, tr.mkdir("home/notes"), "")
		}},
		{name: "temporary clone", current: true, change: func(_ *testing.T, tr *tree, _ string) {
			addRepoRollout(tr, addedID, tr.repo("tmp/review"))
		}},
		{name: "deleted folder", current: true, change: func(_ *testing.T, tr *tree, _ string) {
			addRepoRollout(tr, addedID, tr.path("home/.codex/worktrees/other-deleted/repo"))
		}},
		{name: "witnessed root", current: true, change: func(_ *testing.T, tr *tree, root string) {
			addRepoRollout(tr, addedID, root)
		}},
		{name: "keyless clone", current: true, change: func(_ *testing.T, tr *tree, _ string) {
			addedRollout(tr, addedID, tr.repo("home/keyless"), "")
		}},
		{name: "unknown Claude Code folder", current: true, change: func(_ *testing.T, tr *tree, _ string) {
			// Another app's unknown evidence: the per-app residual.
			tr.write(filepath.Join("home", claudeFile("slug", "claude-added")), claudeTranscript("claude-added", "", fixedNow))
		}},
		{name: "clone of the same repository", change: func(_ *testing.T, tr *tree, _ string) {
			addRepoRollout(tr, addedID, tr.repo("home/clone"))
		}},
		{name: "clone found without a recorded origin", change: func(_ *testing.T, tr *tree, _ string) {
			// The clone's own Git identity decides, not the transcript's record.
			addedRollout(tr, addedID, tr.repo("home/clone"), "")
		}},
		{name: "unreadable repository", change: func(_ *testing.T, tr *tree, _ string) {
			addRepoRollout(tr, addedID, tr.repo("home/unknown"))
		}},
		{name: "unknown Codex folder", change: func(_ *testing.T, tr *tree, _ string) {
			addRepoRollout(tr, addedID, "")
		}},
		{name: "Codex store listed in part", change: func(t *testing.T, tr *tree, _ string) {
			t.Helper()
			// A new day folder that cannot be listed may hold a clone's
			// transcript: what the store holds now is unknown.
			tr.write(filepath.Join("home", ".codex", "sessions", "2026", "10", "10", "rollout-2026-10-10T10-00-00-"+addedID+".jsonl"), codexTranscript(addedID, addedID, tr.repo("home/clone"), fixedNow.Add(-time.Hour)))
			day := tr.path(filepath.Join("home", ".codex", "sessions", "2026", "10", "10"))
			if err := os.Chmod(day, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(day, 0o755) })
		}},
		{name: "removed witness", change: func(t *testing.T, tr *tree, root string) {
			t.Helper()
			addedRollout(tr, addedID, tr.repo("home/other"), addedOtherURL)
			if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replaced witness", change: func(t *testing.T, tr *tree, root string) {
			t.Helper()
			witness := tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))
			body, err := os.ReadFile(witness)
			if err != nil {
				t.Fatal(err)
			}
			next := tr.write("home/next.jsonl", string(body))
			if err := os.Rename(next, witness); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, cfg, root, goneID := addedSourcesFixture(t)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			if c := candidate(t, p, goneID); c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("fixture did not recover: %+v %+v", c, c.Diagnostic)
			}
			tc.change(t, tr, root)
			if err := p.CheckRecovery(t.Context()); (err == nil) != tc.current {
				t.Fatalf("CheckRecovery = %v, want current %v", err, tc.current)
			}
		})
	}
}

// A clone found while planning makes the recovery ambiguous, as a clone
// discovery found would; an unrelated new transcript leaves it recovered,
// and a Codex transcript whose folder cannot be told blocks Codex only.
func TestAddedTranscriptsWhilePlanningJoinTheInventory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		add     func(tr *tree)
		outcome sourcefacts.RecoveryOutcome
		cause   DiagnosticDetail
	}{
		{name: "another repository", add: func(tr *tree) {
			addedRollout(tr, "00000000-0000-0000-0000-0000000000a2", tr.repo("home/other"), addedOtherURL)
		}},
		{name: "plain folder", add: func(tr *tree) { addedRollout(tr, "00000000-0000-0000-0000-0000000000a2", tr.mkdir("home/notes"), "") }},
		{name: "clone of the same repository", outcome: sourcefacts.RecoveryAmbiguous, add: func(tr *tree) {
			addRepoRollout(tr, "00000000-0000-0000-0000-0000000000a2", tr.repo("home/clone"))
		}},
		{name: "unknown Codex folder", outcome: sourcefacts.RecoveryInventoryUnavailable, cause: CauseNativeInventoryChanged, add: func(tr *tree) {
			addRepoRollout(tr, "00000000-0000-0000-0000-0000000000a2", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, cfg, root, goneID := addedSourcesFixture(t)
			store := filepath.Join(env.Home, ".codex", "sessions")
			stats := 0
			env.Lstat = func(path string) (fs.FileInfo, error) {
				if path == store {
					stats++
					// The second store stat begins the inventory renewal, after
					// discovery read every header.
					if stats == 2 {
						tc.add(tr)
					}
				}
				return os.Lstat(path)
			}
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			if stats < 2 {
				t.Fatal("inventory renewal did not run during planning")
			}
			c := candidate(t, p, goneID)
			if tc.outcome == "" {
				if c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatalf("unrelated added transcript blocked recovery: %+v %+v", c, c.Diagnostic)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal("the planned addition failed its own confirmation", err)
				}
				return
			}
			want := DiagnosticDetail(tc.outcome)
			if tc.cause != "" {
				want = tc.cause
			}
			if c.ProjectResolution != nil || c.Skip != SkipWorktreeUnresolved || c.Diagnostic == nil || c.Diagnostic.Detail != want {
				t.Fatalf("got %+v %+v, want %s", c, c.Diagnostic, want)
			}
		})
	}
}

// A clone added while planning is renewed at confirmation as the witness it
// became, and its own agent may keep appending to it.
func TestAddedWitnessRenewsThroughAppends(t *testing.T) {
	tr, env, cfg, root, goneID := addedSourcesFixture(t)
	store := filepath.Join(env.Home, ".codex", "sessions")
	stats := 0
	var added string
	env.Lstat = func(path string) (fs.FileInfo, error) {
		if path == store {
			if stats++; stats == 2 {
				added = addedRollout(tr, "00000000-0000-0000-0000-0000000000a3", tr.repo("home/other"), addedOtherURL)
			}
		}
		return os.Lstat(path)
	}
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectRoot != root || c.ProjectResolution == nil {
		t.Fatalf("%+v %+v", c, c.Diagnostic)
	}
	appendRecord(t, added, `{"more":1}`+"\n")
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal("an appended added witness failed renewal", err)
	}
	if err := os.Remove(added); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("removed added witness renewed")
	}
}

// Headers of added transcripts are read within a count and byte budget;
// past it the change is treated as unknown for every app.
func TestAddedTranscriptInspectionIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		added   int
		files   int
		bytes   int64
		current bool
	}{
		{name: "within budget", added: 2, files: 2, bytes: 1 << 20, current: true},
		{name: "over the count", added: 2, files: 1, bytes: 1 << 20},
		{name: "header cut by the bytes", added: 1, files: 2, bytes: 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, bytes := recoveryAddedSourceLimit, recoveryAddedHeaderBytes
			recoveryAddedSourceLimit, recoveryAddedHeaderBytes = tc.files, tc.bytes
			t.Cleanup(func() { recoveryAddedSourceLimit, recoveryAddedHeaderBytes = files, bytes })
			tr, env, cfg, _, _ := addedSourcesFixture(t)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			inv := inventoryBaseline(t, env)
			other := tr.repo("home/other")
			for n := range tc.added {
				addedRollout(tr, fmt.Sprintf("00000000-0000-0000-0000-0000000000b%d", n), other, addedOtherURL)
			}
			// Past the budget the change is unknown for every app, not only
			// the app whose transcripts were added.
			if added, ok := inv.added(t.Context()); ok != tc.current || (ok && len(added.candidates) != tc.added) {
				t.Fatalf("added = %d, %v; want current %v", len(added.candidates), ok, tc.current)
			}
			if err := p.CheckRecovery(t.Context()); (err == nil) != tc.current {
				t.Fatalf("CheckRecovery = %v, want current %v", err, tc.current)
			}
		})
	}
}

// inventoryBaseline observes env's native stores as planning does.
func inventoryBaseline(t *testing.T, env Environment) *recoverySourceInventory {
	t.Helper()
	inv := newRecoverySourceInventory(env)
	if _, err := enumerateDiscovery(t.Context(), inv.environment(), agentapi.DiscoveryImport, func(agentapi.DiscoveryCandidate) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return inv
}

// Only additions are judged by their headers: a removed or replaced observed
// transcript is a change of unknown effect for every app.
func TestAddedSourcesOnlyForAdditions(t *testing.T) {
	const witnessID = "00000000-0000-0000-0000-000000000011"
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, tr *tree)
		added  int
		ok     bool
	}{
		{name: "unchanged", change: func(*testing.T, *tree) {}, ok: true},
		{name: "added", added: 1, ok: true, change: func(_ *testing.T, tr *tree) {
			addedRollout(tr, "00000000-0000-0000-0000-0000000000e1", tr.repo("home/other"), addedOtherURL)
		}},
		{name: "added in a listed folder", added: 1, ok: true, change: func(_ *testing.T, tr *tree) {
			tr.write(filepath.Join("home", codexFile("00000000-0000-0000-0000-0000000000e2")), codexTranscript("00000000-0000-0000-0000-0000000000e2", "00000000-0000-0000-0000-0000000000e2", tr.repo("home/other"), fixedNow))
		}},
		{name: "removed", change: func(t *testing.T, tr *tree) {
			t.Helper()
			if err := os.Remove(tr.path(filepath.Join("home", codexFile(witnessID)))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replaced", change: func(t *testing.T, tr *tree) {
			t.Helper()
			witness := tr.path(filepath.Join("home", codexFile(witnessID)))
			body, err := os.ReadFile(witness)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tr.write("home/next.jsonl", string(body)), witness); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, _, _, _ := addedSourcesFixture(t)
			inv := inventoryBaseline(t, env)
			tc.change(t, tr)
			added, ok := inv.added(t.Context())
			if ok != tc.ok || len(added.candidates) != tc.added {
				t.Fatalf("added = %d, %v; want %d, %v", len(added.candidates), ok, tc.added, tc.ok)
			}
		})
	}
}

// partialAdmissionFixture plans recovered sessions into the fixture's
// repository, which only an archived session witnesses (so the import
// proposes it), and two sessions in a configured project: one live, one
// recovered into it. others are those two.
func partialAdmissionFixture(t *testing.T, recovered int) (cfg config.Config, p Plan, gone, others []string) {
	t.Helper()
	tr, env, cfg, root, goneID := addedSourcesFixture(t)
	other := tr.repo("home/other")
	cfg.Archive.Projects = []archive.ProjectActivation{project(other, true)}
	write := func(id, cwd, origin string) {
		body := codexTranscript(id, id, cwd, fixedNow.Add(-time.Hour))
		if origin != "" {
			body = strings.Replace(body, `"source":"cli"`, `"git":{"repository_url":"`+origin+`"},"source":"cli"`, 1)
		}
		tr.write(filepath.Join("home", codexFile(id)), body)
	}
	others = []string{"00000000-0000-0000-0000-0000000000c0", "00000000-0000-0000-0000-0000000000c1"}
	write(others[0], other, "")
	write(others[1], tr.path("home/.codex/worktrees/deleted-other/other"), addedOtherURL)
	gone = []string{goneID}
	for n := 1; n < recovered; n++ {
		id := fmt.Sprintf("00000000-0000-0000-0000-0000000000d%d", n)
		write(id, tr.path(fmt.Sprintf("home/.codex/worktrees/deleted-%d/repo", n)), addedRepoURL)
		gone = append(gone, id)
	}
	p = plan(t, env, states{"00000000-0000-0000-0000-000000000011": SkipAlreadyArchived}, cfg, Filters{Harnesses: []string{"codex"}})
	for _, id := range gone {
		if c := candidate(t, p, id); c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil || c.ProjectIncluded {
			t.Fatalf("fixture did not recover %s: %+v %+v", id, c, c.Diagnostic)
		}
	}
	if c := candidate(t, p, others[1]); c.Skip != "" || c.ProjectRoot != other || c.ProjectResolution == nil || !c.ProjectIncluded {
		t.Fatalf("fixture did not recover into the configured project: %+v %+v", c, c.Diagnostic)
	}
	if len(p.Imported()) != recovered+2 {
		t.Fatalf("%+v", p.Candidates)
	}
	return cfg, p, gone, others
}

// register commits cfg as ApplyToConfig changes it for admitted and
// registers admitted's sessions, as an import does.
func register(t *testing.T, cfg config.Config, admitted Plan) (ConfigChanges, RegistrationResult) {
	t.Helper()
	changes, err := ApplyToConfig(&cfg, admitted, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Registration{Context: t.Context(), Home: home, Store: store, AdmittedAt: fixedNow}).Run(admitted.Imported())
	if err != nil {
		t.Fatal(err)
	}
	return changes, result
}

// One recovery whose evidence changed is left out; the others are admitted
// with the configuration they need, and registration accepts them.
func TestAdmitRecoveryLeavesOutChangedSessions(t *testing.T) {
	cfg, p, gone, _ := partialAdmissionFixture(t, 3)
	// Only that session's evidence changed.
	markStale(t, &p, gone[0])
	admitted, changed, err := p.AdmitRecovery(t.Context())
	if err != nil || changed != 1 {
		t.Fatal(changed, err)
	}
	if c := candidate(t, admitted, gone[0]); c.Skip != SkipSourceChanged || c.Diagnostic == nil || c.Diagnostic.Detail != DetailSourceChanged {
		t.Fatalf("changed session not left out: %+v", c)
	}
	if candidate(t, p, gone[0]).Skip != "" {
		t.Fatal("the confirmed plan was changed")
	}
	if len(admitted.Imported()) != len(p.Imported())-1 {
		t.Fatalf("%+v", admitted.Imported())
	}
	changes, result := register(t, cfg, admitted)
	if len(changes.ProjectIDs) != 1 || len(result.Sessions) != len(admitted.Imported()) || result.NotAdmitted != 0 {
		t.Fatal(changes, result)
	}
}

// When the sessions left out were the only ones needing a proposed project,
// that project is not added, and the rest are bound to the configuration
// without it.
func TestAdmitRecoveryDropsProjectsOnlyChangedSessionsNeeded(t *testing.T) {
	cfg, p, gone, others := partialAdmissionFixture(t, 1)
	markStale(t, &p, gone[0])
	confirmedPolicy := candidate(t, p, gone[0]).ProjectResolution.PolicyContext
	admitted, changed, err := p.AdmitRecovery(t.Context())
	if err != nil || changed != 1 {
		t.Fatal(changed, err)
	}
	// Binding the narrowed plan leaves the confirmed plan's resolutions,
	// the left-out session's too, as they were.
	if candidate(t, p, gone[0]).ProjectResolution.PolicyContext != confirmedPolicy {
		t.Fatal("admission rebound the confirmed plan's resolution")
	}
	if imported := admitted.Imported(); len(imported) != 2 || imported[0].ProjectRoot != imported[1].ProjectRoot {
		t.Fatalf("%+v", imported)
	}
	changes, result := register(t, cfg, admitted)
	if len(changes.ProjectIDs) != 0 {
		t.Fatal("a project only the changed session needed was added", changes)
	}
	// The recovery into the configured project is bound to the configuration
	// without the dropped project, or registration would refuse it.
	if len(result.Sessions) != len(others) || result.NotAdmitted != 0 {
		t.Fatal(result)
	}
}

// Nothing is admitted when every session changed, or when the check itself
// ended with its context.
func TestAdmitRecoveryRefusesWhenNothingIsLeft(t *testing.T) {
	_, p, gone, others := partialAdmissionFixture(t, 1)
	markStale(t, &p, gone[0])
	for i := range p.Candidates {
		if p.Candidates[i].NativeSessionID == others[0] || p.Candidates[i].NativeSessionID == others[1] {
			p.Candidates[i].Skip = SkipFilteredOut
		}
	}
	if _, _, err := p.AdmitRecovery(t.Context()); err == nil || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatal("an import with nothing left was admitted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := p.AdmitRecovery(ctx); err == nil {
		t.Fatal("a cancelled check admitted a plan")
	}
}

// A Cursor chat added in a project folder a planned chat was found in names
// that folder's root; one in a new folder, or a store no longer listed in
// full, is a gap in that app's evidence only.
func TestAddedSourcesAttributeUnknownFoldersPerApp(t *testing.T) {
	_, env, cfg, _, _ := addedSourcesFixture(t)
	r := newResolver(env, cfg, Filters{})
	planned := []*work{{t: &transcript{harness: harnessCursor, cursorSlug: "known-slug"}}}
	chat := func(slug string) agentapi.DiscoveryCandidate {
		return agentapi.DiscoveryCandidate{Session: agentapi.NativeSession{Agent: agentmeta.Cursor, NativeID: "chat"}, WorkspaceKey: slug}
	}
	for _, tc := range []struct {
		name  string
		added addedSources
		gaps  recoveryGaps
	}{
		{name: "known folder", added: addedSources{candidates: []agentapi.DiscoveryCandidate{chat("known-slug")}}},
		{name: "new folder", added: addedSources{candidates: []agentapi.DiscoveryCandidate{chat("new-slug")}}, gaps: recoveryGaps{{Agent: "cursor", Cause: CauseNativeInventoryChanged}: true}},
		{name: "store listed in part", added: addedSources{incomplete: map[string]bool{"claude": true}}, gaps: recoveryGaps{{Agent: "claude", Cause: CauseNativeInventoryChanged}: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			witnesses, gaps := addedWitnesses(r, planned, tc.added)
			if len(witnesses) != 0 || len(gaps) != len(tc.gaps) {
				t.Fatalf("planning: %v %v", witnesses, gaps)
			}
			for gap := range tc.gaps {
				if !gaps[gap] {
					t.Fatalf("planning: missing %v in %v", gap, gaps)
				}
			}
			keys, gaps := addedRecoveryBlocks(t.Context(), r, addedSourceSlugs(planned), nil, tc.added)
			if len(keys) != 0 || len(gaps) != len(tc.gaps) {
				t.Fatalf("admission: %v %v", keys, gaps)
			}
			proof := archive.ProjectResolution{Method: "recorded_repository", RecordedRepoKey: archive.RepoKey(addedRepoURL)}
			if !addedAllows(keys, gaps, harnessCodex, proof) {
				t.Fatal("another app's added evidence blocked a Codex recovery")
			}
			if len(tc.gaps) > 0 && addedAllows(keys, gaps, "", proof) {
				t.Fatal("added evidence of unknown folder did not block a session of unknown app")
			}
		})
	}
}

// A check the context ended during, even on its last session, admits nothing:
// a session that read as changed may only have run out of time.
func TestAdmitRecoveryContextEndingDuringTheCheckAdmitsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := config.Config{}
	p := Plan{policyConfig: &cfg, Candidates: []Candidate{
		{NativeSessionID: "current", projectResolutionCurrent: func() bool { return true }},
		{NativeSessionID: "last", projectResolutionCurrent: func() bool { cancel(); return false }},
	}}
	if _, _, err := p.AdmitRecovery(ctx); err == nil {
		t.Fatal("a check that ended with its context admitted a plan")
	}
}

// A transcript added in a configured project names a root the inventory
// already holds through configuration: no Git observation is made for it.
func TestAddedTranscriptInConfiguredProjectNeedsNoGit(t *testing.T) {
	tr, env, cfg, root, goneID := addedSourcesFixture(t)
	other := tr.repo("home/other")
	cfg.Archive.Projects = []archive.ProjectActivation{project(other, true)}
	lookups := map[string]int{}
	lookup := env.RepositoryIdentity
	env.RepositoryIdentity = func(ctx context.Context, path string) sourcefacts.RepositoryIdentity {
		lookups[path]++
		return lookup(ctx, path)
	}
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectRoot != root || c.ProjectResolution == nil {
		t.Fatalf("%+v %+v", c, c.Diagnostic)
	}
	before := lookups[other]
	addedRollout(tr, "00000000-0000-0000-0000-0000000000f1", other, addedOtherURL)
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lookups[other] != before {
		t.Fatal("a configured project's added transcript was looked up as a new clone")
	}
}

// markStale makes one planned session's evidence no longer current, as a
// change only that recovery depends on would. A rewritten transcript would
// not do: rewrites of observed files fail every recovery (compare).
func markStale(t *testing.T, p *Plan, id string) {
	t.Helper()
	for i := range p.Candidates {
		if p.Candidates[i].NativeSessionID == id {
			p.Candidates[i].projectResolutionCurrent = func() bool { return false }
			return
		}
	}
	t.Fatalf("no candidate %s", id)
}

// A recovered session its agent is still appending to is admitted: its
// header, which recovery used, cannot change.
func TestAdmitRecoveryAcceptsAppendsToRecoveredSession(t *testing.T) {
	_, p, gone, _ := partialAdmissionFixture(t, 1)
	appendRecord(t, candidate(t, p, gone[0]).TranscriptPath, `{"more":1}`+"\n")
	admitted, changed, err := p.AdmitRecovery(t.Context())
	if err != nil || changed != 0 || len(admitted.Imported()) != len(p.Imported()) {
		t.Fatal(changed, err)
	}
}

// A plan that was never bound to a recovery policy cannot be narrowed, and
// says so instead of reporting that nothing is left.
func TestAdmitRecoveryRefusesUnboundPartialPlan(t *testing.T) {
	_, p, gone, _ := partialAdmissionFixture(t, 3)
	markStale(t, &p, gone[0])
	p.policyConfig = nil
	if _, _, err := p.AdmitRecovery(t.Context()); !errors.Is(err, errRecoveryUnbound) {
		t.Fatal(err)
	}
}

// Leaving changed sessions out does not rewrite the confirmed plan's
// resolutions.
func TestAdmitRecoveryLeavesConfirmedResolutionsAlone(t *testing.T) {
	_, p, gone, _ := partialAdmissionFixture(t, 1)
	before := map[string]string{}
	for _, c := range p.Candidates {
		if c.ProjectResolution != nil {
			before[c.NativeSessionID] = c.ProjectResolution.PolicyContext
		}
	}
	markStale(t, &p, gone[0])
	if _, changed, err := p.AdmitRecovery(t.Context()); err != nil || changed != 1 {
		t.Fatal(changed, err)
	}
	for _, c := range p.Candidates {
		if c.ProjectResolution != nil && c.ProjectResolution.PolicyContext != before[c.NativeSessionID] {
			t.Fatalf("confirmed resolution of %s rebound", c.NativeSessionID)
		}
	}
}

// A proposed project only the left-out sessions needed, nested with a plain
// folder the import still adds, would leave its folder to that plain
// folder's capture: the plan never looked inside it to keep its folders out.
// Such an import is refused whole; one without the nesting is admitted.
func TestAdmitRecoveryRefusesWhenADroppedProjectNestsInAnAddedFolder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		plain     string
		recovered string
		refused   bool
	}{
		{name: "dropped repository inside the added folder", plain: "/h/code", recovered: "/h/code/foo", refused: true},
		{name: "added folder inside the dropped project", plain: "/h/code/foo/tmp", recovered: "/h/code/foo", refused: true},
		{name: "unrelated folders", plain: "/h/notes", recovered: "/h/code/foo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			p := Plan{policyConfig: &cfg, nested: map[string]nestedFolders{tc.plain: {Complete: true}}, Candidates: []Candidate{
				{NativeSessionID: "plain", ProjectRoot: tc.plain, ProjectKind: ProjectKindDirectory},
				{NativeSessionID: "recovered", ProjectRoot: tc.recovered, ProjectKind: ProjectKindRepository, ProjectResolution: &archive.ProjectResolution{Method: "recorded_repository"}, projectResolutionCurrent: func() bool { return false }},
			}}
			admitted, changed, err := p.AdmitRecovery(t.Context())
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "Nothing was changed") {
					t.Fatalf("admitted %+v (%d changed)", admitted.Imported(), changed)
				}
				return
			}
			if err != nil || changed != 1 || len(admitted.Imported()) != 1 {
				t.Fatal(changed, err)
			}
		})
	}
}
