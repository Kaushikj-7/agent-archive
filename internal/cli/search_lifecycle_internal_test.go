package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

func TestSetupSearchConsentIsLocalExplicitAndOmissionPreservesChoice(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "test", "--region", "us-east-1", "--apps", "claude", "--project", project)
	cfg, _, err := config.Load(home)
	if err != nil || cfg.SearchIndexing || cfg.SearchLifecycleProtection {
		t.Fatal(cfg, err)
	}
	searchRef := env.installation(home, userHome).searchRole().ref()
	if env.jobDefinition(userHome, searchRef).Defined {
		t.Fatal("fresh setup installed search without consent")
	}
	setupYes(t, env, "", 0, "--yes", "--search-indexing", "on")
	cfg, _, err = config.Load(home)
	if err != nil || !cfg.SearchIndexing || !cfg.SearchLifecycleProtection || cfg.SchemaVersion != 16 {
		t.Fatal(cfg, err)
	}
	definition := env.jobDefinition(userHome, searchRef)
	if !definition.Defined || !scheduler.JobState(fakeSched(env).state(searchRef)).Active() {
		t.Fatal("search role missing after explicit opt-in", definition)
	}
	setupYes(t, env, "", 0, "--yes")
	cfg, _, err = config.Load(home)
	if err != nil || !cfg.SearchIndexing {
		t.Fatal("omission lost consent", err)
	}
	setupYes(t, env, "", 0, "--yes", "--search-indexing", "off")
	cfg, _, err = config.Load(home)
	if err != nil || cfg.SearchIndexing || !cfg.SearchLifecycleProtection {
		t.Fatal(cfg, err)
	}
	if env.jobDefinition(userHome, searchRef).Defined || fakeSched(env).state(searchRef) != "missing" {
		t.Fatal("disabled role remains installed")
	}
	if err := filepath.WalkDir(home, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "content.sqlite" {
			t.Error("setup created content cache", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPauseRefusesBeforeSavingWhenSearchPassHoldsFence(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now()
	setUpTestConfig(t, home, "/synthetic/project", at)
	env := testEnv(t, home, at)
	release, err := local.NamedLock(home, "search-worker.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var out, diagnostics bytes.Buffer
	if code := runPauseCommand(&out, &diagnostics, env, true); code != 1 || !strings.Contains(diagnostics.String(), "search indexing") {
		t.Fatal(code, out.String(), diagnostics.String())
	}
	cfg, _, err := config.Load(home)
	if err != nil || cfg.Paused {
		t.Fatal("busy pause changed configuration", err)
	}
}

func TestUninstallPreflightsSearchBeforeStoppingCollector(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	in := env.installation(home, userHome)
	collector, search := in.ref(), in.searchRole().ref()
	fakeSched(env).set(collector, "running").set(search, "unknown")
	var out bytes.Buffer
	if _, _, err := stopCollectors([]scheduler.Ref{collector, search}, &out, userHome, env, false); err == nil {
		t.Fatal("unknown search did not refuse uninstall")
	}
	for _, call := range fakeSched(env).all() {
		if strings.HasPrefix(call, "unload ") {
			t.Fatal("stopped before all role states known", call)
		}
	}
}

func TestPurgePreservesHeldSearchFenceUntilFinalLockRemoval(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	release, err := local.NamedLock(home, "search-worker.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before, err := os.Stat(filepath.Join(home, "search-worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "search-error.log"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if left, err := removeLocalState(home); err != nil || len(left) != 0 {
		t.Fatal(left, err)
	}
	after, err := os.Stat(filepath.Join(home, "search-worker.lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("purge unlinked held search inode", err)
	}
	if _, err = os.Stat(filepath.Join(home, "search-error.log")); !os.IsNotExist(err) {
		t.Fatal("purge left search log", err)
	}
	if !slices.Contains(uninstallLockFiles, "search-worker.lock") {
		t.Fatal("final lock unlink list omitted search fence")
	}
}
