package launchd

import (
	"context"
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

// ChangeTimeout bounds launchctl bootstrap and bootout. Setup --refresh
// absorbs Ctrl-C and SIGTERM while it stops and starts the job, so a launchctl
// that hangs must end on its own: the transaction then fails and rolls back
// (or, when launchctl stays hung, leaves its journal for the next setup),
// instead of a process nothing but SIGKILL can stop.
const ChangeTimeout = 30 * time.Second

// stateTimeout bounds launchctl print, which only asks.
const stateTimeout = 2 * time.Second

// DefaultPATH is the PATH launchd gives a job whose plist sets none.
const DefaultPATH = "/usr/bin:/bin:/usr/sbin:/sbin"

// collectorInterval is how often the collector's job runs.
const collectorInterval = time.Minute

// Scheduler is launchd, through launchctl. A job's definition is
// <user home>/Library/LaunchAgents/<label>.plist, which is where every
// collector plist, earlier release's plist and the prototype's job lives
// (PlistPath).
//
// It runs launchctl only through Run, and builds nothing that runs it: making
// a Scheduler executes no program.
type Scheduler struct {
	// Run runs launchctl, and is required.
	Run scheduler.Runner
	// ChangeTimeout is the bound on bootstrap and bootout; zero means the
	// package's ChangeTimeout. A test shortens it.
	ChangeTimeout time.Duration
}

// Name is "launchd", which the setup journal records for the jobs it made.
func (Scheduler) Name() string { return "launchd" }

// Words are launchd's nouns.
func (Scheduler) Words() scheduler.Words {
	return scheduler.Words{Manager: "launchd", Job: "LaunchAgent", Definition: "plist", Tool: "launchctl", Name: "label"}
}

// DefaultPATH is the PATH launchd gives a job whose plist sets none.
func (Scheduler) DefaultPATH() string { return DefaultPATH }

// Ref is the label of the installation's collector: LaunchLabel for the
// account's default installation, and otherwise the label derived from its
// data directory (CollectorLabel).
func (Scheduler) Ref(inst scheduler.Installation) scheduler.Ref {
	if inst.Role != scheduler.Collector && inst.Role != scheduler.Search {
		return ""
	}
	label := CollectorLabel(inst.DataHome, "")
	if inst.Default {
		label = LaunchLabel
	}
	if inst.Role == scheduler.Search {
		label = strings.Replace(label, LaunchLabel, "com.agent-archive.search", 1)
	}
	return scheduler.Ref(label)
}

// Locate is the site and the job of a plist path: <user home>/Library/LaunchAgents/<label>.plist,
// as every plist a release has journaled is (each has built them with
// filepath.Join, which gives back whatever $HOME's spelling). Any other path
// names no job the scheduler can be asked about, and is refused rather than
// taken for another plist.
func (Scheduler) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	site, ref := scheduler.Site{UserHome: filepath.Dir(filepath.Dir(filepath.Dir(definition)))}, scheduler.Ref(Label(definition))
	if PlistPath(site, ref) != definition {
		return site, ref, fmt.Errorf("%s is not a LaunchAgent plist (<home>/Library/LaunchAgents/<label>.plist); launchd was left as it is", definition)
	}
	return site, ref, nil
}

// Plan is the LaunchAgent plist of spec for inst, as bytes for one artifact,
// the file <user home>/Library/LaunchAgents/<label>.plist. It reads nothing
// and asks launchd nothing. Only the collector's job is defined under launchd:
// it runs the executable with `_collect` every minute, at load too, so a spec
// that says otherwise is refused rather than rendered as something else.
func (s Scheduler) Plan(site scheduler.Site, inst scheduler.Installation, spec scheduler.JobSpec) (scheduler.Plan, error) {
	command, err := inst.Role.Command()
	if err != nil {
		return scheduler.Plan{}, err
	}
	if !slices.Equal(spec.Args, []string{command}) || spec.Interval != collectorInterval || !spec.RunAtLoad {
		return scheduler.Plan{}, errors.New("a LaunchAgent runs the collector: _collect every minute, at load")
	}
	ref := s.Ref(inst)
	plist, err := launchAgent(spec.Executable, spec.DataHome, string(ref), spec.Env, inst.Role)
	if err != nil {
		return scheduler.Plan{}, err
	}
	return scheduler.Plan{Ref: ref, Artifacts: []scheduler.Artifact{scheduler.FileArtifact(PlistPath(site, ref), plist, 0o600)}}, nil
}

// Inspect asks launchd about the job ref names, by its label, reads
// launchctl's answer against the plist at site (see ParseJobState), and reads
// the plist itself (Definition).
func (s Scheduler) Inspect(ctx context.Context, site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	plist := PlistPath(site, ref)
	state, problem := s.probe(ctx, plist)
	status := s.Definition(site, ref)
	status.State, status.Problem = state, problem
	return status
}

// Definition reads the job's plist alone: whether it is there, the program it
// runs, and the environment it sets. A plist that cannot be read, or has no
// program or environment to read, is reported in DefinitionErr with what
// could be read still set, since a launchd job is loaded from the plist as it
// was, not as it is now.
func (Scheduler) Definition(site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	plist := PlistPath(site, ref)
	status := scheduler.Status{Paths: []string{plist}}
	data, err := os.ReadFile(plist)
	if errors.Is(err, os.ErrNotExist) {
		return status
	}
	status.Defined = true
	if err != nil {
		status.DefinitionErr = err
		return status
	}
	program, programErr := LaunchAgentProgram(data)
	environment, environmentErr := LaunchAgentEnvironment(data)
	if programErr == nil {
		status.Program = program
	}
	if environmentErr == nil {
		status.DataHome = environment["AGENT_ARCHIVE_HOME"]
		delete(environment, "AGENT_ARCHIVE_HOME")
		status.Env = environment
	}
	status.DefinitionErr = programErr
	if programErr == nil {
		status.DefinitionErr = environmentErr
	}
	return status
}

// Load loads the plist of the job ref names into this user's GUI session so
// scheduled collection starts immediately rather than waiting for the next
// login. A failure is reported as an incomplete setup, with rollback and a
// retry path.
//
// It runs on a context of its own, bounded by the change timeout, that
// neither ctx's cancellation nor its deadline reaches (context.WithoutCancel):
// an interrupt never stops a change halfway, since the setup journal handles
// what is half applied.
func (s Scheduler) Load(ctx context.Context, site scheduler.Site, ref scheduler.Ref) error {
	plist := PlistPath(site, ref)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.changeTimeout())
	defer cancel()
	output, err := s.Run(ctx, "launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	return nil
}

// Unload stops the job the plist of ref defines. It names the job by its
// service target (gui/UID/label), as Inspect checks it, rather than by the
// plist: bootout by path needs the file, and fails with a misleading
// "Input/output error" when the plist was deleted while the job stayed
// loaded. A label alone does not prove ownership, so it first confirms
// launchd loaded the job from the plist itself, and refuses otherwise, with a
// *scheduler.NotOwnedError or a *scheduler.IndeterminateError. Like Load,
// nothing of ctx but its values reaches it.
func (s Scheduler) Unload(ctx context.Context, site scheduler.Site, ref scheduler.Ref) error {
	plist := PlistPath(site, ref)
	ctx = context.WithoutCancel(ctx)
	switch state, problem := s.probe(ctx, plist); state {
	case scheduler.Loaded, scheduler.Running:
	case scheduler.Missing:
		return nil
	case scheduler.AnotherInstallation:
		return &scheduler.NotOwnedError{Words: s.Words(), Problem: *problem}
	case scheduler.Unknown:
		fallthrough
	default:
		return &scheduler.IndeterminateError{Words: s.Words(), Problem: *problem}
	}
	ctx, cancel := context.WithTimeout(ctx, s.changeTimeout())
	defer cancel()
	output, err := s.Run(ctx, "launchctl", "bootout", ServiceTarget(plist))
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, output)
	}
	return nil
}

func (s Scheduler) changeTimeout() time.Duration {
	if s.ChangeTimeout > 0 {
		return s.ChangeTimeout
	}
	return ChangeTimeout
}

// probe asks launchctl print about the job plist defines, and says what is
// wrong when it cannot be acted on: a Problem for a job launchd runs from
// another plist, and for one it cannot describe.
func (s Scheduler) probe(ctx context.Context, plist string) (scheduler.JobState, *scheduler.Problem) {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	output, err := s.Run(ctx, "launchctl", "print", ServiceTarget(plist))
	state := ParseJobState(string(output), err, plist)
	problem := &scheduler.Problem{Ref: scheduler.Ref(Label(plist)), Expected: plist}
	switch state {
	case scheduler.AnotherInstallation:
		problem.Kind, problem.LoadedFrom = scheduler.ProblemNotOwned, printedPath(string(output))
		problem.Fix = "Uninstall that installation first, or set AGENT_ARCHIVE_HOME to a directory of this installation's own"
		if problem.Ref == LegacyLaunchLabel {
			problem.Fix = prototypeFix
		}
	case scheduler.Unknown:
		problem.Kind = scheduler.ProblemCannotTell
		problem.Fix = "Check that launchctl print gui/$(id -u) works in Terminal"
	case scheduler.Loaded, scheduler.Running, scheduler.Missing:
		problem = nil
	}
	return state, problem
}

// PlistPath is the plist that defines the job ref names at site.
func PlistPath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(site.UserHome, "Library", "LaunchAgents", string(ref)+".plist")
}

// Label is the launchd label of the job a plist defines. Every job this tool
// loads is named after its label, so the file name is the label.
func Label(plist string) string {
	return strings.TrimSuffix(filepath.Base(plist), ".plist")
}

// ServiceTarget is launchd's name for the job a plist defines in this user's
// GUI session.
func ServiceTarget(plist string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), Label(plist))
}

// ParseJobState reads `launchctl print` output (and the error it failed with,
// if it did) for the job plist defines: Missing, Loaded, or Running when
// launchd loaded the label from plist itself; AnotherInstallation when it
// loaded it from another file; and Unknown when launchctl fails or names no
// file to compare.
func ParseJobState(output string, err error, plist string) scheduler.JobState {
	if err != nil {
		if strings.Contains(output, "Could not find service") {
			return scheduler.Missing
		}
		return scheduler.Unknown
	}
	loadedFrom := printedPath(output)
	switch {
	case loadedFrom == "":
		return scheduler.Unknown
	case !local.SameLocation(loadedFrom, plist):
		return scheduler.AnotherInstallation
	case strings.Contains(output, "state = running"):
		return scheduler.Running
	}
	return scheduler.Loaded
}

// printedPath is the plist `launchctl print` output says launchd loaded the
// job from: its first `path =` line, "" when it has none.
func printedPath(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "path = "); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
