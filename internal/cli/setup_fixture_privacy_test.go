package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestSetupFixturePrivacyChangesOnlyExistingArchiveDirectory(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home := filepath.Join(parent, "archive")
	child := filepath.Join(home, "child")
	userHome := t.TempDir()
	for _, path := range []string{home, child} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{parent, home, child, userHome} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	for _, path := range []string{parent, home, child, userHome} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o755)
		if path == home {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s: mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestSetupFixturePrivacyLeavesAbsentAndLinkedHomesUntouched(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	absent := filepath.Join(parent, "absent")
	setupTestEnv(t, absent, t.TempDir(), newFakeKeychain(), time.Now())
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Fatalf("absent home changed: %v", err)
	}
	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	setupTestEnv(t, link, t.TempDir(), newFakeKeychain(), time.Now())
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: %v", err)
	}
	info, err = os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("link target changed: %v", err)
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	setupTestEnv(t, file, t.TempDir(), newFakeKeychain(), time.Now())
	info, err = os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("regular file changed: %v", err)
	}
}

func TestSetupFixturePrivacyDoesNotRepairUnsafeDurableCalls(t *testing.T) {
	t.Parallel()
	for _, useSetup := range []bool{false, true} {
		home := t.TempDir()
		if err := os.Chmod(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		if useSetup {
			setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
		}
		if err := os.Chmod(home, 0o755); err != nil {
			t.Fatal(err)
		}
		if !useSetup {
			testEnv(t, home, time.Now())
		}
		before, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		entered := false
		err = config.WithDurableStorage(home, func(config.DurableStorageGuard) error { entered = true; return nil })
		if err == nil || entered {
			t.Fatalf("unsafe home gained authority: setup=%v entered=%v err=%v", useSetup, entered, err)
		}
		if _, err := os.Lstat(filepath.Join(home, "hooks.lock")); !os.IsNotExist(err) {
			t.Fatalf("unsafe home allocated lock: %v", err)
		}
		after, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil || string(before) != string(after) {
			t.Fatalf("unsafe operation changed config: %v", err)
		}
		info, err := os.Stat(home)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("production repaired unsafe home: %v", err)
		}
	}
}
