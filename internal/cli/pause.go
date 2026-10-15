package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runPauseCommand implements both `pause` and `resume`. Pausing persists
// across restarts and stops new scheduled collection, uploads, and remote
// cleanup without deleting any existing configuration or archived data.
func runPauseCommand(stdout, stderr io.Writer, env Env, paused bool) int {
	command := "resume"
	if paused {
		command = "pause"
	}
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: %s: %s\n", command, fmt.Sprintf(format, args...))
		return 1
	}
	// Read-only checks first, so a command that cannot run leaves no data
	// directory behind.
	home, err := env.readHome()
	if err != nil {
		return fail("resolve the data directory: %v", err)
	}
	if setupjournal.TransactionPending(home) {
		return fail("no settings changed: %s", recoveryPending(home))
	}
	if _, found, err := config.Load(home); err != nil {
		return fail("%v", err)
	} else if !found {
		return fail("%v", errNotSetUp)
	}
	unlock, err := lockCollector(home, command, env.now())
	if err != nil {
		return fail("%s holds the collector lock. No settings changed; retry when it finishes", lockHolder(home))
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return fail("a hook is finishing. No settings changed; retry")
	}
	defer releaseHooks()
	releaseSearch, err := local.NamedLockWait(home, "search-worker.lock", 2*time.Second)
	if err != nil {
		return fail("search indexing is finishing. No settings changed; retry")
	}
	defer releaseSearch()
	// Again under the locks: setup or uninstall may have finished meanwhile.
	if setupjournal.TransactionPending(home) {
		return fail("no settings changed: %s", recoveryPending(home))
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail("%v", err)
	}
	if found && !cfg.Archive.Enabled {
		return fail("integrations are not installed. Run agent-archive setup to reinstall")
	}
	// A process could have stopped after saving Paused but before clearing
	// queued starts. Clear again while still paused before enabling hooks.
	if cfg.Paused && !paused {
		if err := capture.ClearAdmissionIntents(home); err != nil {
			return fail("could not clear pending session starts; still paused: %v", err)
		}
	}
	if _, err := config.SetPaused(home, paused, env.now()); err != nil {
		return fail("%v", err)
	}
	if paused {
		if err := capture.ClearAdmissionIntents(home); err != nil {
			return fail("paused, but could not clear pending session starts: %v", err)
		}
		terminal.Println(stdout, "Paused. Run `agent-archive resume` to continue. Already registered sessions can catch up, including activity written during the pause.")
	} else {
		terminal.Println(stdout, "Resumed. Registered sessions can catch up; new sessions begun while paused are not imported. Run agent-archive sync for an immediate pass.")
	}
	return 0
}
