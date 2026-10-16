package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// testTempPrefix names the folder that holds one test run's
// temporary files.
const testTempPrefix = "agent-archive-cli-test-"

// isolateProcessForTesting makes the package's tests fail closed: a test
// that leaves an Env field nil gets the default, and every default that
// reaches past the test's temporary directories is replaced here, before any
// test runs, with one that stays inside them or stops the test.
//
//   - launchctl: newScheduler makes a launchd scheduler whose launchctl
//     panics, naming the command. launchd is global to
//     the login session, so even `launchctl print` from a test reads the
//     developer's real jobs, and bootstrap or bootout would change them. A
//     test that means to drive launchctl stubs it with stubLaunchctl.
//
//   - The credential store: openCredentialStore panics, so neither the real
//     Keychain nor a credentials file in a real data directory can be
//     reached. Set Env.Credentials (newFakeKeychain).
//
//   - The platform the credential store is named for: credentialOS is
//     platform.Darwin, so the many tests whose fake stands for the Keychain
//     see the Keychain's wording on every runner, Linux CI included. A test
//     of another platform's wording sets it (useCredentialOS).
//
//   - less: detectLessVersion panics. Set Env.LessVersion (testEnv does).
//
//   - The terminal's modes: openTerminalKeys never reads keys, so the
//     session browser reads lines, and no test changes the modes of the
//     terminal running it. Set Env.openKeys to read keys.
//
//   - $HOME and the variables that move app and data directories: HOME is a
//     fresh temporary directory, and AGENT_ARCHIVE_HOME, CLAUDE_CONFIG_DIR,
//     CODEX_HOME and the AWS configuration variables are unset, so
//     os.UserHomeDir, local.Home, and the AWS SDK's profile files all land
//     there rather than in the developer's own ~/.claude, ~/.cursor,
//     ~/.local/share/agent-archive, or ~/.aws.
//
//   - $TMPDIR, and so every t.TempDir: a fresh folder of the run's own under
//     /tmp, so no test reads or depends on what macOS's per-user temporary
//     folder holds (it can hold tens of thousands of entries). The cost of
//     local.CanonicalPath no longer depends on it either: it spells a path
//     without listing its parents.
//
// It returns a function that removes the temporary folders.
func isolateProcessForTesting() func() {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	parent := ""
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		parent = "/tmp"
	}
	// A test binary this one starts (the terminal tests' child) inherits
	// $TMPDIR and nests its folder there. Managed scratch runs do the same,
	// so their owner can remove all fixtures after an interrupted test,
	// and this run's removal covers a nested test binary
	// even when the child is killed before it can clean up.
	if inherited := managedTestTempParent(os.Getenv("TMPDIR"), os.Getenv("AGENT_SCRATCH_DIR")); inherited != "" {
		parent = inherited
	}
	tmp, err := os.MkdirTemp(parent, testTempPrefix)
	must(err)
	must(os.Setenv("TMPDIR", tmp))
	home, err := os.MkdirTemp("", "cli-home-")
	must(err)
	must(os.Setenv("HOME", home))
	// Nor does a test see the agent it may be run from: an agent's variables
	// switch off every prompt. Tests that mean an agent inject them through
	// Env.LookupEnv, and the suite is also run with them set to prove it.
	for _, name := range append([]string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", envNonInteractive}, agentShellEnv()...) {
		must(os.Unsetenv(name))
	}
	newScheduler = func(backend string) (scheduler.Scheduler, error) {
		if backend != "" && backend != "launchd" {
			return nil, fmt.Errorf("a test asked for the %s scheduler: set Env.Scheduler (testEnv does)", backend)
		}
		return launchd.Scheduler{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			panic(fmt.Sprintf("a test reached the real %s %q: set Env.Scheduler (testEnv does), or call stubLaunchctl", name, args))
		}}, nil
	}
	realOpenCredentialStore = openCredentialStore
	openCredentialStore = func() (credentials.CredentialStore, error) {
		panic("a test reached the real credential store: set Env.Credentials (newFakeKeychain)")
	}
	openCloudflare = func(string) cloudflare.API {
		panic("a test reached the real Cloudflare API: set Env.Cloudflare")
	}
	productionCredentialOS = credentialOS
	credentialOS = platform.Darwin
	openAWSBuckets = func(string, string) (BucketFinder, error) {
		return nil, errors.New("no AWS in this test: set Env.AWSBuckets")
	}
	openAWSBucketCreator = func(string, string) (BucketCreator, error) {
		return nil, errors.New("no AWS in this test: set Env.AWSBucketCreator")
	}
	// The mount table is the machine's own: a Linux test that means a network
	// filesystem sets Env.MountTable, and every other reads none, which is no
	// answer and stops nothing.
	readMountTable = func() ([]byte, error) { return nil, errors.New("no mount table in this test: set Env.MountTable") }
	detectLessVersion = func(string) (int, bool) {
		panic("a test reached the real less: set Env.LessVersion (testEnv does)")
	}
	openTerminalKeys = func(io.Reader) (keyTerminal, bool) { return nil, false }
	return func() { _ = os.RemoveAll(home); _ = os.RemoveAll(tmp) }
}

// managedTestTempParent keeps nested test binaries and managed scratch runs
// inside their owner's temporary tree. Ordinary macOS TMPDIR remains excluded.
func managedTestTempParent(inherited, scratch string) string {
	if inherited == "" {
		return ""
	}
	if strings.HasPrefix(filepath.Base(filepath.Clean(inherited)), testTempPrefix) ||
		(scratch != "" && filepath.Clean(inherited) == filepath.Join(filepath.Clean(scratch), "tmp")) {
		return inherited
	}
	return ""
}

func TestManagedScratchTempParent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, inherited, scratch, want string
	}{
		{"managed", "/owned/run-abcdefgh/tmp", "/owned/run-abcdefgh", "/owned/run-abcdefgh/tmp"},
		{"nested", "/tmp/agent-archive-cli-test-123", "", "/tmp/agent-archive-cli-test-123"},
		{"ordinary", "/var/folders/user/tmp", "", ""},
		{"outside-owner", "/other/tmp", "/owned/run-abcdefgh", ""},
		{"owner-without-temp", "", "/owned/run-abcdefgh", ""},
		{"temp-without-owner", "/owned/run-abcdefgh/tmp", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedTestTempParent(tc.inherited, tc.scratch); got != tc.want {
				t.Fatalf("temporary parent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsolationFailsClosed pins isolateProcessForTesting: every default that
// reaches this machine itself stops the test, and the process's own home is a
// temporary one.
func TestIsolationFailsClosed(t *testing.T) {
	t.Parallel()
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s reached the real system instead of stopping the test", name)
			}
		}()
		f()
	}
	// A bare Env{} reaches launchd through newScheduler.
	site, ref := scheduler.Site{UserHome: "/nonexistent"}, scheduler.Ref("com.agent-archive.collector")
	panics("launchctl print", func() { Env{}.scheduler().Inspect(context.Background(), site, ref) })
	panics("launchctl bootstrap", func() { _ = Env{}.scheduler().Load(context.Background(), site, ref) })
	panics("launchctl bootout", func() { _ = Env{}.scheduler().Unload(context.Background(), site, ref) })
	// So does every backend name a configuration or a journal records:
	// launchd's, and "" (this system's own), stop the test, and any other is
	// refused before a scheduler exists, so a configuration that names it gets
	// one that runs nothing.
	for _, name := range []string{"", "launchd"} {
		s, err := newScheduler(name)
		if err != nil {
			t.Fatalf("newScheduler(%q): %v", name, err)
		}
		panics("launchctl print for "+name, func() { s.Inspect(context.Background(), site, ref) })
		panics("launchctl bootstrap for "+name, func() { _ = s.Load(context.Background(), site, ref) })
		panics("launchctl bootout for "+name, func() { _ = s.Unload(context.Background(), site, ref) })
	}
	for _, name := range []string{"systemd", "none", "cron"} {
		if s, err := newScheduler(name); err == nil {
			t.Errorf("newScheduler(%q) = %s, want a refusal", name, s.Name())
		}
		dataHome := t.TempDir()
		if err := config.Save(dataHome, config.Config{BackgroundBackend: name}); err != nil {
			t.Fatal(err)
		}
		s := Env{Home: func() (string, error) { return dataHome, nil }}.scheduler()
		if got := s.Inspect(context.Background(), site, ref); s.Name() != name || got.State != scheduler.Unknown {
			t.Errorf("a configuration recording %s: scheduler %s says %q", name, s.Name(), got.State)
		}
		if s.Load(context.Background(), site, ref) == nil || s.Unload(context.Background(), site, ref) == nil {
			t.Errorf("a configuration recording %s: its scheduler changed a job", name)
		}
	}
	panics("Env{}.credentialStore", func() { _, _ = Env{}.credentialStore() })
	panics("less --version", func() { _, _ = Env{}.lessVersion("less") })
	panics("R2 store", func() {
		_, _ = Env{}.openStore(config.Config{Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2CredentialRef: "r"}})
	})
	panics("Env{}.cloudflareAPI", func() { Env{}.cloudflareAPI("token") })
	if _, err := (Env{}).awsBuckets("default", "us-east-1"); err == nil {
		t.Error("Env{}.awsBuckets reached AWS instead of failing")
	}
	if _, err := (Env{}).awsBucketCreator("default", "us-east-1"); err == nil {
		t.Error("Env{}.awsBucketCreator reached AWS instead of failing")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if account, err := user.Current(); err == nil && filepath.Clean(account.HomeDir) == filepath.Clean(home) {
		t.Fatalf("tests run with the account's real home %s", home)
	}
	for _, name := range []string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s=%s leaks into the tests", name, value)
		}
	}
	// testEnv's side-effecting fields fail rather than reach the machine.
	env := testEnv(t, t.TempDir(), time.Now())
	if _, err := env.credentialStore(); err == nil {
		t.Error("testEnv's credential store must fail unless a test sets one")
	}
	if _, err := env.executable(); err == nil {
		t.Error("testEnv's Executable must fail unless a test sets one")
	}
	if got := env.jobStatus("/nonexistent", "x").State; got != scheduler.Missing {
		t.Errorf("testEnv job state = %q", got)
	}
}

// realOpenCredentialStore is the default openCredentialStore, which
// isolateProcessForTesting replaced, kept so a test can check how it is wired
// (TestOpenCredentialStoreIsWiredToTheDataDirectory).
var realOpenCredentialStore func() (credentials.CredentialStore, error)

// productionCredentialOS is credentialOS as the program starts, before
// isolateProcessForTesting pins it to Darwin for the tests.
var productionCredentialOS platform.OS

// useCredentialOS names the credential store for another platform for one
// test. The test must not be parallel: the variable is shared.
func useCredentialOS(t *testing.T, system platform.OS) {
	t.Helper()
	previous := credentialOS
	credentialOS = system
	t.Cleanup(func() { credentialOS = previous })
}

// launchctlChangeTimeout is the bound the launchd scheduler puts on launchctl
// bootstrap and bootout while a test runs (launchd.ChangeTimeout); a test of
// what a hung launchctl does shortens it, and so must not run in parallel.
var launchctlChangeTimeout = launchd.ChangeTimeout

// stubLaunchctl replaces launchctl for one test: a nil Env.Scheduler then
// means launchd's own code over run, which answers each launchctl call.
func stubLaunchctl(t *testing.T, run func(args ...string) ([]byte, error)) {
	t.Helper()
	stubLaunchctlContext(t, func(_ context.Context, args ...string) ([]byte, error) { return run(args...) })
}

// stubLaunchctlContext is stubLaunchctl for a stand-in that watches the
// command's context.
func stubLaunchctlContext(t *testing.T, run func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	previous := newScheduler
	newScheduler = func(backend string) (scheduler.Scheduler, error) {
		if backend != "" && backend != "launchd" {
			return nil, fmt.Errorf("a test asked for the %s scheduler: stubLaunchctl stands in for launchd alone", backend)
		}
		return launchd.Scheduler{ChangeTimeout: launchctlChangeTimeout, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name != "launchctl" {
				t.Errorf("the scheduler ran %q, not launchctl", name)
			}
			return run(ctx, args...)
		}}, nil
	}
	t.Cleanup(func() { newScheduler = previous })
}
