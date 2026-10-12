// Package gitremote finds a project's git origin remote and turns it into an
// archive.RepoKey, and reads the branch and commit checked out in a directory, best
// effort. It is where the program runs git to ask a name of it; handoff's
// --worktree runs git for its own changes.
//
// Every failure (git not installed, a directory that is not a repository, no
// origin, a slow disk) is an empty result, never an error: a repository key
// and a HEAD are annotations on a session, and no caller may fail, or wait
// long, for them. The hook runtime imports nothing that runs a
// program, so it is handed a resolver by the command line instead (see
// internal/cli's `_hook`).
package gitremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// Timeout bounds one git invocation. A hook has about two seconds in all.
const Timeout = 500 * time.Millisecond

// maxOutput is the most git output read; a remote URL or an object name is
// far shorter.
const maxOutput = 4096

// ErrOutputLimit reports Git output beyond the executor's bounded allowance.
// The returned output contains only the retained prefix.
var ErrOutputLimit = errors.New("git printed more than expected")

// Runner runs git with args in dir and returns its standard output. It must
// stop when ctx ends. Tests substitute one; ExecRunner is the real one.
type Runner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// OriginURL returns the URL of root's origin remote, as git reports it, or ""
// when there is none or git cannot be asked within Timeout. run is nil for
// ExecRunner. It runs `git -C root config --get remote.origin.url`, which
// also answers for a subdirectory of a repository and for a linked worktree.
//
// The URL can carry credentials (https://user:token@host/...), so it is for
// archive.RepoKey and must not be stored or logged.
func OriginURL(ctx context.Context, root string, run Runner) string {
	if root == "" || !filepath.IsAbs(root) {
		return ""
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, root, "-C", root, "config", "--get", "remote.origin.url")
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// Branch returns the branch checked out in dir, as `branch --show-current`
// reports it (the bare name, also when a tag has the same name, and the name
// of a branch with no commits yet), or "" when HEAD is detached, dir is not in
// a repository, git is older than 2.22, or git cannot be asked within
// Timeout. run is nil for ExecRunner. It reads a name and changes nothing.
func Branch(ctx context.Context, dir string, run Runner) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, dir, "-C", dir, "branch", "--show-current")
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// RepoKey is archive.RepoKey of root's origin remote, or "" (see OriginURL).
func RepoKey(ctx context.Context, root string, run Runner) string {
	return archive.RepoKey(OriginURL(ctx, root, run))
}

// ProjectKey returns the origin's repository key and whether Git established
// its identity or absence. Unlike RepoKey, failed or nonportable origins are
// unknown: they cannot authorize an automatic path fallback. A missing config
// entry (Git exit 1 with no output) establishes no origin. run is nil for
// ExecRunner; it must respect ctx. Remote URLs never leave this function.
func ProjectKey(ctx context.Context, root string, run Runner) (string, bool) {
	key, known, _ := projectKey(ctx, root, run)
	return key, known
}

// projectKey also reports whether Git read an origin value it could not
// normalize (a local path, file:// URL or unparsable remote). Only such an
// origin is keyless evidence; a failed read is unknown and may hide any key.
//
// A checkout can carry several remote.origin.url values, and `--get` reports
// only the last. Every value is read, NUL-separated, within the same output
// bound and Timeout. A value that normalizes is the identity, so a trailing
// local path cannot make a checkout with a real origin keyless; the checkout
// is keyless only when no value normalizes and some value is nonempty. Values
// that name different keys are neither keyless nor known: no single key is
// proven, so the identity stays unknown and blocks recovery (the strict
// choice).
func projectKey(ctx context.Context, root string, run Runner) (key string, known, nonportable bool) {
	if root == "" || !filepath.IsAbs(root) || ctx.Err() != nil {
		return "", false, false
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, root, "-C", root, "config", "-z", "--get-all", "remote.origin.url")
	if ctx.Err() != nil {
		return "", false, false
	}
	if err != nil {
		var status interface{ ExitCode() int }
		missing := errors.As(err, &status) && status.ExitCode() == 1 && len(out) == 0
		return "", missing, false
	}
	return originValuesKey(originValues(out))
}

// originValues splits `config -z --get-all` output into values. Each value is
// read as a single `--get` value was: its first line, trimmed.
func originValues(out []byte) []string {
	values := strings.Split(string(out), "\x00")
	if len(values) > 1 && values[len(values)-1] == "" {
		values = values[:len(values)-1] // the last value's terminator
	}
	for i, v := range values {
		values[i] = strings.TrimSpace(strings.SplitN(v, "\n", 2)[0])
	}
	return values
}

// originValuesKey classifies a checkout's origin values (see projectKey).
func originValuesKey(values []string) (key string, known, nonportable bool) {
	unnamed := false
	for _, raw := range values {
		k := archive.RepoKey(raw)
		switch {
		case k == "":
			unnamed = unnamed || raw != ""
		case key == "":
			key = k
		case key != k:
			return "", false, false
		}
	}
	if key != "" {
		return key, true, false
	}
	return "", !unnamed, unnamed
}

// ProjectRoot returns Git's checkout top level, or empty when it cannot be
// established. It never infers full-repository scope from an inherited origin.
// run is nil for ExecRunner and must respect ctx when supplied.
func ProjectRoot(ctx context.Context, root string, run Runner) string {
	if root == "" || !filepath.IsAbs(root) || ctx.Err() != nil {
		return ""
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, root, "-C", root, "rev-parse", "--show-toplevel")
	if err != nil || ctx.Err() != nil {
		return ""
	}
	top := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
	if !filepath.IsAbs(top) || strings.ContainsAny(top, "\x00\r\n") {
		return ""
	}
	return filepath.Clean(top)
}

// Resolver derives repository keys and remembers each project root's answer,
// so a sweep over many sessions of one project asks git once. It is safe for
// concurrent use. The zero value runs the real git.
type Resolver struct {
	// Run replaces ExecRunner; tests set it.
	Run Runner

	mu    sync.Mutex
	cache map[string]string
}

// Key returns RepoKey(root), from the cache after the first call for root.
// A failure is cached too, so an unreachable git costs one timeout per
// project, not one per session.
func (r *Resolver) Key(root string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key, ok := r.cache[root]; ok {
		return key
	}
	key := RepoKey(context.Background(), root, r.Run)
	if r.cache == nil {
		r.cache = map[string]string{}
	}
	r.cache[root] = key
	return key
}

// ExecRunner runs the git found on PATH, without a shell. The directory to
// ask about is in args (-C), not the process's working directory, and is not
// checked first: a stat of a hung mount would block with nothing to stop it,
// and git's own failure on a missing directory is the same answer. The
// environment carries none of the caller's GIT_* variables (a stray GIT_DIR
// would answer for another repository), git never prompts, and output past a
// few kilobytes is ErrOutputLimit, returned with what was kept, not a
// truncated URL.
func ExecRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return execOutput(ctx, dir, maxOutput, args...)
}

// ConfigRunner allows bounded origin/name inventories without enlarging URL limits.
func ConfigRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return execOutput(ctx, dir, 256*1024, args...)
}

func execOutput(ctx context.Context, _ string, limit int, args ...string) ([]byte, error) {
	git, err := realLocator.find()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Env = environment(os.Environ())
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	out := limitedBuffer{limit: limit}
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if out.over {
		return out.buf.Bytes(), ErrOutputLimit
	}
	return out.buf.Bytes(), nil
}

// macOS ships /usr/bin/git as a stub that, without the developer tools, opens
// a graphical "install developer tools" prompt instead of running. A hook or
// the LaunchAgent must never do that.
const macGitStub = "/usr/bin/git"

// xcodeSelectLink is what `xcode-select -p` reads: a symbolic link to the
// active developer directory (the command line tools, or an Xcode wherever it
// was installed, /Applications/Xcode-15.4.app included).
const xcodeSelectLink = "/var/db/xcode_select_link"

// developerGits are where the developer tools usually keep the real git, also
// found without following the link.
var developerGits = []string{
	"/Library/Developer/CommandLineTools/usr/bin/git",
	"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
}

// locator finds the git to run. Its parts are fields so a test can stand in
// for the file system and for macOS.
type locator struct {
	lookPath func(name string) (string, error)
	exists   func(path string) bool
	readlink func(path string) (string, error)
	goos     string
	// stubUsable, when set, answers usableStub in place of checking (the real
	// locator caches the answer for the process).
	stubUsable func() bool
}

var (
	stubOnce   sync.Once
	stubResult bool
)

// realLocator looks on the real PATH and file system, and checks the macOS
// stub once per process.
var realLocator = locator{lookPath: exec.LookPath, exists: fileExists, readlink: os.Readlink, goos: string(platform.Current()), stubUsable: func() bool {
	stubOnce.Do(func() {
		stubResult = locator{exists: fileExists, readlink: os.Readlink}.usableStub()
	})
	return stubResult
}}

// find is the git on PATH, or an error when there is none or it is the macOS
// stub without the developer tools behind it.
func (l locator) find() (string, error) {
	git, err := l.lookPath("git")
	if err != nil {
		return "", err
	}
	if l.goos == "darwin" && git == macGitStub {
		usable := l.stubUsable
		if usable == nil {
			usable = l.usableStub
		}
		if !usable() {
			return "", errors.New("git is not installed (the macOS stub needs the developer tools)")
		}
	}
	return git, nil
}

// usableStub reports whether the developer tools provide a git for the macOS
// stub to hand off to: at a usual location, or under the directory the
// xcode-select link names. A DEVELOPER_DIR is not consulted: refusing is the
// safe error.
func (l locator) usableStub() bool {
	if slices.ContainsFunc(developerGits, l.exists) {
		return true
	}
	target, err := l.readlink(xcodeSelectLink)
	return err == nil && filepath.IsAbs(target) && l.exists(filepath.Join(target, "usr", "bin", "git"))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// environment is env without any GIT_* variable, plus the settings that keep
// git from prompting, taking optional locks, or localizing its output.
func environment(env []string) []string {
	kept := make([]string, 0, len(env)+3)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			kept = append(kept, entry)
		}
	}
	return append(kept, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}

// limitedBuffer keeps the first maxOutput bytes written, discards the rest,
// and records that it did. The buffer is a field, not embedded: embedding
// would promote bytes.Buffer's ReadFrom, which io.Copy prefers, and skip this
// Write's cap.
type limitedBuffer struct {
	buf   bytes.Buffer
	over  bool
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	limit := b.limit
	if limit == 0 {
		limit = maxOutput
	}
	room := limit - b.buf.Len()
	if len(p) > room {
		b.over = true
	}
	if room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
