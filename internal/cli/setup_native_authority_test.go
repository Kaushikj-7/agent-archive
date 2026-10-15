package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

func TestSetupRollbackPreservesNativeAuthorityFenceBeforeFirstRoot(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg := mustLoadConfig(t, home)
	next := cfg
	next.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
	next.NoSkills = !cfg.NoSkills
	sched := recordLaunchd(t, &env, "loaded")
	sched.beforeLoad = func(scheduler.Ref) error { return errors.New("synthetic restart failure") }
	if err := applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env); err == nil {
		t.Fatal("setup did not roll back")
	}
	loaded := mustLoadConfig(t, home)
	if !loaded.NativeAuthorityProtection || len(loaded.NativeRoots) != 0 {
		t.Fatal("rollback lost sticky fence or committed unapproved root")
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	must(t, err)
	if !bytes.Contains(raw, []byte("native-authority-v16")) {
		t.Fatal("rollback restored an older native authority writer")
	}
}

func TestSetupRollbackPreservesNativeAndSearchFencesWithoutGrantingConsent(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg := mustLoadConfig(t, home)
	next := cfg
	next.NativeRoots = []archive.NativeRoot{{Agent: "synthetic", Configured: "/native", Canonical: "/native", Revision: "one"}}
	must(t, config.SetSearchIndexing(&next, true))
	next.NoSkills = !cfg.NoSkills
	sched := recordLaunchd(t, &env, "loaded")
	searchRef := env.installation(home, userHome).searchRole().ref()
	sched.set(searchRef, "missing")
	failed := false
	sched.beforeLoad = func(scheduler.Ref) error {
		if !failed {
			failed = true
			return errors.New("synthetic combined restart failure")
		}
		return nil
	}
	if err := applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env); err == nil {
		t.Fatal("setup did not roll back")
	}
	loaded := mustLoadConfig(t, home)
	if !failed || !loaded.NativeAuthorityProtection || !loaded.SearchLifecycleProtection || loaded.SearchIndexing || len(loaded.NativeRoots) != 0 {
		t.Fatal("rollback lost fences or granted new consent", loaded)
	}
	if env.jobDefinition(userHome, searchRef).Defined || sched.state(searchRef) != "missing" {
		t.Fatal("rollback left a new search job")
	}
}
