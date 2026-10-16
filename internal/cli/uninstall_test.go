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

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// installedFixture runs a real S3 setup into fresh temp homes so uninstall
// tests start from exactly the state setup leaves behind.
func installedFixture(t *testing.T, keychain *fakeKeychain, input string) (home, userHome string, env Env) {
	t.Helper()
	home = t.TempDir()
	userHome = t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env = setupTestEnv(t, home, userHome, keychain, now)
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, strings.NewReader(input), &stdout, &stderr, env); code != 0 {
		t.Fatalf("setup failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	return home, userHome, env
}

func TestUninstallKeepsLocalDataByDefault(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	cfg, found, _ := config.Load(home)
	if !found || cfg.Archive.Enabled {
		t.Fatal("must retain disabled config")
	}
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); !os.IsNotExist(err) {
		t.Fatal("plist remains")
	}
	for _, rel := range []string{".codex/hooks.json", ".claude/settings.json"} {
		b, _ := os.ReadFile(filepath.Join(userHome, rel))
		if strings.Contains(string(b), hooks.Owner) {
			t.Fatal("owned hooks remain")
		}
	}
}

func TestUninstallDeclineChangesNothing(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	fakeSched(env).beforeUnload = func(scheduler.Ref) error {
		t.Fatal("declining must not unload the LaunchAgent")
		return nil
	}
	hookPath := filepath.Join(userHome, ".codex", "hooks.json")
	before, _ := os.ReadFile(hookPath)

	for _, answer := range []string{"n\n", "\n"} { // explicit no, and the default
		var stdout, stderr bytes.Buffer
		code := runUninstallCommand(nil, strings.NewReader(answer), &stdout, &stderr, env)
		if code != 0 {
			t.Fatalf("answer=%q code=%d stderr=%s", answer, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Cancelled") {
			t.Fatalf("stdout=%s", stdout.String())
		}
	}
	if _, found, err := config.Load(home); err != nil || !found {
		t.Fatalf("declining must keep the config: found=%v err=%v", found, err)
	}
	after, _ := os.ReadFile(hookPath)
	if string(after) != string(before) {
		t.Fatal("declining must not touch hook configuration")
	}
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); err != nil {
		t.Fatalf("declining must keep the plist: %v", err)
	}
}

func TestUninstallRejectsTruncatedInputInsteadOfProceeding(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader(""), &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("expected no input to fail, not take the default: code=%d stdout=%s", code, stdout.String())
	}
	if _, found, _ := config.Load(home); !found {
		t.Fatal("truncated input must not remove anything")
	}
}

func TestUninstallFailsBeforePromptingWhenSettingsAreUnreadable(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env)

	if code != 1 {
		t.Fatalf("code=%d, want 1: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("uninstall prompted before finding the settings unreadable: %s", stdout.String())
	}
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); err != nil {
		t.Fatalf("a failed uninstall must keep the plist: %v", err)
	}
}

func TestUninstallPreservesUnrelatedHooksAndSettings(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	claudePath := filepath.Join(userHome, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`
	if err := os.WriteFile(claudePath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, strings.NewReader(s3SetupInput("test-bucket", "us-east-1", "test-profile", false, true, false, t.TempDir())), &stdout, &stderr, env); code != 0 {
		t.Fatalf("setup failed: code=%d stderr=%s", code, stderr.String())
	}
	installed, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(installed), hooks.Owner) {
		t.Fatalf("sanity: setup should have installed our handler:\n%s", installed)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("the user's settings file must survive: %v", err)
	}
	s := string(after)
	if strings.Contains(s, hooks.Owner) || strings.Contains(s, "_hook") {
		t.Fatalf("our handler must be gone:\n%s", s)
	}
	if !strings.Contains(s, `"say done"`) || !strings.Contains(s, `"Read"`) {
		t.Fatalf("unrelated hook or settings lost:\n%s", s)
	}
}

func TestUninstallDeletesStoredR2CredentialsOnly(t *testing.T) {
	t.Parallel()
	keychain := newFakeKeychain()
	// A credential under some other reference stands in for anything else
	// stored under our Keychain service; uninstall must leave it alone.
	if err := keychain.Save(context.Background(), "r2-other-bucket", credentials.R2Credentials{AccessKeyID: "OTHER", SecretAccessKey: "othersecret"}); err != nil {
		t.Fatal(err)
	}
	r2Input := r2SetupInput(t.TempDir(), "supersecret")
	home, _, env := installedFixture(t, keychain, r2Input)
	cfg, _, _ := config.Load(home)
	ref := cfg.Storage.R2CredentialRef

	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if _, err := keychain.Load(context.Background(), ref); !errors.Is(err, credentials.ErrMissingCredential) {
		t.Fatalf("expected the stored R2 secret deleted, got err=%v", err)
	}
	if _, err := keychain.Load(context.Background(), "r2-other-bucket"); err != nil {
		t.Fatalf("an unrelated credential must survive: %v", err)
	}
	combined := stdout.String() + stderr.String()
	for _, secret := range []string{"supersecret", "AKIAEXAMPLE"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("uninstall output must never contain a secret:\n%s", combined)
		}
	}
}

func TestUninstallWithS3NeverOpensKeychain(t *testing.T) {
	t.Parallel()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	env.Credentials = func() (credentials.CredentialStore, error) {
		t.Fatal("an S3 configuration references no Keychain item; uninstall must not open Keychain")
		return nil, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "stored R2 credentials") {
		t.Fatalf("plan should not mention a Keychain item for S3:\n%s", stdout.String())
	}
}

func TestUninstallRemovesLeftoversWithoutAConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	userHome := t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	// The state a setup that failed at config.Save, and whose rollback also
	// failed, would leave: a plist and hooks but no config.
	executable := "/opt/agent-archive/bin/agent-archive"
	changes, err := hooks.Plan(hooks.ResolveFiles(userHome, noEnv, productionAgents), env.installation(home, userHome).hook(executable), []string{"cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Apply(changes); err != nil {
		t.Fatal(err)
	}
	plist, err := launchd.LaunchAgent(executable, home, launchd.Label(env.installation(home, userHome).collectorPlist()), nil)
	if err != nil {
		t.Fatal(err)
	}
	plistPath := env.installation(home, userHome).collectorPlist()
	if err := local.WriteBytes(plistPath, plist); err != nil {
		t.Fatal(err)
	}
	// A launchd that never had this plist loaded reports an error; that
	// must be a warning, not a failure.
	fakeSched(env).beforeUnload = func(scheduler.Ref) error { return errors.New("not loaded") }

	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("expected the plist removed, stat err=%v", err)
	}
	b, _ := os.ReadFile(filepath.Join(userHome, ".cursor", "hooks.json"))
	if strings.Contains(string(b), hooks.Owner) {
		t.Fatalf("cursor hook entry should be removed even without a config:\n%s", b)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallRefusesToRemoveTheUserHome(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	env := setupTestEnv(t, userHome, userHome, newFakeKeychain(), time.Now())
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 1 {
		t.Fatalf("code=%d stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "refusing to remove") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if _, err := os.Stat(userHome); err != nil {
		t.Fatalf("user home must survive: %v", err)
	}
}

func TestUninstallReportsBusyCollectorAndKeepsLocalState(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "holds the collector lock; retry uninstall") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if _, found, _ := config.Load(home); !found {
		t.Fatal("local state must survive while a collector holds the lock")
	}
}

func TestUsageListsUninstall(t *testing.T) {
	t.Parallel()
	// uninstall is an advanced command: the standard help points at the
	// advanced tier, and the advanced tier lists it.
	var out bytes.Buffer
	if code := Run([]string{"--help"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "agent-archive help advanced") {
		t.Fatalf("standard help does not point at the advanced tier: %s", out.String())
	}
	out.Reset()
	if code := Run([]string{"help", "advanced"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "agent-archive uninstall") {
		t.Fatalf("advanced help output missing uninstall: %s", out.String())
	}
}

func TestUninstallLeavesFilesItDidNotCreate(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	// A user who pointed AGENT_ARCHIVE_HOME at a directory of their own.
	foreign := filepath.Join(home, "my-notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("expected a leftover to be reported as incomplete: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "my-notes.txt") {
		t.Fatalf("stderr must name the leftover entry:\n%s", stderr.String())
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "keep me\n" {
		t.Fatalf("foreign file must survive untouched: err=%v content=%q", err, b)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("agent-archive's own config must still be removed")
	}
	// The lock files are agent-archive's own and are removed too; the
	// directory stays only because the foreign file is still in it.
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 || entries[0].Name() != "my-notes.txt" {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("only the foreign file should remain, got %q", names)
	}
}

// `list` keeps a disposable metadata cache under the data directory;
// --delete-local-data must remove it as agent-archive's own state rather than
// report it as a leftover.
func TestUninstallDeleteLocalDataRemovesTheMetadataCache(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	cacheDir := filepath.Join(home, "cache", "metadata")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "00.json"), []byte(`{"key":"sessions/codex/x/metadata.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "cache")); !os.IsNotExist(err) {
		t.Fatalf("cache directory survived uninstall: err=%v", err)
	}
	if strings.Contains(stderr.String(), "cache") {
		t.Fatalf("cache reported as a leftover:\n%s", stderr.String())
	}
}

func TestUninstallCannotResumeRemovedIntegrations(t *testing.T) {
	t.Parallel()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"resume"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "setup") {
		t.Fatal("resume claimed removed integrations were active")
	}
}

// Uninstall only needs the hook files of the apps setup installed. An
// unparsable file of an app that was never selected is reported and left
// alone instead of blocking the collector's removal.
func TestUninstallSkipsAnUnparsableFileOfAnUnselectedApp(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cursor := filepath.Join(userHome, ".cursor", "hooks.json")
	must(t, os.MkdirAll(filepath.Dir(cursor), 0700))
	broken := []byte("{\"version\":1,\"hooks\":{},}\n")
	must(t, os.WriteFile(cursor, broken, 0600))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}
	if !strings.Contains(out.String(), "Skipped "+cursor) {
		t.Fatalf("the skipped file was not reported:\n%s", &out)
	}
	if b, _ := os.ReadFile(cursor); !bytes.Equal(b, broken) {
		t.Fatal("the unparsable file was changed")
	}
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); !os.IsNotExist(err) {
		t.Fatal("the collector was not removed")
	}
	// The same file blocks uninstall once setup did install Cursor hooks.
	_, userHome, env = installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, false, true, t.TempDir()))
	cursor = filepath.Join(userHome, ".cursor", "hooks.json")
	must(t, os.WriteFile(cursor, broken, 0600))
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), cursor) {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}
}

// A hook file edited while uninstall runs is reported as such, with uninstall
// (not setup) as the command to rerun.
func TestUninstallConcurrentEditNamesUninstall(t *testing.T) {
	t.Parallel()
	_, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	settings := filepath.Join(userHome, ".claude", "settings.json")
	sched := fakeSched(env)
	sched.stateFn = func(ref scheduler.Ref) string {
		// Runs after uninstall planned its changes: an editor saves the file.
		b, _ := os.ReadFile(settings)
		must(t, os.WriteFile(settings, append(b, ' '), 0600))
		return sched.state(ref)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env)
	if code != 1 || !strings.Contains(errOut.String(), settings) || !strings.Contains(errOut.String(), "rerun agent-archive uninstall") || strings.Contains(errOut.String(), "retry setup") {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
}

// Uninstall also removes hooks an earlier release left at the legacy path
// when the recorded path is elsewhere.
//
// Regression: hook ownership review, 2026-09 (1a9420b).
func TestUninstallCleansTheLegacyPathToo(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	claudeDir := filepath.Join(userHome, "cfg")
	must(t, os.MkdirAll(claudeDir, 0700))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.LookupEnv = func(k string) (string, bool) { return claudeDir, k == "CLAUDE_CONFIG_DIR" }
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	legacy, err := hooks.Plan(legacyHookFiles(userHome), env.installation(home, userHome).hook("/opt/old/agent-archive"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Apply(legacy); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), legacy[0].Path} {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), hooks.Owner) {
			t.Fatalf("hooks left in %s", path)
		}
	}
}
