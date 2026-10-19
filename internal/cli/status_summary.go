package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

const (
	statusSummaryFile            = "status-summary.json"
	statusProgressFile           = "status-progress.json"
	statusFileLimit        int64 = 64 << 10
	statusConfigLimit      int64 = 1 << 20
	statusProgressInterval       = 5 * time.Second
)

type statusProblem string

const (
	statusProblemNone       statusProblem = ""
	statusProblemCollection statusProblem = "collection"
	statusProblemStorage    statusProblem = "storage"
	statusProblemCapture    statusProblem = "capture"
	statusProblemLocalState statusProblem = "local_state"
)

// statusSummary is disposable display evidence. Its size does not depend on
// session history, and it never grants permission to mutate archive state.
type statusSummary struct {
	Version              int           `json:"version"`
	ConfigurationID      string        `json:"configuration_id"`
	ObservedAt           time.Time     `json:"observed_at"`
	CompletedAt          time.Time     `json:"completed_at,omitzero"`
	LastPublishedAt      time.Time     `json:"last_published_at,omitzero"`
	Pending              int           `json:"pending"`
	PendingKnown         bool          `json:"pending_known"`
	Problem              statusProblem `json:"problem,omitempty"`
	Notices              int           `json:"notices"`
	VerificationFailures int           `json:"verification_failures,omitempty"`
}

type statusPhase string

const (
	statusPreparing  statusPhase = "preparing"
	statusCollecting statusPhase = "collecting"
	statusVerifying  statusPhase = "verifying"
	statusFinishing  statusPhase = "finishing"
)

type statusProgress struct {
	Version         int         `json:"version"`
	ConfigurationID string      `json:"configuration_id"`
	PID             int         `json:"pid"`
	StartedAt       time.Time   `json:"started_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Phase           statusPhase `json:"phase"`
	Processed       int         `json:"processed"`
	Published       int         `json:"published"`
}

// statusConfigurationID binds observations to capture policy and destination.
// Writer-protection upgrades and refreshed observation timestamps do not change
// what a pass collected. Only capture policy, destination and retention do.
func statusConfigurationID(cfg config.Config) string {
	// Use plain policy data, not Config's protected-writer serializer: a
	// projection deliberately omits writer capabilities and issuance floors.
	policy := struct {
		MachineID          string                     `json:"machine_id"`
		Storage            destination.Config         `json:"storage"`
		DestinationSince   time.Time                  `json:"destination_since"`
		Archive            archive.Config             `json:"archive"`
		Harnesses          []string                   `json:"harnesses"`
		ImportedHarnesses  []string                   `json:"imported_harnesses"`
		NativeRoots        []archive.NativeRoot       `json:"native_roots"`
		HistoricalProjects []config.HistoricalProject `json:"historical_projects"`
		CodexCapture       *config.CodexCaptureConfig `json:"codex_capture"`
		Discovery          *config.DiscoveryConfig    `json:"discovery"`
		MuseCapture        *config.MuseCaptureConfig  `json:"muse_capture"`
		RequireSkillUse    bool                       `json:"require_skill_use"`
		SkillEvidence      config.SkillEvidence       `json:"skill_evidence"`
		RetentionDays      int                        `json:"retention_days"`
	}{
		MachineID: cfg.MachineID, Storage: cfg.Storage, DestinationSince: cfg.DestinationSince,
		Archive: cfg.Archive, Harnesses: cfg.Harnesses, ImportedHarnesses: cfg.ImportedHarnesses,
		NativeRoots: cfg.NativeRoots, HistoricalProjects: cfg.HistoricalProjects,
		CodexCapture: cfg.CodexCapture, Discovery: cfg.Discovery, MuseCapture: cfg.MuseCapture,
		RequireSkillUse: cfg.RequireSkillUse,
		SkillEvidence:   cfg.SkillEvidence, RetentionDays: cfg.RetentionDays,
	}
	data, err := json.Marshal(policy)
	if err != nil {
		// An empty identity is never accepted by summary or progress readers.
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// readStatusFile bounds allocation and refuses special files and symlinks before
// decoding. A missing advisory observation stays missing; there is no scan fallback.
func readStatusFile(root *os.Root, name string, limit int64, value any) (found bool, err error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > limit {
		return false, errors.New("status observation is not a bounded regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return false, errors.Join(errors.New("status observation changed"), err)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return false, errors.Join(errors.New("status observation exceeds its read limit"), err)
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return false, errors.Join(errors.New("status observation changed"), err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return false, err
	}
	return true, nil
}

func validStatusSummary(summary statusSummary, configurationID string, now time.Time) bool {
	if configurationID == "" || summary.Version != 1 || summary.ConfigurationID != configurationID || summary.ObservedAt.IsZero() || summary.ObservedAt.After(now) || summary.CompletedAt.After(summary.ObservedAt) || summary.LastPublishedAt.After(summary.ObservedAt) || summary.Pending < 0 || summary.Notices < 0 || summary.VerificationFailures < 0 {
		return false
	}
	switch summary.Problem {
	case statusProblemNone, statusProblemCollection, statusProblemStorage, statusProblemCapture, statusProblemLocalState:
		return true
	}
	return false
}

func summarizedStatusProblem(record state.Status, passErr error) (statusProblem, int) {
	notices := record.UnrefreshableSummaries
	if len(record.QuarantinedFiles) > 0 {
		return statusProblemLocalState, notices
	}
	for _, code := range []string{issueStorageAuth, issueStorageUnavailable} {
		if record.IssueCounts[code] > 0 {
			return statusProblemStorage, notices
		}
	}
	if problem, _, classified := issueHeadline(record); classified {
		if problem != "" {
			return statusProblemCapture, notices
		}
		for _, count := range record.IssueCounts {
			notices += count
		}
		if notices == 0 && record.LastError != "" {
			notices = 1
		}
		if passErr == nil {
			return statusProblemNone, notices
		}
	}
	if passErr != nil || record.LastError != "" || len(record.LastErrors) > 0 {
		return statusProblemCollection, notices
	}
	return statusProblemNone, notices
}

// saveSummary runs under collector.lock after verification and cleanup.
// Reading the already-produced status adds no session or publication enumeration.
func (w *statusProgressWriter) saveSummary(cfg config.Config, passErr error, fullPass bool) (err error) {
	id, now := statusConfigurationID(cfg), w.env.now().UTC()
	if id == "" {
		return errors.New("cannot identify status capture policy")
	}
	record, err := state.OpenReadOnly(w.home).LoadStatus()
	if err != nil {
		return err
	}
	root, err := local.OpenRootedHome(w.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	var previous statusSummary
	found, readErr := readStatusFile(root.Root, statusSummaryFile, statusFileLimit, &previous)
	if readErr != nil || !found || !validStatusSummary(previous, id, now) {
		previous = statusSummary{}
	}
	summary := previous
	summary.Version, summary.ConfigurationID, summary.ObservedAt = 1, id, now
	// A scoped import proves uploads for this configuration, independently
	// of the last whole-archive scan, pending count and health observations.
	if w.progress.Published > 0 {
		summary.LastPublishedAt = w.lastPublishedAt
	}
	if fullPass {
		if !record.LastScanAt.IsZero() && !record.LastScanAt.Before(w.started) {
			summary.CompletedAt = now
			summary.Pending, summary.PendingKnown = record.PendingCount, true
		}
		summary.Problem, summary.Notices = summarizedStatusProblem(record, passErr)
		if w.verification != nil {
			summary.VerificationFailures = w.verification.UnresolvedFailures
		}
	}
	if err := root.Check(); err != nil {
		return err
	}
	return local.RootedWrite(root.Root, statusSummaryFile, summary)
}

type statusProgressWriter struct {
	home            string
	env             Env
	started         time.Time
	progress        statusProgress
	verification    *verificationSummary
	lastPublishedAt time.Time
}

func beginStatusProgress(home string, cfg config.Config, env Env) *statusProgressWriter {
	now := env.now().UTC()
	started := now
	// The lock record's start may precede configuration reload.
	if record, ok := readCollectorLockRecord(home); ok && record.PID == os.Getpid() {
		started = record.Since
	}
	w := &statusProgressWriter{home: home, env: env, started: now, progress: statusProgress{Version: 1, ConfigurationID: statusConfigurationID(cfg), PID: os.Getpid(), StartedAt: started}}
	w.phase(statusPreparing)
	return w
}

func (w *statusProgressWriter) phase(phase statusPhase) {
	w.progress.Phase = phase
	w.write()
}

func (w *statusProgressWriter) processed(progress collector.Progress) {
	w.progress.Processed++
	if progress.Published {
		w.progress.Published++
		w.lastPublishedAt = w.env.now().UTC()
	}
	if w.env.now().Sub(w.progress.UpdatedAt) >= statusProgressInterval {
		w.write()
	}
}

func (w *statusProgressWriter) observe(forward func(collector.Progress)) func(collector.Progress) {
	return func(progress collector.Progress) {
		w.processed(progress)
		if forward != nil {
			forward(progress)
		}
	}
}

func (w *statusProgressWriter) write() {
	if w.progress.ConfigurationID == "" {
		return
	}
	w.progress.UpdatedAt = w.env.now().UTC()
	_ = local.Write(filepath.Join(w.home, statusProgressFile), w.progress)
}

func (w *statusProgressWriter) finish(cfg config.Config, passErr error, fullPass bool) {
	// A scoped import updates only proven uploads, never global health.
	if fullPass || w.progress.Published > 0 {
		if err := w.saveSummary(cfg, passErr, fullPass); err != nil {
			// A failed replacement must not leave an older healthy summary looking current.
			_ = os.Remove(filepath.Join(w.home, statusSummaryFile))
		}
	}
	_ = os.Remove(filepath.Join(w.home, statusProgressFile))
}

func statusProblemText(problem statusProblem) string {
	switch problem {
	case statusProblemStorage:
		return "Storage access failed on the last check."
	case statusProblemCapture:
		return "Some sessions need attention."
	case statusProblemLocalState:
		return "Some local archive state needs recovery."
	case statusProblemCollection:
		return "The last collection did not finish successfully."
	case statusProblemNone:
		return ""
	}
	return fmt.Sprintf("Unknown collection problem %q.", problem)
}
