// Package backfill finds the Claude Code, Codex, and Cursor sessions already
// on this machine and plans importing them into the archive (see
// dev/specs/backfill.md). Planning is read-only: it lists the
// apps' native stores, runs each app's adapter over every transcript it may
// import, and keeps only counts, times, and sizes. File system access, the
// home directory, and the clock come from an injected Environment, so tests
// run over a constructed tree.
package backfill

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// SkipReason says why a found session is not imported. Every session found is
// either imported (an empty reason) or has exactly one reason; when several
// apply, the earliest in skipOrder wins.
type SkipReason string

// Skip reasons, listed in precedence order (skipOrder). Each is shown to the
// user in the backfill plan with a count and, where one exists, the flag or
// setting that would import the sessions anyway.
const (
	// SkipAlreadyArchived means the session is already in the archive.
	SkipAlreadyArchived SkipReason = "already_archived"
	// SkipDuplicateSession means the same session was found more than once;
	// one copy is imported and the rest carry this reason.
	SkipDuplicateSession SkipReason = "duplicate_session"
	// SkipRegisteredNotAdmitted means the session was registered earlier but
	// is not accepted by the current setup (an earlier bucket, or a project
	// excluded and then included again). Backfill does not re-admit it.
	SkipRegisteredNotAdmitted SkipReason = "registered_not_admitted"
	// SkipRemovedByUndo means an earlier backfill undo removed the session.
	// --include-removed imports it again.
	SkipRemovedByUndo SkipReason = "removed_by_undo"
	// SkipRemovedByRetention means retention removed the session.
	// --include-removed imports it again.
	SkipRemovedByRetention SkipReason = "removed_by_retention"
	// SkipFilteredOut means the session does not match the run's filters
	// (such as --since or --until). It is shown only when filters are set.
	SkipFilteredOut SkipReason = "filtered_out"
	// SkipExcludedProject means the session's project is excluded in setup.
	SkipExcludedProject SkipReason = "excluded_project"
	// SkipExcludedHistory means a complete history requires an excluded dependency.
	SkipExcludedHistory SkipReason = "excluded_history"
	// SkipHomeDirectory means the session ran from the home folder itself.
	// --include-home imports it.
	SkipHomeDirectory SkipReason = "home_directory"
	// SkipAboveHome means the session ran from /, /Users, or another
	// directory above home. No flag overrides it.
	SkipAboveHome SkipReason = "above_home"
	// SkipTemporaryDirectory means the session ran from a temporary directory
	// (/tmp, /private/tmp, /var/folders, or $TMPDIR). --include-temp imports
	// it.
	SkipTemporaryDirectory SkipReason = "temporary_directory"
	// SkipProjectUnknown means no working directory, and so no project, could
	// be determined for the session.
	SkipProjectUnknown SkipReason = "project_unknown"
	// SkipWorktreeUnresolved means the session ran in a Git worktree that no
	// longer exists and cannot be mapped to its repository.
	SkipWorktreeUnresolved SkipReason = "worktree_unresolved"
	// SkipIndexRecovery means local identity bookkeeping cannot yet prove whether
	// this session was previously admitted. It cannot be selected for import.
	SkipIndexRecovery SkipReason = "session_index_pending"
	// SkipIdentityMismatch means the session IDs inside the transcript do not
	// match the file (or the Cursor chat's key).
	SkipIdentityMismatch SkipReason = "identity_mismatch"
	// SkipRelatedHistory means native history or independent own-task evidence is pending.
	SkipRelatedHistory SkipReason = "related_history_pending"
	// SkipSourceChanged means the native file changed during a verified read; retry later.
	SkipSourceChanged SkipReason = "source_changed"
	// SkipEmpty means the session holds no conversation.
	SkipEmpty SkipReason = "empty"
	// SkipUnsafeFormat means the transcript is in a format the privacy filter
	// cannot read safely, so nothing of it is kept.
	SkipUnsafeFormat SkipReason = "unsafe_format"
	// SkipTooLarge means the transcript is over the collector's size limits.
	SkipTooLarge SkipReason = "too_large"
	// SkipStartUnknown means the session's start time could not be
	// determined (no record timestamp, or no createdAt for a Cursor chat).
	SkipStartUnknown SkipReason = "start_unknown"
	// SkipStartInFuture means the session starts after the import time,
	// which points to a wrong clock.
	SkipStartInFuture SkipReason = "start_in_future"
)

// skipOrder is the spec's precedence: the first applicable reason wins.
var skipOrder = []SkipReason{
	SkipAlreadyArchived, SkipDuplicateSession, SkipRegisteredNotAdmitted, SkipRemovedByUndo, SkipRemovedByRetention,
	SkipFilteredOut, SkipExcludedProject, SkipExcludedHistory, SkipHomeDirectory, SkipAboveHome, SkipTemporaryDirectory,
	SkipProjectUnknown, SkipWorktreeUnresolved, SkipIndexRecovery, SkipIdentityMismatch, SkipRelatedHistory, SkipSourceChanged,
	SkipEmpty, SkipUnsafeFormat, SkipTooLarge, SkipStartUnknown, SkipStartInFuture,
}

// ArchiveState is what backfill needs to know about the local archive. The
// CLI implements it over state.Store and config.Config.
type ArchiveState interface {
	// Classify returns "" when the native session is not in the archive, or
	// one of the skip codes already_archived, registered_not_admitted,
	// removed_by_retention, removed_by_undo.
	Classify(harness, nativeSessionID string) (SkipReason, error)
}

// ProjectKind is what a resolved project root is, which decides where the
// plan lists it.
type ProjectKind string

const (
	// ProjectKindRepository is a git repository, including one a worktree
	// folded into.
	ProjectKindRepository ProjectKind = "repository"
	// ProjectKindScratch is a desktop app's workspace folder: Claude
	// desktop's scratch-workspaces or Codex desktop's ~/Documents/Codex.
	ProjectKindScratch ProjectKind = "scratch"
	// ProjectKindTemporary is a temporary directory, only with IncludeTemp.
	ProjectKindTemporary ProjectKind = "temporary"
	// ProjectKindHome is the home directory, only with IncludeHome.
	ProjectKindHome ProjectKind = "home"
	// ProjectKindDirectory is any other folder.
	ProjectKindDirectory ProjectKind = "directory"
	// ProjectKindHistorical never includes a live filesystem root.
	ProjectKindHistorical ProjectKind = "historical"
)

// Candidate is one session found on disk and what the plan decided for it.
// It holds paths and native IDs for the import step; nothing here is ever
// printed.
type Candidate struct {
	Harness         string
	NativeSessionID string
	// NativeChild and native relationship IDs describe Codex ownership, not admission.
	NativeChild    bool
	RelatedHistory bool
	ParentNativeID string
	RootNativeID   string
	NativeHome     string
	CodexBinding   *archive.CodexSourceBinding
	// TranscriptPath is the session's transcript file. A Cursor chat found
	// only in Cursor's database has none: its SourceKind is
	// archive.SourceKindCursorSQLite and SourceKey its chat ID.
	TranscriptPath string
	SourceKind     archive.SourceKind
	SourceKey      string
	// ProjectRoot is where the session is imported, in the spelling it is
	// registered under: a configured project's own spelling, or the resolved
	// path. It is set for a skip that has a would-be root (home, temporary,
	// excluded) and empty when no project could be determined.
	HistoricalProject        *archive.HistoricalProjectProof
	ProjectRoot              string
	ProjectResolution        *archive.ProjectResolution
	projectResolutionCurrent func() bool
	projectResolutionReset   func(context.Context)
	ProjectKind              ProjectKind
	// ProjectIncluded is set when the root is a configured, included
	// project; otherwise importing adds it.
	ProjectIncluded bool
	// ProjectExists is whether the root folder still exists.
	ProjectExists   bool
	StartedAt       time.Time
	StartedAtSource archive.StartedAtSource
	Bytes           int64
	// Subagents are the readable Claude Code subagent transcripts of an
	// imported parent.
	Subagents []Subagent
	// SubagentsSkipped counts the parent's subagent transcripts that are too
	// large, that the filter refuses, or that the collector would not
	// register (collector.CheckImportedSubagent); they are not imported.
	SubagentsSkipped int
	Skip             SkipReason
	// Diagnostic explains the winning skip without retaining source contents or locators.
	Diagnostic *Diagnostic
}

// Subagent is one subagent transcript belonging to an imported parent.
type Subagent struct {
	Path string
	// AgentID is the <id> of agent-<id>.jsonl.
	AgentID string
	Bytes   int64
}

// Filters narrows a backfill run.
type Filters struct {
	Harnesses          []string
	Projects           []string
	ProjectMappings    map[string]string
	HistoricalMappings map[string]string
	HistoricalProjects []HistoricalProjectDefinition
	BulkMappings       bool
	MappingFile        string
	// Since and Until are local dates, YYYY-MM-DD, compared inclusively with
	// the session's start.
	Since string
	Until string
	// SinceArg and UntilArg are the --since and --until values as typed
	// when they are relative (an age such as 30d), which name a different
	// day each day: an interrupted import is continued by the same value,
	// not the same day (see BatchFilters). "" for a date, a time, or none.
	SinceArg       string
	UntilArg       string
	IncludeHome    bool
	IncludeTemp    bool
	IncludeRemoved bool
}

// Active reports whether any filter that can exclude a session is set; only
// then does the plan report filtered_out.
func (f Filters) Active() bool {
	return len(f.Harnesses) > 0 || len(f.Projects) > 0 || f.Since != "" || f.Until != ""
}

const dateLayout = "2006-01-02"

// Validate checks the filters' values before any file is read.
func (f Filters) Validate(discovery agentapi.DiscoveryLookup) error {
	for _, h := range f.Harnesses {
		if discovery == nil {
			return errors.New("historical discovery integrations are required")
		}
		if _, known := discovery.LookupDiscovery(h); !known {
			return fmt.Errorf("--harness must be %s, not %q", historicalChoices(discovery.DiscoveryAgents()), h)
		}
	}
	if slices.Contains(f.Projects, "") {
		return errors.New("--project needs a directory")
	}
	var since, until time.Time
	var err error
	if f.Since != "" {
		if since, err = time.Parse(dateLayout, f.Since); err != nil {
			return fmt.Errorf("--since must be a date like 2026-09-01, not %q", f.Since)
		}
	}
	if f.Until != "" {
		if until, err = time.Parse(dateLayout, f.Until); err != nil {
			return fmt.Errorf("--until must be a date like 2026-09-01, not %q", f.Until)
		}
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return errors.New("--until is before --since")
	}
	return nil
}

// Canonicalize resolves external names to the declared discovery identities.
// It returns a copy so caller-owned flag slices are never changed.
func (f Filters) Canonicalize(discovery agentapi.DiscoveryLookup) (Filters, error) {
	if err := f.Validate(discovery); err != nil {
		return Filters{}, err
	}
	f.Harnesses = append([]string(nil), f.Harnesses...)
	for i, name := range f.Harnesses {
		canonical, ok := discovery.CanonicalDiscovery(name)
		if !ok {
			return Filters{}, fmt.Errorf("historical identity unavailable for %q", name)
		}
		f.Harnesses[i] = canonical
	}
	return f, nil
}

// harness is an app whose sessions backfill imports, by the name the archive
// uses for it.
type harness string

const (
	harnessClaude harness = "claude"
	harnessCodex  harness = "codex"
	harnessCursor harness = "cursor"
)

// Environment is everything BuildPlan reads from the machine. A nil function
// uses the real file system. The one exception is the adapter pass: it goes
// through collector.FilterTranscriptFile, the collector's own filter, which
// reads transcripts from the real file system whatever is injected here.
type Environment struct {
	HistoricalSourceAllowed   func(string) bool
	RepositoryIdentity        sourcefacts.RepositoryLookup
	RepositoryIdentityCurrent func(sourcefacts.RepositoryIdentity) bool
	Discovery                 agentapi.DiscoveryLookup
	DatabaseCatalogs          agentapi.DatabaseCatalogLookup
	NativePaths               agentapi.NativePathsLookup
	Worktrees                 agentapi.WorktreeLookup
	Workspaces                agentapi.WorkspaceLookup
	Children                  agentapi.ChildrenLookup
	Imports                   agentapi.ImportsLookup
	Sources                   agentapi.SourcesLookup
	// CodexRollouts shares native selection and its resource ledger with planning.
	CodexRollouts agentapi.CodexRolloutLookup
	// PrepareCodexProof advances qualified inventory without authorizing capture.
	PrepareCodexProof func(context.Context, []string) error
	// Home is the user's home directory, where the apps keep their stores.
	Home string
	// NativeDirectories are observed current and previously configured native locations.
	// Nil entries use the integration's declared default; no native paths live here.
	NativeDirectories map[string][]string
	// TempDirs are the temporary directories (rule 6 of project resolution).
	// Nil means DefaultTempDirs; the CLI adds $TMPDIR.
	TempDirs []string
	Now      func() time.Time

	Stat         func(string) (fs.FileInfo, error)
	Lstat        func(string) (fs.FileInfo, error)
	ReadDir      func(string) ([]fs.DirEntry, error)
	ReadFile     func(string) ([]byte, error)
	Open         func(string) (io.ReadCloser, error)
	EvalSymlinks func(string) (string, error)
	// FileCreated returns a file's birth time, Cursor's start time. The
	// default reads it from the file system where it is recorded (macOS);
	// where it is not (Linux: the standard library exposes no birth time,
	// and statx is not used), or it fails, the modification time is used, so
	// a Cursor chat's start time there is when its transcript was last
	// written, not when it began, and the order of Cursor chats that were
	// resumed can differ from the same archive built on a Mac.
	FileCreated func(string) (time.Time, error)
	// CursorDatabase lists the chats in Cursor's database (composerData
	// entries with messages, not drafts, and not subagents) and reads them.
	// Nil means not checked; the CLI uses CursorDatabaseReaderFor. Undo reads
	// the database under Home instead (see resumedSinceImport).
	CursorDatabase func(ctx context.Context) (CursorDatabaseResult, error)
	// CursorRecoveryDatabase supplies a complete, bounded prefilter evidence
	// catalog. Nil capability keeps automatic deleted-worktree recovery pending
	// when a database exists; no unbounded catalog fallback is permitted.
	CursorRecoveryDatabase func(context.Context, int, int64) (CursorDatabaseResult, error)
	// Workers overrides the filter worker count; zero uses defaultWorkers.
	Workers int
	// OS is the operating system whose app locations are looked in: the
	// macOS desktop-app folders and privacy-protected folders only exist on
	// platform.Darwin, and Cursor keeps its data under ~/Library/Application
	// Support there and under the XDG config home on Linux (see
	// platform.Locations, which answers for every OS, an unknown one
	// included). Empty means platform.Current; only tests set it, so both
	// branches run on any OS.
	OS platform.OS
	// Getenv reads the process environment (XDG_CONFIG_HOME, which places
	// Cursor's data folder off macOS). Nil means os.Getenv.
	Getenv func(string) string
}

// DefaultTempDirs are the temporary directories besides $TMPDIR on the
// environment's operating system (platform.Locations.TempRoots); a fresh
// slice.
func (e Environment) DefaultTempDirs() []string {
	return slices.Clone(e.locations().TempRoots)
}

// operatingSystem is the operating system to answer for.
func (e Environment) operatingSystem() platform.OS {
	return cmp.Or(e.OS, platform.Current())
}

// locations are where the environment's operating system keeps what backfill
// looks for, under Home. Symlinks in the privacy-protected folders' home are
// resolved the way the rest of backfill resolves them (resolved).
func (e Environment) locations() platform.Locations {
	return platform.NewLocations(e.operatingSystem(), e.Home, e.getenv, platform.LocationDeps{
		ResolveSymlinks: e.resolved,
	})
}

func (e Environment) getenv(key string) string {
	if e.Getenv != nil {
		return e.Getenv(key)
	}
	return os.Getenv(key)
}

// cursorStateDatabase is Cursor's state.vscdb under Home, "" where the
// environment's operating system has no known place for it.
func (e Environment) cursorStateDatabase() string { return e.nativeProjectPaths("cursor").Database }

func (e Environment) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Environment) stat(path string) (fs.FileInfo, error) {
	if e.Stat != nil {
		return e.Stat(path)
	}
	return os.Stat(path)
}

func (e Environment) lstat(path string) (fs.FileInfo, error) {
	if e.Lstat != nil {
		return e.Lstat(path)
	}
	return os.Lstat(path)
}

func (e Environment) readDir(path string) ([]fs.DirEntry, error) {
	if e.ReadDir != nil {
		return e.ReadDir(path)
	}
	return os.ReadDir(path)
}

func (e Environment) readFile(path string) ([]byte, error) {
	if e.ReadFile != nil {
		return e.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (e Environment) open(path string) (io.ReadCloser, error) {
	if e.Open != nil {
		return e.Open(path)
	}
	return os.Open(path)
}

func (e Environment) evalSymlinks(path string) (string, error) {
	if e.EvalSymlinks != nil {
		return e.EvalSymlinks(path)
	}
	return filepath.EvalSymlinks(path)
}

// fileCreated is a file's birth time, falling back to its modification time.
func (e Environment) fileCreated(path string) (time.Time, error) {
	var created time.Time
	var err error
	if e.FileCreated != nil {
		created, err = e.FileCreated(path)
	} else {
		created, err = fileBirthTime(path)
	}
	if err == nil && !created.IsZero() {
		return created, nil
	}
	info, statErr := e.lstat(path)
	if statErr != nil {
		if err == nil {
			err = statErr
		}
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

func (e Environment) nativeDirectories(name string) []string {
	if dirs, ok := e.NativeDirectories[name]; ok {
		return dirs
	}
	if e.Discovery != nil {
		if provider, ok := e.Discovery.LookupDiscovery(name); ok {
			return provider.DefaultDirectories(e.Home)
		}
	}
	return nil
}

func (e Environment) tempDirs() []string {
	if e.TempDirs != nil {
		return e.TempDirs
	}
	return e.locations().TempRoots
}

// exists reports whether path exists.
func (e Environment) exists(path string) bool {
	_, err := e.stat(path)
	return err == nil
}

// resolved returns path cleaned with symlinks resolved, as hooks match
// configured projects. A path that no longer exists has its deepest existing
// ancestor resolved and the rest appended, so a missing folder under a
// symlinked parent gets the same spelling as when it existed.
func (e Environment) resolved(path string) string {
	path = filepath.Clean(path)
	if r, err := e.evalSymlinks(path); err == nil {
		return r
	}
	for ancestor := filepath.Dir(path); ; ancestor = filepath.Dir(ancestor) {
		if r, err := e.evalSymlinks(ancestor); err == nil {
			rel, err := filepath.Rel(ancestor, path)
			if err != nil {
				return path
			}
			return filepath.Join(r, rel)
		}
		if filepath.Dir(ancestor) == ancestor {
			return path
		}
	}
}

func (e Environment) nativePathEnvironment(name string) agentapi.NativePathEnvironment {
	return agentapi.NativePathEnvironment{Locations: agentapi.NativeLocations{UserHome: e.Home, Directories: e.nativeDirectories(name)}, OperatingSystem: e.operatingSystem(), Getenv: e.getenv}
}

func (e Environment) nativeProjectPaths(name string) agentapi.NativeProjectPaths {
	if e.NativePaths != nil {
		if provider, ok := e.NativePaths.LookupNativePaths(name); ok {
			return provider.ProjectPaths(e.nativePathEnvironment(name))
		}
	}
	return agentapi.NativeProjectPaths{}
}

func historicalChoices(names []string) string {
	switch len(names) {
	case 0:
		return "a registered historical agent"
	case 1:
		return names[0]
	case 2:
		return strings.Join(names, " or ")
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

// ProjectIdentity returns immutable ownership independently of a live root.
func (c Candidate) ProjectIdentity() string {
	if c.HistoricalProject != nil {
		return c.HistoricalProject.ProjectID
	}
	return archive.ProjectID(c.ProjectRoot)
}
