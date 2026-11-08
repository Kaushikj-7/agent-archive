package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// withRemovedWorktreeSource moves a native fixture's recorded working
// directory into a worktree of its project that no longer exists.
func withRemovedWorktreeSource(t *testing.T, f nativeAdmissionFixture) (nativeAdmissionFixture, string) {
	t.Helper()
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(f.candidate.ProjectRoot, ".claude", "worktrees", "removed-task")
	if err := os.MkdirAll(filepath.Dir(removed), 0o700); err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(f.candidate.ProjectRoot), []byte(removed))
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f, removed
}

func TestNativeImportFilesARemovedWorktreeUnderItsProvedProject(t *testing.T) {
	t.Parallel()
	native, removed := withRemovedWorktreeSource(t, newNativeAdmissionFixture(t))
	f := nativeImportPrepareFixtureFromNative(t, native, "", 8<<20)
	cfg, _, err := config.Load(f.native.store.home)
	if err != nil {
		t.Fatal(err)
	}
	proof, proved := sourcefacts.RemovedWorktreeProject(cfg.Archive.Projects, removed)
	if !proved {
		t.Fatal("synthetic removed worktree not proved")
	}
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner, err := f.native.store.CommitNativeImport(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if owner.ProjectRoot != f.candidate.ProjectRoot || owner.ProjectResolution == nil || *owner.ProjectResolution != proof || owner.NativeBinding.Cwd != removed {
		t.Fatalf("owner project = %q resolution = %+v", owner.ProjectRoot, owner.ProjectResolution)
	}
}

func TestNativeImportRefusesARemovedWorktreeOutsideItsConfirmedProject(t *testing.T) {
	t.Parallel()
	native, _ := withRemovedWorktreeSource(t, newNativeAdmissionFixture(t))
	f := nativeImportPrepareFixtureFromNative(t, native, "", 8<<20)
	other := f.candidate
	other.ProjectRoot = filepath.Join(filepath.Dir(f.candidate.ProjectRoot), "elsewhere")
	g := f
	g.candidate = other
	if p, err := g.prepare(t.Context()); err == nil {
		_ = p.Close() // Already failed the assertion; the close result adds nothing.
		t.Fatal("removed worktree imported under another project")
	}
	// A project no longer included proves nothing.
	cfg, _, err := config.Load(f.native.store.home)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Archive.Projects {
		cfg.Archive.Projects[i].Included = false
	}
	if err := config.Save(f.native.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	if p, err := f.prepare(t.Context()); err == nil {
		_ = p.Close() // Already failed the assertion; the close result adds nothing.
		t.Fatal("removed worktree imported under an excluded project")
	}
}
