package gitremote

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// These bounds cover the complete observation, including repeated branches.
// Exhaustion is uncertainty, never a proof that another clone is unrelated.
const (
	maxOriginRepositories   = 16
	maxIdentityDependencies = 128
)

type originChain struct {
	observer *IdentityObserver
	active   map[string]bool
	keys     map[string]string
	paths    map[string]string
	semantic bool
	nodes    int
}

func (o *IdentityObserver) checkoutIdentity(ctx context.Context, root, top, scope string) sourcefacts.RepositoryIdentity {
	chain := &originChain{observer: o, active: map[string]bool{}, keys: map[string]string{}, paths: map[string]string{}}
	key, failure := chain.observe(ctx, root, top, false)
	if failure != "" {
		return sourcefacts.RepositoryIdentity{Root: top, ObservationScope: scope, Failure: failure, BudgetExhausted: o.budget}
	}
	var validation string
	if chain.semantic {
		validation = "semantic"
	}
	dependencies := make([]sourcefacts.RepositoryDependency, 0, len(chain.paths))
	for path, stamp := range chain.paths {
		dependencies = append(dependencies, sourcefacts.RepositoryDependency{Path: path, Stamp: stamp})
	}
	sort.Slice(dependencies, func(i, j int) bool { return dependencies[i].Path < dependencies[j].Path })
	id := sourcefacts.RepositoryIdentity{Root: top, Key: key, Known: true, ObservationScope: scope, ObservedRoot: root, Validation: validation, Dependencies: dependencies}
	if !ProjectIdentityCurrent(id) {
		return sourcefacts.RepositoryIdentity{Failure: sourcefacts.RecoveryOriginChanged}
	}
	return id
}

func (c *originChain) observe(ctx context.Context, root, top string, terminalRequired bool) (string, sourcefacts.RecoveryOutcome) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", originPathFailure(err)
	}
	if c.active[canonical] {
		return "", sourcefacts.RecoveryOriginCycle
	}
	if failure := c.addPath(root); failure != "" {
		return "", failure
	}
	// Retain the original spelling as an alias dependency, then read metadata
	// from its resolved directory. Lexical cleaning before symlink traversal
	// could select a different repository across a symlink followed by .. .
	alias := root
	root = canonical
	if key, ok := c.keys[canonical]; ok {
		if resolved, err := filepath.EvalSymlinks(alias); err != nil || resolved != canonical {
			return "", sourcefacts.RecoveryOriginChanged
		}
		return key, ""
	}
	if c.nodes >= maxOriginRepositories {
		return "", sourcefacts.RecoveryOriginLimit
	}
	c.nodes++
	c.active[canonical] = true
	defer delete(c.active, canonical)
	values, raw, failure := c.observer.recoveryOrigins(ctx, root)
	if failure != "" {
		return "", failure
	}
	deps, semantic, failure := c.observer.projectDependencies(ctx, root, top)
	if failure != "" {
		return "", failure
	}
	c.semantic = c.semantic || semantic
	for _, dep := range deps {
		if old, exists := c.paths[dep.Path]; exists && old != dep.Stamp {
			return "", sourcefacts.RecoveryOriginChanged
		}
		c.paths[dep.Path] = dep.Stamp
	}
	if len(c.paths) > maxIdentityDependencies {
		return "", sourcefacts.RecoveryOriginLimit
	}
	key, failure := c.resolveValues(ctx, root, top, values)
	if failure != "" {
		return "", failure
	}
	if terminalRequired && key == "" {
		return "", sourcefacts.RecoveryOriginUnresolved
	}
	_, verified, failure := c.observer.recoveryOrigins(ctx, root)
	resolved, aliasErr := filepath.EvalSymlinks(alias)
	if failure != "" || !bytes.Equal(raw, verified) || aliasErr != nil || resolved != canonical || (top != "" && ProjectRoot(ctx, root, c.observer.shortRunner()) != top) {
		return "", sourcefacts.RecoveryOriginChanged
	}
	c.keys[canonical] = key
	return key, ""
}

func (c *originChain) resolveValues(ctx context.Context, root, top string, values []string) (string, sourcefacts.RecoveryOutcome) {
	var key string
	for _, value := range values {
		if value == "" {
			continue
		}
		if !strings.Contains(value, "://") && strings.Contains(value, "::") && !filesystemOriginSpelling(value) {
			return "", sourcefacts.RecoveryOriginUnsupported
		}
		candidate := archive.RepoKey(value)
		if candidate == "" {
			base := top
			if base == "" {
				base = root
			}
			path, ok := localOriginPath(base, value)
			if !ok {
				return "", sourcefacts.RecoveryOriginUnsupported
			}
			target, targetTop, failure := c.observer.originTarget(ctx, path)
			if failure != "" {
				return "", failure
			}
			candidate, failure = c.observe(ctx, target, targetTop, true)
			if failure != "" {
				return "", failure
			}
		}
		if key != "" && key != candidate {
			return "", sourcefacts.RecoveryOriginConflict
		}
		key = candidate
	}
	return key, ""
}

func (c *originChain) addPath(path string) sourcefacts.RecoveryOutcome {
	stamp, ok := repositoryStamp(path)
	if !ok {
		return sourcefacts.RecoveryOriginUnreadable
	}
	if old, exists := c.paths[path]; exists && old != stamp {
		return sourcefacts.RecoveryOriginChanged
	}
	c.paths[path] = stamp
	if len(c.paths) > maxIdentityDependencies {
		return sourcefacts.RecoveryOriginLimit
	}
	return ""
}

// recoveryOrigins reads every configured value and asks Git to expand insteadOf
// without contacting a remote. Both observations are renewed with the same config
// dependencies. A local spelling alone never supplies negative ownership.
func (o *IdentityObserver) recoveryOrigins(ctx context.Context, root string) ([]string, []byte, sourcefacts.RecoveryOutcome) {
	bounded, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	raw, err := o.shortRunner()(bounded, root, "-C", root, "config", "-z", "--get-all", "remote.origin.url")
	if err != nil {
		var status interface{ ExitCode() int }
		if errors.As(err, &status) && status.ExitCode() == 1 && len(raw) == 0 && bounded.Err() == nil {
			return nil, nil, ""
		}
		return nil, nil, sourcefacts.RecoveryOriginUnreadable
	}
	values := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	for _, value := range values {
		if strings.ContainsAny(value, "\r\n") {
			return nil, nil, sourcefacts.RecoveryOriginUnsupported
		}
	}
	rewrites, rewriteErr := o.shortRunner()(bounded, root, "-C", root, "config", "-z", "--get-regexp", `^url\..*\.insteadof$`)
	var status interface{ ExitCode() int }
	if errors.As(rewriteErr, &status) && status.ExitCode() == 1 && len(rewrites) == 0 && bounded.Err() == nil {
		return values, raw, ""
	}
	if rewriteErr != nil || bounded.Err() != nil {
		return nil, nil, sourcefacts.RecoveryOriginUnreadable
	}
	expanded, err := o.shortRunner()(bounded, root, "-C", root, "remote", "get-url", "--all", "origin")
	if err != nil || bounded.Err() != nil {
		return nil, nil, sourcefacts.RecoveryOriginUnsupported
	}
	effective := strings.Split(strings.TrimSuffix(string(expanded), "\n"), "\n")
	if len(effective) != len(values) || slices.ContainsFunc(effective, func(s string) bool { return strings.ContainsAny(s, "\x00\r") }) {
		return nil, nil, sourcefacts.RecoveryOriginUnsupported
	}
	// Keep equality evidence private; raw remote values never enter an identity.
	return effective, append(append(slices.Clone(raw), 0), expanded...), ""
}

func localOriginPath(base, value string) (string, bool) {
	if strings.HasPrefix(value, "file://") {
		u, err := url.Parse(value)
		if err != nil {
			return "", false
		}
		// Compare the complete authority, excluding ports, brackets and Unicode
		// case folds while accepting ASCII localhost case variants.
		localAuthority := u.Host == "" || (len(u.Host) == len("localhost") && strings.EqualFold(u.Host, "localhost"))
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || !localAuthority || !filepath.IsAbs(u.Path) {
			return "", false
		}
		value = u.Path
	} else if !filesystemOriginSpelling(value) || strings.HasPrefix(value, "~") {
		return "", false
	}
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	if !filepath.IsAbs(value) {
		value = joinRepositoryPath(base, value)
	}
	return value, len(value) <= 4096
}

// Git treats a colon as an SCP/transport separator only before the first
// slash. Preserve unambiguous local spellings without consulting path existence.
func filesystemOriginSpelling(value string) bool {
	colon, slash := strings.IndexByte(value, ':'), strings.IndexByte(value, '/')
	return colon < 0 || (slash >= 0 && slash < colon)
}

// Join without cleaning so filesystem traversal resolves symlinks before parents.
func joinRepositoryPath(base, path string) string {
	return strings.TrimSuffix(base, string(filepath.Separator)) + string(filepath.Separator) + path
}

func (o *IdentityObserver) originTarget(ctx context.Context, path string) (string, string, sourcefacts.RecoveryOutcome) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "", originPathFailure(err)
	}
	if !info.IsDir() {
		return "", "", sourcefacts.RecoveryOriginUnsupported
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", originPathFailure(err)
	}
	top := ProjectRoot(ctx, path, o.shortRunner())
	if top != "" {
		if canonical != top {
			return "", "", sourcefacts.RecoveryOriginUnsupported
		}
		return path, top, ""
	}
	// Bare repositories and explicit .git directories have no checkout top.
	bounded, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	raw, err := o.shortRunner()(bounded, path, "-C", path, "rev-parse", "--absolute-git-dir")
	gitDir := strings.TrimSuffix(string(raw), "\n")
	if err != nil || bounded.Err() != nil || gitDir != canonical {
		return "", "", sourcefacts.RecoveryOriginUnreadable
	}
	return path, "", ""
}

func originPathFailure(err error) sourcefacts.RecoveryOutcome {
	if errors.Is(err, os.ErrNotExist) {
		return sourcefacts.RecoveryOriginMissing
	}
	return sourcefacts.RecoveryOriginUnreadable
}
