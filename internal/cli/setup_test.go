package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// fakeKeychain is an in-memory credentials.CredentialStore for tests, since
// the real one is only available on a darwin+cgo build and must never be
// exercised in automated tests regardless of platform.
type fakeKeychain struct {
	mu    sync.Mutex
	items map[string]credentials.R2Credentials
}

func newFakeKeychain() *fakeKeychain {
	return &fakeKeychain{items: map[string]credentials.R2Credentials{}}
}

func (f *fakeKeychain) Save(_ context.Context, reference string, value credentials.R2Credentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[reference] = value
	return nil
}

func (f *fakeKeychain) Load(_ context.Context, reference string) (credentials.R2Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[reference]
	if !ok {
		return credentials.R2Credentials{}, credentials.ErrMissingCredential
	}
	return v, nil
}

func (f *fakeKeychain) Delete(_ context.Context, reference string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, reference)
	return nil
}

// testExecutable creates a real, runnable stand-in for the installed
// agent-archive binary. status checks that the executable setup recorded
// still exists and can run, so a fixed fictional path would read as broken.
func testExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bin", "agent-archive")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func setupTestEnv(t *testing.T, home, userHome string, keychain *fakeKeychain, now time.Time) Env {
	t.Helper()
	// Positive setup fixtures begin with a private existing archive root. Leave
	// absent paths, final links, user homes, and all descendants to their owners.
	info, err := os.Lstat(home)
	if err == nil && info.IsDir() {
		if err := os.Chmod(home, 0o700); err != nil {
			t.Fatal(err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	env := testEnv(t, home, now)
	env.AWSProfiles = func() ([]AWSProfile, error) { return nil, nil }
	env.WorkingDir = func() (string, error) { return "", errors.New("no current project") }
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	executable := testExecutable(t)
	env.Executable = func() (string, error) { return executable, nil }
	env.DetectHarnesses = func(string) []string { return nil }
	env.DiscoverApplications = func(string) map[string]applicationDiscovery { return map[string]applicationDiscovery{} }
	env.Scheduler = newFakeScheduler(t, "missing")
	env.Credentials = func() (credentials.CredentialStore, error) { return keychain, nil }
	// setup and uninstall need a terminal; the scripted answers stand in
	// for one. Output buffers are still not terminals.
	env.IsTerminal = func(stream any) bool { _, ok := stream.(*strings.Reader); return ok }
	return env
}

//lint:ignore unparam tests in other files pass the region explicitly so each setup transcript reads in full
func s3SetupInput(bucket, region, profile string, codex, claude, cursor bool, project string) string {
	yn := func(b bool) string {
		if b {
			return "y"
		}
		return "n"
	}
	answers := []string{yn(codex), yn(claude), yn(cursor)}
	if codex {
		answers = append(answers, "included-projects")
	}
	answers = append(answers, project, "", "s3-existing", profile, bucket, region, "y")
	return strings.Join(answers, "\n") + "\n"
}

func r2SetupInput(project, secret string) string {
	return strings.Join([]string{"y", "n", "n", "included-projects", project, "", "r2-existing", "0123456789abcdef0123456789abcdef", "test-bucket", "ACCESS", secret, "y"}, "\n") + "\n"
}

func setupRun(t *testing.T, env Env, input string, want int) string {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(input), &out, &errOut, env)
	if code != want {
		t.Fatalf("setup exit %d want %d\n%s\n%s", code, want, &out, &errOut)
	}
	return out.String() + errOut.String()
}

func TestSetupFirstTimeProviders(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"s3", "r2"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			kc := newFakeKeychain()
			now := time.Now().UTC()
			env := setupTestEnv(t, home, userHome, kc, now)
			input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
			if provider == "r2" {
				input = r2SetupInput(project, "NEVER_PRINT_THIS")
			}
			output := setupRun(t, env, input, 0)
			if strings.Contains(output, "NEVER_PRINT_THIS") {
				t.Fatal("secret leaked")
			}
			cfg, found, err := config.Load(home)
			if err != nil || !found || cfg.Storage.Provider != provider || cfg.RequireSkillUse || cfg.RetentionDays != 90 {
				t.Fatalf("config: %+v %v", cfg, err)
			}
			if cfg.MachineID == "" || !cfg.Archive.Projects[0].ActivatedAt.Equal(now) {
				t.Fatal("missing activation or identity")
			}
			if _, err := os.Stat(filepath.Join(userHome, ".codex", "hooks.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, "setup-draft.json")); !os.IsNotExist(err) {
				t.Fatal("draft should be removed")
			}
			if provider == "r2" {
				if _, err := kc.Load(context.Background(), cfg.Storage.R2CredentialRef); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSetupCancelAndResumeDraft(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
	const needBucket = "Choose Cloudflare R2 or Amazon S3 for private archive storage."
	if output := setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0); !strings.Contains(output, needBucket) {
		t.Fatalf("a first setup did not say a bucket is needed:\n%s", output)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("cancel activated config")
	}
	if _, err := os.Stat(filepath.Join(userHome, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("cancel installed hooks")
	}
	// The draft names a bucket, so the reminder is not said again.
	if output := setupRun(t, env, "continue\ny\n", 0); strings.Contains(output, needBucket) {
		t.Fatalf("resuming a draft with a bucket said a bucket is needed:\n%s", output)
	}
	if _, found, _ := config.Load(home); !found {
		t.Fatal("resume did not install")
	}
}

func TestSetupTruncatedInputDoesNotEnable(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
	setupRun(t, env, strings.TrimSuffix(input, "y\n"), 1)
	if _, found, _ := config.Load(home); found {
		t.Fatal("EOF enabled capture")
	}
}

func TestSetupStorageFailureKeepsDraftAndOldSecret(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	setupRun(t, env, r2SetupInput(project, "old-private-value"), 0)
	old, _, _ := config.Load(home)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("offline") }
	input := "storage\nchange\nr2-existing\n0123456789abcdef0123456789abcdef\ntest-bucket\nn\nACCESS2\nnew-private-value\ny\n"
	output := setupRun(t, env, input, 1)
	secret, err := kc.Load(context.Background(), old.Storage.R2CredentialRef)
	if err != nil || secret.SecretAccessKey != "old-private-value" {
		t.Fatal("old credential replaced")
	}
	for _, value := range []string{"old-private-value", "new-private-value"} {
		if strings.Contains(output, value) {
			t.Fatal("output leaked secret")
		}
		b, _ := os.ReadFile(filepath.Join(home, "setup-draft.json"))
		if strings.Contains(string(b), value) {
			t.Fatal("draft leaked secret")
		}
	}
	current, _, _ := config.Load(home)
	if current.Storage.R2CredentialRef != old.Storage.R2CredentialRef {
		t.Fatal("active config changed")
	}
}

// After a rebuild the Keychain can refuse the stored R2 item. Choosing
// storage again asks for the key right away, and one run fixes it.
func TestSetupStorageReasksForAnUnreadableStoredCredential(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	setupRun(t, env, r2SetupInput(project, "old-private-value"), 0)
	old, _, _ := config.Load(home)
	if err := kc.Delete(context.Background(), old.Storage.R2CredentialRef); err != nil {
		t.Fatal(err)
	}
	// No answer to "Keep stored R2 credentials?": the key is asked for next.
	output := setupRun(t, env, "storage\nchange\nr2-existing\n0123456789abcdef0123456789abcdef\ntest-bucket\nACCESS2\nnew-private-value\ny\n", 0)
	if strings.Contains(output, "Keep stored R2 credentials?") || !strings.Contains(output, "can't be read from the Keychain") {
		t.Fatalf("setup offered the unreadable credential:\n%s", output)
	}
	current, _, _ := config.Load(home)
	secret, err := kc.Load(context.Background(), current.Storage.R2CredentialRef)
	if err != nil || secret.SecretAccessKey != "new-private-value" {
		t.Fatalf("new credential not stored: %v", err)
	}
}

func TestSetupReconfigurePreservesPauseIdentityActivationAndRemovesHooks(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 0)
	old, _, _ := config.Load(home)
	old.Paused = true
	old.PauseGeneration = "persisted-pause-window"
	if err := config.Save(home, old); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return time.Now().Add(time.Hour) }
	setupRun(t, env, "capture\ny\ny\nn\nn\ny\n\ny\n", 0)
	next, _, _ := config.Load(home)
	if !next.Paused || next.PauseGeneration != old.PauseGeneration || next.MachineID != old.MachineID || !next.Archive.Projects[0].ActivatedAt.Equal(old.Archive.Projects[0].ActivatedAt) {
		t.Fatal("reconfigure reset stable state")
	}
	b, _ := os.ReadFile(filepath.Join(userHome, ".claude", "settings.json"))
	if strings.Contains(string(b), hooks.Owner) {
		t.Fatal("deselected app hooks remain")
	}
}

func TestSetupSchedulerFailureRestoresExistingFiles(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	paths := []string{filepath.Join(home, "config.json"), filepath.Join(userHome, ".codex", "hooks.json"), env.installation(home, userHome).collectorPlist()}
	before := map[string]string{}
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		before[p] = string(b)
	}
	calls := 0
	fakeSched(env).beforeLoad = func(scheduler.Ref) error {
		calls++
		if calls == 1 {
			return errors.New("cannot load new job")
		}
		return nil
	}
	output := setupRun(t, env, "retention\ncustom\n120\ny\n", 1)
	if !strings.Contains(output, "restored") {
		t.Fatal(output)
	}
	for p, want := range before {
		b, _ := os.ReadFile(p)
		if string(b) != want {
			t.Fatalf("did not restore %s", p)
		}
	}
	if setupjournal.TransactionPending(home) {
		t.Fatal("successful rollback left journal")
	}
}

func TestSetupCrashRecoveryPreservesConcurrentEdits(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	path := filepath.Join(home, "config.json")
	c := hooks.Change{Path: path, Before: []byte("before"), After: []byte("after"), Existed: true, Mode: 0600}
	journal := setupjournal.Journal{Changes: []hooks.Change{c}, Plist: filepath.Join(userHome, "Library", "LaunchAgents", "com.agent-archive.collector.plist")}
	if err := local.Write(setupjournal.JournalPath(home), journal); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(setupjournal.JournalPath(home), journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverSetup(home, env); err == nil {
		t.Fatal("must refuse concurrent edit")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "user edit" {
		t.Fatal("overwrote user edit")
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverSetup(home, env); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "before" {
		t.Fatal("did not restore")
	}
}

func TestSetupDestinationRejectsPendingAndRetiresPublishedSessions(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	now := env.now().Add(time.Second)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}
	if err := handleTestHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	input := "storage\nchange\ns3-existing\nprofile\nother-bucket\ny\n"
	output := setupRun(t, env, input, 1)
	if !strings.Contains(output, "pending") {
		t.Fatal(output)
	}
	var out, errOut bytes.Buffer
	env.Now = func() time.Time { return now.Add(time.Minute) }
	if code := runSyncCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	env.Now = func() time.Time { return now.Add(2 * time.Minute) }
	setupRun(t, env, "continue\ny\n", 0)
	cfg, _, _ := config.Load(home)
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if cfg.AcceptSession(regs[0]) || len(cfg.PreviousDestinations) != 1 {
		t.Fatal("old sessions followed destination switch")
	}
}

func TestPromptsRetryInvalidValuesAndDeduplicatePaths(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("maybe\ny\n0\n-1\n30\n"), &out)
	if yes, err := p.guidedYesNo("Enable?"); err != nil || !yes {
		t.Fatal(err)
	}
	if n, err := p.guidedRetention(90); err != nil || n != 30 {
		t.Fatal(n, err)
	}
	root := t.TempDir()
	p = newPrompter(strings.NewReader("/does/not/exist\n"+root+"\n"+root+"/./\n\n"), &out)
	projects, err := promptProjects(p, nil, nil, nil)
	if err != nil || len(projects) != 1 {
		t.Fatal(projects, err)
	}
}

func TestDraftStorageEditKeepsCaptureChoices(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
	setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	setupRun(t, env, "storage\nchange\ns3-existing\nprofile\nother-bucket\ny\n", 0)
	cfg, _, _ := config.Load(home)
	if cfg.Storage.Bucket != "other-bucket" || len(cfg.Archive.Projects) != 1 || len(cfg.Harnesses) != 1 {
		t.Fatal("edit lost capture choices")
	}
}

func TestRestartRemovesOnlyStagedCredentials(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	input := r2SetupInput(project, "staged-secret")
	setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	if len(kc.items) != 1 {
		t.Fatal("missing staged credential")
	}
	setupRun(t, env, "restart\n", 1) // cancel at the first capture prompt after discarding
	if len(kc.items) != 0 {
		t.Fatal("discarded draft leaked credential")
	}
}

// Without a terminal, setup and uninstall stop before asking anything, with
// one line saying why; uninstall --yes works without one.
func TestSetupAndUninstallNeedATerminal(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	env.IsTerminal = func(any) bool { return false }
	for _, cmd := range []string{"setup", "uninstall"} {
		var out, errOut bytes.Buffer
		code := Run([]string{cmd}, strings.NewReader("y\n"), &out, &errOut, env)
		if code != 1 || out.Len() != 0 || strings.Count(errOut.String(), "\n") != 1 || !strings.Contains(errOut.String(), "terminal") {
			t.Fatalf("%s: exit %d stdout=%q stderr=%q", cmd, code, &out, &errOut)
		}
	}
	if cfg, _, _ := config.Load(home); !cfg.Archive.Enabled {
		t.Fatal("refusing changed the configuration")
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall --yes: exit %d\n%s", code, &errOut)
	}
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); !os.IsNotExist(err) {
		t.Fatal("uninstall --yes left the collector")
	}
}

type settingsProbeStore struct {
	storage.ObjectStore
	fail bool
}

func (s settingsProbeStore) Put(ctx context.Context, key string, value []byte) error {
	if s.fail {
		return errors.New("incorrect region or folder")
	}
	return s.ObjectStore.Put(ctx, key, value)
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestFailedProbeAllowsRegionAndPrefixCorrection(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"region", "prefix"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			attempts, publications := 0, 0
			env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
				if _, committed, err := config.Load(home); err != nil {
					t.Fatal(err)
				} else if committed {
					publications++
				} else {
					attempts++
				}
				fixed := cfg.Storage.Region == "eu-west-1"
				if choice == "prefix" {
					fixed = cfg.Storage.Prefix == "allowed/"
				}
				return settingsProbeStore{storagetest.NewMemoryStore(), !fixed}, nil
			}
			value := "eu-west-1"
			if choice == "prefix" {
				value = "allowed/"
			}
			input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
			setupRun(t, env, input+"edit\n"+choice+"\n"+value+"\ny\n", 0)
			if attempts != 2 || publications != 1 {
				t.Fatalf("verification attempts=%d postcommit publications=%d", attempts, publications)
			}
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatalf("saved=%v err=%v", found, err)
			}
			if choice == "region" && cfg.Storage.Region != value || choice == "prefix" && cfg.Storage.Prefix != value {
				t.Fatal(cfg.Storage)
			}
		})
	}
}

// A plain uninstall keeps the configuration with archiving disabled. Setup
// afterwards is setting up again, with the saved answers as defaults, not a
// change to an installation that is running.
//
// Regression: after uninstall, setup opened with "Agent Archive is already
// set up. What would you like to change?".
func TestSetupAfterUninstallSetsUpAgain(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, &errOut)
	}
	// Every question keeps its saved answer.
	output := setupRun(t, env, strings.Repeat("\n", 20), 0)
	if strings.Contains(output, "already set up") || strings.Contains(output, "Save these changes?") {
		t.Fatalf("setup after uninstall offered to change a running installation:\n%s", output)
	}
	if !strings.Contains(output, "Start archiving?") {
		t.Fatalf("setup after uninstall did not ask to start archiving:\n%s", output)
	}
	if cfg, _, _ := config.Load(home); !cfg.Archive.Enabled || cfg.Storage.Bucket != "test-bucket" {
		t.Fatalf("not set up again: %+v", cfg)
	}
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); err != nil {
		t.Fatalf("collector not reinstalled: %v", err)
	}
}

// Setup refuses to write an executable that is about to disappear into the
// hooks and the LaunchAgent: a go run build, which Go deletes on exit, or
// one in the temporary folder.
//
// Regression: go run ./cmd/agent-archive setup installed a go-build path,
// and capture stopped as soon as setup exited.
func TestSetupRefusesATemporaryExecutable(t *testing.T) {
	t.Parallel()
	for name, place := range map[string]func(t *testing.T, env *Env) string{
		"go-build": func(t *testing.T, _ *Env) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "go-build3829104", "b001", "exe", "agent-archive")
		},
		"temporary folder": func(t *testing.T, env *Env) string {
			t.Helper()
			temp := t.TempDir()
			env.TempDir = func() string { return temp + string(filepath.Separator) }
			return filepath.Join(temp, "downloads", "agent-archive")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			executable := place(t, &env)
			if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			env.Executable = func() (string, error) { return executable, nil }
			output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()), 1)
			if !strings.Contains(output, executable) || !strings.Contains(output, "Nothing was changed") || strings.Contains(output, "Step 1") {
				t.Fatalf("setup did not refuse before asking anything:\n%s", output)
			}
			if _, found, _ := config.Load(home); found {
				t.Fatal("setup saved a configuration")
			}
			if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); !os.IsNotExist(err) {
				t.Fatal("setup installed the LaunchAgent")
			}
		})
	}
}

// Only Go's own build directories (go-build and digits) count as temporary
// builds: a binary under a directory merely named like one is set up.
func TestSetupAcceptsADirectoryNamedLikeGoBuild(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"go-build3829104": true, "go-build1": true, "go-build": false, "go-builder": false, "go-build12x": false} {
		if got := isGoBuildDir(name); got != want {
			t.Errorf("isGoBuildDir(%q) = %v, want %v", name, got, want)
		}
	}
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	executable := filepath.Join(t.TempDir(), "src", "go-builder", "bin", "agent-archive")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env.Executable = func() (string, error) { return executable, nil }
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()), 0)
	if cfg, _, _ := config.Load(home); cfg.InstalledExecutable != executable {
		t.Fatalf("installed %q, want %q", cfg.InstalledExecutable, executable)
	}
}
