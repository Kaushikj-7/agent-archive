package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// noEnv is an empty process environment.
func noEnv(string) (string, bool) { return "", false }

func testEnv(t *testing.T, home string, now time.Time) Env {
	t.Helper()
	userHome, accountHome, tempDir := t.TempDir(), t.TempDir(), t.TempDir()
	return Env{
		UserHomeDir: func() (string, error) { return userHome, nil },
		// Tests never run as the account's default installation.
		AccountHome: func() (string, error) { return accountHome, nil },
		Home:        func() (string, error) { return home, nil },
		Now:         func() time.Time { return now },
		// Test executables live in the run's temporary folder, which setup
		// refuses; this one holds none of them.
		TempDir: func() string { return tempDir },
		// Tests must not see the real environment: run inside an agent,
		// CLAUDE_CODE_SESSION_ID would change what `handoff --latest` skips.
		LookupEnv: func(string) (string, bool) { return "", false },
		// Guided bucket creation waits for Cloudflare between checks; a
		// test does not.
		Pause: func(time.Duration) {},
		// Tests never change the real process environment.
		UnsetEnv: func(string) error { return nil },
		// Tests model a Mac (its app folders and Cursor's Library data
		// folder), whatever system runs them.
		OS: platform.Darwin,
		// Nor this machine's ID: a Linux test that means a machine sets it.
		HostFingerprint: func() string { return "" },
		OpenStore: func(config.Config) (storage.ObjectStore, error) {
			return storagetest.NewMemoryStore(), nil
		},
		// Everything below would otherwise reach this machine itself. Reads get
		// a harmless answer; anything that would change the machine fails the
		// test. A test that needs one sets it (setupTestEnv sets them all).
		Scheduler: noLaunchd(t),
		Credentials: func() (credentials.CredentialStore, error) {
			return nil, errors.New("no credential store in this test: set Env.Credentials")
		},
		Executable: func() (string, error) {
			return "", errors.New("no executable in this test: set Env.Executable")
		},
		WorkingDir: func() (string, error) { return "", errors.New("no working directory in this test") },
		// Tests never run git to find a directory's repository key: no
		// directory has an origin remote unless a test says so.
		repoKey:              func(string) string { return "" },
		AWSProfiles:          func() ([]AWSProfile, error) { return nil, nil },
		DetectHarnesses:      func(string) []string { return nil },
		DiscoverApplications: func(string) map[string]applicationDiscovery { return map[string]applicationDiscovery{} },
		Interrupts:           noInterrupts,
		// A less new enough for --mouse, as macOS ships; the tests of the
		// other defaults set their own.
		LessVersion: func(string) (int, bool) { return testLessVersion, true },
		OpenTerminal: func(spec termlaunch.Spec) (string, error) {
			t.Errorf("unexpected new terminal for %q: set Env.OpenTerminal", spec.Argv)
			return "", errors.New("no terminal in this test")
		},
		Clipboard: func([]byte) error {
			t.Error("unexpected clipboard write: set Env.Clipboard")
			return errors.New("no clipboard in this test")
		},
	}
}

// testLessVersion is the less version testEnv reports.
const testLessVersion = 668

// noInterrupts is Env.Interrupts for tests that do not send signals. The
// default installs real handlers for Ctrl-C, SIGTERM, SIGHUP, and SIGQUIT, and while
// any parallel backfill test held them, a signal sent to the test run would
// end it through exitOnSignal with a bare exit status instead of the
// signal's name.
func noInterrupts() (<-chan os.Signal, func()) { return nil, func() {} }

// credentialsTestConfig is a syntactically valid storage destination for
// tests that never actually touch storage (they use OpenStore above, or
// test hook logic that writes only local files).
func credentialsTestConfig() credentials.Config {
	return credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}
}

func TestHelpAndVersion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := Run([]string{"--help"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "agent-archive setup") {
		t.Fatalf("help output missing usage: %s", out.String())
	}

	out.Reset()
	if code := Run([]string{"--version"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.TrimSpace(out.String()) != Version {
		t.Fatalf("version output=%q want=%q", out.String(), Version)
	}
}

func TestUnknownCommandAndNoArgs(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := Run([]string{"bogus"}, nil, &out, &errOut, Env{}); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr=%q", errOut.String())
	}

	errOut.Reset()
	if code := Run(nil, nil, &out, &errOut, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
}

// With no arguments, a Mac that is not set up is told to run setup first;
// one that is set up sees only the command list.
func TestNoArgsSaysWhenNotSetUp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	var out bytes.Buffer
	if code := Run(nil, nil, &out, nil, env); code != 0 || !strings.HasPrefix(out.String(), "Not set up yet. For sessions already on this machine, try `agent-archive handoff`; to start archiving, run `agent-archive setup`.\n\nAgent Archive") {
		t.Fatalf("exit %d\n%s", code, &out)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Fatal("printing help wrote the data directory")
	}
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()), 0)
	out.Reset()
	if code := Run(nil, nil, &out, nil, env); code != 0 || !strings.HasPrefix(out.String(), "Agent Archive") {
		t.Fatalf("exit %d\n%s", code, &out)
	}
}

// must fails the test on a fixture setup error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// publishedThroughSync sets up a project, registers and publishes one Codex
// session into a shared in-memory bucket, and returns the environment, home,
// transcript path, and bucket. Retention is set to 90 days.
func publishedThroughSync(t *testing.T, now time.Time) (Env, string, string, storage.ObjectStore) {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	path := writeCodexTranscript(t, project)
	if err := handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": project, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	result, err := runOnePass(env, false)
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("setup publish: %+v %v", result, err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 90
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	return env, home, path, bucket
}

func writeCodexTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "codex.jsonl")
	content := `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
