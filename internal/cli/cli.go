// Package cli implements the agent-archive command-line interface: the
// hidden `_hook` and `_collect` entry points a real installation's hooks
// and LaunchAgent invoke, and the user-facing
// setup/status/sync/pause/resume/uninstall, read-only list/show, and
// handoff commands. Process-level state (args, stdio, the clock, the home
// directory, launchctl, the credential store) reaches commands through Env, so a test
// can substitute every piece of it; a nil Env field means the real thing.
// A few lower packages still read the process directly: local resolves the
// data directory from AGENT_ARCHIVE_HOME and $HOME, credentials reads the AWS
// configuration files and the environment, and cursorstore asks getconf for the
// user's temporary directory.
package cli

import (
	"cmp"
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// Version is the released version string. scripts/build-release.sh sets it
// with -ldflags "-X .../cli.Version=..."; it stays "dev" for a local build,
// which --version then reports with its commit (see versionString).
var Version = "dev"

// versionString is what --version prints: Version, or for a "dev" build the
// commit it was built from, so a bug report from a source build says which
// code it ran.
func versionString() string {
	info, _ := debug.ReadBuildInfo()
	return describeVersion(Version, info)
}

// describeVersion returns version unchanged unless it is "dev". A "dev"
// build from a checkout that records a VCS revision reports
// "dev-<first 12 hex of the commit>", with "-dirty" when the working tree had
// uncommitted changes; one with no revision, as `go install ...@vX.Y.Z`
// builds it, reports its module version.
func describeVersion(version string, info *debug.BuildInfo) string {
	if version != "dev" || info == nil {
		return version
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		//lint:ignore LV1001 build setting keys are arbitrary text; only these two matter
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return version
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	version += "-" + revision
	if modified {
		version += "-dirty"
	}
	return version
}

// Env carries the process-level dependencies a command needs, so tests can
// substitute a temporary home directory, a fixed clock, and an in-memory
// object store. A nil field defaults to the real thing.
type Env struct {
	// CodexLabelHost overrides opt-in native hosting; never consulted in files mode.
	CodexLabelHost agentapi.LabelHostFactory
	// Agents overrides immutable production composition, including its catalog.
	Agents            *builtin.Registry
	handoffConfigLoad func(string) (config.Config, bool, error)
	nativeFS          nativesessions.FileSystem
	nativeStoreRoots  []nativesessions.StoreRoot
	// sweepClock, set only by tests, adjusts the retention sweep's clock
	// checks (retention.Options.ServerClock and PreviousScanAt). A test that
	// moves Now months ahead moves only this machine's clock; the sweep rightly
	// refuses to delete by it unless the storage clock moves too.
	sweepClock func(*retention.Options)
	// observeFlags, set only by tests, sees every command flag set as it is
	// made, so a test can check each flag against the help text.
	observeFlags func(*commandFlags)
	// observeListBody, set only by tests, counts selected metadata/cache bodies.
	observeListBody func(string, bool)
	// exitProcess, set only by tests, replaces os.Exit where the session
	// browser exits on a signal.
	exitProcess func(int)
	// repoKey, set only by tests, replaces the git lookup of a project's
	// repository key (see repoKeyResolver).
	repoKey func(root string) string
	// gitHead, set only by tests, replaces the git lookup of the commit a
	// session's working directory has checked out (see gitHeadResolver).
	gitHead func(dir string, withDirty bool) (string, *bool)
	// projectGitRunner replaces bounded Git operations in setup tests.
	projectGitRunner gitremote.Runner
	// repoKeyContext replaces bounded setup lookups in tests.
	repoKeyContext func(context.Context, string) string
	// currentBranch, set only by tests, replaces the git lookup of the
	// branch checked out in a directory (see gitBranch).
	currentBranch func(dir string) string
	// openKeys, set only by tests, stands in for stdin read a key at a time
	// on the session browser's screens (see keyTerminal), or reports that
	// keys cannot be read, which keeps the browser reading lines. Defaults
	// to stdin itself when it is a terminal.
	openKeys func(stdin io.Reader) (keyTerminal, bool)
	// backfillCheckpoint, set only by tests, is called inside the
	// configuration commit between writing the batch file and saving the
	// configuration ("batch saved"), after the commit ("committed"), after
	// each registration hold ("registered"), before the upload
	// ("uploading"), and when undo holds its locks and has rechecked its plan
	// ("undoing"), has marked the batch undone but not saved the
	// configuration ("undo marked"), has saved it but not recorded the
	// changes in the batch ("undo configured"), and has recorded them but
	// removed no session ("undo recorded"). A test returns an error from it
	// to stop the import or undo there, as a crash would.
	backfillCheckpoint func(step string) error
	// backfillHoldSteps, when positive (only in tests), caps the steps
	// registration takes per hold of hooks.lock, to force several holds.
	backfillHoldSteps int
	// importCollectorWait, set only by tests, is given how long an import
	// would wait for collector.lock (backfillCollectorWait for backfill,
	// setupImportCollectorWait for setup) and returns the wait to use.
	importCollectorWait func(wait time.Duration) time.Duration
	// projectMatchTimeout, set only by tests, replaces matchProjects'
	// deadline (projectMatchDeadline), so a test of what the matcher finds
	// does not race the clock on a loaded machine.
	projectMatchTimeout time.Duration
	// exitOnSignal, set only by tests, stands in for exitOnSignal (the
	// function) when a signal stops backfill at once.
	exitOnSignal func(os.Signal)
	AWSProfiles  func() ([]AWSProfile, error)
	// AWSBuckets lists an AWS profile's buckets and reads their regions
	// for setup. Defaults to asking S3 with the profile's credentials.
	AWSBuckets func(profile, region string) (BucketFinder, error)
	// AWSBucketCreator opens the client setup creates a new S3 bucket with,
	// for profile, in region. Defaults to S3 with the profile's credentials.
	AWSBucketCreator func(profile, region string) (BucketCreator, error)
	// Cloudflare makes the client guided R2 creation uses for the pasted
	// bootstrap API token. Defaults to the real Cloudflare API.
	Cloudflare func(token string) cloudflare.API
	// RunTokenCommand replaces the bounded explicit management-token subprocess.
	RunTokenCommand func(context.Context, []string, []string) (string, error)
	// Pause waits between guided R2 creation's checks of a key Cloudflare
	// has only just made. Defaults to sleeping; tests skip the wait.
	Pause      func(time.Duration)
	WorkingDir func() (string, error)
	Home       func() (string, error)
	Now        func() time.Time
	// OpenStore builds the object store a collector pass publishes to, from
	// this machine's configured storage destination. Defaults to
	// openConfiguredStore, which resolves real AWS/R2 credentials.
	OpenStore func(config.Config) (storage.ObjectStore, error)
	// Executable returns the absolute path setup installs into hook
	// commands and the LaunchAgent. Defaults to os.Executable.
	Executable func() (string, error)
	// TempDir is the temporary folder setup refuses to install an
	// executable from, since it is cleared automatically. Defaults to
	// os.TempDir; tests set it because their executables live in theirs.
	TempDir func() string
	// UserHomeDir is the real user home directory — where hook config files
	// and ~/Library/LaunchAgents live — as distinct from Home, which is
	// agent-archive's own (possibly redirected) private data directory.
	// Defaults to os.UserHomeDir.
	UserHomeDir func() (string, error)
	// AccountHome is the account's home directory from the user database,
	// which overriding $HOME does not change. Only the data directory under
	// it is the default installation, with the default launchd label.
	// Defaults to os/user.Current's HomeDir.
	AccountHome func() (string, error)
	// HostFingerprint identifies the machine the command runs on, as
	// local.HostFingerprint does (Linux only; "": cannot say). Setup records
	// it beside the machine ID and status compares it, to notice a data
	// directory copied from another machine. Defaults to
	// local.HostFingerprint; it is read only on Linux.
	HostFingerprint func() string
	// MountTable returns this machine's mount table in the format of
	// /proc/self/mountinfo, which setup, setup --refresh and status read on
	// Linux to tell whether the data directory or the systemd unit directory
	// is on a network filesystem. Defaults to reading /proc/self/mountinfo; it
	// is read only on Linux, and a table that cannot be read stops nothing.
	MountTable func() ([]byte, error)
	// DetectHarnesses best-effort detects which applications appear
	// installed under a user home directory, to pre-select setup's
	// application prompts; the user can still include or exclude any of
	// them regardless of what this reports. Defaults to detectHarnesses.
	DetectHarnesses func(userHome string) []string
	// DiscoverApplications performs bounded, read-only installed-version
	// discovery. It must not inspect transcripts, install hooks, or use the network.
	DiscoverApplications func(userHome string) map[string]applicationDiscovery
	// Scheduler is the background job manager: it reports the collector's job
	// state, loads the job setup defined so scheduled collection starts
	// without a login/logout cycle, and stops it again (rolling setup back,
	// or during uninstall). Defaults to the backend the installation's
	// configuration records, through newScheduler: launchd on macOS (through
	// launchctl) and systemd on Linux (through systemctl --user); setup uses
	// this system's own and records it.
	Scheduler scheduler.Scheduler
	// choosesBackend is set for a command that picks the scheduler rather than
	// addressing the installation's recorded one (see choosingBackend).
	choosesBackend bool
	// Credentials opens the credential store setup saves R2 secrets to and
	// uninstall deletes them from. Defaults to credentials.OpenDefault: the
	// Keychain on macOS (which needs a cgo build), a private file under the
	// data directory elsewhere.
	Credentials func() (credentials.CredentialStore, error)
	// LookupEnv reads the process environment. `handoff --latest` uses it to
	// recognize the agent session it is running inside. Defaults to
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// UnsetEnv removes a variable from the process environment, so programs
	// setup starts later do not inherit it. Guided R2 creation uses it for
	// CLOUDFLARE_API_TOKEN once it has read it. Defaults to os.Unsetenv.
	UnsetEnv func(string) error
	// BackfillTempDirs are the temporary directories backfill skips. Nil
	// means this operating system's defaults (backfill.Environment.DefaultTempDirs) plus
	// $TMPDIR; tests set it because their files live in one.
	BackfillTempDirs []string
	// OS is the operating system backfill and the collector look for apps
	// of: the macOS-only backfill inputs and where Cursor keeps its data
	// (Cursor's database included) depend on it. Empty means
	// platform.Current; tests set it so a Mac's layout is exercised on any
	// OS.
	OS platform.OS
	// IsTerminal reports whether stdin or stdout is a terminal. backfill
	// redraws its progress line only on one; whether a command may also ask
	// questions there is Env.interactive, which the
	// AGENT_ARCHIVE_NONINTERACTIVE switch can turn off. Defaults to checking
	// the file descriptor.
	IsTerminal func(any) bool
	// TerminalSize reports the columns and rows of the terminal out writes
	// to, and ok=false when out is not a terminal or its size is unknown.
	// The session browser and pickers read it before each redraw to fit
	// the window. Defaults to asking the terminal.
	TerminalSize func(out io.Writer) (width, height int, ok bool)
	// RunPager runs a pager command with environment ("NAME=value") added
	// to the process's own, stdin as its input and stdout/stderr as its
	// output, until it exits or ctx is cancelled. list, show, status, and
	// purge plan use it for long text. Defaults to `sh -c command`. Tests
	// set it so a listing never spawns less.
	RunPager func(ctx context.Context, command string, environment []string, stdin io.Reader, stdout, stderr io.Writer) error
	// LessVersion reports the version of program (less on PATH, or a path
	// to it), which chooses the default pager's options; known is false
	// when it cannot be told. Defaults to running `program --version` once
	// per process and program.
	LessVersion func(program string) (version int, known bool)
	// LaunchHandoff runs a destination agent attached to this terminal and
	// waits for it to exit. Defaults to running spec.Binary with spec.Args
	// in spec.Dir with spec.Env. Tests replace it to avoid starting an agent.
	LaunchHandoff func(spec launchSpec, stdin io.Reader, stdout, stderr io.Writer) error
	// LookPath finds a destination agent or clipboard provider. Defaults to
	// exec.LookPath.
	LookPath func(string) (string, error)
	// RunGit runs `git -C dir args...` and returns its stdout, with its
	// stderr in the error. `handoff --worktree` uses it. Defaults to the git
	// on PATH; tests point it at temporary repositories.
	RunGit func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// Environ is the process environment a launched agent starts from,
	// less the calling agent's session variables. Defaults to os.Environ.
	Environ func() []string
	// OpenTerminal starts a launched agent in a new terminal window or tab
	// and returns where it opened. Defaults to termlaunch.Open. Tests
	// replace it so no window opens.
	OpenTerminal func(termlaunch.Spec) (string, error)
	// Clipboard replaces the clipboard's contents. Defaults to pbcopy on
	// macOS, or wl-copy, xclip, or xsel for a connected Linux desktop.
	Clipboard func([]byte) error
	// PairingClipboardRead reads clipboard contents for conditional cleanup only.
	PairingClipboardRead func() ([]byte, error)
	// PairingTerminal opens the private terminal for redirected bundle input.
	PairingTerminal func() (io.ReadWriteCloser, error)
	// PairingCode supplies hidden interactive code input in isolated tests.
	PairingCode func() (string, error)
	// PairingRepoRoot is a bounded source scope lookup, injected by tests.
	PairingRepoRoot func(context.Context, string) (string, error)
	// Interrupts delivers the signals that stop a command while it runs
	// (backfill while it plans, registers, and uploads, the full-screen
	// views until they restore the terminal, setup's storage check), and
	// stop ends the delivery. Defaults to os/signal for os.Interrupt,
	// SIGTERM, SIGHUP, and SIGQUIT.
	Interrupts func() (signals <-chan os.Signal, stop func())
	// RefreshCollectorWait is how long setup --refresh waits for a running
	// collector pass to finish before it refuses. Defaults to
	// refreshCollectorWait; tests shorten it.
	RefreshCollectorWait func() time.Duration
	// EffectiveUID is the user ID this process runs as. Defaults to
	// os.Geteuid; tests set it to run as root.
	EffectiveUID func() int
	// FileOwner is the user ID that owns a path (ok false when unknown).
	// Defaults to the file system's.
	FileOwner func(path string) (uid int, ok bool)
}

func (e Env) isTerminal(stream any) bool {
	if e.IsTerminal != nil {
		return e.IsTerminal(stream)
	}
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// interrupts is the signal set of the commands a person runs. SIGQUIT is in
// it so that a kill -QUIT from outside (Ctrl-\ is turned off while a screen
// reads keys) is answered like the others: the terminal is restored and the
// process exits with the shell's status for it, 131. Go's default for SIGQUIT
// is a goroutine dump, which would leave the terminal raw on the alternate
// screen. The collector and the hooks never register these, so their SIGQUIT
// keeps the default.
func (e Env) interrupts() (<-chan os.Signal, func()) {
	if e.Interrupts != nil {
		return e.Interrupts()
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	return signals, func() { signal.Stop(signals) }
}

func (e Env) refreshCollectorWait() time.Duration {
	if e.RefreshCollectorWait != nil {
		return e.RefreshCollectorWait()
	}
	return refreshCollectorWait
}

func (e Env) effectiveUID() int {
	if e.EffectiveUID != nil {
		return e.EffectiveUID()
	}
	return os.Geteuid()
}

func (e Env) fileOwner(path string) (uid int, ok bool) {
	if e.FileOwner != nil {
		return e.FileOwner(path)
	}
	return fileOwner(path)
}

func (e Env) unsetEnv(key string) error {
	if e.UnsetEnv != nil {
		return e.UnsetEnv(key)
	}
	return os.Unsetenv(key)
}

func (e Env) lookupEnv(key string) (string, bool) {
	if e.LookupEnv != nil {
		return e.LookupEnv(key)
	}
	return os.LookupEnv(key)
}

func (e Env) home() (string, error) {
	if e.Home != nil {
		return e.Home()
	}
	return local.Home()
}

func (e Env) readHome() (string, error) {
	if e.Home != nil {
		return e.Home()
	}
	return local.ReadHome()
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) openStore(cfg config.Config) (storage.ObjectStore, error) {
	defer trace.Start("open store").End()
	if e.OpenStore != nil {
		return e.OpenStore(cfg)
	}
	return openConfiguredStore(cfg, e.credentialStore)
}

func (e Env) openStoreContext(ctx context.Context, cfg config.Config) (storage.ObjectStore, error) {
	defer trace.Start("open store").End()
	if e.OpenStore != nil {
		return e.OpenStore(cfg)
	}
	return openConfiguredStoreContext(ctx, cfg, e.credentialStore)
}

func (e Env) executable() (string, error) {
	if e.Executable != nil {
		return e.Executable()
	}
	return os.Executable()
}

// accountHome is Env.AccountHome, or "" when it cannot be read, in which
// case no installation counts as the default one.
func (e Env) accountHome() string {
	lookup := e.AccountHome
	if lookup == nil {
		lookup = func() (string, error) {
			u, err := user.Current()
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		}
	}
	home, err := lookup()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return home
}

func (e Env) tempDir() string {
	if e.TempDir != nil {
		return e.TempDir()
	}
	return os.TempDir()
}

func (e Env) userHomeDir() (string, error) {
	if e.UserHomeDir != nil {
		return e.UserHomeDir()
	}
	return os.UserHomeDir()
}

// cursorDatabase is Cursor's state.vscdb under the user's home, which
// cursor-sqlite sessions are read from, for Env.OS. It is "" when the home
// can't be resolved or the system is not one the program knows; the collector
// reads "" as "the default for this process" (cursorstore.StateDatabase of
// the process's own home and platform.Current), which on an unknown system is
// "" again, so Cursor reads as not installed.
func (e Env) cursorDatabase() string {
	home, err := e.userHomeDir()
	if err != nil {
		return ""
	}
	provider, ok := e.agentRegistry().LookupNativePaths("cursor")
	if !ok {
		return ""
	}
	return provider.ProjectPaths(agentapi.NativePathEnvironment{Locations: agentapi.NativeLocations{UserHome: home}, OperatingSystem: e.operatingSystem(), Getenv: e.getenv}).Database
}

// getenv reads one variable of the Env's environment (LookupEnv; the process
// environment unless a test injects one). backfillEnvironment and
// cursorDatabase both read through it, so they can never disagree.
func (e Env) getenv(key string) string {
	v, _ := e.lookupEnv(key)
	return v
}

// operatingSystem is the operating system whose app locations backfill and
// the collector look for: OS, else the real one.
func (e Env) operatingSystem() platform.OS {
	return cmp.Or(e.OS, platform.Current())
}

func (e Env) detectHarnesses(userHome string) []string {
	if e.DetectHarnesses != nil {
		return e.DetectHarnesses(userHome)
	}
	return detectHarnesses(e.hookFiles(userHome))
}

func (e Env) discoverApplications(userHome string) map[string]applicationDiscovery {
	if e.DiscoverApplications != nil {
		return e.DiscoverApplications(userHome)
	}
	return discoverApplicationsWith(e.agentRegistry(), userHome, e.operatingSystem())
}

func (e Env) credentialStore() (credentials.CredentialStore, error) {
	if e.Credentials != nil {
		return e.Credentials()
	}
	return openCredentialStore()
}

// credentialOS is the platform whose credential store is opened and named:
// platform.Current. It is a variable so a test can see both platforms'
// wording and choices (credentialWords, credentials.OpenDefault) on any OS.
var credentialOS = platform.Current()

// openCredentialStore opens the platform's credential store (see
// credentials.OpenDefault): Env.Credentials's default. The package's tests
// replace it with one that fails the test, so a test that forgets to set
// Env.Credentials can never reach the real Keychain or write a credentials
// file into a real data directory.
var openCredentialStore = func() (credentials.CredentialStore, error) {
	return credentials.OpenDefault(credentials.OpenOptions{
		OS: credentialOS,
		Dir: func() (string, error) {
			home, err := local.ReadHome()
			if err != nil {
				return "", err
			}
			return credentials.FileStoreDir(home), nil
		},
	})
}

// notSetUp reports whether this machine is not archiving: it has no saved
// configuration, or uninstall left one with archiving disabled. It reads
// only, and says nothing when the data directory cannot be read.
func notSetUp(env Env) bool {
	home, err := env.readHome()
	if err != nil {
		return false
	}
	cfg, found, err := config.Load(home)
	return err == nil && (!found || !cfg.Archive.Enabled)
}

const usage = `Agent Archive — archive coding-agent sessions to your private storage.

Get started
  agent-archive setup       Configure apps, projects, and storage
  agent-archive machines    List, rename, or pair machines
  agent-archive status      Check capture and see what to do next

Manage capture
  agent-archive sync        Collect and upload pending changes now
  agent-archive pause       Pause collection, uploads, and cleanup
  agent-archive resume      Resume automatic capture
  agent-archive recover     Start a linked generation for a blocked transcript

Inspect history
  agent-archive list        Find archived sessions
  agent-archive show        Read a session's summary or transcript
  agent-archive stats       See your usage: tokens, cost, agents, projects
  agent-archive feedback    Add explicit feedback from a local file

Import history
  agent-archive backfill    Import sessions already on this machine
  agent-archive historical-project  Manage historical project permissions

Switch agents
  agent-archive handoff     Continue a session in another coding agent

Evaluate agents
  agent-archive eval        Export sessions for an evaluation tool (JSON Lines)

Maintenance
  agent-archive uninstall   Remove integrations; keep local data
  agent-archive purge       Review and remove unreferenced source objects

Run agent-archive COMMAND --help for options and examples.
Use --version to show the installed version.
You supply a private Cloudflare R2 or Amazon S3 bucket. No archive account needed.
Docs: https://github.com/wangjohn/agent-archive/tree/main/docs
`

// Run dispatches one CLI invocation and returns a process exit code. It
// never panics on malformed input; every command reports a problem through
// stderr and a nonzero exit code instead.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	stdin, stdout, stderr = defaultStreams(stdin, stdout, stderr)
	if len(args) == 0 {
		if !nonInteractiveSettingUsable(args, stderr, env) {
			return 2
		}
		if browseInteractive(env, stdin, stdout) && !notSetUp(env) {
			startTrace("list", stderr, env)
			defer finishTraceNow()
			return runListCommand(nil, stdin, stdout, stderr, env)
		}
		if notSetUp(env) {
			terminal.Println(stdout, "Not set up yet — run agent-archive setup.")
			terminal.Println(stdout)
		}
		terminal.Print(stdout, usage)
		return 0
	}
	if err := pairingInvocationError(args, env); err != nil {
		terminal.Println(stderr, err.Error())
		return 1
	}
	if handled, code := commandPreflight(args, stdout, stderr); handled {
		return code
	}
	// After the preflight, which answers `list --help` and its kind without
	// touching the environment, so the setting can still be looked up.
	if !nonInteractiveSettingUsable(args, stderr, env) {
		return 2
	}
	startTrace(args[0], stderr, env)
	defer finishTraceNow()

	switch args[0] {
	case "-h", "--help", "help":
		terminal.Print(stdout, usage)
		return 0
	case "-v", "--version", "version":
		terminal.Println(stdout, versionString())
		return 0
	case "_hook":
		return runHookCommand(args[1:], stdin, stderr, env)
	case "_collect":
		return runCollectCommand(args[1:], stdout, stderr, env)
	case "machines":
		return runMachinesWithInput(args[1:], stdin, stdout, stderr, env)
	case "status":
		return runStatusCommand(args[1:], stdout, stderr, env)
	case "recover":
		return runRecoverCommand(args[1:], stdout, stderr, env)
	case "sync":
		return runSyncCommand(args[1:], stdout, stderr, env)
	case "pause", "resume":
		if !env.newCommandFlags(args[0], stderr).parseFlagsOnly(args[1:]) {
			return 2
		}
		return runPauseCommand(stdout, stderr, env, args[0] == "pause")
	case "setup":
		return runSetupCommand(args[1:], stdin, stdout, stderr, env)
	case "uninstall":
		return runUninstallCommand(args[1:], stdin, stdout, stderr, env)
	case "list":
		return runListCommand(args[1:], stdin, stdout, stderr, env)
	case "show":
		return runShowCommand(args[1:], stdin, stdout, stderr, env)
	case "stats":
		return runStatsCommand(args[1:], stdin, stdout, stderr, env)
	case "feedback":
		return runFeedbackCommand(args[1:], stdout, stderr, env)
	case "handoff":
		return runHandoffCommand(args[1:], stdin, stdout, stderr, env)
	case "historical-project":
		return runHistoricalProjects(args[1:], stdout, stderr, env)
	case "backfill":
		return runBackfillCommand(args[1:], stdin, stdout, stderr, env)
	case "purge":
		return runPurgeCommand(args[1:], stdin, stdout, stderr, env)
	case "eval":
		return runEvalCommand(args[1:], stdin, stdout, stderr, env)
	default:
		terminal.Printf(stderr, "agent-archive: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// defaultStreams stands in for the streams a caller left nil: empty input
// and discarded output.
func defaultStreams(stdin io.Reader, stdout, stderr io.Writer) (in io.Reader, out, errOut io.Writer) {
	in, out, errOut = stdin, stdout, stderr
	if in == nil {
		in = strings.NewReader("")
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	return in, out, errOut
}

func (e Env) listBodyObserver() func(string, bool) { return e.observeListBody }
