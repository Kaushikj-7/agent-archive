package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// DiagnosticCode names the boundary that prevented a capture.
type DiagnosticCode string

// The diagnostic codes status explains (DiagnosticMessage).
const (
	DiagnosticProjectUnavailable  DiagnosticCode = "project_facts_unavailable"
	DiagnosticUnknownSessionStart DiagnosticCode = "session_start_unknown"
	DiagnosticPreActivationStart  DiagnosticCode = "session_started_before_activation"
	// DiagnosticSetupInProgress records a start that arrived while setup's own
	// transaction was open. Hooks do not register anything in that window.
	DiagnosticSetupInProgress DiagnosticCode = "setup_in_progress"
	// DiagnosticHookFailed records a hook that stopped on an internal error
	// (a recovered panic), so the event it carried was not recorded.
	DiagnosticHookFailed DiagnosticCode = "hook_failed"
	// DiagnosticHookBusy records that the hook could not acquire hooks.lock
	// before its deadline. No event identifiers or payload are retained.
	DiagnosticHookBusy DiagnosticCode = "hook_busy"
	// DiagnosticSessionIndexRecovery records bounded identity lookup damage.
	DiagnosticSessionIndexRecovery DiagnosticCode = "session_index_recovery_required"
	// DiagnosticRevisionLeftOut records that the collector left out an
	// earlier revision of a thread that its current history does not
	// continue (a Codex rollout abandoned by a rollback), because it could
	// not be read or did not fit beside the thread's own history. The rest
	// of the session was captured.
	DiagnosticRevisionLeftOut DiagnosticCode = "revision_left_out"
)

// DiagnosticPartial reports whether code records part of a session left
// out, rather than a session that was not captured.
func DiagnosticPartial(code DiagnosticCode) bool {
	return code == DiagnosticRevisionLeftOut
}

// Diagnostic is deliberately content-free. It records only the
// integration boundary that prevented capture; native session identifiers,
// transcript paths, hook payloads, and conversation content never belong here.
type Diagnostic struct {
	Code        DiagnosticCode `json:"code"`
	Harness     string         `json:"harness"`
	ProjectRoot string         `json:"project_root,omitempty"`
	ObservedAt  time.Time      `json:"observed_at"`
}

// DiagnosticsPath is capture-diagnostics.json in the data directory home.
func DiagnosticsPath(home string) string {
	return filepath.Join(home, "capture-diagnostics.json")
}

// ReadDiagnostics returns the stored diagnostics, oldest first, or none when
// the file does not exist yet.
func ReadDiagnostics(home string) ([]Diagnostic, error) {
	var diagnostics []Diagnostic
	if err := local.Read(DiagnosticsPath(home), &diagnostics); err != nil {
		if os.IsNotExist(err) {
			return []Diagnostic{}, nil
		}
		return nil, err
	}
	return diagnostics, nil
}

const (
	// DiagnosticsLockName serializes every read-modify-write of
	// capture-diagnostics.json: each hook-side record and setup's prune.
	DiagnosticsLockName = "diagnostics.lock"
	// pruneDiagnosticsWait is setup's wait. Holders keep the lock only to
	// reread the file, check the configuration and rename (see
	// diagnosticsUpdate), but while other processes sync, APFS can stall a
	// rename for over a second, and a burst of hooks queues several. Setup
	// waits only while the lock is held, so the bound is generous; it still
	// keeps setup from hanging on a holder that never finishes, such as a
	// stopped process.
	pruneDiagnosticsWait = 10 * time.Second
	// diagnosticsAttempts bounds how often an update is built again because
	// another writer replaced the file first. Each retry means another update
	// landed, and diagnostics are written only for rare events.
	diagnosticsAttempts = 5
)

// hookDiagnosticsWait bounds how long a hook waits for diagnostics.lock. A
// diagnostic is advisory, and a hook runs on the user's turn, so on timeout
// the diagnostic is dropped rather than the turn delayed. A variable only so
// a race test can rule out timeout drops and observe lost updates alone.
var hookDiagnosticsWait = 50 * time.Millisecond

// errDiagnosticsKeptChanging is an update that other writers overtook
// diagnosticsAttempts times. A hook drops its diagnostic, as on a busy lock.
var errDiagnosticsKeptChanging = fmt.Errorf("capture diagnostics kept changing while being updated: %w", local.ErrBusy)

// RecordDiagnostic adds a diagnostic under diagnostics.lock, or drops it
// if the lock is not free within hookDiagnosticsWait. Under the lock it rereads
// the configuration: the caller decided the project was included from a
// snapshot, and setup may have excluded it and pruned its diagnostics since.
// Checking against the committed configuration, under the same lock the prune
// takes, means a pruned project's diagnostic can never come back, whichever
// of the two runs first.
func RecordDiagnostic(home string, diagnostic Diagnostic) error {
	return recordDiagnostic(home, diagnostic, false)
}

// RecordSessionDiagnostic records a diagnostic the collector found while
// capturing a registered session whose project root is root. It names the
// configured project that includes root, never root itself, and nothing for
// a root no included project owns, except as Codex's all-projects scope
// records it (with no project).
func RecordSessionDiagnostic(home string, code DiagnosticCode, harness, root string, at time.Time) error {
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled {
		return err
	}
	diagnostic := Diagnostic{Code: code, Harness: archive.CanonicalHarness(harness), ObservedAt: at}
	if project, owned := ConfiguredProjectActivationFor(cfg, root); owned && project.Included {
		diagnostic.ProjectRoot = project.Root
	}
	return RecordDiagnostic(home, diagnostic)
}

// recordDiagnostic can report diagnostics-lock contention to callers whose
// stderr is the only remaining place to explain a dropped hook event.
func recordDiagnostic(home string, diagnostic Diagnostic, busyIsError bool) error {
	if cfg, found, err := config.Load(home); err == nil && found && diagnostic.Harness == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		diagnostic.ProjectRoot = ""
	}
	err := recordUpdate(home, diagnostic).run(home)
	if errors.Is(err, local.ErrBusy) {
		if busyIsError {
			return fmt.Errorf("capture diagnostics busy; status may not show this missed hook: %w", err)
		}
		return nil
	}
	return err
}

// recordUpdate adds diagnostic, for a project the committed configuration
// includes.
func recordUpdate(home string, diagnostic Diagnostic) diagnosticsUpdate {
	diagnostic.ObservedAt = diagnostic.ObservedAt.UTC()
	return diagnosticsUpdate{
		wait: hookDiagnosticsWait,
		admit: func() (bool, error) {
			cfg, found, err := config.Load(home)
			if err != nil {
				return false, fmt.Errorf("load config: %w", err)
			}
			return found && len(IncludedDiagnosticsForConfig([]Diagnostic{diagnostic}, cfg)) > 0, nil
		},
		// Advisory, and rewritten whole: a file that no longer decodes is
		// replaced rather than left to fail every later diagnostic.
		next: func(diagnostics []Diagnostic, _ bool) ([]Diagnostic, bool) {
			// Keep only the latest instance of a reason for an app/project
			// pair. This bounds local status data even when a harness repeats
			// the same hook.
			kept := diagnostics[:0]
			for _, existing := range diagnostics {
				if existing.Code == diagnostic.Code && existing.Harness == diagnostic.Harness && existing.ProjectRoot == diagnostic.ProjectRoot {
					continue
				}
				kept = append(kept, existing)
			}
			kept = append(kept, diagnostic)
			sort.Slice(kept, func(i, j int) bool { return kept[i].ObservedAt.Before(kept[j].ObservedAt) })
			if len(kept) > 50 {
				kept = kept[len(kept)-50:]
			}
			return kept, true
		},
	}
}

// diagnosticsUpdate is one read-modify-write of capture-diagnostics.json
// that holds diagnostics.lock without syncing anything. A durable write syncs
// twice, and on macOS each sync is an F_FULLFSYNC that can take seconds on a
// busy Mac, longer than hooks (hookDiagnosticsWait) and setup's prune
// (pruneDiagnosticsWait) wait for the lock. So run writes and syncs the new
// file before it takes the lock, and syncs the directory after releasing it.
// Under the lock it only rereads the file, checks admit, and renames. A file
// that another writer replaced since it was read is not overwritten: the
// update is built again from what that writer left, so none is lost.
type diagnosticsUpdate struct {
	wait time.Duration
	// admit, when set, says whether the update may be written at all. It is
	// asked before the update is built and again under the lock.
	admit func() (bool, error)
	// next returns the file's new content from the current one, and whether
	// to write it. diagnostics is empty when the file is missing or no longer
	// decodes, and undecodable says which.
	next func(diagnostics []Diagnostic, undecodable bool) ([]Diagnostic, bool)
	// afterStage, a test seam, runs once the new content is synced, before
	// the lock is taken.
	afterStage func()
}

func (u diagnosticsUpdate) run(home string) error {
	path := DiagnosticsPath(home)
	for range diagnosticsAttempts {
		if u.admit != nil {
			if admitted, err := u.admit(); err != nil || !admitted {
				return err
			}
		}
		before, err := readDiagnosticsFile(path)
		if err != nil {
			return err
		}
		content, write := u.next(before.decode())
		var staged *local.Staged
		if write {
			if staged, err = local.Stage(path, content); err != nil {
				return err
			}
			if u.afterStage != nil {
				u.afterStage()
			}
		}
		replaced, changed, err := u.commit(home, before, staged)
		staged.Discard()
		switch {
		case err != nil:
			return err
		case changed:
			continue
		case replaced:
			return staged.SyncDir()
		default:
			return nil
		}
	}
	return errDiagnosticsKeptChanging
}

// commit takes the lock and, when the file is still what the update was built
// from and admit allows it, renames staged (nil for no change) over it.
// changed reports a file another writer replaced meanwhile.
func (u diagnosticsUpdate) commit(home string, before diagnosticsFile, staged *local.Staged) (replaced, changed bool, err error) {
	unlock, err := local.NamedLockWait(home, DiagnosticsLockName, u.wait)
	if err != nil {
		return false, false, fmt.Errorf("lock capture diagnostics: %w", err)
	}
	defer unlock()
	current, err := readDiagnosticsFile(DiagnosticsPath(home))
	if err != nil {
		return false, false, err
	}
	if !current.equal(before) {
		return false, true, nil
	}
	if staged == nil {
		return false, false, nil
	}
	if u.admit != nil {
		if admitted, err := u.admit(); err != nil || !admitted {
			return false, false, err
		}
	}
	if err := staged.Commit(); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// diagnosticsFile is capture-diagnostics.json's bytes as read, or none.
type diagnosticsFile struct {
	data   []byte
	exists bool
}

func readDiagnosticsFile(path string) (diagnosticsFile, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return diagnosticsFile{}, nil
	}
	if err != nil {
		return diagnosticsFile{}, err
	}
	return diagnosticsFile{data: data, exists: true}, nil
}

func (f diagnosticsFile) equal(other diagnosticsFile) bool {
	return f.exists == other.exists && bytes.Equal(f.data, other.data)
}

// decode returns the stored diagnostics, and whether a file that exists no
// longer decodes (with no diagnostics).
func (f diagnosticsFile) decode() ([]Diagnostic, bool) {
	if !f.exists {
		return nil, false
	}
	var diagnostics []Diagnostic
	if err := json.Unmarshal(f.data, &diagnostics); err != nil {
		return nil, true
	}
	return diagnostics, false
}

// IncludedDiagnostics keeps only diagnostics for projects that are
// currently included. A diagnostic is recorded only for an included project,
// but the project may be excluded later; its path must then stop appearing
// in status, not linger until newer entries push it out.
func IncludedDiagnostics(diagnostics []Diagnostic, projects []archive.ProjectActivation) []Diagnostic {
	kept := make([]Diagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		for _, project := range projects {
			if project.Included && filepath.Clean(project.Root) == filepath.Clean(diagnostic.ProjectRoot) {
				kept = append(kept, diagnostic)
				break
			}
		}
	}
	return kept
}

// PruneDiagnostics drops stored diagnostics for projects that are no
// longer included, so an excluded path is not kept on disk either. Setup
// calls it after committing the configuration those projects come from; see
// RecordDiagnostic for why that order plus the shared lock is enough.
func PruneDiagnostics(home string, projects []archive.ProjectActivation) error {
	return pruneUpdate(projects).run(home)
}

// pruneUpdate keeps the diagnostics of included projects. Even when there is
// nothing to drop it takes the lock, so a hook that checked the configuration
// before setup committed it and is renaming its diagnostic finishes first.
func pruneUpdate(projects []archive.ProjectActivation) diagnosticsUpdate {
	return diagnosticsUpdate{
		wait: pruneDiagnosticsWait,
		next: func(diagnostics []Diagnostic, undecodable bool) ([]Diagnostic, bool) {
			if undecodable {
				// Replaced by an empty list: nothing in it can be pruned, and
				// it must not stop setup.
				return []Diagnostic{}, true
			}
			kept := IncludedDiagnostics(diagnostics, projects)
			return kept, len(kept) != len(diagnostics)
		},
	}
}

// DiagnosticMessage says in words what a diagnostic code means.
func DiagnosticMessage(code DiagnosticCode) string {
	switch code {
	case DiagnosticProjectUnavailable:
		return "the working directory or Git worktree mapping could not be validated; retry after it is available"
	case DiagnosticUnknownSessionStart:
		return "the session start could not be established"
	case DiagnosticPreActivationStart:
		return "the session start does not meet the project activation boundary"
	case DiagnosticSetupInProgress:
		return "setup was still in progress, so the session was not registered; start a new session"
	case DiagnosticHookFailed:
		return "a hook stopped on an internal error, so its event was not recorded; please report it"
	case DiagnosticSessionIndexRecovery:
		return "local session identity bookkeeping needs collector recovery; eligible deferred starts remain queued"
	case DiagnosticHookBusy:
		return "a hook could not acquire the capture lock before its deadline; a first-start intent may be recovered on the next collector pass; check status and start a new session if capture did not resume"
	case DiagnosticRevisionLeftOut:
		return "an abandoned branch of the thread (an earlier revision its current history does not continue) could not be read or did not fit within the capture limits, so it was left out; the rest of the session was captured"
	default:
		return "capture evidence was not accepted"
	}
}

// IncludedDiagnosticsForConfig includes bounded Codex scope diagnostics without
// storing a discovered project sample. Legacy project diagnostics retain their rules.
func IncludedDiagnosticsForConfig(diagnostics []Diagnostic, cfg config.Config) []Diagnostic {
	kept := IncludedDiagnostics(diagnostics, cfg.Archive.Projects)
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects && cfg.Archive.Enabled && slices.Contains(cfg.Harnesses, "codex") {
		for _, d := range diagnostics {
			if d.Harness == "codex" && d.ProjectRoot == "" {
				kept = append(kept, d)
			}
		}
	}
	return kept
}
