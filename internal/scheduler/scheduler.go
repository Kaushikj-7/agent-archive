// Package scheduler is the port between agent-archive and whatever runs its
// background collector: launchd on macOS today, and another manager for
// another system as its adapter arrives. It holds the types the commands
// speak in (a job's Ref, where its definitions live, its JobState, the desired
// state of a job as a JobSpec and a Plan) and the interfaces every adapter
// implements, and nothing that runs a program: an adapter runs its manager's
// tool through a Runner it is given, so this package and everything that only
// names it is pure (TestSchedulerImportBoundary). The adapters live under this
// directory (internal/scheduler/launchd), and internal/scheduler/host chooses
// one for the system the program runs on.
package scheduler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Ref names one background job to its scheduler. It is opaque to callers and
// minted by the adapter's own vocabulary: for launchd, the job's label.
type Ref string

// Site is where a scheduler looks for job definitions: the user's own home,
// which is not the data directory. It is per call, not per scheduler, because
// a setup interrupted under one $HOME is recovered under another (a sandbox
// overrides $HOME, and so the definitions' directory, but not the manager).
type Site struct{ UserHome string }

// JobState is what a scheduler says about a job. The values are what
// `status --json` reports as `background` (which adds "broken" of its own),
// and what setup's journal records.
type JobState string

const (
	// Loaded is a job the scheduler has loaded from this site's own
	// definition and is not running at the moment.
	Loaded JobState = "loaded"
	// Running is a loaded job that is running now.
	Running JobState = "running"
	// Missing is a job the scheduler does not have loaded.
	Missing JobState = "missing"
	// Unknown is a job the scheduler cannot say anything about: the manager
	// is unreachable, or its answer names no definition to compare.
	Unknown JobState = "unknown"
	// AnotherInstallation is a job of the same name that the scheduler loaded
	// from another definition, which belongs to another installation.
	AnotherInstallation JobState = "another_installation"
)

// Active reports whether the state is a job the scheduler has loaded, whether
// or not it is running at the moment.
func (s JobState) Active() bool { return s == Loaded || s == Running }

// Installation is what an adapter derives a job's Ref from: the data
// directory one installation of the tool archives into.
type Installation struct {
	// Role selects one of the two managed jobs. Zero preserves collector identity.
	Role Role

	// DataHome is the data directory, canonical (local.CanonicalPath).
	DataHome string
	// Default is whether this is the account's own installation, which keeps
	// the job an earlier release gave every installation. cli decides it.
	Default bool
}

// Role is one of the installation's finite background responsibilities.
type Role string

const (
	// Collector is the original job, retained as the zero value.
	Collector Role = ""
	// Search maintains the optional local content index.
	Search Role = "search"
)

// Command returns the only command allowed for a managed role.
func (r Role) Command() (string, error) {
	switch r {
	case Collector:
		return "_collect", nil
	case Search:
		return "_search-index", nil
	default:
		return "", fmt.Errorf("unknown background role %q", r)
	}
}

// JobSpec is the job an adapter defines: what runs, with what environment,
// how often. Everything is a value; an adapter turns it into its manager's
// definition (Plan) and reads nothing else, so a definition holds only what a
// JobSpec holds.
type JobSpec struct {
	// Executable is the absolute path of the program the job runs.
	Executable string
	// Args are its arguments: "_collect".
	Args []string
	// DataHome is the absolute data directory the job runs for. It reaches the
	// job as AGENT_ARCHIVE_HOME, and the adapter derives the job's log files
	// from it (collector.log, collector-error.log).
	DataHome string
	// Env is the environment the job runs with besides AGENT_ARCHIVE_HOME,
	// which the adapter always sets from DataHome (naming it here is an
	// error). It never carries credentials: a definition is a file in the
	// user's home, so it holds file locations, a PATH and settings.
	Env map[string]string
	// Interval is how often the job runs.
	Interval time.Duration
	// RunAtLoad starts the job once as soon as it is loaded.
	RunAtLoad bool
}

// Artifact is one thing a definition changes. Version 1 of the port is
// file-only: ID is "file:" and the absolute path (see FileArtifact). A backend
// whose definition is not a file (a crontab, a registered task) would need an
// applier on Controller to read what is there and apply After; that is not
// built until such a backend exists.
type Artifact struct {
	ID    string
	After []byte
	Mode  os.FileMode
}

// fileScheme is the prefix of an Artifact ID that names a file.
const fileScheme = "file:"

// FileArtifact is the artifact that puts after in the file at path.
func FileArtifact(path string, after []byte, mode os.FileMode) Artifact {
	return Artifact{ID: fileScheme + path, After: after, Mode: mode}
}

// Path is the file an artifact writes, and false for an artifact that is not
// a file.
func (a Artifact) Path() (string, bool) { return strings.CutPrefix(a.ID, fileScheme) }

// Plan is what defining a job changes: the job's Ref and the artifacts that
// hold its definition. It is pure: an adapter builds it from an installation
// and a JobSpec alone, reading no file and asking no manager. Shared code
// reads what each artifact holds now (the journal's before) and applies them.
type Plan struct {
	Ref       Ref
	Artifacts []Artifact
}

// ProblemKind says what kind of Problem a job has.
type ProblemKind string

const (
	// ProblemNotOwned is a job the manager runs from a definition that is not
	// this installation's.
	ProblemNotOwned ProblemKind = "not_owned"
	// ProblemCannotTell is a job the manager cannot be asked about.
	ProblemCannotTell ProblemKind = "cannot_tell"
)

// Problem is the facts behind a state that blocks a command, not a sentence:
// the commands' messages are templates in cli, filled from these facts and the
// adapter's Words.
type Problem struct {
	// Kind is ProblemNotOwned or ProblemCannotTell.
	Kind ProblemKind
	// Ref is the job.
	Ref Ref
	// LoadedFrom is the definition the manager loaded the job from, for a job
	// that is not owned; empty when the manager did not say.
	LoadedFrom string
	// Expected is the definition this installation expects the job to be
	// loaded from.
	Expected string
	// Reason is what is wrong, as a clause the commands put in their own
	// sentence ("the user manager cannot be reached"), for an adapter that can
	// say more than that the manager did not answer. Empty when it cannot, and
	// the commands' own words stand.
	Reason string
	// Fix is the adapter's next step, as a sentence without its full stop.
	Fix string
	// Manual is the command that stops the job by hand, from a session that can
	// reach the manager, for a job the manager cannot be asked about here:
	// uninstall prints it when it goes on without verifying the job stopped.
	// Empty when there is none.
	Manual string
}

// Words is the small set of nouns a scheduler goes by, for the messages that
// name it: nouns only, no verbs and no plurals.
type Words struct {
	// Manager is the scheduler: "launchd".
	Manager string
	// Job is what it runs: "LaunchAgent".
	Job string
	// Definition is the file that defines a job: "plist".
	Definition string
	// Tool is the command that drives it: "launchctl".
	Tool string
	// Name is what a job is called to its manager: "label".
	Name string
}

// Status is everything a scheduler can say about one job. Program, Env and
// DataHome come from the definition on disk whatever the State is, since
// launchd reports a job whose program is gone as loaded.
type Status struct {
	State JobState
	// Defined is whether a definition for this job exists on disk.
	Defined bool
	// Program is the executable the definition runs, "" when it cannot be
	// read.
	Program string
	// Env is the environment the definition sets, without AGENT_ARCHIVE_HOME
	// (that is DataHome), so it round-trips into JobSpec.Env. It is nil when
	// it cannot be read, and empty, not nil, when the definition sets none.
	Env map[string]string
	// DataHome is the AGENT_ARCHIVE_HOME the definition sets, "" when it sets
	// none.
	DataHome string
	// Paths are the files the definition is in, which uninstall removes.
	Paths []string
	// Problem is set for unknown and another_installation.
	Problem *Problem
	// Degraded says what works, but not robustly.
	Degraded []string
	// DefinitionErr is why a definition that exists could not be read or
	// understood, which refresh tells apart from an absent one: the error of
	// the read, or of the first part that could not be parsed (Program is set
	// when the program was read before it).
	DefinitionErr error
}

// Alias says why a job that is not an installation's own current one is among
// its jobs: setup retires each of them in favor of the current job.
type Alias string

const (
	// EarlierLabel is a collector an earlier release installed for the same
	// data directory under another name.
	EarlierLabel Alias = "earlier_label"
	// Prototype is the job of the tool this one replaced (the prototype's
	// upload job), which the account's default installation retires.
	Prototype Alias = "prototype"
)

// Job is one job of an installation's: its Ref and, for a job that is not
// its own current one, why it is among them. Alias is empty for the current
// job.
type Job struct {
	Ref   Ref
	Alias Alias
}

// Retiree is a job setup retires, as found: shared code builds it, from
// Installed for the jobs and Inspect for their state and files, so it can
// name another backend's job. It lives in the setup journal, which resolves
// Backend to that backend's Controller to stop and restore it.
type Retiree struct {
	// Backend is the name of the scheduler that runs the job (Definer.Name).
	Backend string
	Ref     Ref
	Alias   Alias
	// Artifacts are the job's definition as found: After holds what was
	// there (what recovery puts back) and Mode its permissions. Retiring the
	// job deletes each.
	Artifacts []Artifact
	// WasLoaded is whether the scheduler had the job loaded, running or not.
	WasLoaded bool
}

// Definer defines jobs: the desired state, without touching anything.
type Definer interface {
	// Name is the adapter's name, which the setup journal records to find its
	// adapter again ("launchd").
	Name() string
	// Words are the nouns messages name the scheduler with.
	Words() Words
	// Ref is the job of the installation: for launchd, its label.
	Ref(inst Installation) Ref
	// Plan renders the definition of spec for inst at site, purely.
	Plan(site Site, inst Installation, spec JobSpec) (Plan, error)
	// DefaultPATH is the PATH a job gets when its definition sets none.
	DefaultPATH() string
	// Locate is the job, and the site, that a recorded definition path names:
	// the inverse of where Plan puts an artifact. The setup journal names a
	// job by the path of its definition, and a journal is recovered by
	// whichever setup runs next, perhaps under another $HOME (a sandbox
	// overrides $HOME, and so where definitions are, but not the manager), so
	// recovery addresses each job at the site its own definition is in, never
	// at the current user home. A path no Plan could have written is refused.
	Locate(definition string) (Site, Ref, error)
}

// Inspector asks what a scheduler knows, and changes nothing.
type Inspector interface {
	// Inspect says what the manager and the disk say about the job ref names.
	// It never fails for "cannot tell": that is State unknown and a Problem.
	// It is short (launchd: 2 s) and read-only, so it is safe beside a running
	// collector.
	Inspect(ctx context.Context, site Site, ref Ref) Status
	// Definition is Inspect without the manager: State is empty, and nothing
	// is asked of the scheduler. Refresh reads a definition this way, so a
	// job whose definition it leaves alone is never asked about.
	Definition(site Site, ref Ref) Status
	// Installed is the jobs of inst on disk, its own current job first (even
	// when it has no definition yet), then its aliases: the collectors earlier
	// releases installed for the same data directory, and the prototype's job
	// for the account's default installation. A job of another data directory
	// is never among them. When something about an alias blocks setup (a
	// prototype's job whose definition is not the one the prototype wrote:
	// "preserve it and resolve it before setup") the error says so, and the
	// jobs it could recognize are returned with it, since status and uninstall
	// never touch the prototype and act on those.
	Installed(ctx context.Context, site Site, inst Installation) ([]Job, error)
}

// Controller changes what runs. Its operations run on a bounded context that
// an interrupt never cancels, since the setup journal handles what is half
// applied.
type Controller interface {
	// Load loads the definition on disk for ref, so scheduled collection
	// starts without a login.
	Load(ctx context.Context, site Site, ref Ref) error
	// Unload stops the job ref names, only when the scheduler loaded it from
	// this site's own definition: nil when it is not loaded, and, stopping
	// nothing, a *NotOwnedError when another installation owns it or an
	// *IndeterminateError when the scheduler cannot say.
	Unload(ctx context.Context, site Site, ref Ref) error
}

// Scheduler is the background job manager as every command uses it. A test
// stands in with a fake; the real one for this system is host.Lookup("").
type Scheduler interface {
	Definer
	Inspector
	Controller
}

// NotOwnedError is an Unload refused because the manager runs the job from a
// definition that belongs to another installation. Nothing was stopped.
type NotOwnedError struct {
	Words   Words
	Problem Problem
}

// Error says whose job was left running.
func (e *NotOwnedError) Error() string {
	return fmt.Sprintf("%s's %s job was not loaded from %s; it belongs to another installation and was left running", e.Words.Manager, e.Problem.Ref, e.Problem.Expected)
}

// IndeterminateError is an Unload refused because the manager could not say
// whose the job is. Nothing was stopped.
type IndeterminateError struct {
	Words   Words
	Problem Problem
}

// Error says the job was left as it is.
func (e *IndeterminateError) Error() string {
	return fmt.Sprintf("cannot confirm which %s %s's %s job was loaded from; it was left as it is", e.Words.Definition, e.Words.Manager, e.Problem.Ref)
}

// Runner runs a program and returns its combined standard output and
// standard error, as exec.Cmd.CombinedOutput does: an adapter reads a failed
// command's words from that output (launchctl print's "Could not find
// service"), and quotes it in the errors it returns. Tests replace the Runner
// an adapter is given, so no test reaches the real manager.
type Runner func(ctx context.Context, name string, args ...string) (output []byte, err error)
