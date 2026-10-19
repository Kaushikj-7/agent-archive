package gitremote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// IdentityObserver scopes optional command capability results to one observation pass.
// A change of executable or effective environment discards the cached capability.
type IdentityObserver struct {
	// Run and ConfigRun replace bounded executors in synthetic capability tests.
	Run       Runner
	ConfigRun Runner
	scope     string
	legacy    bool
	probed    bool
	budget    bool
}

// Lookup observes one root; observers are used serially by a recovery sweep.
func (o *IdentityObserver) Lookup(ctx context.Context, root string) sourcefacts.RepositoryIdentity {
	scope, ok := identityObservationScope()
	if !ok {
		return sourcefacts.RepositoryIdentity{}
	}
	if o.scope != scope {
		*o = IdentityObserver{scope: scope, Run: o.Run, ConfigRun: o.ConfigRun}
	}
	o.budget = false
	top := ProjectRoot(ctx, root, o.shortRunner())
	if o.budget {
		return sourcefacts.RepositoryIdentity{BudgetExhausted: true}
	}
	if top != "" {
		return o.checkoutIdentity(ctx, root, top, scope)
	}
	if ctx.Err() != nil {
		return sourcefacts.RepositoryIdentity{}
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return sourcefacts.RepositoryIdentity{}
	}
	// Bare or damaged repository metadata cannot be discarded as scratch.
	if _, headErr := os.Lstat(filepath.Join(root, "HEAD")); headErr == nil {
		if _, objectsErr := os.Lstat(filepath.Join(root, "objects")); objectsErr == nil {
			return sourcefacts.RepositoryIdentity{}
		}
	}
	var dependencies []sourcefacts.RepositoryDependency
	for p, depth := filepath.Clean(root), 0; depth < 64; depth++ {
		_, err := os.Lstat(filepath.Join(p, ".git"))
		if err == nil || !os.IsNotExist(err) {
			return sourcefacts.RepositoryIdentity{}
		}
		stamp, ok := repositoryStamp(filepath.Join(p, ".git"))
		if !ok {
			return sourcefacts.RepositoryIdentity{}
		}
		dependencies = append(dependencies, sourcefacts.RepositoryDependency{Path: filepath.Join(p, ".git"), Stamp: stamp})
		parent := filepath.Dir(p)
		if parent == p {
			stamp, ok := repositoryStamp(root)
			dependencies = append(dependencies, sourcefacts.RepositoryDependency{Path: root, Stamp: stamp})
			return sourcefacts.RepositoryIdentity{Known: ok, Dependencies: dependencies, ObservationScope: scope}
		}
		p = parent
	}
	return sourcefacts.RepositoryIdentity{BudgetExhausted: true}
}

// ProjectIdentityCurrent validates enumerated metadata without spawning Git.
// Semantic identities also require the recovery resolver's second full sweep.
func ProjectIdentityCurrent(id sourcefacts.RepositoryIdentity) bool {
	if !id.Known || len(id.Dependencies) == 0 {
		return false
	}
	scope, ok := identityObservationScope()
	if !ok || id.ObservationScope != scope {
		return false
	}
	for _, dep := range id.Dependencies {
		stamp, ok := repositoryStamp(dep.Path)
		if !ok || stamp != dep.Stamp {
			return false
		}
	}
	return true
}

// Bind retained root evidence as well as capabilities to the effective observer.
// Only a digest enters temporary inventory state, never environment values.
func identityObservationScope() (string, bool) {
	executable, err := realLocator.find()
	if err != nil {
		return "", false
	}
	stamp, ok := repositoryStamp(executable)
	if !ok {
		return "", false
	}
	digest := sha256.Sum256([]byte("local-origin-chain-v1\x00" + stamp + "\x00" + executable + "\x00" + strings.Join(environment(os.Environ()), "\x00")))
	return hex.EncodeToString(digest[:]), true
}

func repositoryStamp(path string) (string, bool) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, locatorErr := os.Lstat(path); !errors.Is(locatorErr, os.ErrNotExist) {
			return "", false
		}
		digest := sha256.Sum256([]byte("missing:" + path))
		return hex.EncodeToString(digest[:]), true
	}
	if err != nil {
		return "", false
	}
	// Same inode, size, mode and modification time identify the observed metadata.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	raw := fmt.Sprintf("%s:%d:%d:%d:%d:%d", path, info.Size(), info.Mode(), info.ModTime().UnixNano(), stat.Dev, stat.Ino)
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:]), true
}

func (o *IdentityObserver) projectDependencies(ctx context.Context, root, top string) ([]sourcefacts.RepositoryDependency, bool, sourcefacts.RecoveryOutcome) {
	bounded, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	raw, err := o.configRunner()(bounded, root, "-C", root, "config", "--show-origin", "--name-only", "-z", "--list")
	if err != nil || bounded.Err() != nil {
		return nil, false, sourcefacts.RecoveryOriginUnreadable
	}
	parts := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(parts)%2 != 0 {
		return nil, false, sourcefacts.RecoveryOriginUnreadable
	}
	paths := map[string]bool{root: true, filepath.Join(root, ".git"): true}
	if top != "" && !o.checkoutAncestorDependencies(root, top, paths) {
		return nil, false, sourcefacts.RecoveryOriginUnreadable
	}
	if !o.probed {
		var unsupported bool
		run := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			raw, err := o.shortRunner()(ctx, dir, args...)
			var status interface{ ExitCode() int }
			unsupported = unsupported || (errors.As(err, &status) && status.ExitCode() == 129 && len(raw) == 0)
			return raw, err
		}
		if !topLevelConfigDependencies(bounded, root, paths, run) {
			if !unsupported || bounded.Err() != nil {
				return nil, false, sourcefacts.RecoveryOriginUnreadable
			}
			o.legacy = true
		}
		o.probed = true
	} else if !o.legacy && !topLevelConfigDependencies(bounded, root, paths, o.shortRunner()) {
		return nil, false, sourcefacts.RecoveryOriginUnreadable
	}
	if !o.legacy {
		// HEAD controls onbranch includes; the worktree/common metadata controls
		// which config Git reads. Ask Git for paths rather than interpreting it.
		metadata, err := o.shortRunner()(bounded, root, "-C", root, "rev-parse", "--path-format=absolute", "--git-path", "HEAD", "--git-path", "config", "--git-path", "config.worktree", "--git-path", "commondir")
		if err != nil || bounded.Err() != nil {
			return nil, false, sourcefacts.RecoveryOriginUnreadable
		}
		names := strings.Split(strings.TrimSuffix(string(metadata), "\n"), "\n")
		if len(names) != 4 {
			return nil, false, sourcefacts.RecoveryOriginUnreadable
		}
		for _, path := range names {
			if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
				return nil, false, sourcefacts.RecoveryOriginUnreadable
			}
			paths[path] = true
		}
	}
	if !includeDependencies(bounded, root, paths, o.configRunner()) {
		return nil, false, sourcefacts.RecoveryOriginUnreadable
	}
	for i := 0; i < len(parts); i += 2 {
		path, ok := strings.CutPrefix(parts[i], "file:")
		if !ok {
			return nil, false, sourcefacts.RecoveryOriginUnreadable
		}
		if !filepath.IsAbs(path) {
			path = joinRepositoryPath(root, path)
		}
		paths[path] = true
	}
	if len(paths) > maxIdentityDependencies {
		return nil, false, sourcefacts.RecoveryOriginLimit
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	var dependencies []sourcefacts.RepositoryDependency
	for _, path := range ordered {
		stamp, ok := repositoryStamp(path)
		if !ok {
			return nil, false, sourcefacts.RecoveryOriginUnreadable
		}
		dependencies = append(dependencies, sourcefacts.RepositoryDependency{Path: path, Stamp: stamp})
	}
	return dependencies, o.legacy, ""
}

func includeDependencies(ctx context.Context, root string, paths map[string]bool, run Runner) bool {
	// An empty or absent include contributes no origin-name entry. Git's
	// typed path query expands home/prefix syntax; relative values use the
	// containing config's directory, as Git does.
	includes, err := run(ctx, root, "-C", root, "config", "--show-origin", "--type=path", "-z", "--get-regexp", `^(include|includeif\..*)\.path$`)
	var status interface{ ExitCode() int }
	absent := errors.As(err, &status) && status.ExitCode() == 1 && len(includes) == 0
	if (err != nil && !absent) || ctx.Err() != nil {
		return false
	}
	if len(includes) != 0 {
		entries := strings.Split(strings.TrimSuffix(string(includes), "\x00"), "\x00")
		if len(entries)%2 != 0 {
			return false
		}
		for i := 0; i < len(entries); i += 2 {
			origin, ok := strings.CutPrefix(entries[i], "file:")
			_, path, valueOK := strings.Cut(entries[i+1], "\n")
			if !ok || !valueOK || path == "" || strings.ContainsAny(path, "\x00\r\n") {
				return false
			}
			if !filepath.IsAbs(origin) {
				origin = joinRepositoryPath(root, origin)
			}
			if !filepath.IsAbs(path) {
				// filepath.Dir cleans too; preserve a symlink/.. spelling in
				// the containing configuration directory before joining.
				directory := origin[:strings.LastIndexByte(origin, filepath.Separator)+1]
				path = joinRepositoryPath(directory, path)
			}
			paths[path] = true
		}
	}
	return true
}

// Git reports all candidate top-level config paths, including absent files.
// Origins from --list alone omit absent/empty global and system configuration.
func topLevelConfigDependencies(ctx context.Context, root string, paths map[string]bool, run Runner) bool {
	for _, variable := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		raw, err := run(ctx, root, "-C", root, "var", variable)
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			var status interface{ ExitCode() int }
			// Git documents exit 1 for a recognized variable with no value;
			// unsupported variable queries are usage errors, not known absence.
			if errors.As(err, &status) && status.ExitCode() == 1 && len(raw) == 0 {
				continue
			}
			return false
		}
		for path := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
				return false
			}
			paths[path] = true
		}
	}
	return true
}

// A configured subtree inherits Git identity only while no nearer checkout
// marker appears. Stamp every candidate through the observed top level so a
// new nested repository invalidates the inventory before it can prove uniqueness.
func (o *IdentityObserver) checkoutAncestorDependencies(root, top string, paths map[string]bool) bool {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	for path, depth := canonical, 0; depth < 64; depth++ {
		paths[filepath.Join(path, ".git")] = true
		if path == top {
			return true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
	o.budget = true
	return false
}

func (o *IdentityObserver) shortRunner() Runner {
	run := o.Run
	if run == nil {
		run = ExecRunner
	}
	return o.budgetRunner(run)
}

func (o *IdentityObserver) configRunner() Runner {
	run := o.ConfigRun
	if run == nil {
		run = ConfigRunner
	}
	return o.budgetRunner(run)
}

func (o *IdentityObserver) budgetRunner(run Runner) Runner {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		raw, err := run(ctx, dir, args...)
		if errors.Is(err, ErrOutputLimit) || ctx.Err() != nil {
			o.budget = true
		}
		return raw, err
	}
}
