package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// projectMatchRequest also supports pairing's home-relative hint after its
// caller validates and resolves it to an absolute destination path. A known
// repository mismatch never falls back to the hint.
type projectMatchRequest struct {
	RepoKey string
	Path    string
	// TransferredScope defers saved-scope consent to the complete rule set.
	TransferredScope bool
}

type projectMatchResult struct {
	RepositoryKeys map[string]string
	Roots          [][]string
	History        backfill.KnownProjectsResult
	Incomplete     bool
	TimedOut       bool
	Capped         bool
}

func (e Env) projectRepoKey(ctx context.Context, root string) string {
	key, known := e.projectOrigin(ctx, root)
	if !known {
		return ""
	}
	return key
}

func (e Env) projectOrigin(ctx context.Context, root string) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	var key string
	if e.repoKeyContext != nil {
		key = e.repoKeyContext(ctx, root)
	} else if e.repoKey != nil {
		key = e.repoKey(root)
	} else {
		return gitremote.ProjectKey(ctx, root, e.projectGitRunner)
	}
	// Test lookups use an empty key to establish no origin. Invalid nonempty
	// answers cannot authorize a path fallback.
	return key, key == "" || archive.IsRepoKey(key)
}

// projectRepository establishes the full checkout for a key-based match.
// The identity-only test seams model candidates already at their top level.
func (e Env) projectRepository(ctx context.Context, root string) (string, string, bool) {
	key, known := e.projectOrigin(ctx, root)
	if !known || key == "" || e.repoKeyContext != nil || e.repoKey != nil {
		return key, root, known
	}
	top := gitremote.ProjectRoot(ctx, root, e.projectGitRunner)
	return key, top, top != ""
}

// projectCandidates caches canonical paths and caps filesystem and Git work.
type projectCandidates struct {
	ctx            context.Context
	result         *projectMatchResult
	canonicalPaths map[string]string
	seen           map[string]bool
	roots          []string
}

func (c *projectCandidates) canonical(path string) string {
	if root, ok := c.canonicalPaths[path]; ok {
		return root
	}
	if c.ctx.Err() != nil {
		return filepath.Clean(path)
	}
	root := local.CanonicalPath(path)
	c.canonicalPaths[path] = root
	return root
}

func (c *projectCandidates) add(path string) {
	if c.result.Capped {
		return
	}
	if c.ctx.Err() != nil {
		c.result.Incomplete = true
		c.result.TimedOut = true
		return
	}
	if path == "" || !filepath.IsAbs(path) {
		return
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return
	}
	root := c.canonical(path)
	if c.seen[root] {
		return
	}
	if len(c.roots) >= 128 {
		c.result.Capped = true
		c.result.Incomplete = true
		return
	}
	c.seen[root] = true
	c.roots = append(c.roots, root)
}

// projectScopeBlocked refuses implicit overlap with exclusions or a wider
// ancestor of an already configured inclusion. Explicit paths remain choices.
func projectScopeBlocked(root string, included, excluded []string) bool {
	for _, exclusion := range excluded {
		if local.PathWithin(root, exclusion) || local.PathWithin(exclusion, root) {
			return true
		}
	}
	for _, existing := range included {
		if existing != root && local.PathWithin(existing, root) {
			return true
		}
	}
	return false
}

// projectMatchDeadline bounds one matchProjects call.
const projectMatchDeadline = 5 * time.Second

// matchProjects shares one deadline across discovery and four bounded Git
// workers. Paths are canonicalized and each repository is queried once.
func matchProjects(ctx context.Context, env Env, userHome string, cfg config.Config, requests []projectMatchRequest) projectMatchResult {
	timeout := projectMatchDeadline
	if env.projectMatchTimeout > 0 {
		timeout = env.projectMatchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := projectMatchResult{Roots: make([][]string, len(requests)), RepositoryKeys: map[string]string{}}
	bf := env.backfillEnvironment(userHome, cfg)
	c := projectCandidates{ctx: ctx, result: &result, canonicalPaths: map[string]string{}, seen: map[string]bool{}}
	var included, excluded []string
	for _, project := range cfg.Archive.Projects {
		if ctx.Err() != nil {
			break
		}
		if project.Included {
			included = append(included, c.canonical(project.Root))
		} else {
			excluded = append(excluded, c.canonical(project.Root))
		}
	}
	if cwd, ok := workingDirOrNone(env); ok {
		c.add(cwd)
	}
	for _, project := range cfg.Archive.Projects {
		c.add(project.Root)
	}
	for _, request := range requests {
		c.add(request.Path)
	}
	keys := make([]string, len(c.roots))
	checkoutRoots := make([]string, len(c.roots))
	known := make([]bool, len(c.roots))
	lookup := func(start int) {
		var mu sync.Mutex
		next := start
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for {
					mu.Lock()
					i := next
					next++
					mu.Unlock()
					if i >= len(c.roots) || ctx.Err() != nil {
						return
					}
					child, done := context.WithTimeout(ctx, 250*time.Millisecond)
					keys[i], checkoutRoots[i], known[i] = env.projectRepository(child, c.roots[i])
					if !known[i] || child.Err() != nil {
						mu.Lock()
						result.Incomplete = true
						result.TimedOut = result.TimedOut || child.Err() != nil
						mu.Unlock()
					}
					done()
				}
			})
		}
		wg.Wait()
	}
	lookup(0)
	unresolved := false
	for _, request := range requests {
		unresolved = unresolved || len(c.match(request, keys, checkoutRoots, known, included, excluded)) == 0
	}
	if unresolved && ctx.Err() == nil && !result.Capped {
		start := len(c.roots)
		result.History = backfill.KnownProjectsBounded(ctx, bf, cfg, 128)
		for _, project := range result.History.Projects {
			c.add(project.Root)
		}
		result.Incomplete = result.Incomplete || result.History.Incomplete()
		keys = append(keys, make([]string, len(c.roots)-start)...)
		checkoutRoots = append(checkoutRoots, make([]string, len(c.roots)-start)...)
		known = append(known, make([]bool, len(c.roots)-start)...)
		lookup(start)
	}
	result.TimedOut = result.TimedOut || ctx.Err() != nil || result.History.TimedOut
	result.Capped = result.Capped || result.History.Capped
	result.Incomplete = result.Incomplete || ctx.Err() != nil
	for i, request := range requests {
		result.Roots[i] = c.match(request, keys, checkoutRoots, known, included, excluded)
	}

	// Canonicalizing newly established checkout roots is native I/O too.
	result.TimedOut = result.TimedOut || ctx.Err() != nil
	result.Incomplete = result.Incomplete || ctx.Err() != nil
	return result
}

func (c *projectCandidates) match(request projectMatchRequest, keys, checkoutRoots []string, known []bool, included, excluded []string) []string {
	var matches []string
	hint := ""
	if request.Path != "" {
		hint = c.canonical(request.Path)
	}
	seen := map[string]bool{}
	for j, candidate := range c.roots {
		if !known[j] {
			continue
		}
		root := c.canonical(checkoutRoots[j])
		if request.RepoKey == "" || keys[j] == "" {
			root = candidate
		}
		if seen[root] || !projectRequestMatches(request.RepoKey, hint, root, keys[j], known[j]) || (!request.TransferredScope && projectScopeBlocked(root, included, excluded)) {
			continue
		}
		seen[root] = true
		c.result.RepositoryKeys[root] = keys[j]
		matches = append(matches, root)
	}
	return matches
}

func printProjectMatches(p *prompter, keys []string, result projectMatchResult) {
	for i, key := range keys {
		reason := "not found"
		if result.Incomplete {
			reason = "incomplete discovery"
		} else if i < len(result.Roots) {
			switch len(result.Roots[i]) {
			case 1:
				continue
			case 0:
			default:
				reason = "multiple clones"
			}
		}
		terminal.Printf(p.out, "Skipped repository %s: %s; use --project DIR to choose a local path.\n", key, reason)
	}
	if result.Incomplete {
		terminal.Printf(p.out, "Project discovery: timeout=%t, cap=%t, unreadable=%d.\n", result.TimedOut, result.Capped, result.History.Unreadable)
	}
}

func projectRequestMatches(key, hint, root, origin string, known bool) bool {
	if key == "" {
		return hint == root
	}
	return known && (origin == key || (origin == "" && hint == root))
}

func printSetupProjectMatches(p *prompter, keys []string, result projectMatchResult) {
	// A key-only command may leave no project included. Explain skips before
	// returning its validation error; invalid keys are never echoed.
	if len(keys) > 0 && len(result.Roots) == len(keys) {
		printProjectMatches(p, keys, result)
	}
}
