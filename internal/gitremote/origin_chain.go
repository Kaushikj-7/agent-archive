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
const maxOriginRepositories = 16
const maxIdentityDependencies = 128

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
	id := sourcefacts.RepositoryIdentity{Root: top, Key: key, Known: true, ObservationScope: scope, ObservedRoot: root}
	if chain.semantic {
		id.Validation = "semantic"
	}
	for path, stamp := range chain.paths {
		id.Dependencies = append(id.Dependencies, sourcefacts.RepositoryDependency{Path: path, Stamp: stamp})
	}
	sort.Slice(id.Dependencies, func(i, j int) bool { return id.Dependencies[i].Path < id.Dependencies[j].Path })
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
	if key, ok := c.keys[canonical]; ok {
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
	if failure != "" || !bytes.Equal(raw, verified) || (top != "" && ProjectRoot(ctx, root, c.observer.shortRunner()) != top) {
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
		if !strings.Contains(value, "://") && strings.Contains(value, "::") {
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
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
			return "", false
		}
		value = u.Path
	} else if strings.Contains(value, ":") || strings.HasPrefix(value, "~") {
		return "", false
	}
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	value = filepath.Clean(value)
	return value, len(value) <= 4096
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
