package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type appStatus struct {
	hooksNeedRepair bool
	// Discovery health is independent of hook execution and publication verification.
	CodexCaptureScope string            `json:"codex_capture_scope,omitempty"`
	Discovery         *discovery.Health `json:"discovery,omitempty"`
	// PreSetupSessions are sessions from the 7 days before discovery's
	// start floor that discovery refused and that are not archived. Absent
	// when there are none.
	PreSetupSessions  *preSetupSessions `json:"pre_setup_sessions,omitempty"`
	PublishedSessions int               `json:"published_sessions"`
	VerifiedSessions  int               `json:"verified_sessions"`
	Configured        bool              `json:"configured"`
	HookObserved      bool              `json:"hook_observed"`
	CapturedLocally   bool              `json:"captured_locally"`
	Published         bool              `json:"published"`
	ReadBackVerified  bool              `json:"read_back_verified"`
	VerifiedAt        time.Time         `json:"verified_at,omitzero"`
	VerificationState string            `json:"verification_state"`
	// VerificationDetail explains a read-back that has not succeeded yet for
	// the current publication: the last error, attempts so far, and when the
	// collector will retry. Empty once every publication is verified.
	VerificationDetail string   `json:"verification_detail,omitempty"`
	Trust              string   `json:"trust"`
	HarnessVersions    []string `json:"observed_harness_versions,omitempty"`
	AdapterVersions    []string `json:"observed_adapter_versions,omitempty"`
	// CaptureGaps lists recorded gaps: sessions whose current transcript can
	// no longer be captured (rewritten or over the size limit), per-session
	// scan issues, and gaps recorded inside captured bundles. A blocked
	// session's last published snapshot, if any, stays retained. These are
	// recorded gaps, not errors.
	CaptureGaps []archive.CaptureGap `json:"capture_gaps,omitempty"`
	// SessionsWithCaptureGaps counts the distinct sessions behind
	// CaptureGaps: one session can record several gaps.
	SessionsWithCaptureGaps int       `json:"sessions_with_capture_gaps,omitempty"`
	Installed               bool      `json:"installed"`
	InstalledVersion        string    `json:"installed_version,omitempty"`
	VersionSource           string    `json:"installed_version_source,omitempty"`
	VersionObservedAt       time.Time `json:"installed_version_observed_at,omitzero"`
	VersionKind             string    `json:"installed_version_kind,omitempty"`
	VersionState            string    `json:"installed_version_state"`
	VersionSupport          string    `json:"installed_version_support"`
	// VersionSupportReason is set when VersionSupport is unverified. Verified
	// captures report the harness's own version (Codex cli_version, Claude
	// Code record version, Cursor hook cursor_version) in HarnessVersions; the
	// installed version comes from discovery and may be numbered differently.
	VersionSupportReason    string              `json:"installed_version_support_reason,omitempty"`
	Capabilities            captureCapabilities `json:"capabilities"`
	verifiedHarnessVersions []string
	// readBackFailure is the read-back record VerificationDetail describes,
	// for the text status to word without codes or ISO times.
	readBackFailure verificationEvidence
	Projects        []projectCaptureStatus `json:"projects"`

	Code  string `json:"code"`
	Hooks string `json:"hooks"`
	// OtherInstallations lists the data directories of other agent-archive
	// installations whose hooks are in this app's hook file (or, for one
	// whose directory cannot be read, its command). This installation never
	// changes them.
	OtherInstallations []string `json:"other_installations,omitempty"`
	Name               string   `json:"name"`
	//lint:ignore LV1001 an open-ended, human-readable label built from many phrasings; statusCode maps it to the stable Code
	State string `json:"state"`
	// Sessions counts the app's hook and discovery registrations, subagents
	// included; SubagentSessions counts the subagents among them.
	Sessions         int `json:"sessions"`
	SubagentSessions int `json:"subagent_sessions"`
	// ImportedSessions counts the app's top-level sessions agent-archive
	// backfill imported that the configuration publishes (AcceptSession).
	ImportedSessions      int `json:"imported_sessions"`
	ImportedChildSessions int `json:"imported_child_sessions,omitempty"`
	// ReplaySessions counts the top-level sessions among Sessions that a
	// replay tool ran (archive.ReplayEnv). They are hook captures, so they
	// count as sessions and verify the app's hooks; list hides them.
	// Absent when there are none.
	ReplaySessions int `json:"replay_sessions,omitempty"`
	// UploadingSessions counts the app's top-level sessions, captured or
	// imported, with work not yet published (state.Outstanding's Pending)
	// that is not a recorded capture gap; Uploading lists them.
	UploadingSessions int                `json:"uploading_sessions"`
	Uploading         []uploadingSession `json:"uploading"`
	// WaitingForTranscriptSessions counts the app's top-level sessions
	// that are pending only because no transcript was ever written for
	// them (state.Outstanding's WaitingForTranscript), such as a Cursor
	// chat with transcripts turned off; they are not uploading.
	WaitingForTranscriptSessions int       `json:"waiting_for_transcript_sessions"`
	LastPublishedAt              time.Time `json:"last_published_at,omitzero"`
	// importedWithIssues counts the app's imports with a capture gap or a
	// failed last scan, which the app's own gaps leave out.
	importedWithIssues int
	// otherProjects counts sessions whose project has no configured pair
	// (a configuration without projects), for status APP's project table.
	otherProjects []projectCaptureStatus
}

// uploadingState is how far an uploading session has got, as status --json
// reports it.
type uploadingState string

const (
	// uploadingPending: published before, with newer work not yet published.
	uploadingPending uploadingState = "uploading"
	// uploadingFirst: never published yet.
	uploadingFirst uploadingState = "first_upload"
	// uploadingFailing: the last pass recorded an issue for the session
	// (Issue names its kind).
	uploadingFailing uploadingState = "failing"
	// uploadingRechecking: the last pass put off its scan without a failure
	// (Deferred names why) and the next pass retries it.
	uploadingRechecking uploadingState = "rechecking"
)

// uploadingSession is one top-level session with work not yet published.
type uploadingSession struct {
	StartedAt        time.Time      `json:"started_at"`
	ArchiveSessionID string         `json:"archive_session_id"`
	Project          string         `json:"project"`
	State            uploadingState `json:"state"`
	Issue            string         `json:"issue,omitempty"`
	Deferred         string         `json:"deferred,omitempty"`
	Imported         bool           `json:"imported,omitempty"`
}

type projectCaptureStatus struct {
	ProjectID         string    `json:"project_id"`
	ProjectRoot       string    `json:"project_root"`
	ActivatedAt       time.Time `json:"activated_at"`
	Configured        bool      `json:"configured"`
	HookObserved      bool      `json:"hook_observed"`
	CapturedLocally   bool      `json:"captured_locally"`
	Published         bool      `json:"published"`
	ReadBackVerified  bool      `json:"read_back_verified"`
	PublishedSessions int       `json:"published_sessions"`
	VerifiedSessions  int       `json:"verified_sessions"`
	VerificationState string    `json:"verification_state"`
	VerifiedAt        time.Time `json:"verified_at,omitzero"`
	// sessions, imported and uploading count the project's top-level
	// sessions as the app's counts of the same names do, for status APP.
	sessions  int
	imported  int
	uploading int
	// Imports can appear in a project row without requiring a fresh capture.
	automaticCapture bool
}

type statusRetention struct {
	Days            int  `json:"retention_days"`
	AutomaticExpiry bool `json:"automatic_expiry"`
}

type statusView struct {
	Retention                  *statusRetention                  `json:"retention,omitempty"`
	IdentityRecovery           *state.SessionIndexRecoveryStatus `json:"identity_recovery,omitempty"`
	MachineRegistrationPending bool                              `json:"machine_registration_pending,omitempty"`
	PrivacyEvidence            storage.PrivacyReport             `json:"privacy_evidence"`
	ConfigurationID            string                            `json:"configuration_id,omitempty"`
	Authentication             storageHealth                     `json:"authentication"`

	Code    string `json:"code"`
	Version int    `json:"schema_version"`
	//lint:ignore LV1001 an open-ended, human-readable label built from many phrasings; statusCode maps it to the stable Code
	State   string `json:"state"`
	Storage string `json:"storage,omitempty"`
	// StorageVerifiedAt is when setup's storage check (write, read, list,
	// and delete of a probe object) last passed for this configuration.
	StorageVerifiedAt time.Time `json:"storage_verified_at,omitzero"`
	// StorageAccessConfirmedAt is the latest confirmation that this
	// destination is reachable with the configured credentials: setup's
	// check, or the collector's last verified storage health for the same
	// configuration (its access probe or a pass that uploaded).
	// StorageAccessConfirmedBy names which: "setup" or "collector".
	StorageAccessConfirmedAt time.Time              `json:"storage_access_confirmed_at,omitzero"`
	StorageAccessConfirmedBy storageAccessConfirmer `json:"storage_access_confirmed_by,omitempty"`
	Privacy                  string                 `json:"privacy"`
	Background               string                 `json:"background"`
	// BackgroundWarnings are what the background job does, but not robustly
	// (systemd: lingering is off, so it stops at logout; a drop-in overrides
	// its unit), one sentence each. Absent when there are none, which is
	// always so on macOS.
	BackgroundWarnings []string    `json:"background_warnings,omitempty"`
	Paused             bool        `json:"paused"`
	SkillEvidence      string      `json:"skill_evidence,omitempty"`
	Projects           []string    `json:"projects"`
	Apps               []appStatus `json:"applications"`
	// AgentSkills lists the agent skill files (the /handoff skill, and any
	// other in agentskills.Registry) setup installed that are there now;
	// AgentSkillsOutOfDate is those an upgrade has outdated, which setup
	// refreshes.
	// AgentSkillsDisabled is set when the person opted out of the skills
	// (setup --no-skills), which setup then neither installs nor refreshes.
	AgentSkills          []string             `json:"agent_skills,omitempty"`
	AgentSkillsOutOfDate []string             `json:"agent_skills_out_of_date,omitempty"`
	AgentSkillsDisabled  bool                 `json:"agent_skills_disabled,omitempty"`
	Collector            state.Status         `json:"collector"`
	CaptureDiagnostics   []capture.Diagnostic `json:"capture_diagnostics,omitempty"`
	// ImportedSessions counts sessions `agent-archive backfill` registered,
	// not their subagents; ImportedPending counts those the collector still
	// has to upload, and ImportedWithIssues those with a capture gap or a
	// failed last scan. LastImport names the most recent import batch.
	ImportedSessions   int    `json:"imported_sessions"`
	ImportedPending    int    `json:"imported_pending"`
	ImportedWithIssues int    `json:"imported_with_issues"`
	LastImport         string `json:"last_import"`
	// Warnings lists advisory local files that could not be read; status
	// still reports everything else.
	Warnings []string `json:"warnings,omitempty"`
	Next     string   `json:"next_action"`
	// configured is whether a configuration exists; the text status shows
	// only the state and next step without one.
	configured bool
	// problem names, in a few words, what Next fixes; the text status leads
	// with it. It is set with Next, and empty when Next is only a tip.
	problem string
	// userHome is the home folder readStatus resolved, for the text status
	// to show paths under it as ~; empty before setup.
	userHome string
	// backgroundTool is the command that drives the background scheduler,
	// which the row for a job that cannot be checked names; empty until
	// readBackground asks the scheduler.
	backgroundTool string
	// lastErrorProblem is the problem chooseNextStep derived from the last
	// pass's errors, and lastErrorByIssue whether it came from the kinds of
	// the failed sessions (issueHeadline). While problem is still it, the
	// headline states those errors and the Storage section need not repeat
	// them.
	lastErrorProblem string
	lastErrorByIssue bool
	// importedApps are apps whose sessions were imported without hooks
	// (config.ImportedHarnesses), with their import counts.
	importedApps []appStatus
}

func runStatusCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("status", stderr)
	jsonOut := fs.Bool("json", false, "print a versioned JSON document")
	verbose := fs.Bool("verbose", false, "also print the codes, exact times and evidence behind each line")
	noPager := fs.Bool("no-pager", false, "print directly to the terminal; do not page through $PAGER")
	appArg, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	app := strings.ToLower(appArg)
	if appArg != "" && !slices.Contains(env.setupNames(), app) {
		return fs.usageError("unknown app %q; choose one of %s", appArg, strings.Join(env.setupNames(), ", "))
	}
	if app != "" && *jsonOut {
		return fs.usageError("an app and --json can't be combined; status --json lists every app under applications")
	}
	view, err := readStatus(env)
	if err != nil {
		terminal.Printf(stderr, "Cannot read archive status: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(stdout, stderr, view)
	}
	// The style is the terminal's, not the pager buffer's.
	sc := statusScreen{style: styleFor(stdout), now: env.now(), home: view.userHome, verbose: *verbose}
	code := 0
	if err := withPager(context.Background(), stdout, stderr, env, *noPager, func(w io.Writer) error {
		if app != "" {
			code = printAppStatus(w, stderr, view, app, sc)
			return nil
		}
		printStatus(w, view, sc)
		return nil
	}); err != nil {
		terminal.Printf(stderr, "Cannot show archive status: %v\n", err)
		return 1
	}
	return code
}

// storageAccessConfirmer is what last confirmed access to the destination,
// as status --json reports it in storage_access_confirmed_by.
type storageAccessConfirmer string

const (
	storageAccessConfirmedBySetup     storageAccessConfirmer = "setup"
	storageAccessConfirmedByCollector storageAccessConfirmer = "collector"
)

// storageLabel names the destination as "provider / bucket / prefix",
// leaving out an empty prefix rather than ending in a bare separator.
func storageLabel(cfg credentials.Config) string {
	parts := []string{cfg.Provider, cfg.Bucket}
	if cfg.Prefix != "" {
		parts = append(parts, cfg.Prefix)
	}
	return strings.Join(parts, " / ")
}

// blockedReasonDetail explains a recorded capture gap. A missing transcript is
// the ordinary end of an archived session's local life, not a defect, and it
// is the one reason that can end on its own, so it does not borrow the
// permanent wording the other reasons need.
func blockedReasonDetail(reason state.BlockedReason) string {
	switch reason {
	case state.BlockedReasonTranscriptMissing:
		return "The application has deleted its own transcript, as each one does on its own schedule. The last published snapshot stays retained and readable, and capture resumes by itself if the file returns."
	case state.BlockedReasonRecordTooLarge:
		return fmt.Sprintf("One record in the transcript (or a plain-text transcript as a whole) is larger than the %d MiB record size limit, so the transcript cannot be read. The last published snapshot, if any, stays retained, and capture resumes when the transcript changes.", archive.MaxRecordBytes>>20)
	case state.BlockedReasonTranscriptRewritten:
		return "The current transcript cannot prove extension of retained history. Automatic capture resumes only if extension can be proved. To preserve this history and capture current activity under a new linked ID, preview agent-archive recover SESSION_ID, then explicitly confirm it."
	case state.BlockedReasonTranscriptTooLarge:
		// Permanent for the current transcript: the general wording below.
	}
	return "The current transcript can no longer be captured; the last published snapshot, if any, stays retained."
}

// readBackProgress summarises how much of an app's evidence is verified:
// projects when the configuration has any, otherwise published sessions.
func readBackProgress(app appStatus) string {
	if len(app.Projects) == 0 {
		return fmt.Sprintf("%d of %d sessions verified", app.VerifiedSessions, app.PublishedSessions)
	}
	verified, required := 0, 0
	for _, pair := range app.Projects {
		if !requiresAutomaticCapture(app, pair) {
			continue
		}
		required++
		if pair.ReadBackVerified {
			verified++
		}
	}
	return fmt.Sprintf("%d of %d projects verified", verified, required)
}

func readStatus(env Env) (view statusView, err error) {
	defer func() {
		view.Code = statusCode(view.State)
		for i := range view.Apps {
			view.Apps[i].Code = statusCode(view.Apps[i].State)
		}
	}()
	// Before setup there is no collector or storage to ask about: the job is
	// missing, as launchd reports a job that is not loaded, and storage not
	// configured; neither is unknown.
	view = statusView{
		Version:        4,
		State:          "Not set up",
		Privacy:        "not_verified",
		Background:     "missing",
		Authentication: storageHealth{State: "not_configured"},
		Projects:       []string{},
		Apps:           []appStatus{},
		Next:           "Run agent-archive setup to get started.",
	}
	home, err := env.readHome()
	if err != nil {
		return view, err
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return view, err
	}
	readSetupProgress(&view, home)
	if !found {
		return view, nil
	}
	readConfiguredStatus(&view, cfg, home, env)
	store := state.OpenReadOnly(home)
	if slices.Contains(cfg.Harnesses, "codex") {
		recovery, _ := store.SessionIndexRecoveryStatus()
		view.IdentityRecovery = &recovery
	}
	sessions := readSessionStatus(&view, cfg, home, store)
	// The collector prunes the list each pass; one that has not run for a
	// while must not show subagents from before the window.
	view.Collector.ExpiredSubagents = state.CarryExpiredSubagents(view.Collector.ExpiredSubagents, nil, env.now())
	for _, name := range cfg.Harnesses {
		view.Apps = append(view.Apps, sessions.appStatus(name, cfg, home, view.Collector.SessionIssues))
	}
	readPreSetupSessions(&view, cfg, home, store, sessions.regs, env.now())
	for _, name := range cfg.ImportedHarnesses {
		if len(cfg.Harnesses) == 0 || slices.Contains(cfg.Harnesses, name) {
			continue
		}
		if app := sessions.appStatus(name, cfg, home, view.Collector.SessionIssues); app.ImportedSessions > 0 {
			view.importedApps = append(view.importedApps, app)
		}
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return view, err
	}
	view.userHome = userHome
	binaryProblem := readInstalledApps(&view, cfg, home, userHome, env)
	background := readBackground(&view, cfg, home, userHome, env)
	chooseNextStep(&view, cfg, home, env, binaryProblem, background)
	return view, nil
}

// readSetupProgress reports a saved or interrupted setup, which is all status
// has to say before the first setup commits a configuration.
func readSetupProgress(view *statusView, home string) {
	if _, err := os.Stat(draftPath(home)); err == nil {
		view.State = "Setup saved"
		view.problem = "Setup stopped before it finished"
		view.Next = "Run agent-archive setup to continue your saved choices."
		if _, _, problem, _ := readDraft(home); problem != "" {
			view.problem = "The saved setup can't be used"
			view.Next = fmt.Sprintf("The saved setup in %s cannot be used (%s). Run agent-archive setup: it offers to move it aside and start again.", draftPath(home), problem)
			view.Warnings = append(view.Warnings, view.Next)
		}
	}
	if setupjournal.TransactionPending(home) {
		view.State = "Setup needs recovery"
		view.problem = "Setup was interrupted"
		view.Next = "Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now."
	}
}

// readConfiguredStatus fills in what the configuration and the advisory
// local files say: storage and its privacy and access evidence, capture
// diagnostics, and the included projects.
func readConfiguredStatus(view *statusView, cfg config.Config, home string, env Env) {
	view.MachineRegistrationPending = registrationPending(home, cfg)
	if view.MachineRegistrationPending {
		view.Warnings = append(view.Warnings, "Machine registration pending; the collector retries independently of capture.")
	}
	view.configured = true
	view.Retention = &statusRetention{Days: cfg.RetentionDays, AutomaticExpiry: cfg.RetentionDays != 0}
	view.SkillEvidence = string(cfg.EffectiveSkillEvidence())
	view.AgentSkillsDisabled = cfg.NoSkills
	view.Background = "unknown"
	view.Storage = storageLabel(cfg.Storage)
	view.StorageVerifiedAt = cfg.StorageVerifiedAt
	view.PrivacyEvidence = currentBucketPrivacy(cfg, env.now())
	view.Privacy = view.PrivacyEvidence.State
	view.ConfigurationID = configurationID(cfg)
	// Advisory files: one that cannot be read is left out with a warning,
	// never a reason to report nothing at all.
	var err error
	view.CaptureDiagnostics, err = capture.ReadDiagnostics(home)
	if err != nil {
		view.Warnings = append(view.Warnings, unreadableWarning(capture.DiagnosticsPath(home), err, "The next capture diagnostic replaces it; deleting it loses only past diagnostics."))
		view.CaptureDiagnostics = nil
	}
	view.CaptureDiagnostics = capture.IncludedDiagnosticsForConfig(view.CaptureDiagnostics, cfg)
	if env.copiedFromAnotherMachine(cfg) {
		first, rest := copiedMachineWarning(home)
		view.Warnings = append(view.Warnings, strings.Join(append([]string{first}, rest...), " "))
	}
	if userHome, err := env.userHomeDir(); err == nil {
		view.Warnings = append(view.Warnings, env.networkHomeWarnings(cfg, home, userHome)...)
		view.Warnings = append(view.Warnings, pairingWarnings(home, env.now())...)
	}
	view.Authentication.State = "unknown"
	if err := local.Read(filepath.Join(home, "storage-health.json"), &view.Authentication); err != nil && !os.IsNotExist(err) {
		view.Warnings = append(view.Warnings, unreadableWarning(filepath.Join(home, "storage-health.json"), err, "The background collector checks storage again and replaces it within a few minutes."))
		view.Authentication = storageHealth{State: "unknown"}
	}
	if view.Authentication.ConfigurationID != "" && view.Authentication.ConfigurationID != view.ConfigurationID {
		view.Authentication.State = "stale_configuration"
	}
	// Setup records when its storage check passed; afterwards the collector
	// confirms the same destination again (its access probe, or a pass that
	// uploaded). Report the latest, so the Access line never says "not
	// confirmed" beside an Authentication line that says verified. Health
	// without this configuration's ID is not evidence for it.
	if !view.StorageVerifiedAt.IsZero() {
		view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy = view.StorageVerifiedAt, storageAccessConfirmedBySetup
	}
	if view.Authentication.State == "verified" && view.Authentication.ConfigurationID == view.ConfigurationID && view.Authentication.CheckedAt.After(view.StorageAccessConfirmedAt) {
		view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy = view.Authentication.CheckedAt, storageAccessConfirmedByCollector
	}
	// The background probe only runs while collection is active, so a paused
	// install keeps its last known state (with its checked time) rather than
	// being called stale for a check that was deliberately not repeated.
	if !cfg.Paused && view.Authentication.State == "verified" && env.now().Sub(view.Authentication.CheckedAt) > storageHealthStaleAfter {
		view.Authentication.State = "stale"
	}
	view.Paused = cfg.Paused
	if userHome, err := env.userHomeDir(); err == nil {
		claudeDir, dataHome := claudeConfigDir(env.installedHookFiles(userHome, cfg)), env.installation(home, userHome).commandDataHome()
		view.AgentSkills = agentskills.Installed(env.agentRegistry(), userHome, claudeDir, dataHome)
		if cfg.NoSkills {
			// Setup removes a file of its own here rather than refreshing it,
			// so it is left over (a restored backup, an interrupted removal),
			// not out of date.
			for _, path := range view.AgentSkills {
				view.Warnings = append(view.Warnings, fmt.Sprintf("The agent skills are turned off, but the %s skill file at %s is still there. Run agent-archive setup to remove it.", skillLabel(path), path))
			}
		} else {
			view.AgentSkillsOutOfDate = agentskills.Stale(env.agentRegistry(), userHome, claudeDir, cfg.InstalledExecutable, dataHome)
			for _, path := range view.AgentSkillsOutOfDate {
				view.Warnings = append(view.Warnings, fmt.Sprintf("The %s skill at %s is out of date. Run agent-archive setup --refresh to refresh it.", skillLabel(path), path))
			}
		}
	}
	for _, p := range cfg.Archive.Projects {
		if p.Included {
			view.Projects = append(view.Projects, p.Root)
		}
	}
}

// statusSessions is the local session state status reads once and then
// attributes to each app: the registrations whose files could be read, and
// skip, which leaves a session whose own files cannot be read out of every
// count, with one warning naming it.
type statusSessions struct {
	store *state.Store
	regs  []archive.SessionRegistration
	skip  func(id string, err error)
	// owed is what each accepted or imported session still owes.
	owed map[string]state.Outstanding
	// deferred is why the last pass put off each deferred session's scan
	// (state.Status.DeferredSessions).
	deferred map[string]string
}

// readSessionStatus reads the collector's status, the pending count, and the
// import counts, and returns the sessions every app's status is built from.
func readSessionStatus(view *statusView, cfg config.Config, home string, store *state.Store) statusSessions {
	var err error
	view.Collector, err = store.LoadStatus()
	if err != nil {
		view.Warnings = append(view.Warnings, unreadableWarning(filepath.Join(home, "status.json"), err, "The collector replaces it on its next pass; agent-archive sync runs one now."))
		view.Collector = state.Status{}
	}
	regs, reqs, stateWarnings := statusState(home, store)
	view.Warnings = append(view.Warnings, stateWarnings...)
	// A session whose own files cannot be read is left out of every count
	// below, with one warning naming it.
	skipped := map[string]bool{}
	skip := func(id string, err error) {
		if !skipped[id] {
			skipped[id] = true
			view.Warnings = append(view.Warnings, fmt.Sprintf("Local state of session %s could not be read (%v); status left that session out.", id, err))
		}
	}
	queued := state.QueuedRequests(reqs)
	// What each session still owes, read once: the pending count and the
	// import counts below both come from it (state.Outstanding).
	owed := map[string]state.Outstanding{}
	pending := 0
	counted := map[string]bool{}
	for _, reg := range regs {
		accepted := cfg.AcceptSession(reg)
		if !accepted && !reg.Imported() {
			continue
		}
		o, err := store.Outstanding(reg, queued[reg.ArchiveSessionID])
		if err != nil {
			skip(reg.ArchiveSessionID, err)
			continue
		}
		owed[reg.ArchiveSessionID] = o
		if accepted && o.Pending() {
			counted[reg.ArchiveSessionID] = true
			pending++
		}
	}
	obligations, obligationErr := store.DurableStorageObligations()
	if obligationErr != nil {
		view.Warnings = append(view.Warnings, "Durable publication evidence requires recovery; automatic cleanup and native substitution are unavailable.")
	}
	registered := map[string]bool{}
	for _, reg := range regs {
		registered[reg.ArchiveSessionID] = true
	}
	orphans := map[string]bool{}
	for _, obligation := range obligations {
		id := obligation.SessionID
		if id == "" {
			pending++
			view.Warnings = append(view.Warnings, "A private durable storage root requires recovery.")
			continue
		}
		if registered[id] && obligation.Namespace == state.GenerationRecoveryStorage {
			if !counted[id] {
				counted[id] = true
				pending++
			}
			view.Warnings = append(view.Warnings, fmt.Sprintf("Session %q retains a generation recovery obligation; recover it before collecting or cleaning up.", id))
		}
		if !registered[id] && !orphans[id] {
			orphans[id] = true
			pending++
			view.Warnings = append(view.Warnings, fmt.Sprintf("Session %q retains durable publication evidence without a readable registration; recover it before collecting or cleaning up.", id))
		}
	}
	if pending > view.Collector.PendingCount {
		view.Collector.PendingCount = pending
	}
	var readable []archive.SessionRegistration
	for _, reg := range regs {
		if !skipped[reg.ArchiveSessionID] {
			readable = append(readable, reg)
		}
	}
	regs = readable
	view.ImportedSessions, view.ImportedPending, view.ImportedWithIssues = importedSessionCounts(cfg, regs, owed, view.Collector.SessionIssues)
	// One unreadable import file must not hide the rest of status.
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		view.Warnings = append(view.Warnings, err.Error())
	}
	if len(batches) > 0 {
		view.LastImport = batches[len(batches)-1].ID
	}
	return statusSessions{store: store, regs: regs, skip: skip, owed: owed, deferred: view.Collector.DeferredSessions}
}

// appStatus is one configured app's capture evidence: its sessions, per
// project and overall, from hook observation to read-back verification.
func (s statusSessions) appStatus(name string, cfg config.Config, home string, issues map[string]string) appStatus {
	app := appStatus{Name: name, State: "waiting for first session", Configured: true, Trust: "unknown", VerificationState: "not_verified", Uploading: []uploadingSession{}}
	pairIndex := map[string]int{}
	configuredRoots := map[string]archive.ProjectActivation{}
	for _, project := range cfg.Archive.Projects {
		if !project.Included {
			continue
		}
		if _, exists := configuredRoots[project.Root]; !exists {
			configuredRoots[project.Root] = project
		}
		if name == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			// Explicit rules remain exceptions in all-mode, not a checklist
			// requiring one capture from every current and future directory.
			continue
		}
		if _, dup := pairIndex[project.Root]; dup {
			// A hand-edited configuration can repeat a root. The first
			// entry owns the pair; a second would never receive evidence
			// and would pin the app unverified.
			continue
		}
		pairIndex[project.Root] = len(app.Projects)
		app.Projects = append(app.Projects, projectCaptureStatus{
			ProjectID: string(project.ProjectID), ProjectRoot: project.Root,
			ActivatedAt: project.ActivatedAt, Configured: true, VerificationState: "not_verified",
		})
	}
	var readBackIssue verificationOutcome
	others := map[string]int{}
	// project is where a session's counts go: its configured pair, or a
	// row of its own when no configured project owns its root.
	project := func(root string) *projectCaptureStatus {
		if position, found := pairIndex[root]; found {
			return &app.Projects[position]
		}
		if name == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			position := len(app.Projects)
			pairIndex[root] = position
			configured, explicit := configuredRoots[root]
			app.Projects = append(app.Projects, projectCaptureStatus{ProjectID: string(archive.ProjectID(root)), ProjectRoot: root, ActivatedAt: configured.ActivatedAt, Configured: explicit, VerificationState: "not_verified"})
			return &app.Projects[position]
		}
		position, found := others[root]
		if !found {
			position = len(app.otherProjects)
			others[root] = position
			app.otherProjects = append(app.otherProjects, projectCaptureStatus{ProjectRoot: root, VerificationState: "not_verified"})
		}
		return &app.otherProjects[position]
	}
	for _, reg := range s.regs {
		// A session is the app's when the app registered it and the
		// configuration publishes it now (AcceptSession), as the
		// collector decides.
		if reg.Harness.Name != name || !cfg.AcceptSession(reg) {
			continue
		}
		// Explicit observation survives imports and discovery. Registrations
		// written before this field existed retain their hook-origin evidence.
		observed := registrationHookObserved(reg)
		if observed {
			app.HookObserved = true
			project(reg.ProjectRoot).HookObserved = true
			if app.State == "waiting for first session" {
				app.State = "hook observed; waiting for capture"
			}
		}
		if reg.Imported() {
			// Import publication is not evidence that this app's hooks captured
			// the session. Actual subsequent hook observation is recorded above.
			// Imports only count toward imports and uploads, not verification.
			// Subagents go with their parent.
			if independentImportedSession(reg) {
				if reg.NativeChild {
					app.ImportedChildSessions++
				}
				app.ImportedSessions++
				project(reg.ProjectRoot).imported++
				if s.owed[reg.ArchiveSessionID].Blocked || issues[reg.ArchiveSessionID] != "" {
					app.importedWithIssues++
				}
				s.addUploading(&app, project(reg.ProjectRoot), reg, issues)
			}
			continue
		}
		if reg.IsChild() {
			app.SubagentSessions++
		} else {
			if reg.Replay != nil {
				app.ReplaySessions++
			}
			project(reg.ProjectRoot).sessions++
			s.addUploading(&app, project(reg.ProjectRoot), reg, issues)
		}
		// AcceptSession only admits a root without a configured pair
		// under its legacy branch (no projects configured at all). The
		// collector still publishes such sessions, so they count toward
		// the app even though there is no pair to attribute them to.
		var pair *projectCaptureStatus
		if position, found := pairIndex[reg.ProjectRoot]; found {
			pair = &app.Projects[position]
		}
		s.addSession(&app, pair, reg, cfg, home, issues, &readBackIssue)
	}
	if name == "codex" {
		health, found, healthErr := discovery.ReadHealth(home)
		if healthErr != nil || !found {
			health.Errors = []string{"discovery health unknown; run sync to produce a current summary"}

		}
		health.Enabled = cfg.Discovery != nil && cfg.Discovery.Enabled && cfg.Archive.Enabled && containsString(cfg.Harnesses, "codex")
		app.Discovery = &health
		app.CodexCaptureScope = string(cfg.EffectiveCodexCaptureScope())
	}
	finishReadBack(&app, readBackIssue)
	sortUploading(app.Uploading)
	return app
}

// addUploading lists a top-level session of app, and counts it for its
// project, when it has work not yet published: state.Outstanding's Pending,
// the definition every pending count uses, less a recorded capture gap,
// which the app's gaps report instead.
func (s statusSessions) addUploading(app *appStatus, project *projectCaptureStatus, reg archive.SessionRegistration, issues map[string]string) {
	o, found := s.owed[reg.ArchiveSessionID]
	if !found || !o.Pending() || o.Blocked {
		return
	}
	if o.WaitingForTranscript {
		// Nothing of it can be uploaded until the app writes a transcript,
		// which a Cursor chat with transcripts turned off never does.
		app.WaitingForTranscriptSessions++
		return
	}
	session := uploadingSession{
		ArchiveSessionID: reg.ArchiveSessionID, Project: reg.ProjectRoot, StartedAt: reg.SessionStartedAt,
		State: uploadingPending, Imported: reg.Imported(),
	}
	switch issue, deferred := issues[reg.ArchiveSessionID], s.deferred[reg.ArchiveSessionID]; {
	case issue != "":
		session.State, session.Issue = uploadingFailing, issue
	case deferred != "" && o.Scan:
		// The last pass put its scan off; the scan journal says it is still
		// owed, so the reason still applies.
		session.State, session.Deferred = uploadingRechecking, deferred
	case !o.Published:
		session.State = uploadingFirst
	}
	app.Uploading = append(app.Uploading, session)
	app.UploadingSessions++
	project.uploading++
}

// sortUploading puts failing sessions first, then the most recently
// started.
func sortUploading(sessions []uploadingSession) {
	slices.SortStableFunc(sessions, func(a, b uploadingSession) int {
		if a.State != b.State && (a.State == uploadingFailing || b.State == uploadingFailing) {
			if a.State == uploadingFailing {
				return -1
			}
			return 1
		}
		if started := b.StartedAt.Compare(a.StartedAt); started != 0 {
			return started
		}
		return strings.Compare(a.ArchiveSessionID, b.ArchiveSessionID)
	})
}

// addSession adds one accepted session's evidence to app and to its project
// pair (nil for a session no configured project owns):
// local capture and gaps, publication, and read-back verification. A session
// whose files cannot be read is skipped where it fails.
func (s statusSessions) addSession(app *appStatus, pair *projectCaptureStatus, reg archive.SessionRegistration, cfg config.Config, home string, issues map[string]string, readBackIssue *verificationOutcome) {
	app.Sessions++
	if pair != nil {
		pair.automaticCapture = true
	}
	if registrationHookObserved(reg) {
		app.HookObserved = true
	}
	gapsBefore := len(app.CaptureGaps)
	if pair != nil && registrationHookObserved(reg) {
		pair.HookObserved = true
	}
	if issue := issues[reg.ArchiveSessionID]; issue != "" {
		app.CaptureGaps = append(app.CaptureGaps, archive.CaptureGap{Code: issue, Detail: issueGapDetail(issue)})
	}
	if reg.Harness.Version != "" && !containsString(app.HarnessVersions, reg.Harness.Version) {
		app.HarnessVersions = append(app.HarnessVersions, reg.Harness.Version)
	}
	if app.State == "waiting for first session" {
		if app.HookObserved {
			app.State = "hook observed; waiting for capture"
		} else {
			app.State = "task found; waiting for capture"
		}
	}
	bundle, _, cacheStatus, found, err := s.store.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		s.skip(reg.ArchiveSessionID, err)
		return
	}
	if cacheStatus == state.CacheStatusBlocked {
		reason, _, e := s.store.LoadBlocked(reg.ArchiveSessionID)
		if e != nil {
			s.skip(reg.ArchiveSessionID, e)
			return
		}
		app.CaptureGaps = append(app.CaptureGaps, archive.CaptureGap{Code: string(reason), Detail: blockedReasonDetail(reason)})
	}
	if found {
		if cacheStatus != state.CacheStatusBlocked {
			app.CapturedLocally = true
			if pair != nil {
				pair.CapturedLocally = true
			}
		}
		app.CaptureGaps = append(app.CaptureGaps, bundle.Capture.Gaps...)
		if version := bundle.Capture.AdapterVersion; version != "" && !containsString(app.AdapterVersions, version) {
			app.AdapterVersions = append(app.AdapterVersions, version)
		}
	}
	// A blocked session with no publication has captured nothing;
	// one that was published earlier still counts as published below.
	if found && cacheStatus != state.CacheStatusBlocked && app.LastPublishedAt.IsZero() {
		app.State = "captured locally"
	}
	if len(app.CaptureGaps) > gapsBefore {
		app.SessionsWithCaptureGaps++
	}
	s.addPublication(app, pair, reg, cfg, home, readBackIssue)
}

// addPublication adds a session's last publication, if any, and its
// read-back verification to app and pair. readBackIssue keeps the worst
// read-back failure seen for the app: a mismatch outranks a transient one.
func (s statusSessions) addPublication(app *appStatus, pair *projectCaptureStatus, reg archive.SessionRegistration, cfg config.Config, home string, readBackIssue *verificationOutcome) {
	publishedBundle, at, published, e := s.store.LoadLastPublished(reg.ArchiveSessionID)
	if e != nil {
		s.skip(reg.ArchiveSessionID, e)
		return
	}
	if !published {
		return
	}
	app.Published = true
	app.PublishedSessions++
	if pair != nil {
		pair.Published = true
		pair.PublishedSessions++
	}
	app.State = "published; read-back pending"
	verification, e := readVerification(home, reg.ArchiveSessionID)
	if e != nil {
		s.skip(reg.ArchiveSessionID, e)
		return
	}
	verificationConfigurationID := sessionVerificationConfigurationID(cfg, reg)
	if verification.ConfigurationID == verificationConfigurationID && verification.PublishedAt.Equal(at) && !verification.VerifiedAt.IsZero() {
		app.VerifiedSessions++
		if pair != nil {
			pair.VerifiedSessions++
			if verification.VerifiedAt.After(pair.VerifiedAt) {
				pair.VerifiedAt = verification.VerifiedAt
			}
		}
		app.VerificationState = "verified_at_recorded_time"
		if verification.VerifiedAt.After(app.VerifiedAt) {
			app.VerifiedAt = verification.VerifiedAt
		}
		app.State = "published; source verified"
		if version := publishedBundle.Capture.Harness.Version; version != "" && !containsString(app.verifiedHarnessVersions, version) {
			app.verifiedHarnessVersions = append(app.verifiedHarnessVersions, version)
		}
	} else if !verification.VerifiedAt.IsZero() {
		app.VerificationState = "stale"
	} else if verification.ConfigurationID == verificationConfigurationID && verification.PublishedAt.Equal(at) && verification.Attempts > 0 {
		// The collector tried and could not read this publication
		// back; a mismatch outranks a transient failure.
		if verification.Outcome == verificationOutcomeMismatch || *readBackIssue != verificationOutcomeMismatch {
			*readBackIssue = verification.Outcome
			app.readBackFailure = verification
			app.VerificationDetail = fmt.Sprintf("%s: %s (attempt %d; next retry %s)", verification.Outcome, verification.LastError, verification.Attempts, formatTimeOrNever(verification.NextRetryAt))
		}
	}
	if at.After(app.LastPublishedAt) {
		app.LastPublishedAt = at
	}
}

// finishReadBack decides, once every session is counted, whether each of
// app's projects and the app as a whole are read-back verified.
func finishReadBack(app *appStatus, readBackIssue verificationOutcome) {
	app.ReadBackVerified = len(app.Projects) > 0
	if len(app.Projects) == 0 || (app.Name == "codex" && app.CodexCaptureScope == string(config.CodexAllProjects)) {
		// Legacy configuration and all-mode require actual automatic
		// publication evidence, never verification of an import-only root.
		app.ReadBackVerified = app.PublishedSessions > 0 && app.VerifiedSessions == app.PublishedSessions
	}
	for i := range app.Projects {
		pair := &app.Projects[i]
		pair.ReadBackVerified = pair.PublishedSessions > 0 && pair.VerifiedSessions == pair.PublishedSessions
		switch {
		case pair.ReadBackVerified:
			pair.VerificationState = "verified_at_recorded_time"
		case pair.Published:
			pair.VerificationState = "incomplete"
		case pair.CapturedLocally:
			pair.VerificationState = "captured_local"
		case pair.HookObserved:
			pair.VerificationState = "hook_observed"
		default:
			pair.VerificationState = "not_verified"
		}
		if requiresAutomaticCapture(*app, *pair) && !pair.ReadBackVerified {
			app.ReadBackVerified = false
		}
	}
	if app.Published && !app.ReadBackVerified {
		app.State = "published; read-back pending"
		app.VerificationState = "incomplete"
		switch readBackIssue {
		case verificationOutcomeMismatch:
			app.VerificationState = "read_back_mismatch"
		case verificationOutcomeFailed:
			app.VerificationState = "read_back_failed"
		case verificationOutcomeVerified:
			// A verified session is not a read-back issue.
		}
	}
}

// Included-project mode keeps its configured capture checklist. In all-mode,
// historical imports alone never add a requirement to start a fresh task there.
func requiresAutomaticCapture(app appStatus, pair projectCaptureStatus) bool {
	return app.Name != "codex" || app.CodexCaptureScope != string(config.CodexAllProjects) || pair.automaticCapture
}

// readInstalledApps fills in each app's installed version, support, and
// hook state, and returns what is wrong with the executable setup installed
// ("" when nothing is).
func readInstalledApps(view *statusView, cfg config.Config, home, userHome string, env Env) (binaryProblem string) {
	// Hooks are checked against the path setup installed, not the path this
	// status process runs from; older configurations fall back to the latter.
	executable, executableErr := cfg.InstalledExecutable, error(nil)
	if executable == "" {
		executable, executableErr = env.executable()
	}
	// Every installed hook runs the executable setup recorded. If that file
	// was moved or deleted, the hook configuration still matches exactly, so
	// comparing it alone would report healthy hooks that fail on every event.
	// An uninstalled archive has no hooks left to break.
	hookFiles := env.installedHookFiles(userHome, cfg)
	if cfg.InstalledExecutable != "" && cfg.Archive.Enabled {
		binaryProblem = executableProblem(cfg.InstalledExecutable)
	}
	if binaryProblem != "" {
		view.Warnings = append(view.Warnings, fmt.Sprintf("The agent-archive executable that setup installed at %s is %s; every app hook runs it, so capture has stopped. Run agent-archive setup --refresh, from an installed agent-archive, to point the hooks at it.", cfg.InstalledExecutable, binaryProblem))
	}
	discovered, err := readApplicationDiscoveries(home)
	if err != nil {
		// Advisory only: a damaged observation file degrades installed
		// versions to unknown rather than hiding the rest of the status.
		view.Warnings = append(view.Warnings, fmt.Sprintf("Installed versions could not be read from %s: %v. Run agent-archive setup to refresh them.", applicationDiscoveriesPath(home), err))
		discovered = map[string]applicationDiscovery{}
	}
	for i := range view.Apps {
		if d := view.Apps[i].Discovery; d != nil && !d.LastAttempt.IsZero() && (env.now().Before(d.LastAttempt) || env.now().Sub(d.LastAttempt) > scanStaleAfter) {
			d.Errors = append(d.Errors, "discovery health stale; run sync for a current scan")
		}
		appDiscovery := discovered[view.Apps[i].Name]
		if appDiscovery.VersionState == "" {
			appDiscovery.VersionState = "unknown"
		}
		if !appDiscovery.ObservedAt.IsZero() && (env.now().Before(appDiscovery.ObservedAt) || env.now().Sub(appDiscovery.ObservedAt) > 24*time.Hour) {
			appDiscovery.VersionState = "stale"
		}
		view.Apps[i].Installed = appDiscovery.Installed
		view.Apps[i].InstalledVersion = appDiscovery.Version
		view.Apps[i].VersionSource = appDiscovery.VersionSource
		view.Apps[i].VersionObservedAt = appDiscovery.ObservedAt
		view.Apps[i].VersionKind = appDiscovery.VersionKind
		view.Apps[i].VersionState = appDiscovery.VersionState
		view.Apps[i].Capabilities = captureCapabilityProfile(env.agentRegistry(), view.Apps[i].Name)
		view.Apps[i].VersionSupport, view.Apps[i].VersionSupportReason = installedVersionSupportDetail(appDiscovery, view.Apps[i].verifiedHarnessVersions)
		in := env.installation(home, userHome)
		installed, e := hooks.Installed(hookFiles, in.hook(executable), view.Apps[i].Name)
		if !installed && e == nil {
			inspection, inspectErr := hooks.Inspect(hookFiles, in.hook(executable), view.Apps[i].Name)
			view.Apps[i].hooksNeedRepair = inspection.Owned
			if inspectErr != nil {
				e = inspectErr
			}
		}
		if others, err := hooks.OtherInstallations(hookFiles, in.owner(), view.Apps[i].Name); err == nil && len(others) > 0 {
			for _, other := range others {
				view.Apps[i].OtherInstallations = append(view.Apps[i].OtherInstallations, cmp.Or(other.DataHome, other.Command, "default"))
			}
			view.Warnings = append(view.Warnings, describeOtherInstallations(hookFiles[view.Apps[i].Name], view.Apps[i].Name, others))
		}
		switch {
		case binaryProblem != "":
			view.Apps[i].Hooks = hooksBroken
		case e != nil:
			view.Apps[i].Hooks = "unknown"
			view.Warnings = append(view.Warnings, fmt.Sprintf("%s hooks could not be checked: %v. Fix or restore %s, then run agent-archive setup.", appName(view.Apps[i].Name), e, hookFiles[view.Apps[i].Name]))
		case executableErr != nil:
			view.Apps[i].Hooks = "unknown"
		case !installed:
			view.Apps[i].Hooks = "missing or incomplete"
		default:
			view.Apps[i].Hooks = "installed"
		}
	}
	return binaryProblem
}

// statusBackground is the background collector as status found it: its job,
// the words its scheduler goes by, and the program the job runs when that
// program is no longer usable (problem says why; both "" otherwise).
// environmentProblems say why the environment the job sets cannot load the
// configured S3 profile.
type statusBackground struct {
	ref                 scheduler.Ref
	words               scheduler.Words
	program             string
	problem             string
	environmentProblems []string
}

// installedRef is the job status reports on: this installation's own, or, when
// it has no definition, the first collector an earlier release installed for
// it under another label.
func installedRef(in installation, userHome string, env Env) scheduler.Ref {
	own := in.ref()
	if env.jobDefinition(userHome, own).Defined {
		return own
	}
	// What blocks setup (the prototype's job) does not block status.
	jobs, _ := in.installed(userHome)
	for _, job := range jobs {
		if job.Alias == scheduler.EarlierLabel {
			return job.Ref
		}
	}
	return own
}

// readBackground reads the background collector's scheduler state, and what
// its definition actually runs.
func readBackground(view *statusView, cfg config.Config, home, userHome string, env Env) statusBackground {
	in := env.installation(home, userHome)
	ref, words := installedRef(in, userHome, env), in.sched().Words()
	job := env.jobStatus(userHome, ref)
	view.Background = string(job.State)
	for _, note := range job.Degraded {
		view.BackgroundWarnings = append(view.BackgroundWarnings, sentence(note))
	}
	view.backgroundTool = words.Tool
	// launchd reports a job whose program is gone as loaded (it only fails
	// when it fires), so read the program the definition actually runs.
	backgroundProgram, backgroundProblem := "", ""
	var environmentProblems []string
	if cfg.Archive.Enabled {
		if job.Program != "" {
			backgroundProgram, backgroundProblem = job.Program, executableProblem(job.Program)
		}
		// The collector has only the environment its definition sets, which
		// may no longer match the files and programs the profile needs.
		if job.Env != nil {
			environmentProblems = collectorEnvironmentProblems(cfg.Storage, job.Env, userHome, in.sched().DefaultPATH())
			view.Warnings = append(view.Warnings, environmentProblems...)
			if drift := env.awsFilesDrift(cfg.Storage, job.Env, userHome); drift != "" {
				view.Warnings = append(view.Warnings, drift)
			}
			view.Warnings = append(view.Warnings, env.xdgDrift(job.Env)...)
		}
	}
	if backgroundProblem != "" {
		view.Background = backgroundBroken
		view.Warnings = append(view.Warnings, fmt.Sprintf("The background collector's %s runs %s, which is %s, so scheduled collection has stopped.", words.Job, backgroundProgram, backgroundProblem))
	}
	return statusBackground{ref: ref, words: words, program: backgroundProgram, problem: backgroundProblem, environmentProblems: environmentProblems}
}

// scanStaleAfter is how old the last scan may be before status says
// collection needs attention.
const scanStaleAfter = 5 * time.Minute

// chooseNextStep sets the overall state and the one next step status
// suggests. Later checks outrank earlier ones: each overwrites the state and
// step of any before it.
func chooseNextStep(view *statusView, cfg config.Config, home string, env Env, binaryProblem string, background statusBackground) {
	view.State = "Ready"
	view.problem = ""
	view.Next = "Keep working. Run agent-archive list to inspect archived sessions."
	if len(view.Apps) == 0 {
		view.State = "Needs attention"
		view.problem = "No apps are selected"
		view.Next = "Run agent-archive setup and select at least one application."
	} else if len(view.Projects) == 0 && !codexOnlyAllProjects(cfg) {
		view.State = "Needs attention"
		view.problem = "No projects are included"
		view.Next = "Run agent-archive setup and include at least one project."
	}
	chooseCaptureStep(view)
	chooseInstallationStep(view, background)
	if !view.Collector.LastScanAt.IsZero() && env.now().Sub(view.Collector.LastScanAt) > scanStaleAfter {
		view.State = "Needs attention"
		view.problem = "No scan in over 5 minutes"
		view.Next = "The last scan is over 5 minutes old. Run agent-archive sync to check collection."
	}
	// sync cannot help while another process holds the collector lock. The
	// holder records when it took the lock, so a pass that started a moment
	// ago is never mistaken for a hung one.
	if record, ok := readCollectorLockRecord(home); ok && env.now().Sub(record.Since) > collectLockStuckAfter && processAlive(record.PID) && collectorLockHeld(home) {
		view.State = "Needs attention"
		view.problem = "Collection is stuck"
		view.Next = fmt.Sprintf("Collection is stuck: %s (process %d) has held the collector lock since %s, %s, well past a pass's time limit. If that command is no longer doing anything, quit process %d (in Activity Monitor or with kill %d); the next pass then resumes.", record.Holder, record.PID, record.Since.UTC().Format("2006-01-02 15:04 UTC"), durationAgo(env.now().Sub(record.Since)), record.PID, record.PID)
	}
	// Failed sessions of kinds that are not about storage lead with their
	// own problem and next step, or with none when there is nothing to do.
	problem, next, byIssue := issueHeadline(view.Collector)
	switch {
	case view.Collector.LastError == "":
	case byIssue:
		if problem != "" {
			view.State = "Needs attention"
			view.problem = problem
			view.Next = next
			view.lastErrorProblem, view.lastErrorByIssue = problem, true
		}
	default:
		view.State = "Needs attention"
		view.problem = generalSyncProblem
		view.Next = "Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage."
		// A Keychain failure has one specific fix; status.json keeps only
		// the error text, so it is recognized from that.
		if action := credentials.RecoveryActionForMessage(view.Collector.LastError); action != "" {
			view.Next = action
		}
		if strings.Contains(view.Collector.LastError, backgroundCredentialProcessFailure) {
			view.problem = "The background collector couldn't get AWS credentials"
			view.Next = "The background collector couldn't get credentials from your AWS profile's credential_process. It runs without most of your shell's environment: if the helper needs a setting the collector doesn't get (see Environment variables in the configuration reference), put it in the helper's own configuration; if it needs you to unlock it or sign in, do that. Check with agent-archive sync, then run agent-archive setup again from a shell where it works."
		}
		view.lastErrorProblem = view.problem
	}
	if len(background.environmentProblems) > 0 {
		view.State = "Needs attention"
		view.problem = "The background collector can't load your AWS profile"
		view.Next = "The background collector cannot load your AWS profile (see the warning above). Run agent-archive setup again from a shell where the profile works, so the collector gets that shell's AWS settings files and PATH."
	}
	if cfg.Paused {
		view.State = "Paused"
		view.problem = "Collection is paused"
		view.Next = "Run agent-archive resume when ready. Registered sessions can catch up after resume."
	}
	// A moved or deleted binary is the root cause of every symptom above (a
	// stale scan, a failing hook), and pausing does not stop the apps from
	// running hooks that now fail, so it outranks all of them.
	if binaryProblem != "" || background.problem != "" {
		moved := cfg.InstalledExecutable
		if binaryProblem == "" {
			moved = background.program
		}
		view.State = "Needs attention"
		view.problem = "agent-archive can't run from where setup installed it"
		view.Next = fmt.Sprintf("agent-archive is no longer usable at %s. Run agent-archive setup --refresh from the binary's new location to point the hooks and background collector at it.", moved)
	}
	if !cfg.Archive.Enabled {
		view.State = "Not installed"
		view.problem = "Agent Archive is uninstalled"
		view.Next = "Local data is kept. Run agent-archive setup to reinstall."
	}
	if setupjournal.TransactionPending(home) {
		view.State = "Setup needs recovery"
		view.problem = "Setup was interrupted"
		view.Next = "Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now."
	}
}

// chooseCaptureStep points at the first project of the first app whose
// capture is not yet read-back verified, if any.
func chooseCaptureStep(view *statusView) {
	for _, app := range view.Apps {
		if app.Name == "codex" && app.CodexCaptureScope == string(config.CodexAllProjects) && app.Sessions == 0 {
			view.State = "Waiting for capture"
			view.problem = "Waiting for the first Codex task"
			if discoveryEnabled(app) {
				view.Next = "Start a supported new Codex task in any non-excluded project, then run agent-archive sync. Hook approval is optional for discovery."
			} else {
				view.Next = "Approve the Codex archive hooks, then start a supported new task in any non-excluded project. Automatic discovery is off."
			}
			break
		}
		for _, pair := range app.Projects {
			if pair.ReadBackVerified || !requiresAutomaticCapture(app, pair) {
				continue
			}
			view.State = "Waiting for capture"
			view.problem = "Waiting for the first " + appName(app.Name) + " session"
			if app.VerifiedSessions > 0 {
				// The app is captured elsewhere; only this project is new.
				view.problem = "Waiting for a " + appName(app.Name) + " session in " + pair.ProjectRoot
			}
			switch {
			case app.Capabilities.FreshStart.State == capabilityUnavailable && !pair.HookObserved && !discoveryEnabled(app):
				view.Next = app.Capabilities.FreshStart.NextAction
			case pair.Published:
				view.problem = appName(app.Name) + "'s upload hasn't been read back yet"
				view.Next = "Run agent-archive sync to retry read-back verification for " + appName(app.Name) + " in " + pair.ProjectRoot + "."
			case pair.HookObserved || (discoveryEnabled(app) && pair.sessions > 0):
				view.problem = appName(app.Name) + " session seen but not uploaded yet"
				view.Next = "Run agent-archive sync to capture and publish the " + appName(app.Name) + " session in " + pair.ProjectRoot + "."
			case discoveryEnabled(app):
				view.Next = "Start a supported new Codex task in " + pair.ProjectRoot + ", then run agent-archive sync. Hook approval is optional for discovery."
			default:
				view.Next = "Review hook approval in " + appName(app.Name) + ", then start a new session in " + pair.ProjectRoot + "."
			}
			break
		}
		if view.State == "Waiting for capture" {
			break
		}
	}
}

// chooseInstallationStep points at hooks that are not installed, then at a
// background collector that is not loaded (plist is its LaunchAgent).
func chooseInstallationStep(view *statusView, background statusBackground) {
	for _, app := range view.Apps {
		if app.Hooks != "installed" && (!discoveryEnabled(app) || app.Hooks == hooksBroken || app.Hooks == "unknown" || app.hooksNeedRepair) {
			view.State = "Needs attention"
			view.problem = appName(app.Name) + " hooks aren't installed"
			if app.Hooks == "unknown" {
				view.problem = appName(app.Name) + " hooks couldn't be checked"
			}
			view.Next = "Run agent-archive setup to check the hooks for " + appName(app.Name) + "."
			if app.Hooks != "unknown" {
				// Missing, incomplete, or running an executable that is gone:
				// refresh reinstalls what setup saved, with no questions.
				view.Next = "Run agent-archive setup --refresh to reinstall the hooks for " + appName(app.Name) + "."
			}
			if len(app.OtherInstallations) > 0 {
				// setup refuses to install beside them, so it is not the way out.
				view.problem = "Another installation's hooks are in " + appName(app.Name)
				view.Next = "Another agent-archive installation's hooks are in " + appName(app.Name) + "'s hook file (see the warning above). Remove that installation, or give this one its own HOME, then run agent-archive setup."
			}
			break
		}
	}
	if !jobActive(scheduler.JobState(view.Background)) {
		view.State = "Needs attention"
		view.problem = "The background collector isn't running"
		if view.Background == "unknown" {
			view.problem = "The background collector couldn't be checked"
		}
		view.Next = "Run agent-archive setup to restore the background collector."
		if view.Background == string(scheduler.AnotherInstallation) {
			// setup refuses to replace that job, so it is not the way out.
			view.problem = "Another installation's collector has this installation's " + background.words.Name
			view.Next = fmt.Sprintf("Another agent-archive installation's collector runs under this installation's %s %s (%s), and setup will not replace it. Set AGENT_ARCHIVE_HOME to a data directory of this installation's own, or uninstall the other installation.", background.words.Manager, background.words.Name, background.ref)
		}
	}
}

// collectorLockHeld reports whether another process holds collector.lock
// right now. flock has no query, so it tries the lock and releases it at
// once; a collector starting in that instant skips one pass, as it would
// for any other holder.
func collectorLockHeld(home string) bool {
	unlock, err := local.Lock(home)
	if err != nil {
		return errors.Is(err, local.ErrBusy)
	}
	unlock()
	return false
}

// importedSessionCounts counts the sessions backfill imported, leaving out
// their subagents, and how many of those are pending (state.Outstanding)
// under cfg, the same definition as status's own pending count. withIssues
// counts those with a recorded capture gap or a failed last scan: status
// leaves imports out of each app's own gaps and issues, so they are
// reported here. owed holds every import's Outstanding.
func importedSessionCounts(cfg config.Config, regs []archive.SessionRegistration, owed map[string]state.Outstanding, issues map[string]string) (imported, pending, withIssues int) {
	for _, reg := range regs {
		if !reg.Imported() || !independentImportedSession(reg) {
			continue
		}
		imported++
		o := owed[reg.ArchiveSessionID]
		if o.Blocked || issues[reg.ArchiveSessionID] != "" {
			withIssues++
		}
		if cfg.AcceptSession(reg) && o.Pending() {
			pending++
		}
	}
	return imported, pending, withIssues
}

// importPending reports whether an imported session is pending under cfg:
// the one definition state.Outstanding gives, for a caller that has not
// listed the queued requests.
func importPending(store *state.Store, cfg config.Config, reg archive.SessionRegistration) (bool, error) {
	owed, err := importOutstanding(store, cfg, reg)
	return owed.Pending(), err
}

// importOutstanding is what an imported session still owes under cfg (see
// state.Outstanding), or nothing for a session cfg does not publish.
func importOutstanding(store *state.Store, cfg config.Config, reg archive.SessionRegistration) (state.Outstanding, error) {
	if !cfg.AcceptSession(reg) {
		return state.Outstanding{}, nil
	}
	_, requested, err := store.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		return state.Outstanding{}, err
	}
	return store.Outstanding(reg, requested)
}

const (
	// hooksBroken: the hook configuration is in place but runs an executable
	// that no longer exists or cannot be run.
	hooksBroken = "broken"
	// backgroundBroken: the LaunchAgent is in place but runs such an executable.
	backgroundBroken = "broken"
)

// executableProblem says why path cannot be run, or "" when it can.
func executableProblem(path string) string {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "missing"
	case err != nil:
		return "unreadable"
	case !info.Mode().IsRegular():
		return "not a regular file"
	case info.Mode().Perm()&0o111 == 0:
		return "not executable"
	}
	return ""
}

func formatTimeOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// Codes are an API; changing display wording must not change them.
func statusCode(label string) string {
	codes := map[string]string{
		"Not set up": "not_configured", "Setup saved": "setup_pending",
		"Ready": "ready", "Waiting for capture": "awaiting_capture",
		"Needs attention": "needs_attention", "Paused": "paused",
		"Not installed": "not_installed", "Setup needs recovery": "recovery_required",
		"waiting for first session":          "awaiting_session",
		"hook observed; waiting for capture": "hook_observed",
		"task found; waiting for capture":    "awaiting_capture",
		"published; read-back pending":       "published",
		"captured locally":                   "captured_local", "published; source verified": "published_source_verified",
	}
	if code, ok := codes[label]; ok {
		return code
	}
	return "unknown"
}

// statusScreen is what the text status is drawn with: the output's style,
// the clock times are shown relative to, the home folder shown as ~, and
// whether to add the rows and Details section status --verbose adds.
type statusScreen struct {
	style   textStyle
	now     time.Time
	home    string
	verbose bool
}

// uploadingRowsShown is how many uploading sessions the default status
// lists under an app; status APP lists them all.
const uploadingRowsShown = 5

// printStatus writes the text status: the overall state, what to do about
// it, the Capture and Storage sections, and anything else worth knowing.
// Its length does not grow with the number of projects: per-project rows,
// the included projects, skill evidence and the Imported line are left to
// status --verbose, which also adds the Details section with the codes,
// exact times and raw errors, and to status --json.
func printStatus(out io.Writer, view statusView, sc statusScreen) {
	s := sc.style
	sc.printHeader(out, view)
	if !view.configured {
		return
	}
	terminal.Printf(out, "\n%s\n", s.bold("Capture"))
	sc.printRows(out, sc.captureRows(view))
	terminal.Printf(out, "\n%s\n", s.bold("Storage"))
	sc.printRows(out, sc.storageRows(view))
	sc.printNotes(out, view)
	if sc.verbose {
		terminal.Printf(out, "\n%s\n", s.bold("Details"))
		printStatusDetails(out, view)
	}
	terminal.Println(out)
	if sc.verbose {
		// The short screen leaves out the tip a Ready status has in place
		// of a problem; --verbose keeps it.
		if view.State == "Ready" {
			for i, line := range sc.proseLines(view.Next) {
				lead := s.dim("Next:") + " "
				if i > 0 {
					lead = "      "
				}
				terminal.Println(out, s.hang(lead, line))
			}
		}
		terminal.Printf(out, "%s %s\n", s.dim("As JSON:"), s.cmd("agent-archive status --json"))
		return
	}
	commands := []string{"agent-archive status --verbose"}
	if app := footerApp(view); app != "" {
		commands = append(commands, "agent-archive status "+app)
	}
	sc.printFooter(out, commands...)
}

// printFooter writes the commands that show more, each as typed, on one
// dim line, or one per line when the terminal is too narrow for that.
func (sc statusScreen) printFooter(out io.Writer, commands ...string) {
	line := "More: " + strings.Join(commands, " · ")
	if sc.style.width > 0 && visibleWidth(line) > sc.style.width {
		line = "More: " + strings.Join(commands, "\n      ")
	}
	for text := range strings.SplitSeq(line, "\n") {
		terminal.Println(out, sc.style.dim(text))
	}
}

// printHeader writes the overall state and, unless all is well, the one
// thing to do now.
func (sc statusScreen) printHeader(out io.Writer, view statusView) {
	terminal.Printf(out, "%s  %s\n", sc.style.bold("Agent Archive"), sc.stateLabel(view.State))
	if view.State != "Ready" {
		terminal.Println(out)
		sc.printNextStep(out, view)
	}
}

// printNotes writes the Notes section, when there is anything in it.
func (sc statusScreen) printNotes(out io.Writer, view statusView) {
	if notes := sc.noteRows(view); len(notes) > 0 {
		terminal.Printf(out, "\n%s\n", sc.style.bold("Notes"))
		sc.printRows(out, notes)
	}
}

// footerApp is the app the footer suggests status APP for: the first with
// sessions, else the first configured, or "" with none.
func footerApp(view statusView) string {
	for _, app := range view.Apps {
		if app.Sessions > 0 || app.ImportedSessions > 0 {
			return app.Name
		}
	}
	if len(view.Apps) > 0 {
		return view.Apps[0].Name
	}
	return ""
}

// stateLabel is the overall state after a dot, green when all is well and
// yellow when it needs the user.
func (sc statusScreen) stateLabel(state string) string {
	label := "● " + state
	//lint:ignore LV1001 the overall state is an open-ended label (statusView.State); statusCode maps it to the stable code
	switch state {
	case "Ready":
		return sc.style.ok(label)
	case "Not installed":
		return sc.style.dim(label)
	}
	return sc.style.warn(label)
}

// printNextStep writes the one thing to do now: what is wrong, and under it
// how to fix it.
func (sc statusScreen) printNextStep(out io.Writer, view statusView) {
	indent := "    "
	problem, _ := sc.headline(view)
	if problem == "" {
		indent = "  "
	} else {
		terminal.Println(out, sc.style.hang("  "+sc.style.warnMark()+" ", sc.tilde(problem)))
	}
	for _, line := range sc.proseLines(view.Next) {
		terminal.Println(out, sc.style.hang(indent, line))
	}
}

// statusRow is one line of a status section: a colored symbol, columns
// aligned with the section's other rows, dim detail after them, and notes on
// the lines below.
type statusRow struct {
	mark   string
	cells  []string
	detail string
	notes  []statusNote
}

// statusNote is a line under a row: an optional symbol and its text.
type statusNote struct {
	mark string
	text string
}

// info is the symbol of a row that is only information.
func (sc statusScreen) info() string { return sc.style.dim("·") }

// printRows writes rows, aligning the columns of those that have more than
// one column or a detail. On a terminal too narrow for a row's detail beside
// its columns, the detail goes on the lines under it.
func (sc statusScreen) printRows(out io.Writer, rows []statusRow) {
	var widths []int
	for _, row := range rows {
		for i, cell := range row.cells {
			if i == len(row.cells)-1 && row.detail == "" {
				break
			}
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	for _, row := range rows {
		var columns strings.Builder
		columns.WriteString("  " + row.mark + " ")
		text := ""
		for i, cell := range row.cells {
			if i == len(row.cells)-1 && row.detail == "" {
				text = cell
				break
			}
			columns.WriteString(cell + strings.Repeat(" ", widths[i]-visibleWidth(cell)+3))
		}
		prefix := columns.String()
		if row.detail != "" {
			text = sc.style.dim(row.detail)
		}
		if sc.style.width > 0 && row.detail != "" && visibleWidth(prefix+text) > sc.style.width {
			// Too narrow for the detail beside the columns: it goes under them.
			terminal.Println(out, strings.TrimRight(prefix, " "))
			prefix = "    "
		}
		terminal.Println(out, strings.TrimRight(sc.style.hang(prefix, text), " "))
		for _, note := range row.notes {
			lead := "    "
			if note.mark != "" {
				lead += note.mark + " "
			}
			terminal.Println(out, sc.style.hang(lead, note.text))
		}
	}
}

// captureRows are the Capture section: one row per app, with the sessions
// it is uploading under it, then what capture skipped and apps whose
// sessions were only imported. status --verbose adds the included
// projects, skill evidence and imports.
func (sc statusScreen) captureRows(view statusView) []statusRow {
	var rows []statusRow
	for _, app := range view.Apps {
		rows = append(rows, sc.appRow(app, uploadingRowsShown))
	}
	if len(view.Apps) == 0 {
		// A configuration without apps (an older one written by hand):
		// the collector still publishes what is registered.
		text := "No apps selected"
		if n := view.Collector.PendingCount; n > 0 {
			text += fmt.Sprintf(" · %d pending", n)
		}
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{text}})
	}
	if recovery := view.IdentityRecovery; recovery != nil {
		mark, text := sc.info(), "Identity recovery: complete"
		switch {
		case recovery.Complete:
		case recovery.Pending && recovery.Phase != "unknown":
			text = "Identity recovery: pending (" + strings.ReplaceAll(recovery.Phase, "-", " ") + "); sync advances bounded local work, additional passes may be needed."
		default:
			mark = sc.style.warnMark()
			text = "Identity recovery: unknown; run agent-archive sync for current bounded local evidence."
		}
		rows = append(rows, statusRow{mark: mark, cells: []string{text}})
	}
	for _, app := range view.importedApps {
		rows = append(rows, sc.importedAppRow(app, uploadingRowsShown))
	}
	for _, diagnostic := range view.CaptureDiagnostics {
		location := ""
		if diagnostic.ProjectRoot != "" {
			location = " in " + sc.path(diagnostic.ProjectRoot)
		}
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{fmt.Sprintf("%s skipped a session%s %s: %s", appName(diagnostic.Harness), location, relativeAge(sc.now, diagnostic.ObservedAt), capture.DiagnosticMessage(diagnostic.Code))}})
	}
	if !sc.verbose {
		return rows
	}
	projects := "none included"
	if len(view.Projects) > 0 {
		shown := make([]string, len(view.Projects))
		for i, project := range view.Projects {
			shown[i] = sc.path(project)
		}
		projects = strings.Join(shown, ", ")
	}
	rows = append(rows, statusRow{mark: sc.info(), cells: []string{"Projects: " + projects}})
	rows = append(rows, statusRow{mark: sc.info(), cells: []string{"Skill evidence: " + view.SkillEvidence}})
	if view.Retention != nil {
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{"Retention: " + config.RetentionLabel(view.Retention.Days)}})
	}
	if view.AgentSkillsDisabled {
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{"Agent skills: turned off; " + sc.style.cmd("agent-archive setup --skills") + " turns them on"}})
	}
	if view.ImportedSessions > 0 {
		imported := fmt.Sprintf("Imported (all destinations): %s, %d waiting to upload", plural(view.ImportedSessions, "session"), view.ImportedPending)
		if view.ImportedWithIssues > 0 {
			imported += fmt.Sprintf(", %d with a capture gap or failed scan", view.ImportedWithIssues)
		}
		if view.LastImport != "" {
			imported += "; last import " + view.LastImport
		}
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{imported}})
	}
	return rows
}

// appRow is one app's capture: its version and hooks, how many sessions it
// has captured, imported and is uploading, the uploading sessions (at most
// limit of them, or all when limit is 0), and anything that needs a closer
// look.
func (sc statusScreen) appRow(app appStatus, limit int) statusRow {
	s := sc.style
	name := appName(app.Name)
	if version := displayVersion(app.InstalledVersion); version != "" {
		name += " " + version
	}
	row := statusRow{mark: s.okMark(), cells: []string{name, hooksLabel(app.Hooks)}, detail: appCounts(app)}
	hint := ""
	if _, ok := hookApproval[app.Name]; ok && app.Hooks == "installed" && !app.HookObserved && !discoveryEnabled(app) {
		hint = "Approve the archive hooks with " + s.cmd("/hooks") + " in " + appName(app.Name) + "."
		if sc.verbose {
			hint = fmt.Sprintf(hookApproval[app.Name], s.cmd("/hooks"))
		}
		// Installation alone does not establish hook execution or trust.
		row.cells[1] = "hooks installed"
	}
	freshStart := app.Capabilities.FreshStart.State == capabilityUnavailable && !discoveryEnabled(app)
	switch {
	case app.Hooks == hooksBroken:
		row.mark = s.failMark()
	case (app.Hooks != "installed" && !optionalCodexHooks(app)), app.readBackFailure.Attempts > 0:
		row.mark = s.warnMark()
	case app.Sessions == 0 && (hint != "" || freshStart):
		// Nothing captured yet, and something the user can do about it.
		row.mark = s.warnMark()
	}
	if hint != "" {
		row.notes = append(row.notes, statusNote{text: hint})
	}
	if freshStart {
		row.notes = append(row.notes, statusNote{s.warnMark(), "New sessions can't be captured yet: " + app.Capabilities.FreshStart.NextAction})
	}
	sc.addCodexDiscoveryRow(&row, app)
	if discoveryEnabled(app) && app.Hooks != "installed" && !optionalCodexHooks(app) {
		row.notes = append(row.notes, statusNote{s.warnMark(), "Capture hooks need attention: " + hooksLabel(app.Hooks) + "; run agent-archive status --verbose for details."})
	}
	row.notes = append(row.notes, sc.preSetupNotes(app)...)

	if sc.verbose {
		row.notes = append(row.notes, statusNote{sc.info(), appProgress(sc.now, app)})
		if len(app.Projects) > 1 && !app.ReadBackVerified {
			for _, pair := range app.Projects {
				mark := sc.info()
				if pair.ReadBackVerified {
					mark = s.okMark()
				}
				row.notes = append(row.notes, statusNote{mark, sc.path(pair.ProjectRoot) + ": " + pairProgress(pair)})
			}
		}
	}
	row.notes = append(row.notes, sc.uploadingNotes(app, limit)...)
	if failure := app.readBackFailure; failure.Attempts > 0 {
		row.notes = append(row.notes, statusNote{s.warnMark(), sc.readBackFailure(failure)})
	}
	//lint:ignore LV1001 the reason codes are untyped constants in capabilities.go, which computes this field
	switch app.VersionSupportReason {
	case supportReasonNoMatchingVersion:
		row.notes = append(row.notes, statusNote{sc.info(), "Sessions verified so far came from a different " + appName(app.Name) + " version."})
	case supportReasonVersionSourceMismatch:
		row.notes = append(row.notes, statusNote{sc.info(), "The installed version and the sessions' versions are numbered differently, so they can't be compared."})
	}
	row.notes = append(row.notes, sc.gapNotes(app)...)
	return row
}

// importedAppRow is an app whose sessions were imported but whose hooks
// were never set up: its imports, and the ones it is uploading.
func (sc statusScreen) importedAppRow(app appStatus, limit int) statusRow {
	// Its hooks were never set up, so there are no sessions of its own to
	// count.
	row := statusRow{mark: sc.info(), cells: []string{appName(app.Name), "imported only"}, detail: strings.TrimPrefix(appCounts(app), "no sessions yet · ")}
	row.notes = append(row.notes, sc.uploadingNotes(app, limit)...)
	row.notes = append(row.notes, sc.gapNotes(app)...)
	return row
}

// gapNotes count an app's sessions and imports with capture gaps; status
// --json has the gaps themselves.
func (sc statusScreen) gapNotes(app appStatus) []statusNote {
	var notes []statusNote
	if n := app.WaitingForTranscriptSessions; n > 0 {
		unit := "session"
		if app.Name == "cursor" {
			unit = "chat"
		}
		text := plural(n, unit) + " have no transcript yet"
		if n == 1 {
			text = "1 " + unit + " has no transcript yet"
		}
		if app.Name == "cursor" {
			text += " (a chat with transcripts turned off in Cursor never gets one)"
		}
		notes = append(notes, statusNote{sc.info(), text})
	}
	if len(app.CaptureGaps) > 0 {
		text := plural(app.SessionsWithCaptureGaps, "session") + " have capture gaps"
		if app.SessionsWithCaptureGaps == 1 {
			text = "1 session has capture gaps"
		}
		if sc.verbose {
			text = fmt.Sprintf("%s with a capture gap (%s recorded; details in status --json)", plural(app.SessionsWithCaptureGaps, "session"), plural(len(app.CaptureGaps), "gap"))
		}
		notes = append(notes, statusNote{sc.info(), text})
	}
	if n := app.importedWithIssues; n > 0 {
		text := fmt.Sprintf("%d imported sessions have a capture gap or failed scan", n)
		if n == 1 {
			text = "1 imported session has a capture gap or failed scan"
		}
		notes = append(notes, statusNote{sc.info(), text})
	}
	return notes
}

// appCounts words how many sessions were found, archived, verified, imported,
// and are uploading. Top-level sessions and subagents are counted separately.
func appCounts(app appStatus) string {
	sessions := app.Sessions - app.SubagentSessions
	parts := []string{"no sessions yet"}
	if app.Sessions > 0 {
		parts[0] = plural(sessions, "session")
		if app.SubagentSessions > 0 {
			parts[0] += " (+" + plural(app.SubagentSessions, "subagent") + ")"
		}
	}
	if app.ReplaySessions > 0 {
		parts[0] += fmt.Sprintf(", %d of them replays", app.ReplaySessions)
	}
	if app.PublishedSessions > 0 {
		archived := fmt.Sprintf("%d archived", app.PublishedSessions)
		if app.VerifiedSessions > 0 {
			archived += fmt.Sprintf(" (%d verified)", app.VerifiedSessions)
		}
		parts = append(parts, archived)
	}
	if app.ImportedSessions > 0 {
		parts = append(parts, fmt.Sprintf("%d imported", app.ImportedSessions))
	}
	if app.UploadingSessions > 0 {
		parts = append(parts, fmt.Sprintf("%d uploading", app.UploadingSessions))
	}
	return strings.Join(parts, " · ")
}

// appProgress words how far an app's captured sessions have got, as status
// --verbose shows it under the app.
func appProgress(now time.Time, app appStatus) string {
	//lint:ignore LV1001 an app's state is an open-ended label (appStatus.State); statusCode maps it to the stable code
	switch app.State {
	case "published; source verified":
		return plural(app.PublishedSessions, "session") + " archived, verified " + relativeAge(now, app.VerifiedAt)
	case "published; read-back pending":
		if !app.VerifiedAt.IsZero() {
			return "uploaded, read-back pending (" + readBackProgress(app) + ")"
		}
		return "uploaded, read-back pending"
	case "captured locally":
		return "captured, not uploaded yet"
	case "hook observed; waiting for capture", "task found; waiting for capture":
		return "session seen, not captured yet"
	}
	return "waiting for first session"
}

// displayVersion is an installed version as the app line shows it: the
// version number alone, without the program name some apps print around
// it ("codex-cli 0.155.0", "2.1.283 (Claude Code)").
func displayVersion(version string) string {
	for field := range strings.FieldsSeq(version) {
		if field[0] >= '0' && field[0] <= '9' {
			return field
		}
	}
	return version
}

// uploadingNotes are the lines under an app for the sessions it is
// uploading: each one's project and start, and only what is unusual about
// it. After limit of them (none when 0), a line counts the rest.
func (sc statusScreen) uploadingNotes(app appStatus, limit int) []statusNote {
	shown := app.Uploading
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	width := 0
	for _, session := range shown {
		width = max(width, visibleWidth(sc.path(session.Project)))
	}
	notes := make([]statusNote, 0, len(shown)+1)
	for _, session := range shown {
		project := sc.path(session.Project)
		text := project + strings.Repeat(" ", width-visibleWidth(project)+3) + sc.started(session.StartedAt)
		mark := " "
		switch session.State {
		case uploadingFailing:
			mark = sc.style.warnMark()
			text += "   " + issueWhat(session.Issue)
		case uploadingFirst:
			text += "   waiting for its first upload"
		case uploadingRechecking:
			// Not a failure: the next pass retries it.
			text += "   " + deferredReasonWhat(session.Deferred)
		case uploadingPending:
			// The usual state: nothing to add.
		}
		notes = append(notes, statusNote{mark, text})
	}
	if rest := len(app.Uploading) - len(shown); rest > 0 {
		notes = append(notes, statusNote{" ", fmt.Sprintf("… and %d more (agent-archive status %s)", rest, app.Name)})
	}
	return notes
}

// issueWhat words a session's issue kind, as the last error does after a
// count.
func issueWhat(code string) string {
	label, ok := issueLabels[code]
	if !ok {
		label = issueLabels[issueCaptureFailed]
	}
	return label.what
}

// started is when a session started, in the clock's time zone: the time
// today, the day otherwise.
func (sc statusScreen) started(t time.Time) string {
	if t.IsZero() {
		return "start unknown"
	}
	local := t.In(sc.now.Location())
	year, month, day := local.Date()
	nowYear, nowMonth, nowDay := sc.now.Date()
	switch {
	case year == nowYear && month == nowMonth && day == nowDay:
		return "started " + local.Format("15:04")
	case year == nowYear:
		return "started " + local.Format("Jan 2")
	}
	return "started " + local.Format("Jan 2 2006")
}

// hookApproval explains, for an app that runs new hooks only once the user
// approves them (hookNextStep), how to, until its first session shows they
// run. %s is the command.
var hookApproval = map[string]string{
	"codex": "Run %s in Codex and approve the archive hooks; agent-archive can't see whether you have.",
}

// hooksLabel words an app's hook state.
func hooksLabel(state string) string {
	//lint:ignore LV1001 appStatus.Hooks is a plain string, set as literals in readInstalledApps
	switch state {
	case "missing or incomplete":
		return "hooks missing"
	case "unknown":
		return "hooks not checked"
	case "installed":
		return "hooks installed"
	}
	return "hooks " + state
}

// pairProgress words how far one project's capture has got.
func pairProgress(pair projectCaptureStatus) string {
	switch {
	case pair.ReadBackVerified:
		return "verified"
	case pair.Published:
		return "uploaded, read-back pending"
	case pair.CapturedLocally:
		return "captured, not uploaded yet"
	case pair.HookObserved:
		return "session seen, not captured yet"
	}
	return "waiting for first session"
}

// readBackFailure words a publication that could not be read back, and
// when the collector tries again.
func (sc statusScreen) readBackFailure(failure verificationEvidence) string {
	text := "Read-back failed"
	if failure.Outcome == verificationOutcomeMismatch {
		text = "Read-back doesn't match what this machine uploaded"
	}
	if failure.LastError != "" {
		text += ": " + failure.LastError
	}
	retry := "retrying on the next pass"
	if failure.NextRetryAt.After(sc.now) {
		retry = "retrying in " + strings.TrimSuffix(relativeAge(failure.NextRetryAt, sc.now), " ago")
		retry = strings.Replace(retry, "in just now", "in under a minute", 1)
	}
	return fmt.Sprintf("%s (%s, %s so far)", text, retry, plural(failure.Attempts, "attempt"))
}

// storageRows are the Storage section: the destination with its last
// upload, the background collector, the bucket's privacy, and the last
// pass's errors the headline does not already state. status --verbose adds
// every error, when access was checked, and the pending count.
func (sc statusScreen) storageRows(view statusView) []statusRow {
	var rows []statusRow
	privacy := statusRow{}
	if view.Storage != "" {
		destination := sc.destinationRow(view)
		private := view.PrivacyEvidence.State == "verified_private"
		if destination.mark == sc.style.okMark() && !sc.verbose && (private || !view.Collector.LastPublishedAt.IsZero()) {
			// When it was checked matters less than when it last worked.
			destination.detail = "reachable"
		}
		if failed, ok := lastPassStorageDetail(view); ok && destination.mark == sc.style.okMark() {
			// The headline says the last pass failed on storage; an older
			// successful check must not say it is fine.
			destination.mark, destination.detail = sc.style.warnMark(), failed
		}
		if private && !sc.verbose {
			if view.PrivacyEvidence.Reason == "r2_public_domains_disabled" {
				destination.detail += " · R2 public access off at setup"
			} else {
				destination.detail += " · bucket private"
			}
		} else {
			privacy = sc.privacyRow(view.PrivacyEvidence)
		}
		if !view.Collector.LastPublishedAt.IsZero() {
			destination.detail += " · uploaded " + relativeAge(sc.now, view.Collector.LastPublishedAt)
		}
		rows = append(rows, destination)
	}
	rows = append(rows, sc.backgroundRow(view))
	if privacy.mark != "" {
		rows = append(rows, privacy)
	}
	// Problems with nothing to do (see issueHeadline) are information, not
	// failures.
	mark, quiet := sc.style.failMark(), false
	if problem, _, ok := issueHeadline(view.Collector); ok && problem == "" {
		mark, quiet = sc.info(), true
	}
	_, stated := sc.headline(view)
	for i, text := range lastErrorRows(view.Collector) {
		if stated[i] && !sc.verbose {
			continue
		}
		if quiet {
			text = "Last pass: " + strings.TrimPrefix(text, "Last error: ")
		}
		rows = append(rows, statusRow{mark: mark, cells: []string{text}})
	}
	if sc.verbose {
		uploads := fmt.Sprintf("Last upload: %s · %d pending", sc.ago(view.Collector.LastPublishedAt), view.Collector.PendingCount)
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{uploads}})
	}
	return rows
}

// headline is the problem status leads with, and which of lastErrorRows it
// states, so the Storage section need not repeat them: while the problem is
// the one chooseNextStep derived from the last pass's errors, a headline
// from the failed sessions' kinds states their summary (not the size-limit
// notice beside it), and the general storage headline states a lone error,
// whose cause it then names. Several errors under the general headline are
// each still shown.
func (sc statusScreen) headline(view statusView) (problem string, stated map[int]bool) {
	problem, stated = view.problem, map[int]bool{}
	if problem == "" || problem != view.lastErrorProblem {
		return problem, stated
	}
	rows := lastErrorRows(view.Collector)
	if view.lastErrorByIssue {
		// The headline names one kind. The summary says no more only when
		// that is the one kind with a headline; other kinds with nothing
		// to do add nothing the user must act on.
		if len(headlineIssueCodes(view.Collector.IssueCounts)) != 1 {
			return problem, stated
		}
		for i, recorded := range view.Collector.LastErrors {
			if !collector.IsSizeLimitProblem(recorded) {
				stated[i] = true
			}
		}
		return problem, stated
	}
	if len(rows) != 1 {
		return problem, stated
	}
	stated[0] = true
	if problem == generalSyncProblem {
		cause, raw := strings.CutPrefix(rows[0], "Last error: ")
		if !raw {
			// A plain cause (storageErrorCause) reads on after the colon.
			cause = strings.ToLower(cause[:1]) + cause[1:]
		}
		problem += ": " + cause
	}
	return problem, stated
}

// headlineIssueCodes are the issue codes of the last pass that have a
// headline of their own (issueLabel.headline), each counted at least once.
func headlineIssueCodes(counts map[string]int) []string {
	var codes []string
	for _, code := range issueCodes {
		if counts[code] > 0 && issueLabels[code].headline != "" {
			codes = append(codes, code)
		}
	}
	return codes
}

// lastPassStorageDetail words, for the destination row, what the last
// pass's storage failure was, while the headline is the one derived from
// it: the destination was not reachable as the collector's health last
// recorded, so the row must not say it is. ok is false otherwise, and for
// a failure that is not recognizably about storage (a retention clock hold,
// which proves storage answered; a local file; a read-back check): the
// general headline covers those too, but they say nothing against storage.
func lastPassStorageDetail(view statusView) (detail string, ok bool) {
	if view.problem == "" || view.problem != view.lastErrorProblem || view.lastErrorByIssue {
		return "", false
	}
	recorded := view.Collector.LastErrors
	if len(recorded) == 0 && view.Collector.LastError != "" {
		recorded = strings.Split(view.Collector.LastError, "; ")
	}
	for _, problem := range recorded {
		switch storageErrorCause(problem) {
		case "Storage refused access":
			return "refused access on the last pass", true
		case "Storage didn't accept the credentials":
			return "didn't accept the credentials on the last pass", true
		case "The bucket doesn't exist":
			return "bucket not found on the last pass", true
		case "The bucket is in a different region":
			return "bucket in a different region on the last pass", true
		}
	}
	for _, problem := range recorded {
		if strings.Contains(problem, backgroundCredentialProcessFailure) || credentials.RecoveryActionForMessage(problem) != "" {
			return "couldn't get credentials on the last pass", true
		}
	}
	if slices.ContainsFunc(recorded, storageProblem) {
		return "unreachable on the last pass", true
	}
	return "", false
}

// storageProblem reports whether a problem the collector recorded as text is
// about reaching storage: a call to S3 failed, storage could not be opened,
// the collector's own storage access check failed, or the network did.
func storageProblem(text string) bool {
	for _, match := range recordedOperationService.FindAllStringSubmatch(text, -1) {
		if match[1] == s3.ServiceID {
			return true
		}
	}
	if strings.Contains(text, "open storage: ") || strings.Contains(text, "background storage access failed") {
		return true
	}
	for _, network := range []string{"dial tcp", "no such host", "connection refused", "connection reset", "i/o timeout", "network is unreachable", "TLS handshake timeout"} {
		if strings.Contains(text, network) {
			return true
		}
	}
	return false
}

// generalSyncProblem is the headline for a failed pass whose errors say
// nothing more specific.
const generalSyncProblem = "The last sync failed"

// destinationRow is where sessions go, and whether storage was last found
// reachable with the configured credentials.
func (sc statusScreen) destinationRow(view statusView) statusRow {
	s := sc.style
	row := statusRow{cells: []string{storageURL(view.Storage)}}
	auth := view.Authentication
	checked := ""
	if !auth.CheckedAt.IsZero() {
		checked = ", checked " + relativeAge(sc.now, auth.CheckedAt)
	}
	// A storage check newer than the recorded health (setup's, after new
	// credentials) outranks it: the health is then out of date, not wrong now.
	health := auth.State
	if view.StorageAccessConfirmedAt.After(auth.CheckedAt) {
		health = ""
	}
	//lint:ignore LV1001 storageHealth.State is an untyped string set in collect.go and verification.go
	switch health {
	case "authentication_failed":
		row.mark, row.detail = s.failMark(), "sign-in failed"+checked
	case "credentials_expired":
		row.mark, row.detail = s.failMark(), "credentials expired"+checked
	case "credentials_unavailable":
		row.mark, row.detail = s.failMark(), "credentials unavailable"+checked
	case "storage_unavailable":
		row.mark, row.detail = s.failMark(), "unreachable"+checked
	case "stale_configuration":
		row.mark, row.detail = s.warnMark(), "storage settings changed since the last check"
	case "stale":
		row.mark, row.detail = s.warnMark(), "last reachable "+relativeAge(sc.now, auth.CheckedAt)+"; the collector checks every few minutes"
	}
	if row.mark != "" {
		return row
	}
	switch view.StorageAccessConfirmedBy {
	case storageAccessConfirmedByCollector:
		row.mark, row.detail = s.okMark(), "reachable, checked "+relativeAge(sc.now, view.StorageAccessConfirmedAt)
	case storageAccessConfirmedBySetup:
		row.mark, row.detail = s.okMark(), "reachable, checked by setup "+relativeAge(sc.now, view.StorageAccessConfirmedAt)
	default:
		row.mark, row.detail = sc.info(), "not checked yet"
	}
	return row
}

// storageURL shows a "provider / bucket / prefix" label as
// provider://bucket/prefix.
func storageURL(label string) string {
	parts := strings.SplitN(label, " / ", 3)
	if len(parts) < 2 {
		return label
	}
	return parts[0] + "://" + strings.Join(parts[1:], "/")
}

// privacyRow is what the last inspection found about the bucket's public
// access, with the provider's guidance when it is not known to be private.
func (sc statusScreen) privacyRow(report storage.PrivacyReport) statusRow {
	s := sc.style
	checked := ""
	if report.CheckedAt != nil {
		checked = ", checked " + relativeAge(sc.now, *report.CheckedAt)
	}
	review := statusNote{text: "Check public access: " + s.cmd(report.GuidanceURL)}
	//lint:ignore LV1001 storage.PrivacyReport.State is an untyped string owned by package storage
	switch report.State {
	case "verified_private":
		if report.Reason == "r2_public_domains_disabled" {
			return statusRow{mark: s.okMark(), cells: []string{"R2 public access off at setup"}, detail: "r2.dev off; no enabled custom domains" + checked}
		}
		return statusRow{mark: s.okMark(), cells: []string{"Bucket is private"}, detail: "public access blocked" + checked}
	case "public_or_risky":
		detail := "public access is allowed"
		//lint:ignore LV1001 storage.PrivacyReport.Reason is an untyped string owned by package storage
		switch report.Reason {
		case "public_bucket_policy":
			detail = "its bucket policy is public"
		case "public_bucket_acl":
			detail = "its access list grants public access"
		case "r2_public_access_enabled":
			detail = "R2 public access was enabled at setup"
		}
		return statusRow{mark: s.failMark(), cells: []string{"Bucket may be public"}, detail: detail + checked, notes: []statusNote{review}}
	}
	detail := ""
	//lint:ignore LV1001 storage.PrivacyReport.Reason is an untyped string owned by package storage
	switch report.Reason {
	case "inspection_unavailable":
		detail = "this storage can't be inspected"
	case "r2_management_credentials_not_configured":
		detail = "R2 object credentials can't inspect public access"
	case "r2_public_access_not_fully_checked":
		detail = "not every R2 public-access setting could be checked" + checked
	case "public_access_controls_not_fully_verified":
		detail = "some public access settings couldn't be read" + checked
	case "inspection_stale":
		detail = "the last check is over a day old" + checked
		if report.CheckedAt != nil && report.CheckedAt.After(sc.now) {
			detail = "the last check is dated in the future; the clock may have changed"
		}
	case "storage_configuration_changed":
		detail = "storage settings changed since the last check"
	}
	if !sc.verbose {
		return statusRow{mark: s.warnMark(), cells: []string{"Bucket privacy not verified"}, detail: sc.shortPrivacyReason(report)}
	}
	return statusRow{mark: s.warnMark(), cells: []string{"Bucket privacy not verified"}, detail: detail, notes: []statusNote{review}}
}

// shortPrivacyReason is why bucket privacy isn't verified, in a few words,
// for the short status. Only when this machine can't inspect the bucket at all
// does the provider's guidance (checking public access by hand) fix it, so
// only then is its link added; --verbose always has it.
func (sc statusScreen) shortPrivacyReason(report storage.PrivacyReport) string {
	see := ""
	if report.GuidanceURL != "" {
		see = "; see " + report.GuidanceURL
	}
	//lint:ignore LV1001 storage.PrivacyReport.Reason is an untyped string owned by package storage
	switch report.Reason {
	case "inspection_unavailable":
		return "this storage can't be inspected" + see
	case "r2_management_credentials_not_configured":
		return "R2 object credentials can't inspect it" + see
	case "r2_public_access_not_fully_checked":
		return "some R2 public-access settings couldn't be read"
	case "public_access_controls_not_fully_verified":
		return "some settings couldn't be read"
	case "inspection_stale":
		if report.CheckedAt != nil && report.CheckedAt.After(sc.now) {
			return "last check dated in the future"
		}
		return "not checked in over a day"
	case "storage_configuration_changed":
		return "not checked since storage changed"
	}
	return "not checked yet"
}

// backgroundRow is the background collector: whether launchd runs it, and
// when it last scanned.
func (sc statusScreen) backgroundRow(view statusView) statusRow {
	s := sc.style
	scan := "last scan " + sc.ago(view.Collector.LastScanAt)
	if view.Collector.LastScanAt.IsZero() {
		scan = "no scan yet"
	}
	switch {
	case view.Background == backgroundBroken:
		return statusRow{mark: s.failMark(), cells: []string{"Background collector is broken"}, detail: "the program it runs is gone or can't be run"}
	case view.Background == string(scheduler.AnotherInstallation):
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector belongs to another installation"}}
	case view.Background == "unknown":
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector state unknown"}, detail: view.backgroundTool + " couldn't say"}
	case !jobActive(scheduler.JobState(view.Background)):
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector isn't running"}}
	case view.Paused:
		return statusRow{mark: sc.info(), cells: []string{"Background collector paused"}, detail: scan}
	case !view.Collector.LastScanAt.IsZero() && sc.now.Sub(view.Collector.LastScanAt) > scanStaleAfter:
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector on"}, detail: scan}
	}
	return statusRow{mark: s.okMark(), cells: []string{"Background collector on"}, detail: scan}
}

// noteRows are what the collector could not read or refresh, and status's
// warnings.
func (sc statusScreen) noteRows(view statusView) []statusRow {
	var rows []statusRow
	if n := len(view.Collector.QuarantinedFiles); n > 0 {
		text := "1 local state file couldn't be read and was moved aside; its session keeps its other evidence."
		if n > 1 {
			text = fmt.Sprintf("%d local state files couldn't be read and were moved aside; their sessions keep their other evidence.", n)
		}
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{text + " See status --json for the files, then delete them."}})
	}
	if n := view.Collector.UnrefreshableSummaries; n > 0 {
		text := "1 session summary can't be refreshed by this version and stays as published until the session changes."
		if n > 1 {
			text = fmt.Sprintf("%d session summaries can't be refreshed by this version and stay as published until their sessions change.", n)
		}
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{text}})
	}
	for _, warning := range view.Warnings {
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{sc.tilde(warning)}})
	}
	for _, warning := range view.BackgroundWarnings {
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{sc.tilde(warning)}})
	}
	return rows
}

// ago is how long ago t was, or "never".
func (sc statusScreen) ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return relativeAge(sc.now, t)
}

// path shows path with the home folder as ~.
func (sc statusScreen) path(path string) string {
	if sc.home == "" {
		return path
	}
	return displayPath(path, sc.home)
}

// tilde shows the paths under the home folder in text as ~ paths.
func (sc statusScreen) tilde(text string) string {
	if sc.home == "" {
		return text
	}
	home := filepath.Clean(sc.home) + string(filepath.Separator)
	if home == "//" {
		return text // every path is under /; ~ would only obscure them
	}
	var b strings.Builder
	for {
		i := strings.Index(text, home)
		if i < 0 {
			break
		}
		// Only a path that starts at the home folder: not one that merely
		// contains it, like /Volumes/Backup/Users/jo/.
		if i > 0 && pathByte(text[i-1]) {
			b.WriteString(text[:i+1])
			text = text[i+1:]
			continue
		}
		b.WriteString(text[:i] + "~" + string(filepath.Separator))
		text = text[i+len(home):]
	}
	return b.String() + text
}

// pathByte reports whether c can be part of a path, so that a home folder
// right after it is not where the path starts.
func pathByte(c byte) bool {
	return c == '/' || c == '.' || c == '_' || c == '-' || c == '~' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// prose readies a next step for the screen: ~ paths, the warnings it refers
// to are below it rather than above, and its first command is highlighted,
// the one to run.
func (sc statusScreen) prose(text string) string {
	text = strings.ReplaceAll(sc.tilde(text), "the warning above", "the warning below")
	if loc := proseCommand.FindStringIndex(text); loc != nil {
		text = text[:loc[0]] + sc.style.cmd(text[loc[0]:loc[1]]) + text[loc[1]:]
	}
	return text
}

// proseLines readies a next step for the screen as prose does, one line for
// each sentence that names a command, so that every command is highlighted
// and no line highlights two. A sentence that names two commands is split
// after the comma or semicolon before the second. A sentence without a
// command stays on the line before it.
func (sc statusScreen) proseLines(text string) []string {
	var pieces []string
	for _, sentence := range sentences(text) {
		pieces = append(pieces, clauses(sentence)...)
	}
	var lines []string
	line := ""
	for _, sentence := range pieces {
		if line != "" && proseCommand.MatchString(line) && proseCommand.MatchString(sentence) {
			lines = append(lines, sc.prose(line))
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += sentence
	}
	return append(lines, sc.prose(line))
}

// sentences splits text after each full stop that a space and a capital
// letter follow.
func sentences(text string) []string {
	var out []string
	for {
		i := sentenceEnd.FindStringIndex(text)
		if i == nil {
			return append(out, text)
		}
		out = append(out, text[:i[0]+1])
		text = text[i[1]-1:]
	}
}

var sentenceEnd = regexp.MustCompile(`\. [A-Z]`)

// clauses splits a sentence that names more than one command after the last
// ", " or "; " before each command but the first, so each part names one.
// A command with no such break before it stays with the one before it.
func clauses(sentence string) []string {
	commands := proseCommand.FindAllStringIndex(sentence, -1)
	var out []string
	start := 0
	for i := 1; i < len(commands); i++ {
		between := sentence[commands[i-1][1]:commands[i][0]]
		cut := max(strings.LastIndex(between, ", "), strings.LastIndex(between, "; "))
		if cut < 0 {
			continue
		}
		cut += commands[i-1][1] + 1
		out = append(out, sentence[start:cut])
		start = cut + 1
	}
	return append(out, sentence[start:])
}

// proseCommand matches an agent-archive command, with its flags, in a
// sentence.
var proseCommand = regexp.MustCompile(`agent-archive (?:` + strings.Join(slices.Sorted(maps.Keys(commandHelp)), "|") + `)\b(?: --[a-z][a-z-]*)*`)

// lastErrorRows are the Storage section's lines for the collector's last
// errors, one per problem the pass recorded. A storage refusal is shown by
// its plain cause (see storage.Diagnose); the raw error text is in status
// --verbose. Anything else, like the collector's own counts of failed
// sessions, is shown as recorded.
//
// A status file written before Status.LastErrors existed has only the joined
// LastError. It is split where the collector joined problems, on "; ", and
// shown on one line as before (see legacyLastErrorText).
func lastErrorRows(status state.Status) []string {
	if len(status.LastErrors) == 0 {
		if status.LastError == "" {
			return nil
		}
		return []string{legacyLastErrorText(status.LastError)}
	}
	rows := make([]string, 0, len(status.LastErrors))
	for _, problem := range status.LastErrors {
		if cause := storageErrorCause(problem); cause != "" {
			rows = append(rows, cause)
			continue
		}
		rows = append(rows, "Last error: "+problem)
	}
	return rows
}

// legacyLastErrorText is the one line an older status file's joined
// lastError is shown as: a lone storage refusal's plain cause, or "Last
// error: " and its problems, each storage refusal among them by its cause.
func legacyLastErrorText(lastError string) string {
	parts := strings.Split(lastError, "; ")
	plain := false
	for i, part := range parts {
		if cause := storageErrorCause(part); cause != "" {
			parts[i], plain = cause, true
		}
	}
	if plain && len(parts) == 1 {
		return parts[0]
	}
	return "Last error: " + strings.Join(parts, "; ")
}

// storageErrorCause names, in a few words, the storage failure an error
// recorded as text reports, or returns "" when it reports none that
// storage.Diagnose recognizes. The status file keeps only the error's text,
// so the provider's error code is read back from where the SDK writes it
// ("api error AccessDenied: Access Denied") and diagnosed as a code; a
// message that merely mentions a code elsewhere is not taken for it.
//
// Like storage.Diagnose, a failure while fetching credentials (an STS, SSO
// or other sign-in service call inside the storage call) is a credential
// problem whatever its code, not a refusal by the bucket. Such text is left
// as recorded, since the code alone would misname it.
func storageErrorCause(text string) string {
	for _, match := range recordedOperationService.FindAllStringSubmatch(text, -1) {
		if match[1] != s3.ServiceID {
			return ""
		}
	}
	for _, match := range recordedErrorCode.FindAllStringSubmatch(text, -1) {
		switch storage.Diagnose(&smithy.GenericAPIError{Code: match[1]}).Cause {
		case storage.CauseAccessDenied:
			return "Storage refused access"
		case storage.CauseNoCredentials:
			return "Storage didn't accept the credentials"
		case storage.CauseNoSuchBucket:
			return "The bucket doesn't exist"
		case storage.CauseWrongRegion:
			return "The bucket is in a different region"
		case storage.CauseNetwork, storage.CauseOther:
			// Not recognized from a code alone.
		}
	}
	return ""
}

// recordedErrorCode matches a provider error code where an error's text puts
// it: after "api error " or a ": ", and before ": ".
var recordedErrorCode = regexp.MustCompile(`(?:^|api error |: )([A-Z][A-Za-z]+): `)

// recordedOperationService matches the service of each SDK call an error's
// text names ("operation error STS: AssumeRole"), the way storage.Diagnose
// reads the chain of operation errors.
var recordedOperationService = regexp.MustCompile(`operation error ([^:]+): `)

// skillLabel is the name status gives the skill whose file is at path
// (".../skills/handoff/SKILL.md" is "/handoff").
func skillLabel(path string) string { return agentskills.Label(filepath.Base(filepath.Dir(path))) }

// printStatusDetails writes the Details section of status --verbose: every
// line the text status printed before it was redesigned, with its codes,
// exact times, full paths and raw errors.
func printStatusDetails(out io.Writer, view statusView) {
	terminal.Printf(out, "  State:         %s (%s)\n", view.State, view.Code)
	if view.Storage != "" {
		terminal.Printf(out, "  Storage:       %s\n  Access:        %s\n", view.Storage, storageAccessLine(view))
		printBucketPrivacy(out, view.PrivacyEvidence)
	}
	checked := "not checked yet"
	if !view.Authentication.CheckedAt.IsZero() {
		checked = "checked " + formatTimeOrNever(view.Authentication.CheckedAt)
	}
	if view.Authentication.Context != "" {
		checked += "; " + view.Authentication.Context
	}
	terminal.Printf(out, "  Authentication: %s (%s)\n", view.Authentication.State, checked)
	terminal.Printf(out, "  Background:    %s\n", view.Background)
	if view.Paused {
		terminal.Println(out, "  Collection:    paused")
	}
	terminal.Printf(out, "  Projects:      %d included\n  Pending:       %d session(s)\n  Last scan:     %s\n  Last publish:  %s\n", len(view.Projects), view.Collector.PendingCount, formatTimeOrNever(view.Collector.LastScanAt), formatTimeOrNever(view.Collector.LastPublishedAt))
	terminal.Printf(out, "  Skill evidence: %s\n", view.SkillEvidence)
	if view.Retention != nil {
		terminal.Printf(out, "  Retention:     %s\n", config.RetentionLabel(view.Retention.Days))
	}
	if view.ImportedSessions > 0 {
		imported := fmt.Sprintf("%d session(s), %d waiting to upload", view.ImportedSessions, view.ImportedPending)
		if view.ImportedWithIssues > 0 {
			imported += fmt.Sprintf(", %d with a capture gap or failed scan", view.ImportedWithIssues)
		}
		if view.LastImport != "" {
			imported += "; last import " + view.LastImport
		}
		terminal.Printf(out, "  Imported:      %s\n", imported)
	}
	for _, app := range view.Apps {
		printAppDetails(out, app)
	}
	if view.AgentSkillsDisabled {
		terminal.Println(out, "  Agent skills:  turned off (agent-archive setup --skills turns them on)")
	}
	for _, path := range view.AgentSkills {
		line := displayPath(path, view.userHome)
		if slices.Contains(view.AgentSkillsOutOfDate, path) {
			line += " (out of date; run agent-archive setup --refresh)"
		}
		terminal.Printf(out, "  %-14s %s\n", skillLabel(path)+":", line)
	}
	for _, diagnostic := range view.CaptureDiagnostics {
		terminal.Printf(out, "  Capture skipped in %s (%s): %s at %s.\n", diagnostic.ProjectRoot, appName(diagnostic.Harness), capture.DiagnosticMessage(diagnostic.Code), formatTimeOrNever(diagnostic.ObservedAt))
	}
	// Each problem as the collector recorded it, one per line; a status
	// file from before LastErrors has only the joined text, shown as is.
	lastErrors := view.Collector.LastErrors
	if len(lastErrors) == 0 && view.Collector.LastError != "" {
		lastErrors = []string{view.Collector.LastError}
	}
	for _, problem := range lastErrors {
		terminal.Printf(out, "  Last error:    %s\n", problem)
	}
	if n := len(view.Collector.QuarantinedFiles); n > 0 {
		terminal.Printf(out, "  Quarantined:   %d local state file(s) could not be read and were moved aside; their sessions keep their other evidence. See status --json for the files, then delete them.\n", n)
	}
	if n := view.Collector.UnrefreshableSummaries; n > 0 {
		terminal.Printf(out, "  Summaries:     %d session summary(ies) cannot be refreshed by this version and stay as published until the session changes.\n", n)
	}
	for i, line := range subagentDetailLines(view.Collector) {
		label := "  Subagents:     "
		if i > 0 {
			label = "                 "
		}
		terminal.Printf(out, "%s%s\n", label, line)
	}
	for _, warning := range view.Warnings {
		terminal.Printf(out, "  Warning:       %s\n", warning)
	}
}

// subagentDetailLines are the Details lines about subagents that are not a
// problem: those waiting for their transcripts, those resumed and still
// running, and those dropped in the last week because Claude Code never
// wrote them, counted by type.
func subagentDetailLines(collector state.Status) []string {
	var lines []string
	if n := collector.PendingNativeLinks; n > 0 {
		lines = append(lines, fmt.Sprintf("%d native child parent links unresolved; children capture independently", n))
	}
	if n := collector.WaitingSubagents; n > 0 {
		lines = append(lines, fmt.Sprintf("%d waiting for their transcripts", n))
	}
	if n := collector.RunningSubagents; n > 0 {
		lines = append(lines, fmt.Sprintf("%d still running (archived up to their last stop)", n))
	}
	expired := collector.ExpiredSubagents
	if len(expired) == 0 {
		return lines
	}
	lines = append(lines, fmt.Sprintf("%d not archived in the last 7 days (Claude Code never wrote their transcripts; nothing to do)", len(expired)))
	counts := map[string]int{}
	for _, entry := range expired {
		counts[entry.AgentType]++
	}
	types := slices.Collect(maps.Keys(counts))
	// Most common first; on a tie the unknown type ("") comes before named
	// ones, then names alphabetically.
	slices.SortFunc(types, func(a, b string) int {
		if c := counts[b] - counts[a]; c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	groups := make([]string, 0, len(types))
	for _, agentType := range types {
		name := agentType
		if name == "" {
			name = "unknown type"
		}
		groups = append(groups, fmt.Sprintf("%d %s", counts[agentType], name))
	}
	return append(lines, strings.Join(groups, ", "))
}

// printAppDetails writes one app's lines in the Details section.
func printAppDetails(out io.Writer, app appStatus) {

	gaps := ""
	if len(app.CaptureGaps) > 0 {
		gaps = fmt.Sprintf("; %d with a capture gap", app.SessionsWithCaptureGaps)
	}
	terminal.Printf(out, "  %s: %s (%s; %d session(s)%s); hooks %s\n", appName(app.Name), app.State, app.Code, app.Sessions, gaps, app.Hooks)
	if app.Discovery != nil {
		terminal.Printf(out, "    Automatic %s.\n", discoveryLabel(app.Discovery))
		terminal.Printf(out, "    Last attempt: %s; registered %d in that scan.\n", formatTimeOrNever(app.Discovery.LastAttempt), app.Discovery.Registered)
		if skipped, reasons := discoverySkipped(app.Discovery); skipped > 0 {
			terminal.Printf(out, "    Unsupported/incomplete/malformed observations: %d (%s); update agent-archive for source format support.\n", skipped, reasons)
		}
	}

	if app.Hooks == "installed" {
		observation := "execution not yet observed"
		if app.HookObserved {
			observation = "execution observed"
		}
		terminal.Println(out, "    Hooks installed; "+observation+".")
	}
	if discoveryEnabled(app) {
		terminal.Println(out, "    Hook approval is optional while automatic discovery is enabled.")
	}
	if app.Trust == "unknown" {
		terminal.Println(out, "    Hook trust: unknown here; it is granted inside the app and is not observable from this machine's files.")
	}
	terminal.Printf(out, "    Installed version: %s; support %s%s.\n", installedVersionLabel(app), app.VersionSupport, versionSupportNote(app))
	if app.Capabilities.FreshStart.State == capabilityUnavailable {
		terminal.Printf(out, "    Fresh-start capture: unavailable. %s\n", app.Capabilities.FreshStart.NextAction)
	}
	for _, pair := range app.Projects {
		terminal.Printf(out, "    Project %s: %s.\n", pair.ProjectRoot, pair.VerificationState)
	}
	switch {
	case app.ReadBackVerified && !app.VerifiedAt.IsZero():
		terminal.Printf(out, "    Read-back verified: %s; evidence is for that publication.\n", formatTimeOrNever(app.VerifiedAt))
	case !app.VerifiedAt.IsZero():
		// Some evidence exists but not every project (or session) is
		// covered, so do not call the app verified.
		terminal.Printf(out, "    Last read-back: %s (%s).\n", formatTimeOrNever(app.VerifiedAt), readBackProgress(app))
	}
	if app.VerificationDetail != "" {
		terminal.Printf(out, "    Read-back: %s\n", app.VerificationDetail)
	}
	if len(app.CaptureGaps) > 0 {
		terminal.Printf(out, "    Capture gaps: %d recorded across %d session(s); see status --json for details.\n", len(app.CaptureGaps), app.SessionsWithCaptureGaps)
	}
}

// storageAccessLine is the Details section's Access line: when access to
// the destination was last confirmed, and by what.
func storageAccessLine(view statusView) string {
	switch view.StorageAccessConfirmedBy {
	case storageAccessConfirmedBySetup:
		return "confirmed " + formatTimeOrNever(view.StorageAccessConfirmedAt) + " by setup's storage check (write, read, list, delete)"
	case storageAccessConfirmedByCollector:
		return "confirmed " + formatTimeOrNever(view.StorageAccessConfirmedAt) + " by the collector's last successful storage access"
	}
	return "not confirmed yet"
}

// printBucketPrivacy writes the Details section's bucket privacy lines: the
// last inspection's result, when it ran, its reason code, and the
// provider's guidance.
func printBucketPrivacy(out io.Writer, report storage.PrivacyReport) {
	//lint:ignore LV1001 storage.PrivacyReport.State is an untyped string owned by package storage
	switch report.State {
	case "verified_private":
		if report.Reason == "r2_public_domains_disabled" {
			terminal.Println(out, "  Bucket privacy: r2.dev off and no enabled custom domains at setup's last check.")
		} else {
			terminal.Println(out, "  Bucket privacy: native public access blocked at the last check.")
		}
	case "public_or_risky":
		terminal.Println(out, "  Bucket privacy: public configuration detected; review access before archiving.")
	default:
		terminal.Println(out, "  Bucket privacy not verified.")
	}
	checked := "never"
	if report.CheckedAt != nil {
		checked = formatTimeOrNever(*report.CheckedAt)
	}
	terminal.Printf(out, "    Checked: %s; %s.\n    Review: %s\n", checked, report.Reason, report.GuidanceURL)
}

// versionSupportNote explains an unverified installed version without
// changing the support state or reason code.
func versionSupportNote(app appStatus) string {
	//lint:ignore LV1001 the reason codes are untyped constants in capabilities.go, which computes this field
	switch app.VersionSupportReason {
	case supportReasonNoVerifiedCapture:
		return " (no session from this version has been published and read back yet)"
	case supportReasonNoMatchingVersion:
		return " (verified sessions came from a different version)"
	case supportReasonVersionSourceMismatch:
		return " (installed version and captured versions use different numbering; cannot be compared)"
	}
	return ""
}

// installedVersionLabel is an app's installed version, or why it is not
// known.
func installedVersionLabel(app appStatus) string {
	if app.InstalledVersion != "" {
		return app.InstalledVersion
	}
	if app.VersionState != "" {
		return app.VersionState
	}
	return "unknown"
}

func discoveryEnabled(app appStatus) bool {
	return app.Discovery != nil && app.Discovery.Enabled
}

// Empty origin is the documented legacy hook-registration encoding. Other
// origins need durable observation rather than capture/publication inference.
func registrationHookObserved(reg archive.SessionRegistration) bool {
	return !reg.HookObservedAt.IsZero() || reg.Origin == archive.SessionOriginHook || reg.Origin == ""
}

// optionalCodexHooks identifies a genuinely absent optional installation;
// broken owned hooks and foreign installations still require attention.
func optionalCodexHooks(app appStatus) bool {
	return discoveryEnabled(app) && app.Name == "codex" && app.Hooks != "installed" && app.Hooks != hooksBroken && app.Hooks != "unknown" && !app.hooksNeedRepair && len(app.OtherInstallations) == 0
}

func discoveryLabel(d *discovery.Health) string {
	switch {
	case !d.Enabled:
		return "discovery off"
	case len(d.Errors) > 0 || d.Outcomes["source_unavailable"] > 0:
		return "discovery unknown or source unavailable"
	case d.LastAttempt.IsZero():
		return "discovery waiting for first scan"
	case d.Pending:
		return "discovery coverage pending"
	case !d.Supported && d.Outcomes["incomplete_metadata"] > 0:
		return "discovery: waiting for complete task metadata"
	case !d.Supported:
		return "discovery: no supported task observed in last scan"
	default:
		return "discovery: supported observations"
	}
}

func discoverySkipped(d *discovery.Health) (int, string) {
	var reasons []string
	total := 0
	for _, reason := range []string{"unsupported_producer", "unsupported_execution", "unsupported_format", "unsupported_history", "incomplete_metadata", "invalid_metadata", "oversized_metadata", "invalid_identity", "invalid_source", "invalid_candidate", "invalid_relationship", "child_history_pending", "fork_history_pending", "related_history_pending", "inherited_history"} {
		if n := d.Outcomes[reason]; n > 0 {
			total += n
			reasons = append(reasons, fmt.Sprintf("%s: %d", strings.ReplaceAll(reason, "_", " "), n))
		}
	}
	return total, strings.Join(reasons, ", ")
}

func discoveryProgress(now time.Time, app appStatus) string {
	d := app.Discovery
	parts := []string{}
	if !d.LastAttempt.IsZero() {
		parts = append(parts, "Last discovery attempt "+relativeAge(now, d.LastAttempt))
	}
	if d.Pending {
		parts = append(parts, "discovery/reconciliation pending; sync advances bounded work, additional scans may be needed")
	}
	if d.Outcomes["admission_retry"] > 0 {
		parts = append(parts, "admission retry pending")
	}
	parts = append(parts, fmt.Sprintf("%d registered → %d queued → %d published → %d read-back verified", app.Sessions, app.UploadingSessions, app.PublishedSessions, app.VerifiedSessions))
	return strings.Join(parts, "; ")
}

func (sc statusScreen) addCodexDiscoveryRow(row *statusRow, app appStatus) {
	if app.Name != "codex" || app.Discovery == nil {
		return
	}
	s := sc.style
	row.cells[1] = discoveryLabel(app.Discovery)
	if !sc.verbose && discoveryEnabled(app) {
		switch {
		case len(app.Discovery.Errors) > 0 || app.Discovery.Outcomes["source_unavailable"] > 0:
			row.cells[1] = "capture status unknown"
		case app.Discovery.LastAttempt.IsZero():
			row.cells[1] = "waiting for first scan"
		case app.Discovery.Pending:
			row.cells[1] = "capture check pending"
		case app.Sessions == 0 && !app.Discovery.Supported && app.Discovery.Outcomes["incomplete_metadata"] > 0:
			row.cells[1] = "waiting for task metadata"
		case app.Sessions == 0 && !app.Discovery.Supported:
			row.cells[1] = "waiting for supported task"
		default:
			row.cells[1] = "automatic capture"
		}
	}
	if app.CodexCaptureScope == string(config.CodexAllProjects) {
		row.cells[1] += "; all current and future Codex projects"
	} else {
		row.cells[1] += "; included projects"
	}
	if sc.verbose && discoveryEnabled(app) {
		row.notes = append(row.notes, statusNote{sc.info(), discoveryProgress(sc.now, app)})
		if formats := discoveryFormats(app.Discovery); formats != "" {
			row.notes = append(row.notes, statusNote{sc.info(), formats})
		}
	}

	if discoveryEnabled(app) {
		pending := app.Discovery.Outcomes[string(sourcefacts.RecoveryAmbiguous)] + app.Discovery.Outcomes[string(sourcefacts.RecoverySubtreeUnavailable)]
		if pending > 0 {
			row.mark = s.warnMark()
			row.notes = append(row.notes, statusNote{s.warnMark(), fmt.Sprintf("Project attribution pending for %d observations: multiple checkouts or unproved subtree scope. Review historical imports with backfill --dry-run --map-project OLD_CWD=CONFIGURED_ROOT; start future chats in a configured checkout.", pending)})
		}
		if app.Discovery.Outcomes[string(sourcefacts.RecoveryInventoryUnavailable)] > 0 || app.Discovery.Outcomes[string(sourcefacts.RecoveryBudgetExhausted)] > 0 {
			row.notes = append(row.notes, statusNote{sc.info(), "Project repository inventory pending. Restore configured checkout access if needed and ensure local Git supports config path queries, then run sync to advance bounded recovery."})
		}
	}
	if discoveryEnabled(app) && (len(app.Discovery.Errors) > 0 || app.Discovery.Outcomes["source_unavailable"] > 0) {
		row.mark = s.warnMark()
		row.notes = append(row.notes, statusNote{s.warnMark(), "Discovery health is unknown or a source is unavailable. Restore source access if needed, then run agent-archive sync for a current bounded scan."})
	}
	if skipped, reasons := discoverySkipped(app.Discovery); discoveryEnabled(app) && skipped > 0 {
		if skipped == app.Discovery.Outcomes["incomplete_metadata"] {
			row.notes = append(row.notes, statusNote{sc.info(), fmt.Sprintf("Last scan saw %d records waiting for complete task metadata. Start a supported new task, then run sync; capture is not yet established.", skipped)})
		} else {
			row.mark = s.warnMark()
			text := fmt.Sprintf("Last scan skipped %d observations; run agent-archive status --verbose for details.", skipped)
			if sc.verbose {
				text = fmt.Sprintf("Last scan skipped %d unsupported, incomplete or malformed observations (%s). Update agent-archive for format support; use hooks or deliberate backfill only where that source format is supported.", skipped, reasons)
			}
			row.notes = append(row.notes, statusNote{s.warnMark(), text})
		}
	}
}

func discoveryFormats(d *discovery.Health) string {
	var formats []string
	for _, f := range d.Formats {
		formats = append(formats, fmt.Sprintf("%s (%s, %s, %s)", f.Version, f.Source, f.Profile, strings.ReplaceAll(string(f.Evidence), "_", " ")))
	}
	if len(formats) == 0 {
		return ""
	}
	return "Recorded session formats: " + strings.Join(formats, "; ") + ". Compatibility permits discovery; capture still requires consent and publication/read-back."
}

func independentImportedSession(reg archive.SessionRegistration) bool {
	return !reg.IsChild() || reg.NativeChild
}
