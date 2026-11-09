package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// The tests that set credentialOS (useCredentialOS, or linuxFixture) are not
// parallel: the package's parallel tests read it (they run after every serial
// test, so a serial test that restores it can never race them). The tests
// that pass the platform to the wording functions themselves are parallel.

// Every credential message on macOS is the message it was before the file
// store existed; the other platform's words are new and never name the
// Keychain.
func TestCredentialWordsOnBothPlatforms(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	const dataDir = "/data/agent-archive"
	darwin := map[string]string{
		"open":              "open Keychain: cause",
		"storage open":      "keychain unavailable: cause",
		"check label":       "Keychain",
		"fix unavailable":   "Use the release build of agent-archive, which can open the Keychain, or store in Amazon S3 with agent-archive setup --yes --provider s3.",
		"fix locked":        "Unlock the login Keychain (log in, or open Keychain Access), then run agent-archive setup again, or store in Amazon S3 with agent-archive setup --yes --provider s3.",
		"staged note":       "A Keychain item it staged, if any, stays in the Keychain (service agent-archive).",
		"unreadable draft":  `The saved setup in /d/setup-draft.json cannot be read (bad), so a Keychain item it staged, if any, is not deleted. Look for items of service "agent-archive" in Keychain Access.`,
		"undeleted":         `2 stored credential(s) could not be deleted from Keychain service "agent-archive": cause. To remove them yourself, run: security delete-generic-password -s agent-archive -a r1 && security delete-generic-password -s agent-archive -a r2; or delete those items in Keychain Access`,
		"undeleted locked":  `2 stored credential(s) could not be deleted from Keychain service "agent-archive": ` + credentials.ErrKeychainLocked.Error() + `. Unlock the login Keychain (log in, or open Keychain Access). To remove them yourself, run: security delete-generic-password -s agent-archive -a r1 && security delete-generic-password -s agent-archive -a r2; or delete those items in Keychain Access`,
		"store name in use": "the Keychain",
	}
	linux := map[string]string{
		"open":              "open credentials file: cause",
		"storage open":      "open the credentials file: cause",
		"check label":       "Credentials file",
		"fix unavailable":   "Fix what the message says, then run agent-archive setup again, or store in Amazon S3 with agent-archive setup --yes --provider s3.",
		"fix locked":        "Fix what the message says, then run agent-archive setup again, or store in Amazon S3 with agent-archive setup --yes --provider s3.",
		"staged note":       "A credentials file it staged, if any, stays in /data/agent-archive/credentials.",
		"unreadable draft":  "",
		"undeleted":         "2 stored credential(s) could not be deleted from the credentials folder /data/agent-archive/credentials: cause. To remove them yourself, delete the files in that folder",
		"undeleted locked":  "2 stored credential(s) could not be deleted from the credentials folder /data/agent-archive/credentials: " + credentials.ErrKeychainLocked.Error() + ". To remove them yourself, delete the files in that folder",
		"store name in use": "the credentials file",
	}
	// An unknown platform has no store (credentials.OpenDefault refuses it),
	// so its words never claim the Keychain or a file: the store is "the
	// credential store", and what a Keychain is not is worded as the file
	// store's.
	unknown := map[string]string{}
	for name, text := range linux {
		unknown[name] = strings.ReplaceAll(strings.ReplaceAll(text, "open credentials file", "open credential store"), "open the credentials file", "open the credential store")
	}
	unknown["check label"] = "Credential store"
	unknown["store name in use"] = "the credential store"
	for goos, want := range map[platform.OS]map[string]string{platform.Darwin: darwin, platform.Linux: linux, platform.Unknown: unknown} {
		got := map[string]string{
			"open":              openCredentialStoreError(goos, cause).Error(),
			"storage open":      storageOpenError(goos, cause).Error(),
			"check label":       credentialCheckLabel(goos),
			"fix unavailable":   credentialCheckFix(goos, false),
			"fix locked":        credentialCheckFix(goos, true),
			"staged note":       stagedCredentialLeftNote(goos, dataDir),
			"unreadable draft":  unreadableDraftUninstallNote(goos, "/d/setup-draft.json", "bad"),
			"undeleted":         undeletedCredentialsProblem(goos, dataDir, []string{"r1", "r2"}, cause, credentialFolder{remains: true}),
			"undeleted locked":  undeletedCredentialsProblem(goos, dataDir, []string{"r1", "r2"}, credentials.ErrKeychainLocked, credentialFolder{remains: true}),
			"store name in use": "the " + credentials.StoreName(goos),
		}
		for name, wantText := range want {
			if got[name] != wantText {
				t.Errorf("%s: %s = %q, want %q", goos, name, got[name], wantText)
			}
		}
		if goos != platform.Darwin {
			for name, text := range got {
				if strings.Contains(text, "Keychain") && name != "undeleted locked" {
					t.Errorf("%s: %s names the Keychain: %q", goos, name, text)
				}
			}
		}
	}
}

// The credential check's hint about a release build is about the Keychain's
// cgo build, so it must not appear on Linux; there the failure's own message
// says what to fix.
func TestPreflightCredentialCheckOnLinux(t *testing.T) {
	useCredentialOS(t, platform.Linux)
	dir := filepath.Join(t.TempDir(), credentials.CredentialsDirName)
	store, err := credentials.NewFileStore(dir)
	must(t, err)
	const ref = "setup-0123456789abcdef0123456789abcdef"
	must(t, store.Save(context.Background(), ref, credentials.R2Credentials{AccessKeyID: "KEY", SecretAccessKey: "secret-value"}))
	env := credentialsOnly{func() (credentials.CredentialStore, error) { return store, nil }}

	// A private file: the check passes, and says so without the Keychain.
	if check := keychainCheck(env, ref); !check.OK || check.Label != "Credentials file" || strings.Contains(check.Detail, "Keychain") {
		t.Fatalf("check of a private file: %+v", check)
	}
	// No key saved yet: a folder that does not exist is not a failure.
	if check := keychainCheck(credentialsOnly{func() (credentials.CredentialStore, error) {
		return credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return filepath.Join(t.TempDir(), "none"), nil }, LookupEnv: noEnv})
	}}, ""); !check.OK {
		t.Fatalf("check with nothing saved: %+v", check)
	}

	// A file open to other users blocks setup, naming the file and the fix.
	path := filepath.Join(dir, ref+".json")
	must(t, os.Chmod(path, 0o644))
	check := keychainCheck(env, ref)
	if check.OK || check.Label != "Credentials file" {
		t.Fatalf("check of an open file: %+v", check)
	}
	if !strings.Contains(check.Detail, "chmod 600 '"+path+"'") {
		t.Errorf("detail does not give the fix: %q", check.Detail)
	}
	for _, text := range []string{check.Label, check.Detail, check.Fix} {
		for _, banned := range []string{"Keychain", "release build"} {
			if strings.Contains(text, banned) {
				t.Errorf("Linux check mentions %q: %q", banned, text)
			}
		}
	}
	if strings.Contains(check.Detail, "secret-value") || strings.Contains(check.Detail, "KEY") {
		t.Errorf("detail contains a secret: %q", check.Detail)
	}
	// The store failing to open at all reads the same.
	failed := keychainCheck(credentialsOnly{func() (credentials.CredentialStore, error) { return nil, credentials.ErrUnavailable }}, ref)
	if failed.OK || strings.Contains(failed.Fix, "release build") || strings.Contains(failed.Fix, "Keychain") || !strings.Contains(failed.Fix, "--provider s3") {
		t.Errorf("Linux check of a store that does not open: %+v", failed)
	}
}

// credentialsOnly is all keychainCheck needs of the command environment.
type credentialsOnly struct {
	open func() (credentials.CredentialStore, error)
}

func (c credentialsOnly) credentialStore() (credentials.CredentialStore, error) { return c.open() }

// macOS's check reads exactly as it did.
func TestPreflightCredentialCheckOnMacOSKeepsItsWording(t *testing.T) {
	useCredentialOS(t, platform.Darwin)
	ok := keychainCheck(credentialsOnly{func() (credentials.CredentialStore, error) { return newFakeKeychain(), nil }}, "")
	if !ok.OK || ok.Label != "Keychain" || ok.Detail != "opens (for the R2 key)" {
		t.Fatalf("check that opens: %+v", ok)
	}
	unavailable := keychainCheck(credentialsOnly{func() (credentials.CredentialStore, error) { return nil, credentials.ErrUnavailable }}, "")
	if unavailable.OK || unavailable.Label != "Keychain" ||
		unavailable.Detail != "cannot be opened, so an R2 key cannot be kept (credential store unavailable)" ||
		unavailable.Fix != "Use the release build of agent-archive, which can open the Keychain, or store in Amazon S3 with agent-archive setup --yes --provider s3." {
		t.Fatalf("unavailable check: %+v", unavailable)
	}
	locked := keychainCheck(credentialsOnly{func() (credentials.CredentialStore, error) {
		return lockedKeychain{newFakeKeychain()}, nil
	}}, "")
	if locked.OK || !strings.HasPrefix(locked.Fix, "Unlock the login Keychain") {
		t.Fatalf("locked check: %+v", locked)
	}
}

// A store that opens on Linux is a real file store: setup --yes saves the key
// as a private file under the data directory, and uninstall deletes it with
// the rest, without naming the Keychain.
func TestSetupAndUninstallKeepTheR2KeyInAFileOnLinux(t *testing.T) {
	useCredentialOS(t, platform.Linux)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	dir := credentials.FileStoreDir(home)
	env.Credentials = func() (credentials.CredentialStore, error) {
		return credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return dir, nil }, LookupEnv: noEnv})
	}
	output := setupYes(t, env, "linux-secret-value\n", 0, "--yes", "--provider", "r2", "--r2-account", "https://"+testR2Account+".r2.cloudflarestorage.com/my-bucket",
		"--r2-access-key-id", "KEY", "--project", project, "--apps", "claude")
	if strings.Contains(output, "Keychain") || strings.Contains(output, "linux-secret-value") {
		t.Fatalf("setup output names the Keychain or a secret:\n%s", output)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config: %v %v", found, err)
	}
	path := filepath.Join(dir, cfg.Storage.R2CredentialRef+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("setup did not write the credentials file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode = %04o", info.Mode().Perm())
	}
	if dirInfo, err := os.Stat(dir); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("credentials folder: %v %v", dirInfo, err)
	}

	// setup --yes again, with no key given, keeps the stored one: it reads it back.
	setupYes(t, env, "", 0, "--yes")

	// With the file made readable by others, setup --yes stops before doing
	// anything, and names the fix.
	must(t, os.Chmod(path, 0o644))
	output = setupYes(t, env, "", 1, "--yes")
	if !strings.Contains(output, "✗ Credentials file: cannot be opened") || !strings.Contains(output, "chmod 600") || strings.Contains(output, "Keychain") || strings.Contains(output, "release build") {
		t.Fatalf("setup --yes over an insecure file:\n%s", output)
	}
	must(t, os.Chmod(path, 0o600))

	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("uninstall: code=%d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "Keychain") {
		t.Errorf("uninstall names the Keychain on Linux:\n%s\n%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(home)
		t.Fatalf("data directory survived the purge (%v): %v", err, entries)
	}
}

// A credential uninstall cannot delete one by one is not a problem when the
// purge removes the whole credentials folder in the same run: the file is
// gone, and saying otherwise would send the user to delete what is not there.
// The rest of the purge still happens.
func TestUninstallPurgeDoesNotReportCredentialFilesThePurgeRemovesOnLinux(t *testing.T) {
	useCredentialOS(t, platform.Linux)
	fake := newFakeKeychain()
	home, _, env := installedFixture(t, fake, r2SetupInput(t.TempDir(), "linux-secret-value"))
	store, err := credentials.NewFileStore(credentials.FileStoreDir(home))
	must(t, err)
	must(t, store.Save(context.Background(), "setup-abc", credentials.R2Credentials{AccessKeyID: "k", SecretAccessKey: "linux-secret-value"}))
	env.Credentials = func() (credentials.CredentialStore, error) {
		return &failingDeleteKeychain{fakeKeychain: fake, deleteErr: fmt.Errorf("%w: /d/x.json is owned by another user", credentials.ErrInsecurePermissions)}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "could not be deleted") {
		t.Errorf("uninstall reports a credential the purge removed:\n%s\n%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("local files were not removed: %v", err)
	}
}

// A credentials folder that is a link is not followed: the purge removes the
// link, the files it pointed to are still there, and uninstall says so.
func TestUninstallPurgeReportsCredentialFilesBehindALinkOnLinux(t *testing.T) {
	useCredentialOS(t, platform.Linux)
	home, _, env := installedFixture(t, newFakeKeychain(), r2SetupInput(t.TempDir(), "linux-secret-value"))
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	must(t, os.Mkdir(elsewhere, 0o700))
	kept := filepath.Join(elsewhere, "setup-abc.json")
	must(t, os.WriteFile(kept, []byte(`{}`), 0o600))
	must(t, os.Symlink(elsewhere, credentials.FileStoreDir(home)))
	// The real file store, which refuses a linked folder.
	env.Credentials = func() (credentials.CredentialStore, error) {
		return credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return credentials.FileStoreDir(home), nil }, LookupEnv: noEnv})
	}
	cfg, _, _ := config.Load(home)
	cfg.RetiredCredentialRefs = append(cfg.RetiredCredentialRefs, "setup-abc")
	must(t, config.Save(home, cfg))

	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 1 {
		t.Fatalf("code=%d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	message := stderr.String()
	for _, want := range []string{"could not be deleted", "is a link to " + elsewhere, "delete the files in " + elsewhere} {
		if !strings.Contains(message, want) {
			t.Errorf("stderr must mention %q:\n%s", want, message)
		}
	}
	if strings.Contains(message, "security delete-generic-password") || strings.Contains(message, "Keychain") {
		t.Errorf("stderr names the Keychain's recovery on Linux:\n%s", message)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("uninstall followed the link and deleted %s: %v", kept, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("local files were not removed: %v", err)
	}
}

// The wording of a credential left behind depends on what the purge did to
// the credentials folder.
func TestUndeletedCredentialsProblemFollowsTheFolder(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	const dataDir = "/data/agent-archive"
	still := undeletedCredentialsProblem(platform.Linux, dataDir, []string{"r"}, cause, credentialFolder{remains: true})
	if want := "1 stored credential(s) could not be deleted from the credentials folder /data/agent-archive/credentials: cause. To remove them yourself, delete the files in that folder"; still != want {
		t.Errorf("folder still there = %q", still)
	}
	if gone := undeletedCredentialsProblem(platform.Linux, dataDir, []string{"r"}, cause, credentialFolder{}); gone != "" {
		t.Errorf("folder removed with the purge = %q, want nothing to report", gone)
	}
	linked := undeletedCredentialsProblem(platform.Linux, dataDir, []string{"r"}, cause, credentialFolder{isLink: true, linkTarget: "/mnt/creds"})
	if !strings.Contains(linked, "is a link to /mnt/creds") || !strings.Contains(linked, "delete the files in /mnt/creds") {
		t.Errorf("linked folder = %q", linked)
	}
	// The link is reported whether or not anything is left at its path.
	if got := undeletedCredentialsProblem(platform.Linux, dataDir, []string{"r"}, cause, credentialFolder{isLink: true, linkTarget: "/mnt/creds", remains: true}); got != linked {
		t.Errorf("linked folder that remains = %q", got)
	}
	// macOS is unaffected by the folder.
	if got := undeletedCredentialsProblem(platform.Darwin, dataDir, []string{"r"}, cause, credentialFolder{}); !strings.Contains(got, "security delete-generic-password") {
		t.Errorf("darwin = %q", got)
	}

	// lookCredentialFolder / afterPurge read the real folder.
	base := t.TempDir()
	if got := lookCredentialFolder(base); got.isLink || got.afterPurge(base).remains {
		t.Errorf("no folder: %+v", got)
	}
	must(t, os.Mkdir(credentials.FileStoreDir(base), 0o700))
	if got := lookCredentialFolder(base); got.isLink || !got.afterPurge(base).remains {
		t.Errorf("real folder: %+v", got)
	}
	linkBase := t.TempDir()
	target := t.TempDir()
	must(t, os.Symlink(target, credentials.FileStoreDir(linkBase)))
	if got := lookCredentialFolder(linkBase); !got.isLink || got.linkTarget != target {
		t.Errorf("linked folder: %+v", got)
	}
}

// The credentials folder is one of the entries uninstall removes, so a
// purge leaves nothing behind and does not report it as an unrelated file.
func TestPurgeRemovesTheCredentialsFolder(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "agent-archive")
	store, err := credentials.NewFileStore(credentials.FileStoreDir(home))
	must(t, err)
	must(t, store.Save(context.Background(), "setup-abc", credentials.R2Credentials{AccessKeyID: "k", SecretAccessKey: "s"}))
	leftover, err := removeLocalState(home)
	if err != nil || len(leftover) != 0 {
		t.Fatalf("leftover = %v, err = %v", leftover, err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("the data directory still holds %v", entries)
	}
}

// setup --yes and the environment store read the same two variables.
func TestEnvironmentVariableNamesAgreeWithTheCredentialsPackage(t *testing.T) {
	t.Parallel()
	if envR2AccessKeyID != credentials.EnvR2AccessKeyID || envR2SecretAccessKey != credentials.EnvR2SecretAccessKey {
		t.Fatalf("setup reads %s and %s, the environment store %s and %s", envR2AccessKeyID, envR2SecretAccessKey, credentials.EnvR2AccessKeyID, credentials.EnvR2SecretAccessKey)
	}
}

// The default opener is wired to the platform and, off macOS, to the data
// directory ($AGENT_ARCHIVE_HOME, as everything else resolves it).
func TestOpenCredentialStoreIsWiredToTheDataDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agent-archive")
	t.Setenv("AGENT_ARCHIVE_HOME", home)

	useCredentialOS(t, platform.Linux)
	store, err := realOpenCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	// The store the program opens is the undecorated one: setup's questions
	// about what is stored (credentials.LoadStored) reach the credentials file
	// only through it.
	if _, ok := store.(credentials.StoredLoader); !ok {
		t.Fatalf("the Linux store is %T, which does not implement credentials.StoredLoader", store)
	}
	must(t, store.Save(context.Background(), "setup-abc", credentials.R2Credentials{AccessKeyID: "k", SecretAccessKey: "s"}))
	if _, err = os.Stat(filepath.Join(home, "credentials", "setup-abc.json")); err != nil {
		t.Fatalf("the default store does not write under the data directory: %v", err)
	}
	// Opening the store did not need the environment's key, and reading it
	// through the fallback is what the environment names.
	t.Setenv(credentials.EnvR2AccessKeyID, "env-key")
	t.Setenv(credentials.EnvR2SecretAccessKey, "env-secret")
	if got, err := store.Load(context.Background(), "no-such-file"); err != nil || got.AccessKeyID != "env-key" {
		t.Fatalf("the environment fallback: %+v %v", got, err)
	}

	// On macOS it is the Keychain's store, or the unavailable error of a
	// build without one; it does not need a data directory.
	useCredentialOS(t, platform.Darwin)
	t.Setenv("AGENT_ARCHIVE_HOME", filepath.Join(t.TempDir(), "not", "created"))
	store, err = realOpenCredentialStore()
	if err != nil {
		if !errors.Is(err, credentials.ErrUnavailable) {
			t.Fatalf("darwin: %v", err)
		}
		return
	}
	if got := fmt.Sprintf("%T", store); got != "*credentials.KeychainStore" {
		t.Fatalf("darwin store is %s, want the Keychain store", got)
	}
}

// What a failure to open the store says, recorded in status.json and shown by
// sync, follows the platform.
func TestOpeningTheStoreForR2Failure(t *testing.T) {
	cfg := config.Config{Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2CredentialRef: "r"}}
	fail := func() (credentials.CredentialStore, error) { return nil, credentials.ErrUnavailable }
	for goos, want := range map[platform.OS]string{
		platform.Darwin:  "keychain unavailable: credential store unavailable",
		platform.Linux:   "open the credentials file: credential store unavailable",
		platform.Unknown: "open the credential store: credential store unavailable",
	} {
		useCredentialOS(t, goos)
		_, err := openConfiguredStore(cfg, fail)
		if err == nil || err.Error() != want || !errors.Is(err, credentials.ErrUnavailable) {
			t.Errorf("%s: %v, want %q", goos, err, want)
		}
	}
}

// The program's own credentialOS is the platform it runs on: the tests pin
// it to Darwin, so a hard-coded value would otherwise go unseen.
func TestCredentialOSIsTheRunningPlatform(t *testing.T) {
	t.Parallel()
	want := platform.Unknown
	switch runtime.GOOS {
	case "darwin":
		want = platform.Darwin
	case "linux":
		want = platform.Linux
	}
	if productionCredentialOS != want {
		t.Fatalf("credentialOS starts as %q, want %q for %s", productionCredentialOS, want, runtime.GOOS)
	}
}

// linuxFixture is a Linux setup environment whose credential store is the real
// one for Linux (the credentials file, with the environment as its fallback)
// and whose environment is vars: what the shell setup runs in exports. It
// sets credentialOS, so a test that calls it must not be parallel.
func linuxFixture(t *testing.T, vars map[string]string) (env Env, home, project string) {
	t.Helper()
	useCredentialOS(t, platform.Linux)
	home, project = t.TempDir(), t.TempDir()
	env = setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env = withEnvironment(env, vars)
	env.Credentials = func() (credentials.CredentialStore, error) {
		return credentials.OpenDefault(credentials.OpenOptions{
			OS:        platform.Linux,
			Dir:       func() (string, error) { return credentials.FileStoreDir(home), nil },
			LookupEnv: func(key string) (string, bool) { v, ok := vars[key]; return v, ok },
		})
	}
	return env, home, project
}

// collectorView is the store as the scheduled collector sees it: the
// credentials file, and none of the interactive shell's variables.
func collectorView(t *testing.T, home string) credentials.CredentialStore {
	t.Helper()
	store, err := credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return credentials.FileStoreDir(home), nil }, LookupEnv: noEnv})
	must(t, err)
	return store
}

var r2URL = "https://" + testR2Account + ".r2.cloudflarestorage.com/my-bucket"

// With the R2 variables exported and no credentials file, setup must not
// count the environment as a stored key: it asks for the key and saves it to
// the file, where the scheduled collector (which does not inherit the shell's
// environment) finds it. Interactive setup does not offer to keep it.
func TestInteractiveSetupOnLinuxDoesNotKeepAKeyThatIsOnlyInTheEnvironment(t *testing.T) {
	vars := map[string]string{envR2AccessKeyID: "ENVKEY", envR2SecretAccessKey: "env-secret-value"}
	env, home, project := linuxFixture(t, vars)
	setupYes(t, env, "", 0, "--yes", "--provider", "r2", "--r2-account", r2URL, "--project", project, "--apps", "claude")
	cfg, _, _ := config.Load(home)
	// setup --yes took the exported key and saved it to the file.
	if got, err := collectorView(t, home).Load(context.Background(), cfg.Storage.R2CredentialRef); err != nil || got.AccessKeyID != "ENVKEY" || got.SecretAccessKey != "env-secret-value" {
		t.Fatalf("the collector cannot read the key setup --yes was given: %+v %v", got, err)
	}

	// The file is lost; the shell still exports the variables.
	must(t, os.Remove(filepath.Join(credentials.FileStoreDir(home), cfg.Storage.R2CredentialRef+".json")))
	output := setupRun(t, env, "storage\nchange\nr2-existing\n"+testR2Account+"\nmy-bucket\nACCESS2\nnew-private-value\ny\n", 0)
	if strings.Contains(output, "Keep stored R2 credentials?") || !strings.Contains(output, "can't be read from the credentials file; enter them again.") {
		t.Fatalf("setup counted the environment's key as stored:\n%s", output)
	}
	if strings.Contains(output, "Keychain") {
		t.Errorf("Linux setup names the Keychain:\n%s", output)
	}
	current, _, _ := config.Load(home)
	if got, err := collectorView(t, home).Load(context.Background(), current.Storage.R2CredentialRef); err != nil || got.AccessKeyID != "ACCESS2" || got.SecretAccessKey != "new-private-value" {
		t.Fatalf("the typed key is not in the file the collector reads: %+v %v", got, err)
	}
}

// setup --yes with the variables exported saves the key to the file store
// (so the collector works afterwards), whether or not a configuration and a
// missing file already exist; and a rerun with no key given, which would keep
// the stored key, does not mistake the environment for it.
func TestSetupYesOnLinuxSavesTheExportedKeyToTheFileAndDoesNotKeepOnlyTheEnvironment(t *testing.T) {
	vars := map[string]string{envR2AccessKeyID: "ENVKEY", envR2SecretAccessKey: "env-secret-value"}
	env, home, project := linuxFixture(t, vars)
	setupYes(t, env, "", 0, "--yes", "--provider", "r2", "--r2-account", r2URL, "--project", project, "--apps", "claude")
	first, _, _ := config.Load(home)
	oldPath := filepath.Join(credentials.FileStoreDir(home), first.Storage.R2CredentialRef+".json")
	must(t, os.Remove(oldPath))

	// A rerun that gives no key: it would keep the stored one, and there is none.
	output := setupYes(t, env, "", 1, "--yes")
	if !strings.Contains(output, "the stored R2 key can't be read from the credentials file; pass --r2-access-key-id") || strings.Contains(output, "Keychain") {
		t.Fatalf("setup --yes kept a key that is only in the environment:\n%s", output)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Fatal("a refused run wrote the credentials file")
	}

	// With the storage flags it takes the exported key as a new one and saves it.
	setupYes(t, env, "", 0, "--yes", "--provider", "r2", "--r2-account", r2URL)
	next, _, _ := config.Load(home)
	if next.Storage.R2CredentialRef == "" || next.Storage.R2CredentialRef == first.Storage.R2CredentialRef {
		t.Fatalf("setup --yes did not stage a new key: %q", next.Storage.R2CredentialRef)
	}
	if got, err := collectorView(t, home).Load(context.Background(), next.Storage.R2CredentialRef); err != nil || got.AccessKeyID != "ENVKEY" || got.SecretAccessKey != "env-secret-value" {
		t.Fatalf("the collector cannot read the key setup --yes saved: %+v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(credentials.FileStoreDir(home), next.Storage.R2CredentialRef+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", info, err)
	}
}

// setup's check that the credential store opens counts a key saved in the
// file, not one in the environment.
func TestPreflightCredentialProbeOnLinuxReadsOnlyWhatIsStored(t *testing.T) {
	useCredentialOS(t, platform.Linux)
	vars := map[string]string{envR2AccessKeyID: "ENVKEY", envR2SecretAccessKey: "env-secret-value"}
	home := t.TempDir()
	reads := 0
	store, err := credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return credentials.FileStoreDir(home), nil }, LookupEnv: func(key string) (string, bool) {
		reads++
		v, ok := vars[key]
		return v, ok
	}})
	must(t, err)
	check := keychainCheck(credentialsOnly{func() (credentials.CredentialStore, error) { return store, nil }}, "setup-0123456789abcdef0123456789abcdef")
	if !check.OK {
		t.Fatalf("check: %+v", check)
	}
	if reads != 0 {
		t.Errorf("the credential check read the environment %d times", reads)
	}
}

// Interactive setup that skips the storage questions (it only edits the apps)
// still confirms the saved key is stored before it commits: with the file
// gone and the variables exported, it refuses rather than commit a
// configuration whose collector could not read its key.
func TestSetupThatSkipsStorageRefusesAConfigWhoseKeyFileIsGoneOnLinux(t *testing.T) {
	vars := map[string]string{envR2AccessKeyID: "ENVKEY", envR2SecretAccessKey: "env-secret-value"}
	env, home, project := linuxFixture(t, vars)
	setupYes(t, env, "", 0, "--yes", "--provider", "r2", "--r2-account", r2URL, "--project", project, "--apps", "claude")
	before, _, _ := config.Load(home)
	must(t, os.Remove(filepath.Join(credentials.FileStoreDir(home), before.Storage.R2CredentialRef+".json")))

	// Edit the captured apps only: no storage question is asked.
	output := setupRun(t, env, "capture\ny\ny\nn\nn\nn\nincluded-projects\ny\n\ny\n", 1)
	if !strings.Contains(output, "stored R2 credential is unavailable; enter it again") {
		t.Fatalf("setup committed, or refused for another reason:\n%s", output)
	}
	if strings.Contains(output, "Keep stored R2 credentials?") {
		t.Errorf("setup offered to keep a key that is only in the environment:\n%s", output)
	}
	// The active configuration is untouched.
	after, _, _ := config.Load(home)
	if after.Storage.R2CredentialRef != before.Storage.R2CredentialRef {
		t.Fatalf("the active configuration changed: %q", after.Storage.R2CredentialRef)
	}
}

// The runtime paths keep the environment fallback: with no file and the
// variables set, the store the program opens on Linux loads an R2 key, so a
// container configured only by its environment can capture.
func TestOpenedStoreOnLinuxServesTheRuntimeFromTheEnvironment(t *testing.T) {
	useCredentialOS(t, platform.Linux)
	store, err := credentials.OpenDefault(credentials.OpenOptions{
		OS:  platform.Linux,
		Dir: func() (string, error) { return credentials.FileStoreDir(t.TempDir()), nil },
		LookupEnv: func(key string) (string, bool) {
			v, ok := map[string]string{envR2AccessKeyID: "ENVKEY", envR2SecretAccessKey: "env-secret-value"}[key]
			return v, ok
		},
	})
	must(t, err)
	cfg := config.Config{Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2CredentialRef: "setup-0123456789abcdef0123456789abcdef", R2AccountID: testR2Account}}
	if _, err = openConfiguredStore(cfg, func() (credentials.CredentialStore, error) { return store, nil }); err != nil {
		t.Fatalf("the runtime cannot open R2 storage from the environment's key: %v", err)
	}
}

// A relative link's target is reported where it points, from the folder that
// holds the link, not as written.
func TestLookCredentialFolderResolvesARelativeLink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	must(t, os.Symlink(filepath.Join("..", "shared", "creds"), credentials.FileStoreDir(base)))
	got := lookCredentialFolder(base)
	if want := filepath.Join(filepath.Dir(base), "shared", "creds"); !got.isLink || got.linkTarget != want {
		t.Fatalf("relative link = %+v, want target %s", got, want)
	}
	if msg := undeletedCredentialsProblem(platform.Linux, base, []string{"r"}, errors.New("cause"), got); !strings.Contains(msg, "is a link to "+got.linkTarget) || strings.Contains(msg, "..") {
		t.Errorf("message = %q", msg)
	}
	// An absolute link is reported as it is.
	abs := t.TempDir()
	other := t.TempDir()
	must(t, os.Symlink(other, credentials.FileStoreDir(abs)))
	if got := lookCredentialFolder(abs); got.linkTarget != other {
		t.Errorf("absolute link = %+v", got)
	}
}
