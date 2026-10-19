package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// saveImportedSession registers an imported Codex session with a transcript.
func saveImportedSession(t *testing.T, store *state.Store, now time.Time, id, project string) archive.SessionRegistration {
	t.Helper()
	path := filepath.Join(project, id+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic","cli_version":"1.2.3"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: id, NativeSessionID: "native-" + id, ProjectID: archive.ProjectID(project), ProjectRoot: project,
		Harness: archive.Harness{Name: "codex", Version: "1.2.3"}, TranscriptPath: path,
		SessionStartedAt: now.AddDate(-1, 0, 0), RegisteredAt: now, AdmittedAt: now,
		Origin: archive.SessionOriginImport, StartedAtSource: archive.StartedAtSourceTranscript, ImportBatch: archive.NewImportBatch("batch-1"),
	}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

// Imports are read back, but they are not evidence that an app's hooks work:
// the app stays "waiting for first session", unobserved and unverified, and
// the imports are reported on their own line.
func TestStatusDoesNotPromoteAnAppOnImports(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	project := t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	saveImportedSession(t, store, now, "published", project)
	if result, err := collector.Run(context.Background(), store, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now }}); err != nil || len(result.Published) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if summary, err := verifyPublications(home, cfg, testEnv(t, home, now), store, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("read-back did not run on the import: %#v %v", summary, err)
	}
	saveImportedSession(t, store, now, "waiting", project)

	env := pairStatusEnv(t, home, userHome, now, "codex")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if app.State != "waiting for first session" || app.HookObserved || app.Sessions != 0 || app.Published || app.ReadBackVerified || app.VerificationState != "not_verified" {
		t.Fatalf("imports promoted the app: %#v", app)
	}
	if pair := app.Projects[0]; pair.HookObserved || pair.CapturedLocally || pair.Published || pair.ReadBackVerified || pair.VerificationState != "not_verified" {
		t.Fatalf("imports promoted the project pair: %#v", pair)
	}
	if view.State != "Waiting for capture" {
		t.Fatalf("state=%q", view.State)
	}
	if view.ImportedSessions != 2 || view.ImportedPending != 1 || view.LastImport != "" {
		t.Fatalf("imported=%d pending=%d last=%q", view.ImportedSessions, view.ImportedPending, view.LastImport)
	}

	var out strings.Builder
	if code := runStatusCommand([]string{"codex"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "discovery off; included projects   no sessions yet · 2 imported · 1 uploading\n") || strings.Contains(out.String(), "Imported:") {
		t.Fatalf("status text (exit %d):\n%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "· Imported (all destinations): 2 sessions, 1 waiting to upload\n") {
		t.Fatalf("status --verbose (exit %d):\n%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 {
		t.Fatalf("status --json exit %d: %s", code, out.String())
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(out.String()), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["imported_sessions"] != float64(2) || fields["imported_pending"] != float64(1) || fields["last_import"] != "" {
		t.Fatalf("status --json imported fields: %v %v %v", fields["imported_sessions"], fields["imported_pending"], fields["last_import"])
	}
}

// Imports are left out of each app's gaps and scan issues, so status counts
// the ones that have either on the Imported line instead.
func TestStatusReportsImportsWithGapsOrFailedScans(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	project := t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	saveImportedSession(t, store, now, "fine", project)
	gone := saveImportedSession(t, store, now, "gone", project)
	if err := os.Remove(gone.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	saveImportedSession(t, store, now, "failing", project)
	if _, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	if _, blocked, err := store.LoadBlocked("gone"); err != nil || !blocked {
		t.Fatalf("test precondition: the missing transcript is a recorded gap: %v %v", blocked, err)
	}
	// What runOnePass records for a session whose scan failed.
	state, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	state.SessionIssues = map[string]string{"failing": "capture_or_publication_failed"}
	if err := store.SaveStatus(state); err != nil {
		t.Fatal(err)
	}

	env := pairStatusEnv(t, home, userHome, now, "codex")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.ImportedSessions != 3 || view.ImportedWithIssues != 2 || len(view.Apps[0].CaptureGaps) != 0 {
		t.Fatalf("imported=%d with issues=%d app gaps=%#v", view.ImportedSessions, view.ImportedWithIssues, view.Apps[0].CaptureGaps)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"codex"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "· 2 imported sessions have a capture gap or failed scan\n") {
		t.Fatalf("status text (exit %d):\n%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "· Imported (all destinations): 3 sessions, 0 waiting to upload, 2 with a capture gap or failed scan\n") {
		t.Fatalf("status text (exit %d):\n%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), `"imported_with_issues": 2`) {
		t.Fatalf("status --json (exit %d):\n%s", code, out.String())
	}
	for _, importedOnly := range []bool{false, true} {
		if importedOnly {
			cfg = mustLoadConfig(t, home)
			cfg.Harnesses, cfg.ImportedHarnesses = []string{"claude"}, []string{"codex"}
			must(t, config.Save(home, cfg))
		}
		out.Reset()
		if code := runStatusCommand([]string{"--issues"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "2 imported sessions have a capture gap or failed scan") {
			t.Fatalf("status --issues importedOnly=%v (exit %d):\n%s", importedOnly, code, &out)
		}
	}
}

// Without imports, the text status has no Imported line.
func TestStatusWithoutImportsHasNoImportedLine(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	project := t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	publishPairSession(t, home, store, storagetest.NewMemoryStore(), cfg, now, "hook", project, true)
	var out strings.Builder
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, pairStatusEnv(t, home, userHome, now, "codex")); code != 0 || strings.Contains(out.String(), "Imported:") || strings.Contains(out.String(), " imported") {
		t.Fatalf("status text (exit %d):\n%s", code, out.String())
	}
}

func TestStatusObservesLaterHookOnImportWithoutPromotingCapture(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	must(t, os.Chmod(home, 0700))
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	reg := saveImportedSession(t, store, now, "imported", project)
	env := pairStatusEnv(t, home, userHome, now, "codex")
	before, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if before.Apps[0].HookObserved || before.Apps[0].Projects[0].HookObserved {
		t.Fatal("import alone established hooks")
	}
	payload := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": reg.NativeSessionID, "cwd": project}
	if err := handleTestHookEvent(home, "codex", payload, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := after.Apps[0]
	if !app.HookObserved || !app.Projects[0].HookObserved {
		t.Fatal("later actual hook observation lost")
	}
	if app.Sessions != 0 || app.ImportedSessions != 1 || app.CapturedLocally || app.Published || app.ReadBackVerified || app.Projects[0].ReadBackVerified {
		t.Fatalf("imported hook promoted fresh capture: %#v", app)
	}
	if app.State != "hook observed; waiting for capture" {
		t.Fatalf("state=%q", app.State)
	}
	saved, found, err := store.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || !saved.Imported() || !saved.InBatch("batch-1") || !saved.AdmittedAt.Equal(reg.AdmittedAt) {
		t.Fatalf("hook changed import: %#v %v", saved, err)
	}
}
