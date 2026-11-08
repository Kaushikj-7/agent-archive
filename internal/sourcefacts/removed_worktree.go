package sourcefacts

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// RemovedWorktreeMethod names a project resolution proved for a session whose
// recorded working directory, a removed worktree, lies inside its project.
const RemovedWorktreeMethod = "removed_worktree"

// RemovedWorktreeProject proves which configured project a session's removed
// worktree belonged to, from the working directory the session recorded. The
// proof holds only when all of these are true:
//   - the recorded directory is absolute and no longer exists;
//   - the nearest existing directory above it that holds .git holds it as a
//     directory, so it is a repository's own root, not another worktree;
//   - that repository resolves physically to itself; and
//   - it is exactly the root of the configured project that owns the recorded
//     directory, which is included, with no other configured project between.
//
// Anything else is not proof, and the session stays unresolved. The returned
// resolution records the original directory, so the archive keeps that the
// worktree is gone.
func RemovedWorktreeProject(projects []archive.ProjectActivation, cwd string) (archive.ProjectResolution, bool) {
	if !filepath.IsAbs(cwd) {
		return archive.ProjectResolution{}, false
	}
	cwd = filepath.Clean(cwd)
	if _, err := os.Lstat(cwd); !errors.Is(err, os.ErrNotExist) {
		return archive.ProjectResolution{}, false
	}
	repo := ""
	for p := filepath.Dir(cwd); repo == ""; p = filepath.Dir(p) {
		info, err := os.Lstat(filepath.Join(p, ".git"))
		switch {
		case err == nil && info.IsDir():
			repo = p
		case err == nil || !errors.Is(err, os.ErrNotExist) || filepath.Dir(p) == p:
			return archive.ProjectResolution{}, false
		}
	}
	physical, known := PhysicalProject(repo)
	if !known || physical.Root != canonicalExisting(repo) {
		return archive.ProjectResolution{}, false
	}
	canonical := filepath.Join(physical.Root, cwd[len(repo):])
	owner, owned := ConfiguredOwner(projects, canonical, canonicalExisting)
	if !owned || !owner.Included || canonicalExisting(owner.Root) != physical.Root {
		return archive.ProjectResolution{}, false
	}
	policy := RecoveryContext(projects, nil, filepath.Clean)
	return archive.ProjectResolution{OriginalCwd: cwd, CanonicalCwd: canonical, Root: owner.Root, Method: RemovedWorktreeMethod, Context: policy, PolicyContext: policy}, true
}

// canonicalExisting resolves a path's symlinks, or returns it cleaned when it
// cannot be resolved.
func canonicalExisting(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}
