package cli

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// installation locates one data directory's integrations: the hook command
// its hooks run and its background collector's LaunchAgent.
type installation struct {
	role     scheduler.Role
	ports    agentapi.HooksLookup
	home     string // the data directory
	userHome string // $HOME, where LaunchAgents and app configs live
	// accountHome is the account's own home directory from the user
	// database, which a sandbox that only overrides $HOME does not change.
	accountHome string
	// sched is the scheduler that names this installation's job, made when
	// asked (making one runs nothing, but most callers never need it).
	sched func() scheduler.Scheduler
}

func (e Env) installation(home, userHome string) installation {
	return installation{ports: e.agentRegistry(), home: home, userHome: userHome, accountHome: e.accountHome(), sched: e.scheduler}
}

func (in installation) searchRole() installation {
	in.role = scheduler.Search
	return in
}

// defaultDataHome is the data directory of the account's own default
// installation: ~/.local/share/agent-archive under the account's real home,
// spelled canonically (local.CanonicalPath).
func (in installation) defaultDataHome() string {
	return local.CanonicalPath(filepath.Join(in.accountHome, ".local", "share", "agent-archive"))
}

// isDefault reports whether this is the account's default installation,
// however its directory is spelled (a symlink, or another case on a
// case-insensitive volume): the same test hooks use to tell installations
// apart (local.SameLocation). Anything else (AGENT_ARCHIVE_HOME set
// elsewhere, or a sandbox that overrides $HOME and so moves the data
// directory with it) is not, and gets labels and hook commands of its own.
func (in installation) isDefault() bool {
	return in.accountHome != "" && local.SameLocation(in.home, in.defaultDataHome())
}

// hook is what setup installs into the apps' hook files: a non-default data
// directory travels in the command. It also identifies this installation's
// handlers among other installations' in the same files (see hooks.Hook).
func (in installation) hook(executable string) hooks.Hook {
	dataHome, defaultHome := "", ""
	if !in.isDefault() {
		dataHome = in.home
	}
	if in.accountHome != "" {
		defaultHome = in.defaultDataHome()
	}
	ports := in.ports
	if ports == nil {
		ports = productionAgents
	}
	return hooks.Hook{Ports: ports, Executable: executable, DataHome: dataHome, DefaultDataHome: defaultHome}
}

// commandDataHome is the AGENT_ARCHIVE_HOME this installation's commands
// run with (see hook): "" for the default one. Its /handoff skills name it
// too, which is how each installation tells its own apart.
func (in installation) commandDataHome() string { return in.hook("").DataHome }

// owner identifies this installation's hook handlers, for removing them;
// the executable they run does not matter there.
func (in installation) owner() hooks.Hook { return in.hook("") }

// otherInstallationProblems describes, one message per file, the hook
// handlers another installation (another data directory) put in the hook
// files of apps: setup will not install beside them, and uninstall and
// status leave them alone. A file that cannot be read is skipped; setup and
// status report that on their own.
func (in installation) otherInstallationProblems(files hooks.Files, apps []string) []string {
	var problems []string
	for _, app := range apps {
		others, err := hooks.OtherInstallations(files, in.owner(), app)
		if err != nil || len(others) == 0 {
			continue
		}
		problems = append(problems, describeOtherInstallations(files[app], app, others))
	}
	return problems
}

// describeOtherInstallations says whose handlers are in path and how to
// resolve it: every command that finds another installation's hooks
// describes them this way.
func describeOtherInstallations(path, app string, others []hooks.OtherInstallation) string {
	var owners, fixes []string
	for _, other := range others {
		switch {
		case other.Command != "":
			owners = append(owners, fmt.Sprintf("an edited agent-archive hook whose data directory cannot be read (%s)", other.Command))
			fixes = append(fixes, "remove that hook from the file by hand")
		case other.Default:
			owner := "the default installation"
			if other.DataHome != "" {
				owner += " in " + other.DataHome
			}
			owners = append(owners, owner)
			fixes = append(fixes, "run agent-archive uninstall with AGENT_ARCHIVE_HOME unset")
		default:
			owners = append(owners, "the installation in "+other.DataHome)
			fixes = append(fixes, fmt.Sprintf("run AGENT_ARCHIVE_HOME=%s agent-archive uninstall", shellQuote(other.DataHome)))
		}
	}
	return fmt.Sprintf("%s holds %s hooks of another agent-archive installation (%s), which this installation never changes. Setup installs no %s hooks beside them, since two installations would each capture every session. To remove them, %s. To test an installation on its own, give it its own HOME (or CLAUDE_CONFIG_DIR and CODEX_HOME), so the apps' hook files are separate too.",
		path, appName(app), strings.Join(owners, "; "), appName(app), strings.Join(fixes, "; "))
}

// shellQuote quotes s for a shell command line the user copies, only when it
// needs quoting.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+~") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// schedulerInstallation is the identity the scheduler derives this
// installation's job from.
func (in installation) schedulerInstallation() scheduler.Installation {
	return scheduler.Installation{DataHome: local.CanonicalPath(in.home), Default: in.isDefault(), Role: in.role}
}

// ref is the background collector's job: the scheduler names it from the
// installation (launchd's label is the default one only for the default
// installation).
func (in installation) ref() scheduler.Ref { return in.sched().Ref(in.schedulerInstallation()) }

// installed is the jobs this installation has under its scheduler: its own
// current job first, then its aliases (see scheduler.Inspector.Installed).
// The error is what blocks setup; status and uninstall ignore it, since they
// never touch the prototype's job, and use the jobs returned with it.
func (in installation) installed(userHome string) ([]scheduler.Job, error) {
	return in.sched().Installed(context.Background(), userSite(userHome), in.schedulerInstallation())
}

// hookFiles resolves each app's hook file from the environment this command
// runs in (CLAUDE_CONFIG_DIR, CODEX_HOME); see hooks.ResolveFiles.
func (e Env) hookFiles(userHome string) hooks.Files {
	return hooks.ResolveFiles(userHome, e.lookupEnv, e.agentRegistry())
}

// legacyHookFiles are the fixed paths every release before hook_files
// installed into, whatever CLAUDE_CONFIG_DIR or CODEX_HOME said.
func legacyHookFiles(userHome string, lookups ...agentapi.HooksLookup) hooks.Files {
	ports := agentapi.HooksLookup(productionAgents)
	if len(lookups) > 0 {
		ports = lookups[0]
	}
	return hooks.ResolveFiles(userHome, func(string) (string, bool) { return "", false }, ports)
}

// installedHookFiles is where setup installed each app's hooks: the paths it
// recorded, so status and uninstall find them from a shell without the
// variables setup saw. An app a configuration from before the record has no
// entry for was installed at its legacy path, never where the current
// environment points.
func (e Env) installedHookFiles(userHome string, cfg config.Config) hooks.Files {
	files := legacyHookFiles(userHome, e.agentRegistry())
	for app, path := range cfg.HookFiles {
		if filepath.IsAbs(path) {
			files[app] = path
		}
	}
	return files
}
