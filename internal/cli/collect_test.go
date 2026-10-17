package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A scheduled pass that meets a corrupt state file still publishes the rest,
// names the quarantined file in status, and keeps collector-error.log (which
// launchd appends to and never rotates) bounded.
func TestCollectPassReportsQuarantinedStateAndTrimsErrorLog(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleTestHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "registrations", "broken.json"), []byte(`{"archive_session_id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	errorLog := filepath.Join(home, "collector-error.log")
	if err := os.WriteFile(errorLog, bytes.Repeat([]byte("agent-archive: collect: old failure\n"), (errorLogMaxBytes/36)+1), 0o600); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SessionIssues["broken"] != "local_state_unreadable" || len(status.QuarantinedFiles) != 1 || !strings.HasPrefix(status.QuarantinedFiles[0], "registrations/broken.json.") {
		t.Fatalf("status = %#v", status)
	}
	if status.LastPublishedAt.IsZero() {
		t.Fatal("the readable session was not published")
	}
	if info, err := os.Stat(errorLog); err != nil || info.Size() > errorLogKeepBytes {
		t.Fatalf("collector-error.log was not trimmed: %v %v", info.Size(), err)
	}
}

// collectFixture sets up one hook-registered codex session and an Env whose
// passes all publish to the same in-memory bucket.
func collectFixture(t *testing.T, now time.Time) (home string, env Env, remote *storagetest.MemoryStore) {
	t.Helper()
	home = t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": writeCodexTranscript(t, dir)}
	if err := handleTestHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	remote = storagetest.NewMemoryStore()
	env = testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	return home, env, remote
}

func theRegistration(t *testing.T, home string) archive.SessionRegistration {
	t.Helper()
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v %v", regs, err)
	}
	return regs[0]
}

// A registration that cannot be read at all (a root-owned file left by a
// sudo run, say) fails only its own session: read-back verification skips
// it, and the retention sweep still runs.
func TestCollectPassSweepsDespiteUnreadableRegistration(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	storageClockFollows(t, &env, func() time.Time { return env.Now() })
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	id := theRegistration(t, home).ArchiveSessionID

	locked := filepath.Join(home, "registrations", "locked.json")
	if err := os.WriteFile(locked, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(72 * time.Hour) }
	result, err := runOnePass(env, false)
	if err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	if result.Errors["locked"] == nil {
		t.Fatalf("the unreadable registration was not reported: %#v", result.Errors)
	}
	if _, err := os.Stat(filepath.Join(home, "registrations", id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the retention sweep did not run: expired session still registered (%v)", err)
	}
}

// Past the soft deadline a pass starts no new session. Sessions it did not
// reach keep their work, and nothing is reported as failing.
func TestCollectPassSoftDeadlineStartsNoNewSession(t *testing.T) {
	previous := collectSoftDeadline
	collectSoftDeadline = 0
	t.Cleanup(func() { collectSoftDeadline = previous })
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)

	result, err := runOnePass(env, false)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 || result.Scanned != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	status, err := state.OpenReadOnly(home).LoadStatus()
	if err != nil || status.LastError != "" {
		t.Fatalf("status = %#v %v", status, err)
	}

	collectSoftDeadline = previous
	if result, err := runOnePass(env, false); err != nil || len(result.Published) != 1 {
		t.Fatalf("the next pass did not pick the session up: %#v %v", result, err)
	}
}

// Recorded references keep legitimate legacy cached bundles readable after a
// source schema bump. The legacy persistence API validates this authority;
// selecting protocol2 state cannot be modified into the same legacy premise.
func TestReadBackUsesRecordedSourceAfterSchemaBump(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	producerHome, producerEnv, remote := collectFixture(t, now)
	if result, err := runOnePass(producerEnv, false); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	reg := theRegistration(t, producerHome)
	produced, err := state.OpenReadOnly(producerHome).LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, at, found := produced.LastPublished()
	if !found {
		t.Fatal("missing actual producer publication")
	}
	source, sourceFound := produced.LastPublishedSource()
	if !sourceFound {
		t.Fatal("missing actual producer recorded reference")
	}
	metadata, err := state.OpenReadOnly(producerHome).PublishedMetadata(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(producerHome)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle.SchemaVersion = 1
	if err := legacy.SavePublication(bundle, at, source, metadata); err != nil {
		t.Fatalf("complete legacy persistence: %v", err)
	}
	env := testEnv(t, home, now)
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("summary = %#v %v", summary, err)
	}
}

func TestReadBackRefusesSelectingCachedSchemaTampering(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, remote := collectFixture(t, now)
	if result, err := runOnePass(env, false); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	id := theRegistration(t, home).ArchiveSessionID
	publishedPath := filepath.Join(home, "published", id+".json")
	var raw map[string]any
	if err := local.Read(publishedPath, &raw); err != nil {
		t.Fatal(err)
	}
	raw["bundle"].(map[string]any)["schema_version"] = 1
	if err := local.Write(publishedPath, raw); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(verificationPath(home, id)); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if summary, err := verifyPublications(home, cfg, env, store, remote); !errors.Is(err, state.ErrDurableStorageRecovery) || summary.Verified != 0 {
		t.Fatalf("tampered selecting summary = %#v %v", summary, err)
	}
	after, err := os.ReadFile(publishedPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("tampered authority changed: %v", err)
	}
}

// A read-back verification that fails (here: one session's published state
// cannot be read) is reported, but only after the retention sweep has run:
// another, expired session is still cleaned up in the same pass.
func TestCollectPassSweepsWhenVerificationFails(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	storageClockFollows(t, &env, func() time.Time { return env.Now() })
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 3
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	expired := theRegistration(t, home).ArchiveSessionID

	// A second session, published two days later, so it is not yet expired
	// when the first one is.
	later := now.Add(48 * time.Hour)
	dir := t.TempDir()
	cfg, _, _ = config.Load(home)
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(dir), Root: dir, Included: true, ActivatedAt: now})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-2", "cwd": dir, "transcript_path": writeCodexTranscript(t, dir)}
	if err := handleTestHookEvent(home, "codex", payload, later); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return later }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 2 {
		t.Fatalf("registrations = %v %v", regs, err)
	}
	unreadable := regs[0].ArchiveSessionID
	if unreadable == expired {
		unreadable = regs[1].ArchiveSessionID
	}
	// Force a fresh read-back of that session, which cannot read its state.
	if err := os.Remove(verificationPath(home, unreadable)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, "published", unreadable+".json"), 0); err != nil {
		t.Fatal(err)
	}

	env.Now = func() time.Time { return now.Add(96 * time.Hour) }
	_, err = runOnePass(env, false)
	if err == nil || !strings.Contains(err.Error(), "read-back verification") {
		t.Fatalf("pass error = %v, want the verification failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "registrations", expired+".json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the retention sweep did not run: expired session still registered (%v)", statErr)
	}
	status, statusErr := state.OpenReadOnly(home).LoadStatus()
	if statusErr != nil || !strings.Contains(status.LastError, "read-back verification") {
		t.Fatalf("status = %#v %v", status, statusErr)
	}
	// The failure is added beside the pass's summary of failed sessions,
	// which stays with the counts it was built from.
	if len(status.LastErrors) != 2 || !strings.HasPrefix(status.LastErrors[0], "1 session couldn't be removed after the retention period") ||
		!strings.HasPrefix(status.LastErrors[1], "read-back verification") || status.IssueCounts[issueRetentionFailed] != 1 {
		t.Fatalf("last errors %q counts %v", status.LastErrors, status.IssueCounts)
	}
}

// Read-back verification works within the pass's deadline: with no time
// left, a due read-back is deferred to the next pass, not attempted.
func TestReadBackDefersOnceThePassDeadlineHasPassed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, remote := collectFixture(t, now)
	if result, err := runOnePass(env, false); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if err := os.Remove(verificationPath(home, theRegistration(t, home).ArchiveSessionID)); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, err := verifyPublicationsWithin(ctx, home, cfg, env, store, remote)
	if err != nil || summary.Attempted != 0 || summary.Deferred != 1 {
		t.Fatalf("summary = %#v %v", summary, err)
	}
}

func TestCollectorKeepsLastPublicationOnUnchangedPass(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now()
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	env := testEnv(t, home, now)
	if err := handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	first, _ := store.LoadStatus()
	env.Now = func() time.Time { return now.Add(time.Minute) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	next, _ := store.LoadStatus()
	if next.LastPublishedAt.IsZero() || !next.LastPublishedAt.Equal(first.LastPublishedAt) {
		t.Fatal("lost last publication")
	}
}

func TestCollectCommandSilentlyNoopsWhenPausedOrNotSetUp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("not-set-up _collect should be silent: code=%d stderr=%s", code, errOut.String())
	}

	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("paused _collect should be silent: code=%d stderr=%s", code, errOut.String())
	}
}

func TestCollectCommandRunsQuietlyOnSuccess(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleTestHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 || errOut.Len() != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.LastPublishedAt.IsZero() {
		t.Fatal("expected a publish to have happened")
	}
}

func TestCollectCommandRecordsPreflightFailureInStatus(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	env := testEnv(t, home, time.Now())
	openErr := errors.New("simulated broken storage credentials")
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, openErr }

	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.LastError, openErr.Error()) {
		t.Fatalf("expected status.LastError to record the preflight failure, got %q", status.LastError)
	}
}
