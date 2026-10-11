package backfill

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// resolution is where one working directory is imported, or why it is not.
type resolution struct {
	// root is the project root, in the spelling it is registered under. It is
	// also set for the skips that have a would-be root (excluded, home,
	// temporary), so --project can match them.
	root string
	kind ProjectKind
	// included is set when a configured, included project owns the directory.
	included bool
	skip     SkipReason
	outcome  sourcefacts.RecoveryOutcome
	// cause names the missing witness evidence behind an inventory outcome;
	// gap is the per-app gap that blocked recorded recovery (either outcome).
	cause   DiagnosticDetail
	gap     *recoveryGap
	proof   *archive.ProjectResolution
	current *resolutionCheck
}

// resolver applies the spec's project resolution rules. It never runs git:
// worktrees are followed through their .git files.
type resolutionCheck struct {
	valid func() bool
	reset func(context.Context)
}

type resolver struct {
	env     Environment
	cfg     config.Config
	filters Filters
	// home and homeRaw are the home directory resolved and as given.
	home    string
	homeRaw string
	// workspaces are the folders desktop apps start chats in (rule 5).
	workspaces []workspaceFolder
	temps      []string
	// worktreeStores are the folders Codex and Cursor keep their worktrees
	// in; a missing worktree there cannot be mapped to its repository.
	worktreeStores          []string
	cache                   map[string]resolution
	recovery                *sourcefacts.RecoveryResolver
	recoverySourcesCurrent  func(harness, archive.ProjectResolution) bool
	recoverySourcesReset    func(context.Context)
	databaseRecoveryCurrent func(context.Context) bool
	// recoveryGaps are the witness inventory's gaps, per app. gapRecovery
	// holds the resolver, over gapProjects, for sessions a gap blocks, keyed
	// by whether the blocking gap is a budget (see recoveryFor).
	recoveryGaps          recoveryGaps
	gapRecovery           map[bool]*sourcefacts.RecoveryResolver
	gapProjects           []archive.ProjectActivation
	inventoryAdded        func(context.Context) (addedSources, bool)
	mappingRecovery       *sourcefacts.RecoveryResolver
	workspaceReset        func()
	requireWitnessFormats func(string)
	proposedRootEligible  func(string) bool
}

func newResolver(env Environment, cfg config.Config, filters Filters) *resolver {
	var workspaces []workspaceFolder
	resolvedHome := env.resolved(env.Home)
	lookInDocuments := documentsInUse(env, resolvedHome, cfg)
	for _, folder := range workspaceFolders(env) {
		if !lookInDocuments && local.PathWithin(folder, filepath.Join(env.Home, "Documents")) {
			// Resolving a folder in ~/Documents looks inside Documents, which
			// makes macOS ask a terminal without access for it. Unless
			// Documents is in use already, the folder is matched as spelled,
			// under home as given and resolved: a session there still maps
			// to it, and a plan that has none never touches Documents.
			rel, _ := filepath.Rel(env.Home, folder)
			root := filepath.Join(resolvedHome, rel)
			workspaces = append(workspaces, workspaceFolder{root: root, forms: uniquePaths(filepath.Clean(folder), root)})
			continue
		}
		workspaces = append(workspaces, workspaceFolder{root: env.resolved(folder), forms: uniquePaths(filepath.Clean(folder), env.resolved(folder))})
	}
	var temps []string
	for _, t := range env.tempDirs() {
		if t != "" {
			temps = append(temps, uniquePaths(filepath.Clean(t), env.resolved(t))...)
		}
	}
	var worktreeStores []string
	var stores []string
	if env.NativePaths != nil {
		for _, name := range env.NativePaths.NativePathAgents() {
			stores = append(stores, env.nativeProjectPaths(name).Worktrees...)
		}
	}
	for _, store := range stores {
		worktreeStores = append(worktreeStores, uniquePaths(filepath.Clean(store), env.resolved(store))...)
	}
	recovery := sourcefacts.NewRecoveryResolver(cfg.Archive.Projects, filters.ProjectMappings, env.resolved, env.RepositoryIdentity, nil)
	recovery.MaxOperations = 1024
	recovery.Validate = env.RepositoryIdentityCurrent
	return &resolver{
		env:            env,
		cfg:            cfg,
		filters:        filters,
		home:           env.resolved(env.Home),
		homeRaw:        filepath.Clean(env.Home),
		workspaces:     workspaces,
		temps:          temps,
		worktreeStores: worktreeStores,
		cache:          map[string]resolution{},
		recovery:       recovery,
	}
}

func uniquePaths(paths ...string) []string {
	var out []string
	for _, p := range paths {
		dup := false
		for _, o := range out {
			dup = dup || o == p
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if local.PathWithin(path, root) {
			return true
		}
	}
	return false
}

// resolveSessionEvidence maps a session's working directory to a project,
// applying the spec's rules in order (the first that matches wins), then
// recovery. Only the gaps that can affect a session of app h block its
// recorded recovery (recoveryGaps.blocking).
func (r *resolver) resolveSessionEvidence(ctx context.Context, h harness, cwd, key string) resolution {
	recovery, gap := r.recoveryFor(ctx, h)
	if r.mappingRecovery != nil && r.filters.ProjectMappings[filepath.Clean(cwd)] != "" {
		recovery, gap = r.mappingRecovery, nil
	}
	// The app is part of the key: admission validity depends on it.
	cacheKey := cwd + "\x00" + r.env.resolved(cwd) + "\x00" + key + "\x00" + recovery.Context + "\x00" + string(h)
	if gap != nil {
		cacheKey += "\x00" + gap.Agent + "\x00" + string(gap.Cause)
	}
	if cached, ok := r.cache[cacheKey]; ok && !r.env.exists(cwd) {
		return cached
	}
	res := r.resolveUncached(cwd)
	// Existing filesystem evidence and nearest configured ownership take precedence.
	if !r.env.exists(cwd) && (res.skip == SkipWorktreeUnresolved || (!r.env.exists(cwd) && !res.included && res.kind == ProjectKindDirectory && res.skip == "" && (key != "" || r.filters.ProjectMappings[filepath.Clean(cwd)] != ""))) {
		if r.hasRepositoryEvidence(cwd) {
			return resolution{skip: SkipWorktreeUnresolved}
		}
		proof, outcome := recovery.Recover(ctx, cwd, key)
		if outcome == "" {
			checked, valid := false, false
			check := &resolutionCheck{reset: func(ctx context.Context) {
				checked = false
				if r.recoverySourcesReset != nil {
					r.recoverySourcesReset(ctx)
				}
				recovery.ResetValidationContext(ctx)
			}, valid: func() bool {
				if !checked {
					checked = true
					valid = !r.env.exists(cwd) && r.env.exists(proof.Root) && !r.hasRepositoryEvidence(cwd) && (proof.Method == "explicit_mapping" || r.recoverySourcesCurrent == nil || r.recoverySourcesCurrent(h, proof)) && recovery.CurrentSlice(proof)
				}
				return valid
			}}
			owner, configured := r.configured(proof.Root)
			if !configured && r.requireWitnessFormats != nil {
				r.requireWitnessFormats(proof.Root)
			}
			// Exact maps still validate against configured targets alone. Retained
			// evidence records the full observed union separately from that check.
			visibleProof := proof
			visibleProof.Context = r.recovery.Context
			res = resolution{root: proof.Root, kind: r.kindOf(proof.Root), included: configured && owner.included, proof: &visibleProof, current: check}
		}
		if outcome != "" {
			res = unrecovered(gap, outcome)
		}
		if outcome == sourcefacts.RecoveryBudgetExhausted || outcome == sourcefacts.RecoveryInventoryUnavailable {
			return res
		}
	}
	r.cache[cacheKey] = res
	return res
}

// unrecovered is a failed recovery. The outcome a blocking gap produces
// carries that gap, and an inventory outcome names its cause.
func unrecovered(gap *recoveryGap, outcome sourcefacts.RecoveryOutcome) resolution {
	var blocking *recoveryGap
	var cause DiagnosticDetail
	if gap != nil && outcome == gap.blockedOutcome() {
		blocking = gap
		if outcome == sourcefacts.RecoveryInventoryUnavailable {
			cause = gap.Cause
		}
	}
	return resolution{skip: SkipWorktreeUnresolved, outcome: outcome, gap: blocking, cause: cause}
}

func (r *resolver) resolve(cwd string) resolution {
	if cached, ok := r.cache[cwd]; ok {
		return cached
	}
	res := r.resolveUncached(cwd)
	r.cache[cwd] = res
	return res
}

func (r *resolver) resolveUncached(cwd string) resolution {
	// Rule 1: the working directory. Without one there is no project.
	if cwd == "" || !filepath.IsAbs(cwd) {
		return resolution{skip: SkipProjectUnknown}
	}
	dir := r.env.resolved(cwd)

	// Rule 2: the nearest configured ancestor, on resolved paths, as hooks
	// decide. An exclusion always beats the defaults below.
	if res, ok := r.configured(dir); ok {
		return res
	}

	// Rules 3 and 4: a worktree folds into its repository; a repository is
	// its own project. The repository then goes through rule 2 itself, so a
	// worktree outside its repository lands in the configured project, or is
	// excluded with it. A repository in a temporary directory (a throwaway
	// clone, or a worktree of one) is not a project: rule 6 takes it, so
	// adding it never captures every later session there. A worktree in a
	// temporary directory of a repository elsewhere still folds into it.
	repo, found, skip := r.repository(dir)
	if skip != "" {
		return resolution{skip: skip}
	}
	if found && r.temporaryRepository(repo) {
		if res, ok := r.configured(repo); ok {
			return res
		}
		// The repository, not the session's folder in it, is what
		// --include-temp adds: one project per clone, as before rule 6
		// took them, and a worktree of the clone kept elsewhere joins it.
		return r.tempRule(repo)
	}
	if found {
		if res, ok := r.configured(repo); ok {
			return res
		}
		if r.homeOrAbove(repo) {
			// A missing ~/.claude/worktrees/<name> maps to home by its path;
			// unless home is configured, it is never a repository project.
			return r.homeRule(repo)
		}
		return resolution{root: repo, kind: ProjectKindRepository}
	}

	// Rule 5: chats a desktop app started in its own workspace folder share
	// one project, that folder.
	for _, ws := range r.workspaces {
		if withinAny(dir, ws.forms) {
			return resolution{root: ws.root, kind: ProjectKindScratch}
		}
	}

	// Rule 6: temporary directories.
	if withinAny(dir, r.temps) {
		return r.tempRule(dir)
	}

	// Rule 7: home and everything above it.
	if r.homeOrAbove(dir) {
		return r.homeRule(dir)
	}

	// Rule 8: anything else is its own project, whether or not it exists.
	return resolution{root: dir, kind: ProjectKindDirectory}
}

// workspaceFolder is one desktop app's workspace folder: root is its
// resolved path, forms are its spellings as given and resolved.
type workspaceFolder struct {
	root  string
	forms []string
}

// documentsInUse reports whether a configured project, included or
// excluded, is in ~/Documents: then the person already gave this machine's
// capture access to it, and looking in it (resolving the Codex workspace
// folder's symlinks) asks nothing new.
func documentsInUse(env Environment, resolvedHome string, cfg config.Config) bool {
	for _, p := range cfg.Archive.Projects {
		for _, home := range uniquePaths(filepath.Clean(env.Home), resolvedHome) {
			if local.PathWithin(filepath.Clean(p.Root), filepath.Join(home, "Documents")) {
				return true
			}
		}
	}
	return false
}

// workspaceFolders are the folders desktop apps start chats in, under home:
// Claude desktop's scratch chats and Codex desktop's dated workspaces
// (<date>/<name>), platform.Locations' ClaudeDesktopScratch and
// CodexDocuments. Both are Mac desktop-app locations; on any other operating
// system there are none, and those paths are not consulted.
func workspaceFolders(env Environment) []string {
	var out []string
	if env.NativePaths != nil {
		for _, name := range env.NativePaths.NativePathAgents() {
			out = append(out, env.nativeProjectPaths(name).DesktopWorkspaces...)
		}
	}
	return out
}

func (r *resolver) isWorkspaceFolder(resolved string) bool {
	for _, ws := range r.workspaces {
		if resolved == ws.root {
			return true
		}
	}
	return false
}

// temporaryRepository reports whether repo, a repository root, is in a
// temporary directory, so rule 6 takes it rather than rule 4. A temporary
// directory that holds home (a container or sandbox whose HOME is under
// /tmp) does not make the person's own repositories in home temporary.
func (r *resolver) temporaryRepository(repo string) bool {
	inHome := local.PathWithin(repo, r.home) || local.PathWithin(repo, r.homeRaw)
	for _, temp := range r.temps {
		if local.PathWithin(repo, temp) && (!inHome || !r.homeOrAbove(temp)) {
			return true
		}
	}
	return false
}

// tempRule is rule 6 for dir, a session folder (or a repository) in a
// temporary directory: skipped, or with --include-temp its own project.
func (r *resolver) tempRule(dir string) resolution {
	if r.filters.IncludeTemp {
		return resolution{root: dir, kind: ProjectKindTemporary}
	}
	return resolution{root: dir, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}
}

// configured applies rule 2 to dir.
func (r *resolver) configured(dir string) (resolution, bool) {
	project, ok := r.configuredOwner(dir)
	if !ok {
		return resolution{}, false
	}
	if !project.Included {
		return resolution{root: project.Root, kind: r.kindOf(project.Root), skip: SkipExcludedProject}, true
	}
	return resolution{root: project.Root, kind: r.kindOf(project.Root), included: true}, true
}

// homeOrAbove reports whether dir is home or one of its ancestors.
func (r *resolver) homeOrAbove(dir string) bool {
	return local.PathWithin(r.home, dir) || local.PathWithin(r.homeRaw, dir)
}

// homeRule is rule 7 for home or a folder above it. Only home itself can
// become a project, with --include-home; a folder above it (/, /Users) would
// capture every session on the machine, so above_home has no override.
func (r *resolver) homeRule(dir string) resolution {
	if dir != r.home && dir != r.homeRaw {
		return resolution{root: dir, kind: ProjectKindHome, skip: SkipAboveHome}
	}
	if r.filters.IncludeHome {
		return resolution{root: r.home, kind: ProjectKindHome}
	}
	return resolution{root: r.home, kind: ProjectKindHome, skip: SkipHomeDirectory}
}

// configuredOwner is the nearest configured project containing dir, compared
// on resolved paths: the rule hooks use (configuredProjectActivationFor).
func (r *resolver) configuredOwner(dir string) (project archiveProject, found bool) {
	p, ok := sourcefacts.ConfiguredOwner(r.cfg.Archive.Projects, dir, r.env.resolved)
	return archiveProject{Root: p.Root, Included: p.Included}, ok
}

type archiveProject struct {
	Root     string
	Included bool
}

// missingWorktree handles a worktree that cannot be followed: its folder or
// its git directory is gone. A Claude Code worktree maps to its repository by
// path; a Codex or Cursor one cannot be mapped.
func (r *resolver) missingWorktree(dir string) (repo string, found bool, skip SkipReason) {
	if r.env.Worktrees != nil {
		for _, provider := range r.env.Worktrees.WorktreeResolvers() {
			if repo, ok := provider.MissingWorktreeRepository(dir); ok {
				return r.env.resolved(repo), true, ""
			}
		}
	}
	if withinAny(dir, r.worktreeStores) {
		return "", false, SkipWorktreeUnresolved
	}
	return "", false, ""
}

// repository applies rules 3 and 4. It walks up from dir looking for .git. A
// .git directory makes its parent the root. A .git file is a linked worktree
// (or a submodule): gitdir: names its git directory, and a commondir there
// leads to the main repository's .git, whose parent is the root. The walk
// never reaches home or anything above it, so a home-directory repository
// (dotfiles) does not claim every folder under home.
func (r *resolver) repository(dir string) (repo string, found bool, skip SkipReason) {
	if !r.env.exists(dir) {
		if repo, found, skip := r.missingWorktree(dir); found || skip != "" {
			return repo, found, skip
		}
	}
	for d := dir; ; {
		if r.homeOrAbove(d) {
			return "", false, ""
		}
		gitPath := filepath.Join(d, ".git")
		if info, err := r.env.stat(gitPath); err == nil {
			if info.IsDir() {
				return d, true, ""
			}
			main, ok := r.worktreeMain(d)
			if !ok {
				// The git directory the file names is gone: the worktree's
				// repository was removed or moved. Only a Claude Code
				// worktree can still be mapped, by its path.
				if repo, found, _ := r.missingWorktree(d); found {
					return repo, true, ""
				}
				return "", false, SkipWorktreeUnresolved
			}
			return r.env.resolved(main), true, ""
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false, ""
		}
		d = parent
	}
}

// worktreeMain follows a .git file to the main repository. ok is false when
// the git directory it names, or the common directory, does not exist.
// Anything else it cannot follow leaves the checkout holding the file as its
// own root.
func (r *resolver) worktreeMain(checkout string) (string, bool) {
	return sourcefacts.WorktreeMain(checkout, r.env.readFile, r.env.exists, false)
}

// kindOf says what an existing configured root is.
func (r *resolver) kindOf(root string) ProjectKind {
	resolved := r.env.resolved(root)
	switch {
	case r.isWorkspaceFolder(resolved):
		return ProjectKindScratch
	case resolved == r.home:
		return ProjectKindHome
	case withinAny(resolved, r.temps):
		return ProjectKindTemporary
	case r.env.exists(filepath.Join(resolved, ".git")):
		return ProjectKindRepository
	}
	return ProjectKindDirectory
}

// workspaceMatcher lazily opens one compact native workspace inventory per
// native owner. Shared admission still applies configured-project and Git rules.
type workspaceMatcher struct {
	env        Environment
	agent      string
	candidates []string
	pass       agentapi.WorkspacePass
}

func (m *workspaceMatcher) match(ctx context.Context, key string) (string, bool, error) {
	if m.pass == nil {
		if m.env.Workspaces == nil {
			return "", false, nil
		}
		provider, ok := m.env.Workspaces.LookupWorkspace(m.agent)
		if !ok {
			return "", false, nil
		}
		var err error
		m.pass, err = provider.OpenWorkspace(ctx, agentapi.WorkspaceRequest{Candidates: m.candidates, Environment: m.env.nativePathEnvironment(m.agent), Files: workspaceFiles{m.env}, ResolvePath: m.env.resolved})
		if err != nil {
			return "", false, err
		}
	}
	matches, err := m.pass.MatchWorkspace(ctx, key)
	if err != nil {
		return "", false, err
	}
	if len(matches) != 1 {
		return "", false, nil
	}
	return matches[0], true, nil
}

type workspaceFiles struct{ env Environment }

func (h workspaceFiles) ReadDir(path string) ([]fs.DirEntry, error) { return h.env.readDir(path) }

func (h workspaceFiles) ReadFile(path string) ([]byte, error) { return h.env.readFile(path) }

func cursorWorkspaceStorage(env Environment) string {
	return env.nativeProjectPaths("cursor").WorkspaceStorage
}

func workspaceMetadataFolder(env Environment, agent string, data []byte) string {
	if env.Workspaces != nil {
		if provider, ok := env.Workspaces.LookupWorkspace(agent); ok {
			folders := provider.WorkspaceFolders(agentapi.WorkspaceEvidence{Purpose: agentapi.WorkspaceProjectFile, Bytes: data})
			if len(folders) == 1 {
				return folders[0]
			}
		}
	}
	return ""
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// hasRepositoryEvidence conservatively preserves a live or damaged ancestor's
// ownership before recorded recovery, using the injected bounded filesystem.
func (r *resolver) hasRepositoryEvidence(cwd string) bool {
	for path, depth := filepath.Clean(cwd), 0; depth < 64; depth++ {
		_, err := r.env.lstat(filepath.Join(path, ".git"))
		if !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
	return true
}
