package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// refreshCollectorWait is how long setup --refresh waits for a collector pass
// that is running: the background collector starts one every minute, and one
// that is uploading holds collector.lock for seconds, so an upgrade that
// landed then would otherwise fail through no fault of the person's. Longer
// than this, something is wrong with the pass, and refresh says to retry.
const refreshCollectorWait = 10 * time.Second

// refreshRefusalError is why setup --refresh changed nothing: it ran before
// anything was written, so the message ends by saying so.
type refreshRefusalError struct{ err error }

func (r *refreshRefusalError) Error() string { return r.err.Error() }

func (r *refreshRefusalError) Unwrap() error { return r.err }

func refuse(format string, args ...any) error {
	return &refreshRefusalError{err: fmt.Errorf(format, args...)}
}

// refreshPlan is what setup --refresh will change, and how to say so.
type refreshPlan struct {
	journal setupjournal.Journal
	// hookApps are the apps whose hook file changes, and hookFiles those files.
	hookApps  []string
	hookFiles []string
	// plist is true when the collector's LaunchAgent plist changes;
	// restarted, when its job is loaded and is stopped and started again to
	// run the new plist.
	plist     bool
	restarted bool
	// definition is what the scheduler calls that file ("plist", "unit file").
	definition string
	// skills are the skill files written or removed; left are the other
	// files at the skills' paths, which are not setup's and stay.
	skills []string
	left   []string
	// executable is the path the hooks now run, and previous the one the
	// configuration recorded, when that changes.
	executable string
	previous   string
}

// empty reports whether there is nothing to change.
func (p refreshPlan) empty() bool { return len(p.journal.Changes) == 0 }

// runSetupRefresh is setup --refresh: it brings the hook files, the
// collector's LaunchAgent plist and the skill files up to date for the
// saved configuration and the executable now running, and changes nothing
// else. It asks no question, needs no terminal, never touches storage,
// credentials, projects, retention, or the LaunchAgent's job load state,
// with one exception: a plist that changes for a job that is loaded is
// loaded again, so the job runs the new one. It returns the exit code.
func runSetupRefresh(out, errOut io.Writer, env Env, verbose bool) int {
	plan, userHome, err := refreshSetup(env)
	if err != nil {
		var refused *refreshRefusalError
		if errors.As(err, &refused) {
			err = fmt.Errorf("%w. Nothing was changed", err)
		}
		terminal.Printf(errOut, "agent-archive: setup --refresh: %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		var blocked *setupjournal.RecoveryBlockedError
		if errors.As(err, &blocked) {
			terminal.Println(errOut, blocked.Guidance())
		}
		return 1
	}
	if plan.empty() {
		terminal.Println(out, "nothing to refresh")
	} else {
		terminal.Println(out, plan.summary())
		if verbose {
			for _, change := range plan.journal.Changes {
				terminal.Printf(out, "  %s\n", displayPath(change.Path, userHome))
			}
		}
		if containsString(plan.hookApps, "codex") {
			terminal.Println(out, hookNextStep["codex"])
		}
	}
	for _, path := range plan.left {
		terminal.Print(out, leftSkillLine(agentskills.TitleFor(filepath.Base(filepath.Dir(path))), path, userHome))
	}
	return 0
}

// refreshSetup takes the locks setup takes, plans the refresh from the
// saved configuration, and applies it in one journaled transaction.
func refreshSetup(env Env) (plan refreshPlan, userHome string, err error) {
	// Read-only checks first, so a machine that never ran setup gets no data
	// directory from this command.
	home, err := env.readHome()
	if err != nil {
		return plan, "", fmt.Errorf("resolve the data directory: %w", err)
	}
	userHome, err = env.userHomeDir()
	if err != nil {
		return plan, "", err
	}
	saved, err := refreshableConfig(home)
	if err != nil {
		return plan, userHome, err
	}
	// Before the locks, which are file locks in the data directory.
	if problem := env.networkRefreshProblem(saved, home, userHome); problem != "" {
		return plan, userHome, refuse("%s", problem)
	}
	if problem := env.rootProblem(home, userHome); problem != "" {
		return plan, userHome, refuse("%s", problem)
	}
	exe, err := env.executable()
	if err != nil {
		return plan, userHome, refuse("cannot find the running agent-archive: %v", err)
	}
	// Every hook and the LaunchAgent will run this path, so one that is not
	// a lasting, runnable binary would leave capture dead as soon as this
	// exits: the check setup makes, and status's.
	if problem := env.temporaryExecutableProblem(exe); problem != "" {
		return plan, userHome, refuse("%s Refresh from an installed agent-archive (the installer, or a build in a lasting place)", problem)
	}
	if problem := executableProblem(exe); problem != "" {
		return plan, userHome, refuse("the running agent-archive at %s is %s, so the hooks cannot be pointed at it", exe, problem)
	}

	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return plan, userHome, refuse("another setup is running (%v); retry when it finishes", err)
	}
	defer release()
	unlock, err := lockCollectorWait(home, "setup --refresh", env.now(), env.refreshCollectorWait())
	if err != nil {
		return plan, userHome, refuse("%s holds the collector lock; retry when it finishes", lockHolder(home))
	}
	defer unlock()
	// A hook holds hooks.lock for milliseconds while it registers a session,
	// and an installer that upgraded in the middle of a busy session should
	// not fail for that.
	releaseHooks, err := local.NamedLockWait(home, "hooks.lock", min(env.refreshCollectorWait(), 2*time.Second))
	if err != nil {
		return plan, userHome, refuse("a hook is finishing; retry")
	}
	defer releaseHooks()
	// Again under the locks: setup or uninstall may have finished meanwhile.
	cfg, err := refreshableConfig(home)
	if err != nil {
		return plan, userHome, err
	}
	if err := protectSetupWriter(home, cfg); err != nil {
		return plan, userHome, err
	}
	if plan, err = planSetupRefresh(home, userHome, exe, cfg, env); err != nil {
		return plan, userHome, err
	}
	if plan.empty() {
		return plan, userHome, nil
	}
	// From here files change, and a job may be stopped, until the journal is
	// gone: a Ctrl-C, a closed terminal, or a SIGTERM in that time would leave
	// the transaction to be recovered by hand and capture stopped meanwhile.
	// Registering for the signals absorbs them until Commit returns.
	_, stopSignals := env.interrupts()
	defer stopSignals()
	err = setupjournal.Commit(home, plan.journal, env.backends())
	// A skill file removed leaves the directories written for it.
	agentskills.RemoveEmptyDirs(env.agentRegistry(), userHome, claudeConfigDir(env.installedHookFiles(userHome, cfg)))
	return plan, userHome, err
}

// rootProblem is why this process, running as root, must not refresh the
// installation in home and userHome, or "" when it may: the refresh would
// write hook files, skill files, the LaunchAgent plist, the journal and the
// locks as root, in a home directory that belongs to another user (sudo can
// keep HOME), and their apps and collector could then neither read nor
// replace them. install.sh already skips the refresh as root; this covers
// the person who runs it by hand.
func (e Env) rootProblem(home, userHome string) string {
	if e.effectiveUID() != 0 {
		return ""
	}
	for _, dir := range []string{userHome, home} {
		if uid, ok := e.fileOwner(dir); ok && uid != 0 {
			return fmt.Sprintf("this is running as root, but %s belongs to another user, so the refresh would leave root-owned files there that their apps cannot change. Run it as that user, without sudo", dir)
		}
	}
	return ""
}

// refreshableConfig is the saved configuration when there is one setup
// --refresh can refresh: setup completed, is not interrupted, and has not
// been uninstalled. Any other state has nothing installed to bring up to
// date, or is setup's to settle first.
func refreshableConfig(home string) (config.Config, error) {
	if setupjournal.TransactionPending(home) {
		return config.Config{}, refuse("%s", recoveryPending(home))
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return config.Config{}, err
	}
	if !found {
		return config.Config{}, refuse("%s", errNotSetUp)
	}
	if !cfg.Archive.Enabled {
		return config.Config{}, refuse("integrations are not installed (agent-archive was uninstalled); run agent-archive setup to install them again")
	}
	return cfg, nil
}

// planSetupRefresh plans the changes that bring cfg's installation up to
// date for the executable now running, exe. It reads files and asks
// launchd only about a job whose plist would change, and writes nothing.
func planSetupRefresh(home, userHome, exe string, cfg config.Config, env Env) (refreshPlan, error) {
	var plan refreshPlan
	in := env.installation(home, userHome)
	// Hooks are refreshed where setup recorded them: this command may run in
	// a shell that lacks the CLAUDE_CONFIG_DIR or CODEX_HOME setup saw.
	files := env.installedHookFiles(userHome, cfg)
	if problems := in.otherInstallationProblems(files, cfg.Harnesses); len(problems) > 0 {
		return plan, &refreshRefusalError{err: &otherInstallationError{problems: problems}}
	}
	hookChanges, err := hooks.Plan(files, in.hook(exe), cfg.Harnesses)
	if err != nil {
		return plan, refuse("%v", err)
	}
	var changes []hooks.Change
	for i, change := range hookChanges {
		if change.Existed && bytes.Equal(change.Before, change.After) {
			continue
		}
		changes = append(changes, change)
		plan.hookApps = append(plan.hookApps, cfg.Harnesses[i])
		plan.hookFiles = append(plan.hookFiles, change.Path)
	}
	next := cfg
	next.InstalledExecutable = exe
	claudeDir, dataHome := claudeConfigDir(files), in.commandDataHome()
	skillChanges, _, err := planAgentSkills(userHome, claudeDir, claudeDir, next, exe, dataHome, env.agentRegistry())
	if err != nil {
		return plan, refuse("%v", err)
	}
	changes = append(changes, skillChanges...)
	for _, change := range skillChanges {
		plan.skills = append(plan.skills, change.Path)
	}
	if !cfg.NoSkills {
		plan.left = leftSkillFiles(agentskills.Files(env.agentRegistry(), userHome, claudeDir, cfg.Harnesses, exe, dataHome), skillChanges)
	}
	definition := env.jobDefinition(userHome, in.ref())
	jobChanges, err := refreshJob(in, userHome, home, exe, definition)
	if err != nil {
		return plan, refuse("%v", err)
	}
	if len(jobChanges) > 0 {
		changes = append(changes, jobChanges...)
		plan.plist = true
		plan.definition = in.sched().Words().Definition
	}
	if cfg.InstalledExecutable != exe {
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return plan, err
		}
		change, err := fileChange(filepath.Join(home, "config.json"), append(data, '\n'))
		if err != nil {
			return plan, err
		}
		changes = append(changes, change)
		plan.executable, plan.previous = exe, cfg.InstalledExecutable
	}
	// The job is left as it is: no file change needs launchd, and none asks
	// it anything. A new plist for a loaded job is the one thing that does,
	// since launchd runs the definition it loaded, not the file.
	plan.journal = setupjournal.Journal{NativeAuthorityProtection: cfg.NativeAuthorityProtection, Changes: changes, Plist: definitionPath(definition), Backend: in.sched().Name(), JobRef: string(in.ref()), FilesOnly: true}
	if plan.plist {
		if err := planJobRestart(&plan, in, userHome, env); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// planJobRestart decides what the journal does with the job of a plist that
// changes: a job that is loaded is stopped and loaded again, so it runs the
// new plist (the ordinary setup transaction), and a job that is not loaded
// stays that way. A state it cannot read, or a job of another installation
// under this label, refuses, as setup does.
func planJobRestart(plan *refreshPlan, in installation, userHome string, env Env) error {
	ref, words := in.ref(), in.sched().Words()
	job := env.jobStatus(userHome, ref)
	switch {
	case job.State == scheduler.Unknown:
		return refuse("%s", unknownJobMessage(words, problemOf(job)))
	case job.State == scheduler.AnotherInstallation:
		return refuse("%s's %s job was loaded from a %s other than %s, so it belongs to another installation; refresh leaves it running and changes nothing", words.Manager, ref, words.Definition, problemOf(job).Expected)
	case jobActive(job.State):
		plan.journal.FilesOnly, plan.journal.WasLoaded, plan.restarted = false, true, true
	}
	return nil
}

// refreshJob is the change that points the collector's definition (the
// LaunchAgent plist) at exe, keeping everything else in it (the environment
// setup verified the storage with, which this shell may not have): it
// redefines the job from what the definition on disk says, with exe as its
// program. It changes every artifact the definition is in (a scheduler may
// keep a job in more than one file), and nothing when the definition already
// runs exe, or there is none: creating a definition is setup's.
func refreshJob(in installation, userHome, home, exe string, definition scheduler.Status) ([]hooks.Change, error) {
	if !definition.Defined {
		return nil, nil
	}
	unreadable := func(err error) error {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return err
		}
		return fmt.Errorf("%s cannot be read (%w); run agent-archive setup to write it again", definitionPath(definition), err)
	}
	if definition.Program == "" && definition.DefinitionErr != nil {
		return nil, unreadable(definition.DefinitionErr)
	}
	if definition.Program == exe {
		return nil, nil
	}
	if definition.DefinitionErr != nil {
		return nil, unreadable(definition.DefinitionErr)
	}
	plan, err := in.planJob(userHome, collectorJob(exe, home, definition.Env))
	if err != nil {
		return nil, err
	}
	return artifactChanges(plan.Artifacts)
}

// definitionPath is the file a job's definition is in, "" when the scheduler
// named none.
func definitionPath(status scheduler.Status) string {
	if len(status.Paths) == 0 {
		return ""
	}
	return status.Paths[0]
}

// leftSkillFiles are the paths of files that setup would write but the
// person's own file is there: not among changes, and not holding what it
// would write already.
func leftSkillFiles(files []agentskills.File, changes []hooks.Change) []string {
	written := map[string]bool{}
	for _, change := range changes {
		written[change.Path] = true
	}
	var left []string
	for _, f := range files {
		if written[f.Path] {
			continue
		}
		if current, err := os.ReadFile(f.Path); err != nil || !bytes.Equal(current, f.Content) {
			left = append(left, f.Path)
		}
	}
	return left
}

// leftSkillLine says setup left the file at path (of the skill titled title)
// as it is, because it is not this installation's.
func leftSkillLine(title, path, userHome string) string {
	return fmt.Sprintf("Left %s as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so %s is not installed there.\n", displayPath(path, userHome), title)
}

// summary is the one line that says what was refreshed.
func (p refreshPlan) summary() string {
	var parts []string
	if len(p.hookApps) > 0 {
		parts = append(parts, appList(p.hookApps)+" hooks")
	}
	switch {
	case p.restarted:
		parts = append(parts, "the background collector (restarted)")
	case p.plist:
		parts = append(parts, "the background collector's "+p.definition+" (its job is not loaded, and was left so)")
	}
	if len(p.skills) > 0 {
		parts = append(parts, countNoun(len(p.skills), "skill file"))
	}
	if len(parts) == 0 {
		parts = append(parts, "the recorded path of agent-archive")
	}
	line := "refreshed " + joinProse(parts)
	if p.executable != "" {
		line += "; agent-archive now runs from " + p.executable
		if p.previous != "" {
			line += " (it was " + p.previous + ")"
		}
	}
	return line
}

// joinProse joins parts as "a", "a and b", or "a, b, and c".
func joinProse(parts []string) string {
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
}

// refreshCompanions names the flags given besides --refresh and --verbose,
// spelled as the person types them, or "" when there are none: refresh
// takes no answers, so any other flag is a mistake, not something to
// ignore.
func refreshCompanions(fs *commandFlags) string {
	var other []string
	fs.Visit(func(f *flag.Flag) {
		//lint:ignore LV1001 flag names are the ones setup defines
		switch f.Name {
		case "refresh", "verbose":
		default:
			other = append(other, "--"+f.Name)
		}
	})
	return strings.Join(other, " and ")
}
