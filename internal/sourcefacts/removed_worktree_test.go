package sourcefacts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// removedWorktreeRepo makes a repository with a configured, included project
// at its root, and returns the root and a removed worktree path inside it.
func removedWorktreeRepo(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude", "worktrees"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, ".claude", "worktrees", "removed-task")
}

func included(roots ...string) []archive.ProjectActivation {
	out := make([]archive.ProjectActivation, 0, len(roots))
	for _, r := range roots {
		out = append(out, archive.ProjectActivation{Root: r, ProjectID: archive.ProjectID(r), Included: true})
	}
	return out
}

func TestRemovedWorktreeIsProvedForItsConfiguredRepository(t *testing.T) {
	t.Parallel()
	root, cwd := removedWorktreeRepo(t)
	projects := included(root)
	proof, ok := RemovedWorktreeProject(projects, cwd)
	if !ok || proof.Root != root || proof.OriginalCwd != cwd || proof.CanonicalCwd != cwd || proof.Method != RemovedWorktreeMethod || proof.PolicyContext != RecoveryContext(projects, nil, filepath.Clean) {
		t.Fatalf("proof = %+v, %v", proof, ok)
	}
	// A configured spelling through a symlink keeps its spelling.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if proof, ok := RemovedWorktreeProject(included(link), filepath.Join(link, ".claude", "worktrees", "removed-task")); !ok || proof.Root != link || proof.CanonicalCwd != cwd {
		t.Fatalf("symlinked proof = %+v, %v", proof, ok)
	}
}

func TestRemovedWorktreeIsNotProvedWithoutTheWholeProof(t *testing.T) {
	t.Parallel()
	root, cwd := removedWorktreeRepo(t)
	existing := filepath.Join(root, "present")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	// A linked worktree or submodule keeps .git as a file; it is not the
	// configured repository's own root.
	linked := filepath.Join(root, "linked")
	if err := os.Mkdir(linked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(root, "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	other, _ := removedWorktreeRepo(t)
	excluded := included(root)
	excluded[0].Included = false
	nested := append(included(root), archive.ProjectActivation{Root: inner, ProjectID: archive.ProjectID(inner), Included: true})
	plain, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		projects []archive.ProjectActivation
		cwd      string
	}{
		"relative directory":        {included(root), "worktrees/removed-task"},
		"directory still exists":    {included(root), existing},
		"not configured":            {included(other), cwd},
		"excluded project":          {excluded, cwd},
		"inside a linked checkout":  {included(root), filepath.Join(linked, "removed-task")},
		"nested configured project": {nested, filepath.Join(inner, "removed-task")},
		"configured below the repo": {included(inner), filepath.Join(inner, "removed-task")},
		"no repository above":       {included(plain), filepath.Join(plain, "removed-task")},
	} {
		if proof, ok := RemovedWorktreeProject(c.projects, c.cwd); ok {
			t.Errorf("%s: proved %+v", name, proof)
		}
	}
}
