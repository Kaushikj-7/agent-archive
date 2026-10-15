// Package systemd is the Linux scheduler: the user's own systemd manager
// (systemctl --user), which runs the collector from a timer. It holds
// everything that knows systemd's vocabulary: the two unit files a job is
// (a oneshot service and the timer that starts it, written and read in
// unit.go), the unit names that are the jobs' refs, and the systemctl calls
// that ask about, load and stop a job (manage.go).
//
// It is pure over the scheduler.Runner it is given: it imports no os/exec,
// and constructing a Scheduler runs nothing.
package systemd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// ChangeTimeout bounds a load or an unload, as launchd's does: setup absorbs
// Ctrl-C while it changes a job, so a systemctl that hangs must end on its
// own, and the setup journal handles what was left half done.
const ChangeTimeout = 30 * time.Second

// stateTimeout bounds the questions Inspect asks together (the version, the
// units, lingering), which only read.
const stateTimeout = 3 * time.Second

// DefaultPATH is the PATH systemd's own compiled-in default gives a service
// that sets none: /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin, with
// /sbin:/bin after them (systemd.exec(5), $PATH; the last two are the
// default of a build for a system whose /bin and /sbin are not links into
// /usr, and harmless on one where they are).
//
// What a user manager's services actually get is its own PATH, which a
// distribution may extend: a service started by the user manager of Ubuntu
// 24.04 (systemd 255), run to see it, had
// /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games:/usr/local/games:/snap/bin,
// this value followed by what /etc/environment adds. It is never the login
// shell's: nothing in ~/.profile or ~/.bashrc reaches it, so no directory
// under the home (~/.local/bin, where aws-vault, 1Password's op and pipx
// installs go, or ~/go/bin, ~/.cargo/bin) is on it. This value is therefore
// what the collector's environment can rely on and no more, and the job's
// PATH is this shell's usable entries followed by it (cli's collectorPath),
// which is how a helper under the home is found.
const DefaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// collectorRef is the unit name (without .service or .timer) of the default
// installation's collector; another installation's is followed by "-" and the
// first 12 hex digits of the SHA-256 of its data directory, the digest the
// launchd label uses.
const collectorRef = "agent-archive-collector"

// collectorInterval is how often the collector's job runs.
const collectorInterval = time.Minute

// Scheduler is the user's systemd manager, through systemctl. A job's
// definition is two files in the user unit directory (see UnitDir):
// <ref>.service and <ref>.timer.
//
// It runs systemctl and loginctl only through Run, and builds nothing that
// runs them: making a Scheduler executes no program.
type Scheduler struct {
	// Run runs a program, and is required.
	Run scheduler.Runner
	// ChangeTimeout is the bound on a load and an unload; zero means the
	// package's ChangeTimeout. A test shortens it.
	ChangeTimeout time.Duration
}

// Name is "systemd", which the setup journal records for the jobs it made.
func (Scheduler) Name() string { return "systemd" }

// Words are systemd's nouns.
func (Scheduler) Words() scheduler.Words {
	return scheduler.Words{Manager: "systemd", Job: "user timer", Definition: "unit file", Tool: "systemctl", Name: "unit name"}
}

// DefaultPATH is the PATH systemd gives a service that sets none.
func (Scheduler) DefaultPATH() string { return DefaultPATH }

// Ref is the unit name of the installation's collector: agent-archive-collector
// for the account's default installation, and otherwise that followed by "-"
// and the first 12 hex digits of the SHA-256 of the cleaned data directory
// (the same digest launchd's CollectorLabel takes).
func (Scheduler) Ref(inst scheduler.Installation) scheduler.Ref {
	if inst.Role != scheduler.Collector && inst.Role != scheduler.Search {
		return ""
	}
	base := collectorRef
	if inst.Role == scheduler.Search {
		base = "agent-archive-search"
	}
	if inst.Default {
		return scheduler.Ref(base)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(inst.DataHome)))
	return scheduler.Ref(base) + "-" + scheduler.Ref(hex.EncodeToString(sum[:])[:12])
}

// validRef reports whether ref is a name Ref could have made: a ref reaches
// file paths and systemctl's arguments, so nothing else is accepted.
func validRef(ref scheduler.Ref) bool {
	suffix, ok := strings.CutPrefix(string(ref), collectorRef)
	if !ok {
		suffix, ok = strings.CutPrefix(string(ref), "agent-archive-search")
	}
	if !ok {
		return false
	}
	if suffix == "" {
		return true
	}
	digits, ok := strings.CutPrefix(suffix, "-")
	return ok && len(digits) == 12 && strings.Trim(digits, "0123456789abcdef") == ""
}

// unitDir is the user unit directory under a home: .config/systemd/user.
var unitDir = filepath.Join(".config", "systemd", "user")

// UnitDir is where the site's user's own units go: <user home>/.config/
// systemd/user, whatever this process's XDG_CONFIG_HOME says. The user
// manager searches $XDG_CONFIG_HOME/systemd/user instead only when its own
// environment has the variable (systemd.unit(5), "Unit File Load Path"),
// and then searches it in place of ~/.config/systemd/user, not as well. Its
// own environment is the one it started with, from PID 1 and the login's
// PAM session: environment.d files and `systemctl --user set-environment`
// change only what its services get. A shell's export never reaches it, so
// the shell's XDG_CONFIG_HOME would name a directory the manager does not
// search. A manager that does have another XDG_CONFIG_HOME (set by pam_env
// or a user@.service drop-in) does not find the units, Load fails with
// systemctl's "unit file ... does not exist" and this directory, and setup
// rolls back. A
// sandbox or a test that names another home keeps its units under that home.
func (Scheduler) UnitDir(site scheduler.Site) string {
	return filepath.Join(site.UserHome, unitDir)
}

func (s Scheduler) servicePath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(s.UnitDir(site), string(ref)+".service")
}

func (s Scheduler) timerPath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(s.UnitDir(site), string(ref)+".timer")
}

// timersWantsDir is the directory in the unit directory where `systemctl
// enable` links a timer whose [Install] says WantedBy=timers.target.
const timersWantsDir = "timers.target.wants"

// enableLink is the symbolic link `systemctl --user enable` makes for the
// job's timer, and the third thing a job is on disk besides its two unit
// files: <unit directory>/timers.target.wants/<ref>.timer, pointing at the
// timer's file. Unload's `disable` removes it; one is left when the unit files
// were deleted without it (uninstall --skip-scheduler, or a rollback after
// the manager could not be asked), and then it dangles, which systemd reports
// at every start of the manager.
//
// ownEnableLink is its path when it exists and is this job's own: a symbolic
// link whose target is the job's timer file (its absolute path, or a relative
// one resolved against the directory the link is in, each compared as a
// location, so a dangling one counts). Anything else at that path (a file, a
// directory, a link that points at another home's timer or at some other
// unit) is not ours to remove, and it is not returned.
func (s Scheduler) ownEnableLink(site scheduler.Site, ref scheduler.Ref) (string, bool) {
	link := filepath.Join(s.UnitDir(site), timersWantsDir, string(ref)+".timer")
	target, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if !local.SameLocation(target, s.timerPath(site, ref)) {
		return "", false
	}
	return link, true
}

// Locate is the site and the job of a unit file path, the service's or the
// timer's: <home>/.config/systemd/user/<ref>.service or .timer, the inverse
// of UnitDir. A path no Plan could have written is refused rather than taken
// for another unit.
func (Scheduler) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	refuse := fmt.Errorf("%s is not a unit file of this tool in a user unit directory (<home>/.config/systemd/user/%s[-<12 hex digits>].service or .timer); systemd was left as it is", definition, collectorRef)
	stem, ok := strings.CutSuffix(definition, ".service")
	if !ok {
		stem, ok = strings.CutSuffix(definition, ".timer")
	}
	if !ok || !filepath.IsAbs(definition) || filepath.Clean(definition) != definition {
		return scheduler.Site{}, "", refuse
	}
	dir, ref := filepath.Split(stem)
	home, ok := strings.CutSuffix(filepath.Clean(dir), string(filepath.Separator)+unitDir)
	if !ok || !validRef(scheduler.Ref(ref)) {
		return scheduler.Site{}, "", refuse
	}
	if home == "" {
		home = string(filepath.Separator)
	}
	return scheduler.Site{UserHome: home}, scheduler.Ref(ref), nil
}

// Plan is the collector's two unit files for inst at site, as artifacts:
// <ref>.service, then <ref>.timer, in the unit directory. It reads nothing
// and asks systemd nothing. Only the collector's job is defined: it runs the
// executable with `_collect` every minute, at load too, so a spec that says
// otherwise is refused rather than rendered as something else.
//
// The files are private (0600), as launchd's plists are, though the unit
// convention is 0644: they hold no credential, but the environment may hold
// a proxy address with its user name and password in it, and the user manager,
// which runs as the user, reads them either way.
func (s Scheduler) Plan(site scheduler.Site, inst scheduler.Installation, spec scheduler.JobSpec) (scheduler.Plan, error) {
	command, err := inst.Role.Command()
	if err != nil {
		return scheduler.Plan{}, err
	}
	if !slices.Equal(spec.Args, []string{command}) || spec.Interval != collectorInterval || !spec.RunAtLoad {
		return scheduler.Plan{}, errors.New("a systemd user timer runs the collector: _collect every minute, at load")
	}
	ref := s.Ref(inst)
	service, err := renderRoleService(spec.Executable, spec.DataHome, spec.Env, inst.Role)
	if err != nil {
		return scheduler.Plan{}, err
	}
	return scheduler.Plan{Ref: ref, Artifacts: []scheduler.Artifact{
		scheduler.FileArtifact(s.servicePath(site, ref), service, 0o600),
		scheduler.FileArtifact(s.timerPath(site, ref), renderRoleTimer(string(ref), inst.Role), 0o600),
	}}, nil
}

// Installed is the installation's own job: systemd has no earlier names to
// look for, so there are no aliases, and nothing is asked of the disk or the
// manager.
func (s Scheduler) Installed(_ context.Context, _ scheduler.Site, inst scheduler.Installation) ([]scheduler.Job, error) {
	return []scheduler.Job{{Ref: s.Ref(inst)}}, nil
}

// Definition reads the job's unit files alone, never the manager's view of
// them: whether they are there, the program the service runs and the
// environment it sets. `systemctl show` would fold in a drop-in's settings,
// which refresh must not bake into a new unit. A service that cannot be read,
// or has no program to read, is reported in DefinitionErr, with what could be
// read still set. A timer with no service is a definition with no program.
func (s Scheduler) Definition(site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	if !validRef(ref) {
		return scheduler.Status{DefinitionErr: fmt.Errorf("%q is not a job of this tool", ref)}
	}
	servicePath, timerPath := s.servicePath(site, ref), s.timerPath(site, ref)
	status := scheduler.Status{Paths: []string{servicePath, timerPath}}
	if link, ok := s.ownEnableLink(site, ref); ok {
		status.Paths = append(status.Paths, link)
	}
	data, err := os.ReadFile(servicePath)
	_, timerErr := os.Stat(timerPath)
	serviceAbsent := errors.Is(err, os.ErrNotExist)
	if serviceAbsent && errors.Is(timerErr, os.ErrNotExist) {
		return status
	}
	status.Defined = true
	switch {
	case serviceAbsent:
		status.DefinitionErr = fmt.Errorf("the service unit %s is missing", servicePath)
	case err != nil:
		status.DefinitionErr = err
	default:
		program, environment, readErr := readService(data)
		status.DefinitionErr = readErr
		if readErr == nil {
			status.Program, status.DataHome = program, environment["AGENT_ARCHIVE_HOME"]
			delete(environment, "AGENT_ARCHIVE_HOME")
			status.Env = environment
		}
	}
	return status
}

func (s Scheduler) changeTimeout() time.Duration {
	if s.ChangeTimeout > 0 {
		return s.ChangeTimeout
	}
	return ChangeTimeout
}
