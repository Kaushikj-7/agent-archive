package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestStatusRequiresEveryApplicationProjectPair(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	projectA, projectB := t.TempDir(), t.TempDir()
	cfg := pairTestConfig(now, []string{"codex", "claude"}, projectA, projectB)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-a", projectA, true)
	publishPairSession(t, home, store, remote, cfg, now, "codex-b", projectB, false)

	env := pairStatusEnv(t, home, userHome, now, "codex", "claude")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified || len(view.Apps[0].Projects) != 2 || !view.Apps[0].Projects[0].ReadBackVerified || view.Apps[0].Projects[1].ReadBackVerified {
		t.Fatalf("Codex pair coverage=%#v", view.Apps[0])
	}
	if view.Apps[1].ReadBackVerified || view.Apps[1].Projects[0].HookObserved {
		t.Fatalf("Claude inherited another app's evidence: %#v", view.Apps[1])
	}
	if !strings.Contains(view.Next, projectB) {
		t.Fatalf("next action does not name uncovered pair: %q", view.Next)
	}
}

func TestPairVerificationSurvivesUnrelatedChangesButNotReactivationOrDestinationChange(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	projectA, projectB := t.TempDir(), t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, projectA, projectB)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	remote := storagetest.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-a", projectA, true)
	env := pairStatusEnv(t, home, userHome, now, "codex")

	cfg.Archive.Projects = cfg.Archive.Projects[:1]
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].ReadBackVerified {
		t.Fatalf("unchanged pair lost verification: %#v", view.Apps[0])
	}
	cfg.Harnesses = append(cfg.Harnesses, "claude")
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].ReadBackVerified {
		t.Fatalf("adding an app invalidated unchanged pair: %#v", view.Apps[0])
	}
	cfg.Harnesses = []string{"codex"}

	projectC := t.TempDir()
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(projectC), Root: projectC, Included: true, ActivatedAt: now})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Apps[0].Projects) != 2 || !view.Apps[0].Projects[0].ReadBackVerified || view.Apps[0].Projects[1].VerificationState != "not_verified" || view.Apps[0].ReadBackVerified {
		t.Fatalf("adding an unrelated project changed the verified pair: %#v", view.Apps[0])
	}
	cfg.Archive.Projects = cfg.Archive.Projects[:1]

	cfg.Archive.Projects[0].ActivatedAt = now.Add(time.Hour)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified || view.Apps[0].Projects[0].HookObserved {
		t.Fatalf("pre-reactivation evidence accepted: %#v", view.Apps[0])
	}

	cfg.Archive.Projects[0].ActivatedAt = now.Add(-time.Hour)
	cfg.Storage.AWSProfile = "changed-destination-identity"
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified {
		t.Fatalf("destination change retained verification: %#v", view.Apps[0])
	}
}

func TestStatusCountsLegacySessionsWithoutConfiguredProjects(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	// Older programmatic configurations carry no projects; AcceptSession
	// admits their sessions and the collector publishes them.
	cfg := pairTestConfig(now, []string{"codex"})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	remote := storagetest.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-legacy", project, false)
	env := pairStatusEnv(t, home, userHome, now, "codex")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if len(app.Projects) != 0 || app.Sessions != 1 || !app.HookObserved || !app.CapturedLocally || !app.Published || app.PublishedSessions != 1 || app.ReadBackVerified || app.VerificationState != "incomplete" {
		t.Fatalf("legacy session dropped from status: %#v", app)
	}
	if !strings.Contains(view.Next, "include at least one project") {
		t.Fatalf("next action should still ask for a project: %q", view.Next)
	}
	if summary, err := verifyPublications(home, cfg, testEnv(t, home, now), store, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("verify summary=%#v err=%v", summary, err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if app := view.Apps[0]; !app.ReadBackVerified || app.VerifiedSessions != 1 || app.State != "published; source verified" {
		t.Fatalf("legacy session verification not reported: %#v", app)
	}
}

func TestStatusIgnoresDuplicateProjectRoots(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cfg := pairTestConfig(now, []string{"codex"}, project, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	remote := storagetest.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-a", project, true)
	view, err := readStatus(pairStatusEnv(t, home, userHome, now, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if app := view.Apps[0]; len(app.Projects) != 1 || !app.ReadBackVerified || !app.Projects[0].ReadBackVerified {
		t.Fatalf("duplicate root pinned the app unverified: %#v", app)
	}
}

func TestStatusTextDoesNotCallPartialReadBackVerified(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	projectA, projectB := t.TempDir(), t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, projectA, projectB)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	remote := storagetest.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-a", projectA, true)
	publishPairSession(t, home, store, remote, cfg, now, "codex-b", projectB, false)
	env := pairStatusEnv(t, home, userHome, now, "codex")
	var out strings.Builder
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, env); code != 0 {
		t.Fatalf("status exit %d: %s", code, out.String())
	}
	if text := out.String(); strings.Contains(text, "archived, verified") || !strings.Contains(text, "uploaded, read-back pending (1 of 2 projects verified)") {
		t.Fatalf("partial read-back reported as verified:\n%s", text)
	}
	if summary, err := verifyPublications(home, cfg, testEnv(t, home, now), store, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("verify summary=%#v err=%v", summary, err)
	}
	out.Reset()
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "2 sessions archived, verified just now") {
		t.Fatalf("complete read-back not reported as verified (exit %d):\n%s", code, out.String())
	}
}

func pairTestConfig(now time.Time, apps []string, roots ...string) config.Config {
	projects := make([]archive.ProjectActivation, 0, len(roots))
	for _, root := range roots {
		projects = append(projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: now.Add(-time.Hour)})
	}
	return config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: apps, Archive: archive.Config{Enabled: true, Projects: projects}}
}

// publishPairSession registers and publishes a Codex session, verifying it
// when verify is set.
func publishPairSession(t *testing.T, home string, store *state.Store, remote *storagetest.MemoryStore, cfg config.Config, now time.Time, id, project string, verify bool) {
	t.Helper()
	path := filepath.Join(project, id+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic","cli_version":"1.2.3"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "native-" + id, ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: "codex", Version: "1.2.3"}, TranscriptPath: path, SessionStartedAt: now, RegisteredAt: now}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Run(context.Background(), store, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("publish result=%#v err=%v", result, err)
	}
	if verify {
		env := testEnv(t, home, now)
		if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Verified != 1 {
			t.Fatalf("verify summary=%#v err=%v", summary, err)
		}
	}
}

func pairStatusEnv(t *testing.T, home, userHome string, now time.Time, apps ...string) Env {
	t.Helper()
	executable := "/opt/agent-archive/bin/agent-archive"
	env := testEnv(t, home, now)
	plan, err := hooks.Plan(hooks.ResolveFiles(userHome, noEnv, productionAgents), env.installation(home, userHome).hook(executable), apps)
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Apply(plan); err != nil {
		t.Fatal(err)
	}
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.Executable = func() (string, error) { return executable, nil }
	env.Scheduler = newFakeScheduler(t, "running")
	return env
}

// Cursor fresh-start capture is no longer impossible: an absent or empty
// transcript at sessionStart proves a new conversation. Status must therefore
// ask the user for the one thing that will actually produce a capture — a new
// session — instead of the old "this cannot work yet" wording.
func TestStatusSuggestsCursorCaptureOnceFreshStartIsProvable(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	cfg := pairTestConfig(now, []string{"cursor"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := pairStatusEnv(t, home, userHome, now, "cursor")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if capability := view.Apps[0].Capabilities.FreshStart; capability.State != capabilityDocumented {
		t.Fatalf("Cursor fresh-start capability: %#v", capability)
	}
	if !strings.Contains(view.Next, "then start a new session") {
		t.Fatalf("status did not ask for the session that would prove capture: %q", view.Next)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"--verbose"}, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "New sessions can't be captured yet") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
}
