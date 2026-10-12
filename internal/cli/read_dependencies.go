package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"io"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// Run passes Env at the command boundary. The read commands and their
// helpers declare only the capabilities they use; Env supplies the defaults.
type readOnlyStoreDependencies interface {
	readHome() (string, error)
	openStore(config.Config) (storage.ObjectStore, error)
}

type browseReadDependencies interface {
	metadataCacheDependencies
	interrupts() (<-chan os.Signal, func())
}

type metadataCacheDependencies interface {
	readHome() (string, error)
}

type sessionBrowseDependencies interface {
	interactive(any) bool
	terminalSizeDependencies
}

// terminalSizeDependencies is what the session pickers and browser read
// before each redraw to fit the window.
type terminalSizeDependencies interface {
	terminalSize(io.Writer) (width, height int, ok bool)
}

type sessionSelectionDependencies interface {
	interrupts() (<-chan os.Signal, func())
	metadataCacheDependencies
	terminalSizeDependencies
	scopeDependencies
	now() time.Time
}

// sessionBrowserDependencies is what the interactive session browser uses:
// the pager for transcripts and the details, the window's size, and
// interrupts so it can restore the screen before it exits, and stdin read
// a key at a time on a terminal.
type sessionBrowserDependencies interface {
	pagerDependencies
	terminalSizeDependencies
	interrupts() (<-chan os.Signal, func())
	exit(int)
	openKeyTerminal(io.Reader) (keyTerminal, bool)
}

type listCommandDependencies interface {
	readOnlyStoreDependencies
	sessionBrowserDependencies
	scopeDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

// statsBrowserDependencies is what the interactive stats screen uses: the
// alternate screen and its interrupts, the window's size before each redraw,
// stdin read a key at a time, the folder a saved page goes to, and the
// environment for its "~/".
type statsBrowserDependencies interface {
	altScreenDependencies
	terminalSizeDependencies
	openKeyTerminal(io.Reader) (keyTerminal, bool)
	workingDir() (string, error)
	lookupEnv(string) (string, bool)
}

// statsCommandDependencies is what `stats` uses: list's store and cache, the
// pager, the interactive screen, and the environment for the character set.
type statsCommandDependencies interface {
	readOnlyStoreDependencies
	pagerDependencies
	statsBrowserDependencies
	// isTerminal is whether stdout is a terminal, whatever the agent switch
	// says: stats --html never fills one with markup.
	isTerminal(any) bool
	interrupts() (<-chan os.Signal, func())
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type showCommandDependencies interface {
	readOnlyStoreDependencies
	sessionBrowserDependencies
	scopeDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type showQueryDependencies interface {
	metadataCacheDependencies
	sessionBrowserDependencies
	scopeDependencies
	now() time.Time
}

type pagerDependencies interface {
	interactive(any) bool
	lookupEnv(string) (string, bool)
	runPager(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
	lessVersion(string) (int, bool)
	// interrupts and exit let withPager leave Ctrl-C to the pager and
	// stop it on other signals.
	interrupts() (<-chan os.Signal, func())
	exit(int)
}

type handoffResolverDependencies interface {
	readHome() (string, error)
	openStore(config.Config) (storage.ObjectStore, error)
	now() time.Time
	cursorDatabase() string
	// repoKeyResolver looks up the repository key of a directory.
	repoKeyResolver() func(root string) string
}

type handoffFileDependencies interface {
	now() time.Time
}

type currentSessionDependencies interface {
	runtimeLookup() agentapi.RuntimeLookup
	lookupEnv(string) (string, bool)
}

type handoffOptionsDependencies interface {
	newCommandFlags(string, io.Writer) *commandFlags
}

type handoffTargetDependencies interface {
	interrupts() (<-chan os.Signal, func())
	handoffResolverDependencies
	currentSessionDependencies
	workingDirDependencies
}

type workingDirDependencies interface {
	workingDir() (string, error)
}

type nativeHandoffDependencies interface {
	tempDir() string
	nativeFiles() nativesessions.FileSystem
	nativeRoots(string) ([]nativesessions.StoreRoot, error)
	workingDirDependencies
	currentSessionDependencies
	sessionBrowserDependencies
	now() time.Time
}

type handoffCommandDependencies interface {
	loadHandoffConfig(string) (config.Config, bool, error)
	nativeHandoffDependencies
	handoffOptionsDependencies
	handoffTargetDependencies
	handoffCheckoutDependencies
	scopeDependencies
	sessionBrowserDependencies
	handoffLaunchDependencies
	handoffDestinationDependencies
}

type launchSpecDependencies interface {
	launcherLookup() agentapi.LauncherLookup
	runtimeLookup() agentapi.RuntimeLookup
	lookPath(string) (string, error)
	environ() []string
}

type handoffLaunchDependencies interface {
	launchSpecDependencies
	worktreeDependencies
	executable() (string, error)
	tempDir() string
	workingDirDependencies
	now() time.Time
	launchHandoff(launchSpec, io.Reader, io.Writer, io.Writer) error
	openTerminal(termlaunch.Spec) (string, error)
}

// handoffDestinationDependencies is what the destination prompt uses: which
// agents are installed, the pager for printing, the clipboard, and the
// directories a written file's path is resolved against.
type handoffDestinationDependencies interface {
	launchSpecDependencies
	pagerDependencies
	workingDirDependencies
	userHomeDir() (string, error)
	clipboard([]byte) error
	clipboardAvailable() bool
}
