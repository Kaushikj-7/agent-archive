package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// activeSourceWindow is how recently a local source session must have been
// active for a launch into its checkout to be treated as sharing it with a
// running agent.
const activeSourceWindow = 2 * time.Minute

// worktreeDependencies is what choosing the launch directory uses: the
// source's registration and clock for the active check, whether it may ask
// (env.interactive), and git for --worktree.
type worktreeDependencies interface {
	readHome() (string, error)
	now() time.Time
	interactive(any) bool
	currentSessionDependencies
	runGit(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// errHandoffCanceled is the answer N to the active-source question.
var errHandoffCanceled = errors.New("canceled; nothing was launched")

// prepareLaunchDir returns the directory the agent starts in: dir, or with
// --worktree (or the answer w to the active-source question) the matching
// directory of a new git worktree beside dir's checkout. stdin is only
// checked for whether it may be asked on; the answer is read from answers, which a command
// asking several questions shares between them (a *bufio.Reader of the
// default size is used as is, so typed-ahead answers are not lost).
func prepareLaunchDir(env worktreeDependencies, opts handoffOptions, target handoffTarget, dir string, stdin, answers io.Reader, stderr io.Writer) (string, error) {
	branch := opts.branch
	if !opts.worktree {
		useWorktree, err := checkActiveSource(env, opts, target, dir, stdin, answers, stderr)
		if err != nil || !useWorktree {
			return dir, err
		}
	}
	return createHandoffWorktree(env, branch, target, dir, stderr)
}

// checkActiveSource looks for a source session another agent may still be
// working in, in the checkout the launch would share with it. When it may
// ask (a terminal, with AGENT_ARCHIVE_NONINTERACTIVE off) it asks whether to
// continue there, cancel, or use a worktree instead; otherwise, as inside an
// agent, it warns and continues.
func checkActiveSource(env worktreeDependencies, opts handoffOptions, target handoffTarget, dir string, stdin, answers io.Reader, stderr io.Writer) (useWorktree bool, err error) {
	// Only this machine's own sessions have a checkout here to share.
	if target.source != "local" {
		return false, nil
	}
	now := env.now()
	// The same test that puts the live dot on the picker's row.
	if !activeNow(now, target.lastActivityAt) {
		return false, nil
	}
	var reg archive.SessionRegistration
	var ok bool
	if target.native != nil {
		c := target.native
		reg = archive.SessionRegistration{NativeSessionID: c.NativeID, ProjectRoot: c.Directory, Harness: archive.Harness{Name: c.Ref.Harness}}
		ok = true
	} else {
		reg, ok = sourceRegistration(env, target.bundle.ArchiveSessionID)
	}
	if !ok || !sameProject(reg.ProjectRoot, dir) {
		return false, nil
	}
	age := relativeAge(now, target.lastActivityAt)
	if opts.to != "" && isCallingAgent(env, reg) {
		// Handing off the agent this command runs in: it is active by
		// definition, and it asked for this.
		terminal.Println(stderr, "handoff: note: the session being handed off is the agent running this command, in the same checkout; both can edit its files until one stops (--worktree gives the new agent its own checkout)")
		return false, nil
	}
	if !env.interactive(stdin) || !env.interactive(stderr) {
		terminal.Printf(stderr, "handoff: warning: the source session was active %s in this checkout; both agents can edit its files (--worktree gives the new agent its own checkout)\n", age)
		return false, nil
	}
	p := newPrompter(answers, stderr)
	p.source = stdin
	defer p.close()
	question := fmt.Sprintf("The source session was active %s; where should the new agent continue?", age)
	answer, err := p.guidedChoice(promptModel{Question: question, Helpers: []string{"In the same checkout, both agents can edit its files until one stops."}, Default: "no",
		Primary: []option{{"yes", "Continue in the same checkout"}, {"no", "Cancel"}, {"w", "Continue in a new git worktree"}}})
	if err != nil {
		return false, fmt.Errorf("%w (%w)", errHandoffCanceled, err)
	}
	if answer == "yes" {
		return false, nil
	}
	if answer == "w" {
		return true, nil
	}
	return false, errHandoffCanceled
}

// sourceRegistration loads a session's registration on this machine. ok is
// false when there is none to read, which leaves the active check nothing to
// compare.
func sourceRegistration(env worktreeDependencies, id string) (archive.SessionRegistration, bool) {
	home, err := env.readHome()
	if err != nil {
		return archive.SessionRegistration{}, false
	}
	reg, found, err := state.OpenReadOnly(home).LoadRegistration(id)
	return reg, found && err == nil
}

// isCallingAgent reports whether reg is the agent session running this
// command, by its session variable, or for Cursor (which exposes no ID) any
// Cursor session.
func isCallingAgent(env currentSessionDependencies, reg archive.SessionRegistration) bool {
	harness := archive.CanonicalHarness(reg.Harness.Name)
	for _, observation := range runtimeObservations(env) {
		if observation.NativeID != "" && string(observation.Agent) == harness && observation.NativeID == reg.NativeSessionID {
			return true
		}
	}
	return projectRuntime(env, harness) != ""
}

// createHandoffWorktree adds a worktree on a new branch at HEAD beside dir's
// checkout, carries the checkout's uncommitted changes and untracked (not
// ignored) files into it, and returns the directory in it matching dir. It
// never changes the original checkout or the stash stack: `git stash create`
// only writes an unreferenced commit. Once the worktree exists it is never
// removed, since it may by then hold copies of someone's work.
func createHandoffWorktree(env worktreeDependencies, branch string, target handoffTarget, dir string, stderr io.Writer) (string, error) {
	ctx := context.Background()
	top, err := gitPath(ctx, env, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("--worktree needs a git checkout, and %s is not in one: %w", dir, err)
	}
	prefix, err := gitPath(ctx, env, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", err
	}
	short := handoffShortID(target)
	if branch == "" {
		branch = "handoff/" + short
	}
	path := top + "-handoff-" + short
	if err := checkWorktreeTarget(ctx, env, top, branch, path); err != nil {
		return "", err
	}
	op, err := operationInProgress(ctx, env, top)
	if err != nil {
		return "", err
	}
	if op != "" && op != "bisect" {
		// Only the diff against HEAD would be carried, not the operation's
		// state, so the new agent would see a plain edit instead.
		return "", fmt.Errorf("a %s is in progress in %s; finish or abort it before using --worktree", op, top)
	}
	// Read everything to carry before the worktree exists, so a failure
	// here leaves nothing behind.
	carry, err := readWorktreeCarry(ctx, env, top)
	if err != nil {
		return "", err
	}
	if _, err := gitText(ctx, env, top, "worktree", "add", "-b", branch, path, carry.head); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	left := func(err error) (string, error) {
		return "", fmt.Errorf("created worktree %s on branch %s, but could not carry this checkout's changes into it: %w; the worktree is left in place for you to inspect or remove (`git worktree remove --force %s`)", path, branch, err, path)
	}
	if carry.stash != "" {
		if _, err := gitText(ctx, env, path, "stash", "apply", carry.stash); err != nil {
			return left(err)
		}
	}
	copied, skipped, err := copyUntracked(top, path, carry.untracked)
	if err != nil {
		return left(err)
	}
	for _, name := range skipped {
		terminal.Printf(stderr, "handoff: warning: did not copy %s into the worktree (not a file or symlink, such as a nested repository)\n", name)
	}
	terminal.Printf(stderr, "handoff: created worktree %s on branch %s (carried %d changed and %d untracked files)\n", path, branch, carry.changed, copied)
	if op == "bisect" {
		// Bisecting leaves the working tree as it is, so its changes carry;
		// the bisect itself stays in this checkout.
		terminal.Printf(stderr, "handoff: note: a bisect is in progress in %s; the worktree starts at the commit it has checked out, without the bisect\n", top)
	}
	if _, err := os.Lstat(filepath.Join(top, ".gitmodules")); err == nil {
		terminal.Println(stderr, "handoff: note: submodules are not checked out in the worktree, and changes inside them were not carried (`git submodule update --init` there checks them out)")
	}
	launch := filepath.Join(path, filepath.FromSlash(prefix))
	if info, statErr := os.Stat(launch); statErr == nil && info.IsDir() {
		return launch, nil
	}
	// dir was inside an ignored or untracked-only directory.
	terminal.Printf(stderr, "handoff: %s is not in the worktree; starting at its top level\n", prefix)
	return path, nil
}

// gitPath removes only Git's output terminator, preserving path whitespace.
func gitPath(ctx context.Context, env worktreeDependencies, dir string, args ...string) (string, error) {
	out, err := env.runGit(ctx, dir, args...)
	return strings.TrimSuffix(string(out), "\n"), err
}

// gitText runs git in dir and returns its output without surrounding space.
func gitText(ctx context.Context, env worktreeDependencies, dir string, args ...string) (string, error) {
	out, err := env.runGit(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// checkWorktreeTarget refuses a branch name git would not take as given, and
// a worktree path or branch that already exists.
func checkWorktreeTarget(ctx context.Context, env worktreeDependencies, top, branch, path string) error {
	// check-ref-format --branch also expands forms such as @{-1}; only a
	// name it returns unchanged is taken as given. It accepts "@", which is
	// HEAD's shorthand, and older versions accept a leading "-".
	if branch == "@" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%q is not a valid branch name", branch)
	}
	if checked, err := gitText(ctx, env, top, "check-ref-format", "--branch", branch); err != nil || checked != branch {
		return fmt.Errorf("%q is not a valid branch name", branch)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists; remove it (`git worktree remove %s` if it is an earlier handoff's worktree) and retry", path, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	refs, err := gitText(ctx, env, top, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if slices.Contains(strings.Split(refs, "\n"), "refs/heads/"+branch) {
		return fmt.Errorf("branch %s already exists; name a new one with --branch NAME", branch)
	}
	return nil
}

// worktreeCarry is what a new worktree carries from the checkout: the
// commit it starts at, the stash commit holding uncommitted changes ("" when
// there are none) and how many files those touch, and the untracked files.
type worktreeCarry struct {
	head      string
	stash     string
	changed   int
	untracked []string
}

// readWorktreeCarry records the checkout at top's uncommitted changes and
// lists its untracked (not ignored) files, changing nothing in it.
func readWorktreeCarry(ctx context.Context, env worktreeDependencies, top string) (worktreeCarry, error) {
	// `git stash create` cannot record intent-to-add (`git add -N`)
	// entries, and fails with a message that does not say which.
	intent, err := env.runGit(ctx, top, "diff", "--name-only", "--diff-filter=A", "-z")
	if err != nil {
		return worktreeCarry{}, fmt.Errorf("list uncommitted changes: %w", err)
	}
	if names := splitNul(intent); len(names) > 0 {
		return worktreeCarry{}, fmt.Errorf("--worktree cannot carry files added with `git add -N` (%s); stage them with `git add`, or undo with `git reset -- <file>`, and retry", strings.Join(names, ", "))
	}
	head, err := gitText(ctx, env, top, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return worktreeCarry{}, fmt.Errorf("--worktree needs a commit to start from: %w", err)
	}
	stash, err := gitText(ctx, env, top, "stash", "create")
	if err != nil {
		return worktreeCarry{}, fmt.Errorf("record uncommitted changes: %w", err)
	}
	changed := 0
	if stash != "" {
		// The worktree starts where the changes were recorded, even if
		// another agent in this checkout has committed since.
		if head, err = gitText(ctx, env, top, "rev-parse", "--verify", stash+"^1"); err != nil {
			return worktreeCarry{}, fmt.Errorf("record uncommitted changes: %w", err)
		}
		names, err := env.runGit(ctx, top, "diff", "--name-only", "-z", stash+"^1", stash)
		if err != nil {
			return worktreeCarry{}, fmt.Errorf("list uncommitted changes: %w", err)
		}
		changed = len(splitNul(names))
	}
	listed, err := env.runGit(ctx, top, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return worktreeCarry{}, fmt.Errorf("list untracked files: %w", err)
	}
	return worktreeCarry{head: head, stash: stash, changed: changed, untracked: splitNul(listed)}, nil
}

// operationInProgress names a merge, rebase, `git am`, cherry-pick, revert, or
// bisect under way in the checkout at top, or returns "" when there is none.
// --git-path finds each where it lives, including a linked worktree's own
// git directory.
func operationInProgress(ctx context.Context, env worktreeDependencies, top string) (string, error) {
	type operation struct {
		path string
		name string
	}
	ops := []operation{
		{"MERGE_HEAD", "merge"}, {"rebase-merge", "rebase"}, {"rebase-apply", "rebase or `git am`"},
		{"CHERRY_PICK_HEAD", "cherry-pick"}, {"REVERT_HEAD", "revert"}, {"BISECT_LOG", "bisect"},
	}
	args := []string{"rev-parse"}
	for _, op := range ops {
		args = append(args, "--git-path", op.path)
	}
	out, err := env.runGit(ctx, top, args...)
	if err != nil {
		return "", err
	}
	paths := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(paths) != len(ops) {
		return "", fmt.Errorf("git rev-parse --git-path: unexpected output %q", out)
	}
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(top, path)
		}
		if _, err := os.Lstat(path); err == nil {
			return ops[i].name, nil
		}
	}
	return "", nil
}

// handoffShortID names a session's worktree and default branch: the first 8
// characters of its archive ID, limited to characters safe in both.
func handoffShortID(target handoffTarget) string {
	name := handoffFileName(target.bundle)
	if target.native != nil {
		name = target.native.NativeID
	}
	if len(name) > 8 {
		name = name[:8]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
}

// copyUntracked copies the named files from the src tree to the dst tree,
// with their permission bits, and symlinks as symlinks (never followed). Both
// trees are opened as os.Root, so no name, and no symlink along the way, can
// reach outside them. Anything else, such as a nested repository git lists
// as a directory, is returned in skipped.
func copyUntracked(src, dst string, names []string) (copied int, skipped []string, err error) {
	from, err := os.OpenRoot(src)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = from.Close() }()
	to, err := os.OpenRoot(dst)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = to.Close() }()
	for _, name := range names {
		rel := filepath.FromSlash(strings.TrimSuffix(name, "/"))
		if !filepath.IsLocal(rel) {
			return copied, skipped, fmt.Errorf("refusing to copy %q: outside the checkout", name)
		}
		info, err := from.Lstat(rel)
		if err != nil {
			return copied, skipped, err
		}
		if info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular() {
			skipped = append(skipped, name)
			continue
		}
		if parent := filepath.Dir(rel); parent != "." {
			if err := to.MkdirAll(parent, 0o777); err != nil {
				return copied, skipped, err
			}
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			link, err := from.Readlink(rel)
			if err == nil {
				err = to.Symlink(link, rel)
			}
			if err != nil {
				return copied, skipped, err
			}
		} else if err := copyRootFile(from, to, rel, info.Mode().Perm()); err != nil {
			return copied, skipped, err
		}
		copied++
	}
	return copied, skipped, nil
}

// copyRootFile copies one regular file between roots, creating it (never
// replacing one) with mode perm regardless of the umask.
func copyRootFile(from, to *os.Root, name string, perm fs.FileMode) error {
	in, err := from.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := to.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// splitNul splits git's -z output into its names.
func splitNul(data []byte) []string {
	var names []string
	for name := range bytes.SplitSeq(data, []byte{0}) {
		if len(name) > 0 {
			names = append(names, string(name))
		}
	}
	return names
}

func (e Env) runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if e.RunGit != nil {
		return e.RunGit(ctx, dir, args...)
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, message)
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
