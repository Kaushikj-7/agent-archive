package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const defaultPrefix = "agent-archive/"

const defaultRetentionDays = config.DefaultRetentionDays

type setupDraft struct {
	RetentionEdited   bool          `json:"retention_edited,omitempty"`
	NewInstallation   bool          `json:"new_installation,omitempty"`
	DiscoveryReviewed bool          `json:"discovery_reviewed,omitempty"`
	GuidedSlotID      string        `json:"guided_slot_id,omitempty"`
	PairingID         string        `json:"pairing_id,omitempty"`
	StagedRefs        []string      `json:"staged_credential_refs,omitempty"`
	Version           int           `json:"version"`
	Config            config.Config `json:"config"`
	Step              int           `json:"step"`
	CredentialRef     string        `json:"staged_credential_ref,omitempty"`
	// StopImported lists imported-only apps whose imports the person chose to
	// stop publishing. Config.ImportedHarnesses itself always comes from the
	// committed configuration (see carriedImportedHarnesses).
	StopImported []string `json:"stop_imported,omitempty"`
	// FailedRegion is the S3 region a storage check just failed for. Setup
	// asks for the region again, rather than checking that one again.
	FailedRegion string `json:"failed_region,omitempty"`
}

// draftFormat is the setupDraft.Version this release writes and reads.
const draftFormat = 1

func draftPath(home string) string { return filepath.Join(home, "setup-draft.json") }

// readDraft reads the saved setup. found is whether one exists; problem says
// why one that exists cannot be used (it does not decode, or another version
// of agent-archive wrote it), and err is any other failure to read it.
func readDraft(home string) (draft setupDraft, found bool, problem string, err error) {
	err = local.Read(draftPath(home), &draft)
	switch {
	case os.IsNotExist(err):
		return setupDraft{}, false, "", nil
	case state.IsUndecodable(err):
		return setupDraft{}, true, fmt.Sprintf("it does not read as a saved setup: %v", err), nil
	case err != nil:
		return setupDraft{}, true, "", err
	case draft.Version > draftFormat:
		return setupDraft{}, true, fmt.Sprintf("a newer version of agent-archive saved it (format %d)", draft.Version), nil
	case draft.Version != draftFormat || draft.Step < 0 || draft.Step > 2:
		return setupDraft{}, true, "it is not in a format this version saves", nil
	}
	return draft, true, "", nil
}

// offerUnusableDraft reads the saved setup, and when one exists but cannot
// be used, names it and offers to move it aside so setup can go on without
// it (declining leaves it, and stops). have is whether a usable draft was
// read.
func offerUnusableDraft(p *prompter, home string) (saved setupDraft, have bool, err error) {
	saved, found, problem, err := readDraft(home)
	if err != nil {
		return setupDraft{}, false, fmt.Errorf("read the saved setup %s: %w", draftPath(home), err)
	}
	if problem == "" {
		return saved, found, nil
	}
	p.warn(fmt.Sprintf("The saved setup in %s cannot be used: %s.", draftPath(home), problem),
		"Moving it aside keeps it, renamed, for reference, and setup starts again from your current settings.",
		stagedCredentialLeftNote(credentialOS, home))
	move, err := p.setupYesNo("Move it aside and continue?", true)
	if err != nil {
		return setupDraft{}, false, err
	}
	if !move {
		return setupDraft{}, false, fmt.Errorf("the saved setup in %s cannot be used (%s); move it aside or delete it, then run agent-archive setup", draftPath(home), problem)
	}
	aside, err := moveAside(draftPath(home))
	if err != nil {
		return setupDraft{}, false, err
	}
	p.note("Moved it to " + aside + ".")
	return setupDraft{}, false, nil
}

func runSetupCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("setup", stderr)
	abandon := fs.Bool("abandon-recovery", false, "keep every file as it is now and discard an interrupted setup")
	refresh := fs.Bool("refresh", false, "bring hooks, the background job's definition, and skills up to date, and nothing else")
	opts, parsed := setupFlags(fs, args)
	if !parsed {
		return 2
	}
	if opts.pair || opts.pairFile != "" {
		return runPairingSetupCommand(opts, *refresh, *abandon, fs, stdin, stdout, stderr, env)
	}
	if *refresh {
		if other := refreshCompanions(fs); other != "" {
			return fs.usageError("--refresh takes no other flag than --verbose, and %s was given", other)
		}
		return runSetupRefresh(stdout, stderr, env, opts.verbose)
	}
	if opts.requireSkillSupplied && opts.noRequireSkillSupplied {
		return fs.usageError("--require-skill-use and --no-require-skill-use contradict each other; give one")
	}
	if opts.noSkills && opts.skills {
		return fs.usageError("--no-skills and --skills contradict each other; give one")
	}
	if *abandon {
		if err := abandonRecovery(stdout, env); err != nil {
			terminal.Printf(stderr, "agent-archive: setup: %v\n", err)
			return 1
		}
		return 0
	}
	// Setup picks the scheduler (this system's own) and records it; every
	// other command addresses the one recorded.
	env = env.choosingBackend()
	if opts.given() && !opts.yes {
		return fs.usageError("answers given as flags need --yes (or run agent-archive setup alone to be asked)")
	}

	// Every step asks something, so without a terminal setup would stop at
	// its first question with nothing but an end-of-input error.
	if !opts.yes && !env.interactive(stdin) {
		terminal.Println(stderr, "agent-archive: setup: setup asks questions and needs a terminal. Nothing was changed. Run agent-archive setup in Terminal, or pass the answers with --yes (see agent-archive setup --help)."+env.overrideHint(stdin))
		return 1
	}
	// Every hook and the LaunchAgent run this path, so one that is about to
	// disappear would leave capture dead as soon as setup exits.
	if exe, err := env.executable(); err == nil {
		if problem := env.temporaryExecutableProblem(exe); problem != "" {
			terminal.Printf(stderr, "agent-archive: setup: %s Nothing was changed. Build or install agent-archive somewhere lasting (for example with go build -o ~/bin/agent-archive ./cmd/agent-archive, or the installer), then run setup from there.\n", problem)
			return 1
		}
	}
	// Before anything is created or locked: the data directory's lock is a file
	// lock, and a network filesystem is what makes it unreliable.
	if code, refused := refuseNetworkHome(opts, stdout, stderr, env); refused {
		return code
	}
	if opts.yes {
		if err := setupWithoutQuestions(opts, stdin, stdout, stderr, env); err != nil {
			terminal.Printf(stderr, "Setup incomplete: %v\n", err)
			var blocked *setupjournal.RecoveryBlockedError
			if errors.As(err, &blocked) {
				terminal.Println(stderr, blocked.Guidance())
			}
			var other *otherInstallationError
			if errors.As(err, &other) {
				terminal.Println(stderr, other.guidance())
			}
			var blocker *preflightError
			if errors.As(err, &blocker) {
				terminal.Println(stderr, blocker.guidance())
			}
			return setupExitCode(err)
		}
		return 0
	}
	if err := setup(stdin, stdout, stderr, env, opts.verbose, opts.skillsChoice(), opts.allowNetworkHome); err != nil {
		var pairingRequest *setupPairingRedirectError
		if errors.As(err, &pairingRequest) {
			opts.pair = true
			opts.pairingInput = pairingRequest.input
			return runPairingSetupCommand(opts, false, false, fs, pairingRequest.source, stdout, stderr, env)
		}
		// The checks above already name each blocker, marked ✗, so the exit
		// only says what to do. setup --yes names them again on standard
		// error, which is what a script reads.
		var blocker *preflightError
		if errors.As(err, &blocker) {
			terminal.Println(stderr, "Setup incomplete. "+blocker.guidance())
			return 1
		}
		terminal.Printf(stderr, "Setup incomplete: %v\n", err)
		var blocked *setupjournal.RecoveryBlockedError
		if errors.As(err, &blocked) {
			terminal.Println(stderr, blocked.Guidance())
			return 1
		}
		var other *otherInstallationError
		if errors.As(err, &other) {
			terminal.Println(stderr, other.guidance())
			return 1
		}
		terminal.Println(stderr, "Run agent-archive setup to continue.")
		return setupExitCode(err)
	}
	return 0
}

// setup is interactive setup. verbose prints a failed storage check's own
// error under its diagnosis; skills is --no-skills or --skills, which no
// question follows; allowNetworkHome is --allow-network-home.
func setup(stdin io.Reader, out, errOut io.Writer, env Env, verbose bool, skills skillsChoice, allowNetworkHome bool) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
	// Only another wizard is excluded while prompting; the old collector keeps working.
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	if err = recoverSetup(home, env); err != nil {
		return err
	}
	existing, found, err := config.Load(home)
	if err != nil {
		return err
	}
	// After uninstall the configuration stays, with archiving disabled:
	// its answers are the defaults, but this is setting up again, not a
	// change to a running installation.
	installed := found && existing.Archive.Enabled
	stopApps := startAnnouncedActivity(out, "Checking installed applications...")
	discoveries := env.discoverApplications(userHome)
	discoveredAt := env.now()
	// The review shows these; discoveries themselves are recorded unchanged.
	detected := env.detectHarnesses(userHome)
	stopApps()
	reviewed := reviewDiscoveries(discoveries, detected)
	p := newPrompter(stdin, out)
	p.tokenCommand = append([]string(nil), existing.CloudflareTokenCommand...)
	p.guidedSpacing = true
	defer p.close()
	p.now = env.now
	p.projectCurrent, _ = currentProject(env, userHome)
	p.projectScan = &setupProjectSearch{env: env, home: userHome}
	known := knownProjectsOnce(env, userHome, p.projectScan)
	// Said before any question: setup will refuse to install an app's hooks
	// beside another installation's (see applySetup).
	for _, problem := range env.installation(home, userHome).otherInstallationProblems(env.hookFiles(userHome), env.setupNames()) {
		p.warn(problem)
	}
	// What applying the setup needs is checked before any question, so a
	// hook file setup cannot edit, or a launchctl or Keychain that does not
	// answer, stops setup here rather than after every answer. The Keychain
	// is checked when the saved or unfinished setup stores in R2; choosing
	// R2 later finds a Keychain that does not open at that question. A
	// draft that cannot be read is dealt with after these checks. While a
	// draft is saved, setup --yes refuses to run, so no fix may send the
	// user there; nor may one for an installed app, which --yes keeps.
	unfinished, draftSaved, _, _ := readDraft(home)
	scope := setupPreflightScope(env.setupNames(), detected, existing, unfinished, draftSaved, installed)
	checks := preflight(env, home, userHome, scope)
	checks.print(p)
	if checks.blocked() {
		return &preflightError{checks: checks}
	}
	// Offer pairing only after the ordinary setup preflight passes. Returning
	// releases setup's lock before the pairing transaction acquires it.
	pairOptions, pairInput, err := firstSetupPairingQuestion(setupOptions{}, stdin, out, env)
	if err != nil {
		return err
	}
	if pairOptions.pair {
		return &setupPairingRedirectError{input: pairInput, source: stdin}
	}
	p.in = newPrompter(pairInput, out).in
	// A saved draft that names a bucket has already been past this.
	if !found && unfinished.Config.Storage.Bucket == "" {
		terminal.Println(p.out, "Choose Cloudflare R2 or Amazon S3 for private archive storage. Setup will guide you through connecting your account.")
	}
	draft, done, err := selectSetupDraft(p, home, userHome, env, existing, found, installed, known)
	if err != nil {
		return err
	}
	if done {
		if skills != skillsUnchanged {
			terminal.Println(p.out, "The agent skills were not changed: setup made no change this run. To change only the skills, run "+p.style.cmd("agent-archive setup --yes "+skills.flag())+".")
		}
		return nil
	}
	draft.NewInstallation = !found
	// The committed setting and this run's flag decide, never a saved draft's.
	draft.Config.NoSkills = skills.noSkills(existing.NoSkills)
	draft.Config.AllowNetworkHome = env.networkHomeOptIn(home, userHome, allowNetworkHome, existing)
	err = runSetupDraft(p, draft, home, userHome, exe, env, existing, installed, reviewed, discoveries, discoveredAt, errOut, known, verbose)
	// However setup ended, a bucket it created and did not keep is not left
	// without a word.
	noteUnusedCreatedBuckets(p, home)
	noteUnusedCreatedR2(p, home)
	return err
}

func runSetupDraft(p *prompter, draft setupDraft, home, userHome, exe string, env Env, existing config.Config, installed bool, reviewed, discoveries map[string]applicationDiscovery, discoveredAt time.Time, errOut io.Writer, known func(config.Config) []backfill.KnownProject, verbose bool) error {
	savedPath := draftPath(home)
	save := func() error { return local.Write(savedPath, draft) }
	var verifiedStorage credentials.Config
	for {
		retry, err := advanceSetupDraft(p, &draft, save, savedPath, home, userHome, env, known, &verifiedStorage, verbose)
		if err != nil {
			return err
		}
		if retry {
			continue
		}
		if err := promptDiscovery(p, &draft, existing); err != nil {
			return err
		}
		prepareDiscoveryHomes(&draft.Config, env, userHome)
		if err := save(); err != nil {
			return err
		}
		done, err := reviewAndCommitSetup(p, &draft, save, home, userHome, exe, env, existing, installed, reviewed, discoveries, discoveredAt, errOut, &verifiedStorage, known)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// retiredStagedRefs decides which opaque credential references become eligible
// for cleanup after commit, without writing to the Keychain or draft.
func retiredStagedRefs(retired, staged []string, active string) []string {
	result := append([]string(nil), retired...)
	for _, ref := range staged {
		if ref != active && !containsString(result, ref) {
			result = append(result, ref)
		}
	}
	return result
}

// reviewedSetupConfig computes the configuration shown at review and passed
// to applySetup. Draft persistence and installation happen elsewhere.
func reviewedSetupConfig(existing config.Config, draft setupDraft) config.Config {
	cfg := draft.Config
	if existing.RetentionDays == 0 && existing.RetentionProtection && !draft.RetentionEdited {
		cfg.RetentionDays = 0
		cfg.RetentionProtection = true
	}
	// The token source is local configuration, never an answer restored from a draft.
	cfg.CloudflareTokenCommand = append([]string(nil), existing.CloudflareTokenCommand...)
	// Existing-machine setup keeps the committed label: a resumed draft may
	// predate a rename. First setup retains its optional chosen draft label.
	if existing.MachineID != "" || existing.MachineName != "" {
		cfg.MachineName = existing.MachineName
	}
	// Ordinary setup cannot introduce credential provenance. Preserve the current
	// binding only while its destination and credential reference stay the same.
	cfg.MachineAssignment = nil
	if config.ValidMachineID(draft.GuidedSlotID) && draft.Config.MachineAssignment != nil && draft.Config.MachineAssignment.Kind == config.MachineAssignmentR2Own && draft.Config.MachineAssignment.SlotID == draft.GuidedSlotID && draft.Config.MachineAssignment.IssuerID == draft.Config.MachineID && draft.Config.MachineAssignment.DestinationID == cfg.DestinationID() {
		cfg.MachineAssignment = draft.Config.MachineAssignment
	}
	if cfg.DestinationID() == existing.DestinationID() && cfg.Storage.R2CredentialRef == existing.Storage.R2CredentialRef {
		cfg.MachineAssignment = existing.MachineAssignment
	}
	if cfg.RetentionDays == 0 && !cfg.RetentionProtection {
		cfg.RetentionDays = defaultRetentionDays
	}
	cfg.HistoricalProjects = config.ReconcileHistoricalProjects(existing.HistoricalProjects, cfg.HistoricalProjects)
	cfg.HistoricalProjectProtection = cfg.HistoricalProjectProtection || existing.HistoricalProjectProtection
	cfg.ImportedHarnesses = carriedImportedHarnesses(existing.ImportedHarnesses, cfg.Harnesses, draft.StopImported)
	return cfg
}

func setupPreflightScope(available []string, detected []string, existing config.Config, unfinished setupDraft, draftSaved, installed bool) preflightScope {
	scope := preflightScope{
		apps:          preflightApps(available, detected, existing.Harnesses, slices.Concat(existing.DeclinedHarnesses, unfinished.Config.DeclinedHarnesses), unfinished.Config.Harnesses),
		r2:            existing.Storage.Provider == credentials.ProviderR2 || unfinished.Config.Storage.Provider == credentials.ProviderR2,
		credentialRef: firstNonEmpty(existing.Storage.R2CredentialRef, unfinished.Config.Storage.R2CredentialRef),
	}
	switch {
	case draftSaved:
		scope.kept = scope.apps
	case installed:
		scope.kept = existing.Harnesses
	}
	return scope
}

func selectSetupDraft(p *prompter, home, userHome string, env Env, existing config.Config, found, installed bool, known func(config.Config) []backfill.KnownProject) (setupDraft, bool, error) {
	initial := existing
	if !found {
		initial.SkillEvidence = config.SkillEvidenceMetadata
		initial.RetentionDays = defaultRetentionDays
	}
	draft := setupDraft{Version: draftFormat, Config: initial, NewInstallation: !found}
	saved, haveDraft, err := offerUnusableDraft(p, home)
	if err != nil {
		return setupDraft{}, false, err
	}
	saved, haveDraft, err = recoverSetupResources(p, home, saved, haveDraft, initial, env)
	if errors.Is(err, errResourceRecoveryStopped) {
		return setupDraft{}, true, nil
	}
	if err != nil {
		return setupDraft{}, false, err
	}
	if haveDraft {
		choice, e := p.setupMenu("You have an unfinished setup. What would you like to do?", "continue",
			option{"continue", "Continue where you left off"},
			option{"capture", "Change apps and projects"},
			option{"storage", "Change storage"},
			option{"retention", "Change how long sessions are kept"},
			option{"restart", "Start over"})
		if e != nil {
			return setupDraft{}, false, e
		}
		if choice != "restart" {
			draft = saved
			// A draft from before skill policy existed is still a fresh setup
			// when no configuration was ever committed. Use the fresh default
			// rather than silently treating the unfinished draft as legacy.
			if !found && draft.Config.SkillEvidence == "" {
				draft.Config.SkillEvidence = config.SkillEvidenceMetadata
			}
			// Projects an import added after this draft was saved are kept:
			// the draft never saw them, so it cannot have meant to drop them.
			draft.Config.Archive.Projects = withBackfilledProjects(draft.Config.Archive.Projects, existing.Archive.Projects, backfilledProjects(env))
			if choice == "storage" {
				draft.Step = 1
			}
			if choice == "capture" {
				if e = chooseCapture(p, &draft.Config, userHome, env, known); e != nil {
					return setupDraft{}, false, e
				}
				if e = offerStopImported(p, &draft, existing); e != nil {
					return setupDraft{}, false, e
				}
				draft.Step = 2
				if draft.Config.Storage.Provider == "" {
					draft.Step = 1
				}
			}
			if choice == "retention" {
				days := draft.Config.RetentionDays
				if days == 0 && !draft.Config.RetentionProtection {
					days = defaultRetentionDays
				}
				if e = editSetupDraftRetention(p, &draft, days); e != nil {
					return setupDraft{}, false, e
				}
			}
		} else {
			if e = discardDraft(home, saved, existing, env); e != nil {
				return setupDraft{}, false, e
			}
		}
	} else if installed {
		choice, e := p.setupMenu("Agent Archive is already set up. What would you like to change?", "capture",
			option{"capture", "Apps and projects"},
			option{"storage", "Storage (bucket and credentials)"},
			option{"retention", "How long sessions are kept"},
			option{"all", "All settings"},
			option{"exit", "Nothing, exit"})
		if e != nil {
			return setupDraft{}, false, e
		}
		if choice == "exit" {
			terminal.Println(p.out, "Nothing was changed.")
			return setupDraft{}, true, nil
		}
		// One area is not three steps: its headings go without "Step n of 3".
		p.singleArea = choice != "all"
		//lint:ignore LV1001 menu keys are the option keys listed just above
		switch choice {
		case "storage":
			draft.Step = 1
		case "retention":
			draft.Step = 2
			if err = editSetupDraftRetention(p, &draft, existing.RetentionDays); err != nil {
				return setupDraft{}, false, err
			}
		}
		if choice == "capture" { // Storage is still verified, but its prompts are skipped.
			if err = chooseCapture(p, &draft.Config, userHome, env, known); err != nil {
				return setupDraft{}, false, err
			}
			if err = offerStopImported(p, &draft, existing); err != nil {
				return setupDraft{}, false, err
			}
			draft.Step = 2
		}
	}
	return draft, false, nil
}

func editSetupDraftRetention(p *prompter, draft *setupDraft, days int) error {
	draft.RetentionEdited = true
	var err error
	draft.Config.RetentionDays, err = p.setupRetentionDays(days)
	if draft.Config.RetentionDays == 0 {
		draft.Config.RetentionProtection = true
	}
	return err
}

func advanceSetupDraft(p *prompter, draft *setupDraft, save func() error, savedPath, home, userHome string, env Env, known func(config.Config) []backfill.KnownProject, verifiedStorage *credentials.Config, verbose bool) (bool, error) {
	var err error
	if draft.Step == 0 {
		if err = chooseCapture(p, &draft.Config, userHome, env, known); err != nil {
			return false, err
		}
		draft.Step = 1
		if err = save(); err != nil {
			return false, err
		}
	}
	// The apps are chosen now, so whether applySetup would refuse them
	// beside another installation's hooks is known: say so before the
	// storage and retention questions, not after them. The answers so
	// far are saved for the next run.
	if problems := env.installation(home, userHome).otherInstallationProblems(env.hookFiles(userHome), draft.Config.Harnesses); len(problems) > 0 {
		if err = save(); err != nil {
			return false, err
		}
		return false, &otherInstallationError{problems: problems}
	}
	if draft.Step == 1 {
		p.setupStep(2, "Connect storage")
		active, installed, _ := config.Load(home)
		offerKeep := installed && active.Storage == draft.Config.Storage && draft.FailedRegion == ""
		cfg, secret, saveSecret, e := promptStorage(p, draft.Config.Storage, env, draft.FailedRegion, offerKeep)
		if e != nil {
			return false, e
		}
		// The storage questions asked for a failed region again.
		draft.FailedRegion = ""
		if p.guided != nil && p.guided.c.slot != nil {
			s := p.guided.c.slot
			draft.GuidedSlotID = s.SlotID
			draft.Config.MachineID = s.IssuerID
			draft.Config.MachineAssignment = &config.MachineAssignment{DestinationID: s.DestinationID, Kind: config.MachineAssignmentR2Own, AccessKeyID: s.ProviderID, RecipientID: s.RecipientID, IssuerID: s.IssuerID, SlotID: s.SlotID}
		}
		if saveSecret {
			if e = stageStorageSecret(draft, save, env, &cfg, secret); e != nil {
				return false, p.rollbackGuidedCreation(e)
			}
		}
		draft.Config.Storage = cfg
		if p.guided != nil && p.guided.c.slot != nil {
			s := p.guided.c.slot
			staged := *s
			staged.State = issuance.OwnIntent
			staged.SecretRef = cfg.R2CredentialRef
			if e = issuance.Save(p.guided.c.home, staged); e != nil {
				draft.Step = 1
				_ = save()
				return false, p.rollbackGuidedCreation(e)
			}
			*s = staged
		}
		if p.guided != nil && p.guided.c.privacy.CheckedAt != nil {
			report := p.guided.c.privacy
			report.ConfigurationID = privacyConfigurationID(draft.Config)
			draft.Config.BucketPrivacy = &report
		}
		draft.Step = 2
		if err = save(); err != nil {
			return false, p.rollbackGuidedCreation(err)
		}
		// A key guided bucket creation made is staged: its bootstrap token
		// is no longer needed.
		p.finishGuidedCreation(draft, save)
	}
	if err = save(); err != nil {
		return false, err
	}
	return verifySetupDraftStorage(p, draft, save, savedPath, userHome, env, known, verifiedStorage, verbose)
}

// stageStorageSecret stores a key typed in, or made by guided bucket
// creation, under a new staged reference: the opaque reference is journaled in
// the draft before the Keychain is written, so a crash between them is
// recoverable. cfg gets the reference.
func stageStorageSecret(draft *setupDraft, save func() error, env Env, cfg *credentials.Config, secret credentials.R2Credentials) error {
	keychain, err := env.credentialStore()
	if err != nil {
		return openCredentialStoreError(credentialOS, err)
	}
	id, err := local.ID()
	if err != nil {
		return err
	}
	cfg.R2CredentialRef = "setup-" + id
	draft.CredentialRef = cfg.R2CredentialRef
	draft.StagedRefs = append(draft.StagedRefs, cfg.R2CredentialRef)
	draft.Config.Storage = *cfg
	if err = save(); err != nil {
		return err
	}
	if err = keychain.Save(context.Background(), cfg.R2CredentialRef, secret); err != nil {
		return fmt.Errorf("save staged credential: %w", err)
	}
	return nil
}

func verifySetupDraftStorage(p *prompter, draft *setupDraft, save func() error, savedPath, userHome string, env Env, known func(config.Config) []backfill.KnownProject, verifiedStorage *credentials.Config, verbose bool) (bool, error) {
	if draft.Config.Storage.Provider == credentials.ProviderR2 {
		kc, e := env.credentialStore()
		if e != nil {
			return false, e
		}
		if _, e = credentials.LoadStored(context.Background(), kc, draft.Config.Storage.R2CredentialRef); e != nil {
			draft.Step = 1
			draft.Config.Storage.R2CredentialRef = ""
			_ = save()
			return false, fmt.Errorf("stored R2 credential is unavailable; enter it again during setup")
		}
	}
	if draft.Config.Storage != *verifiedStorage {
		// A draft resumed past the storage questions (after changing
		// apps, say) still doesn't check a failed region again.
		if s := &draft.Config.Storage; s.Provider == credentials.ProviderS3 && draft.FailedRegion != "" && s.Region == draft.FailedRegion {
			p.storageStage(storageDetails)
			region, err := askFailedRegion(p, s.Region)
			if err != nil {
				return false, err
			}
			s.Region = region
		}
		draft.FailedRegion = ""
		terminal.Println(p.out, "")
		guidedPrivacy := p.guidedR2Privacy(draft.Config)
		e := runStorageCheck(p, &draft.Config, env)
		if errors.Is(e, errStorageCheckInterrupted) {
			return false, e
		}
		if e != nil {
			return recoverSetupStorageFailure(p, draft, save, savedPath, userHome, env, known, e, verbose)
		}
		report := p.guidedR2Privacy(draft.Config)
		if report == nil {
			report = guidedPrivacy
		}
		if report != nil {
			draft.Config.BucketPrivacy = report
			if e := save(); e != nil {
				return false, e
			}
		}
		*verifiedStorage = draft.Config.Storage
	}
	return false, nil
}

func recoverSetupStorageFailure(p *prompter, draft *setupDraft, save func() error, savedPath, userHome string, env Env, known func(config.Config) []backfill.KnownProject, checkErr error, verbose bool) (bool, error) {
	d := printStorageFailure(p, draft.Config.Storage, checkErr, verbose, "")
	printGuidedLeftovers(p, draft.Config.Storage)
	// Saved to ask the storage questions again, so that
	// "Continue where you left off" never repeats a check that just failed.
	if err := local.Write(savedPath, reopenStorage(*draft, d)); err != nil {
		return false, err
	}
	var choice string
	var promptErr error
	for {
		choice, promptErr = p.guidedChoice(promptModel{Question: "What next?", Default: "fix", Primary: []option{{"fix", storageFixLabel(draft.Config.Storage, d)}, {"retry", "Retry the check"}, {"edit", "Change other settings"}}, Secondary: []actionOption{{"details", "d", "Diagnostic details"}, {"cancel", "q", "Stop; keep your draft"}}})
		if promptErr != nil || choice != "details" {
			break
		}
		printStorageFailure(p, draft.Config.Storage, checkErr, true, "")
	}
	if promptErr != nil || choice == "cancel" {
		return false, &storageCheckError{err: checkErr, outcome: "your answers are kept"}
	}
	// Retry and "Change other settings" keep the draft as it is, at the check.
	if choice == "fix" && d.Cause == storage.CauseWrongRegion && draft.Config.Storage.Provider == credentials.ProviderS3 {
		p.storageStage(storageDetails)
		// When S3 didn't name the bucket's region, ask S3 for it, so the
		// default is not the region that just failed.
		region := d.Region
		if region == "" {
			region = firstNonEmpty(lookUpBucketRegion(p, env, draft.Config.Storage), draft.Config.Storage.Region)
		}
		var err error
		if draft.Config.Storage.Region, err = promptRegion(p, "Bucket region", region); err != nil {
			return false, err
		}
	} else if choice == "fix" {
		*draft = reopenStorage(*draft, d)
	}
	if choice == "edit" {
		if err := editSetupReview(env.setupNames(), p, draft, userHome, backfilledProjects(env), known); err != nil {
			return false, err
		}
	}
	if err := save(); err != nil {
		return false, err
	}
	return true, nil
}

func reviewAndCommitSetup(p *prompter, draft *setupDraft, save func() error, home, userHome, exe string, env Env, existing config.Config, installed bool, reviewed map[string]applicationDiscovery, discoveries map[string]applicationDiscovery, discoveredAt time.Time, errOut io.Writer, verifiedStorage *credentials.Config, known func(config.Config) []backfill.KnownProject) (bool, error) {
	var err error
	// Review what will be committed, not what a draft may have saved.
	draft.Config = reviewedSetupConfig(existing, *draft)
	if err = draft.Config.ValidateMachine(); err != nil {
		return false, err
	}
	hookFiles, installedHookFiles := env.hookFiles(userHome), env.installedHookFiles(userHome, existing)
	blocked := showSetupReview(p, draft.Config, setupReview{existing: existing, reconfiguring: installed, discoveries: reviewed, hookFiles: hookFiles, installedHookFiles: installedHookFiles, userHome: userHome, sourceRoots: env.nativeSessionDirectories(userHome, draft.Config)})
	var implications bytes.Buffer
	notes := newPrompter(strings.NewReader(""), &implications)
	defer notes.close()
	notes.style = p.style
	notes.now = p.now
	if err = reviewChanges(home, existing, draft.Config, notes, env); err != nil {
		return false, err
	}
	if existing.Paused {
		notes.note("Capture stays paused until you run agent-archive resume.")
	}
	reviewHookFiles(env.agentRegistry(), notes, draft.Config.Harnesses, hookFiles, installedHookFiles, existing.Harnesses, len(existing.HookFiles) > 0, userHome)
	warnCollectorEnvironment(notes, draft.Config.Storage, userHome, env)

	p.reviewModel.implications = implications.String()
	p.renderer().block(implications.String())
	action, e := reviewAction(p, installed, blocked, existing.MachineID == "")
	for e == nil && action == "details" {
		if e = showSetupReviewDetails(p, env, errOut); e != nil {
			break
		}
		renderSetupReview(p, p.reviewModel, false)
		action, e = reviewAction(p, installed, blocked, existing.MachineID == "")
	}
	if e != nil {
		return false, e
	}
	if action == "machine" {
		if err = chooseSetupMachineName(p, &draft.Config, env); err != nil {
			return false, err
		}
		return false, save()
	}
	if action == "check" {
		// The storage check runs again. S3 privacy is read again; guided R2
		// retains its setup-time management-API check because the bootstrap
		// token has been discarded.
		*verifiedStorage = credentials.Config{}
		return false, nil
	}
	if action == "cancel" {
		terminal.Println(p.out, "Cancelled. Active settings are unchanged; your setup draft is saved.")
		return true, nil
	}
	if action == "edit" {
		if err = editSetupReview(env.setupNames(), p, draft, userHome, backfilledProjects(env), known); err != nil {
			return false, err
		}
		if err = save(); err != nil {
			return false, err
		}
		return false, nil
	}

	draft.Config.RetiredCredentialRefs = retiredStagedRefs(draft.Config.RetiredCredentialRefs, draft.StagedRefs, draft.Config.Storage.R2CredentialRef)
	// Re-read under the machine lock in applySetup; it rejects concurrent config changes.
	skills := planSkillOptOut(env, home, userHome, exe, existing, draft.Config)
	if err = applySetup(home, userHome, exe, existing, &draft.Config, draft.StopImported, env); err != nil {
		return false, err
	}
	return true, finishSetup(p, errOut, home, draft.Config, existing.Paused, discoveries, discoveredAt, setupFinish{env: env, userHome: userHome, interactive: true, skills: skills})
}

// setupFinish is what finishSetup needs beyond the committed
// configuration: the machine it runs on, and whether it may ask a last
// question (interactive setup) or not (setup --yes).
type setupFinish struct {
	env         Env
	userHome    string
	interactive bool
	// skills is what setup removed and left alone of the agent skills, when
	// they are turned off.
	skills skillOptOut
}

// finishSetup follows a committed setup: it records the apps' versions,
// removes the saved draft, drops diagnostics of excluded projects, imports
// the chosen projects' sessions of the last 7 days, and says what to do next.
func finishSetup(p *prompter, errOut io.Writer, home string, cfg config.Config, paused bool, discoveries map[string]applicationDiscovery, discoveredAt time.Time, finish setupFinish) error {
	if err := reconcileCommittedGuidedSlot(home); err != nil {
		terminal.Println(errOut, "Guided key ledger commit pending; configuration is saved.")
	}
	if err := recordApplicationDiscoveries(home, discoveries, discoveredAt); err != nil {
		terminal.Printf(p.out, "Warning: installed application versions could not be recorded: %v\n", err)
	}
	if err := os.Remove(draftPath(home)); err != nil && !os.IsNotExist(err) {
		terminal.Println(errOut, "Configuration is committed; saved setup draft cleanup pending. Rerun setup to retry cleanup.")
	}
	// The configuration is committed; a diagnostic for a project that
	// was just excluded is stale local state, not a reason to fail.
	if e := capture.PruneDiagnostics(home, cfg.Archive.Projects); e != nil {
		terminal.Printf(errOut, "Could not prune capture diagnostics for excluded projects: %v\n", e)
	}
	if e := capture.PruneAdmissionIntents(home, cfg); e != nil {
		terminal.Printf(errOut, "Could not prune pending session starts after setup: %v\n", e)
	}
	completion := "Setup complete · Automatic capture is on"
	if len(cfg.Harnesses) == 0 {
		completion = "Setup complete · Capture is configured"
	}
	if len(cfg.Harnesses) == 1 && cfg.Harnesses[0] == "codex" && (cfg.Discovery == nil || !cfg.Discovery.Enabled) {
		completion = "Setup complete · Codex hook capture is configured"
	}
	if paused {
		completion = "Setup complete · Capture stays paused"
	}
	p.renderer().block(p.style.okMark() + " " + completion + "\n")
	if err := publishMachineAfterSetup(home, finish.env); err != nil {
		p.warn("Machine registration pending; capture is configured and the collector will retry.")
	}
	// The sessions of the last 7 days, the one setup runs from among them,
	// are imported without a question: automatic capture admits only
	// sessions that start after setup. A paused machine imports nothing
	// (backfill refuses too); resume says so.
	if !paused {
		importRecentSessions(p, errOut, home, finish.userHome, finish.env)
	}
	printAgentSkills(p, cfg, finish.userHome, claudeConfigDir(finish.env.installedHookFiles(finish.userHome, cfg)), finish.env.installation(home, finish.userHome).commandDataHome(), finish.skills, finish.env.agentRegistry())
	printNextSteps(p, cfg, paused)
	p.note("Another machine: agent-archive machines pair")
	if finish.interactive {
		choice, e := p.guidedChoice(promptModel{Question: "More next steps?", Default: "done", Primary: []option{{"done", "Finish setup"}}, Secondary: []actionOption{{"details", "d", "Machine transfer details"}}})
		if e == nil && choice == "details" {
			printAnotherMachine(p, cfg, finish.userHome, finish.env)
		}
	}
	return nil
}

// printAgentSkills says in one line per skill (/handoff, and any other in
// agentskills.Registry) where setup installed it, and names each path it
// left alone because it is not setup's; then one line on how to opt out of
// them. With the skills turned off (opt-out) it says instead what it
// removed and left alone, and how to turn them on.
func printAgentSkills(p *prompter, cfg config.Config, userHome, claudeDir, dataHome string, optOut skillOptOut, sources ...agentapi.SkillsLookup) {
	if cfg.NoSkills {
		printSkillOptOut(p, agentskills.Registry, optOut, userHome)
		return
	}
	ports := agentapi.SkillsLookup(productionAgents)
	if len(sources) > 0 {
		ports = sources[0]
	}
	files := agentskills.Files(ports, userHome, claudeDir, cfg.Harnesses, cfg.InstalledExecutable, dataHome)
	if printSkillFiles(p, agentskills.Registry, files, userHome) {
		terminal.Println(p.out, "To remove the agent skills and keep them off, run "+p.style.cmd("agent-archive setup --no-skills")+".")
	}
}

// skillOptOut is what setup does to the agent skills while they are turned
// off: the skill files of setup's it removes, and the other files at their
// paths that it leaves alone.
type skillOptOut struct {
	removed []string
	kept    []string
}

// planSkillOptOut is the skillOptOut of applying cfg over old, planned
// before setup applies it (afterwards the removed files are gone, and
// nothing could tell them from files that were never there). It is empty
// unless the skills are off. A failure to plan is left to the setup
// transaction, which plans the same removals and reports it.
func planSkillOptOut(env Env, home, userHome, executable string, old, cfg config.Config) skillOptOut {
	if !cfg.NoSkills {
		return skillOptOut{}
	}
	files, previousFiles := env.hookFiles(userHome), env.installedHookFiles(userHome, old)
	changes, kept, err := planAgentSkills(userHome, claudeConfigDir(files), claudeConfigDir(previousFiles), cfg, executable, env.installation(home, userHome).commandDataHome(), env.agentRegistry())
	if err != nil {
		return skillOptOut{}
	}
	var out skillOptOut
	for _, change := range changes {
		out.removed = append(out.removed, change.Path)
	}
	out.kept = kept
	return out
}

// printSkillOptOut reports opt-out's removals and the files it left, each
// under its skill, then that the skills are off and how to turn them on.
func printSkillOptOut(p *prompter, skills []agentskills.Skill, optOut skillOptOut, userHome string) {
	for _, skill := range skills {
		if removed := displayPaths(skillPaths(optOut.removed, skill.Name), userHome); len(removed) > 0 {
			terminal.Printf(p.out, "Removed /%s: %s\n", skill.Name, strings.Join(removed, ", "))
		}
		for _, path := range skillPaths(optOut.kept, skill.Name) {
			terminal.Printf(p.out, "Left %s as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so setup does not remove it.\n", displayPath(path, userHome))
		}
	}
	terminal.Println(p.out, "Agent skills are turned off. To install them, run "+p.style.cmd("agent-archive setup --skills")+".")
}

// skillPaths is those of paths that are the file of the skill named name.
func skillPaths(paths []string, name string) []string {
	var out []string
	for _, path := range paths {
		if filepath.Base(filepath.Dir(path)) == name {
			out = append(out, path)
		}
	}
	return out
}

// displayPaths is displayPath of each of paths.
func displayPaths(paths []string, userHome string) []string {
	out := make([]string, len(paths))
	for i, path := range paths {
		out[i] = displayPath(path, userHome)
	}
	return out
}

// printSkillFiles is printAgentSkills for the files of skills, each one
// reported under its own skill. It reports whether it named any installed.
func printSkillFiles(p *prompter, skills []agentskills.Skill, files []agentskills.File, userHome string) (installedAny bool) {
	for _, skill := range skills {
		var installed []string
		for _, f := range files {
			if f.Skill != skill.Name {
				continue
			}
			if current, err := os.ReadFile(f.Path); err == nil && bytes.Equal(current, f.Content) {
				installed = append(installed, displayPath(f.Path, userHome))
				continue
			}
			terminal.Print(p.out, leftSkillLine(skill.Title(), f.Path, userHome))
		}
		if len(installed) > 0 {
			what := skill.Title()
			if skill.Summary != "" {
				what += ", which " + skill.Summary
			}
			terminal.Printf(p.out, "Installed %s: %s\n", what, strings.Join(installed, ", "))
			installedAny = true
		}
	}
	return installedAny
}

// verifyStorage checks that the credentials are accepted, then that setup can
// write, read, and delete in the configured bucket, and records the bucket's
// privacy evidence and the check
// time in cfg. connectErr is a failure to build a client at all; accessErr
// is a failed check, which new settings may fix.
func verifyStorage(cfg *config.Config, env Env) (connectErr, accessErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := env.openStore(*cfg)
	if err != nil {
		return fmt.Errorf("connect storage: %w", err), nil
	}
	// A wrong account, key, or profile fails here on one cheap listing that
	// writes nothing, before the round trip's upload and clean-up each fail
	// in turn. Only VerifyAccess decides that the bucket works.
	if err = storage.Probe(ctx, store); err != nil {
		return nil, err
	}
	if err = storage.VerifyAccess(ctx, store); err != nil {
		return nil, err
	}
	cfg.BucketPrivacy = inspectBucketPrivacy(*cfg, store, env.now())
	cfg.StorageVerifiedAt = env.now().UTC()
	return nil, nil
}

// errStorageCheckInterrupted is the storage check's error when Ctrl-C (or
// another interrupt) stopped it.
var errStorageCheckInterrupted = errors.New("the storage check was interrupted")

// storageCheckInterruptedError is errStorageCheckInterrupted with the signal
// that stopped the check, so setup still exits with the shell's status for
// it, as it does when a signal stops it anywhere else.
type storageCheckInterruptedError struct{ sig os.Signal }

func (e *storageCheckInterruptedError) Error() string { return errStorageCheckInterrupted.Error() }

func (e *storageCheckInterruptedError) Is(target error) bool {
	return target == errStorageCheckInterrupted
}

// setupExitCode is setup's exit status for err: the shell's status for a
// signal that stopped the storage check, else 1.
func setupExitCode(err error) int {
	var interrupted *storageCheckInterruptedError
	if errors.As(err, &interrupted) {
		if s, ok := interrupted.sig.(syscall.Signal); ok {
			return 128 + int(s)
		}
	}
	var guided *guidedInterruptedError
	if errors.As(err, &guided) {
		if s, ok := guided.sig.(syscall.Signal); ok {
			return 128 + int(s)
		}
	}
	return 1
}

// storageCheckMayPrompt reports whether the storage check may run a program
// that asks the user something on the terminal: an S3 profile's
// credential_process, which the AWS SDK runs with the terminal's standard
// input and error so a helper such as aws-vault can ask for an MFA code. A
// spinner would draw over that question.
func storageCheckMayPrompt(storage credentials.Config, env Env) bool {
	if storage.Provider != credentials.ProviderS3 || storage.AWSProfile == "" {
		return false
	}
	// The AWS files are under the user's home, as for profile discovery,
	// not agent-archive's data directory.
	userHome, err := env.userHomeDir()
	if err != nil {
		return true
	}
	configFile, credentialsFile := awsFiles(userHome, env.lookupEnv)
	return credentialProcess(configFile, credentialsFile, storage.AWSProfile) != ""
}

// runStorageCheck runs the storage check on cfg, saying so on one line. Where
// the terminal can redraw a line, a spinner runs on it and the line then
// resolves in place: to "✓ Connected to your storage.", or on a failure to
// the diagnosis's own ✗ headline, which the caller prints next. Elsewhere
// the line is written plainly, and a failure leaves a blank line after it.
// No spinner runs while a profile's credential_process may be asking
// something on the terminal. The spinner is stopped on every path before
// anything else is written. An
// interrupt stops the spinner and returns at once with
// errStorageCheckInterrupted.
func runStorageCheck(p *prompter, cfg *config.Config, env Env) error {
	p.storageStage(storageVerification)
	release := p.suspendPrompts()
	defer release()
	const label = "Checking your storage connection…"
	style := p.style
	if style.live && storageCheckMayPrompt(cfg.Storage, env) {
		style.live = false
	}
	if !style.live {
		terminal.Println(p.out, label)
	}
	sp := style.spin(p.out, label)
	defer sp.stop()
	interrupts, stopInterrupts := env.interrupts()
	defer stopInterrupts()
	// The check runs on a copy, so a check still running after an
	// interrupt, which setup does not wait for, can never write to cfg.
	checked := *cfg
	done := make(chan error, 1)
	go func() {
		connectErr, accessErr := verifyStorage(&checked, env)
		if connectErr != nil {
			accessErr = connectErr
		}
		done <- accessErr
	}()
	var err error
	select {
	case err = <-done:
	case sig := <-interrupts:
		sp.stop()
		terminal.Println(p.out, p.style.failMark()+" Stopped checking your storage connection.")
		return &storageCheckInterruptedError{sig: sig}
	}
	sp.stop()
	if err != nil {
		if !style.live {
			terminal.Println(p.out, "")
		}
		return err
	}
	*cfg = checked
	// The object key cannot inspect R2's public domains. Keep the management
	// evidence from this run for the independent privacy result.
	if report := p.guidedR2Privacy(*cfg); report != nil {
		cfg.BucketPrivacy = report
	}
	terminal.Println(p.out, p.style.okMark()+" Connected to your storage.")
	printReviewPrivacy(p, *cfg)
	return nil
}

// hookNextStep says what each app needs before its hooks capture, and which
// sessions they capture: Codex asks to review new hooks in /hooks, while
// Claude Code and Cursor read them when a session starts. It does not say
// that sessions already open are left out: setup imports those
// (importRecentSessions), and the collector keeps them current.
// Codex and Claude Code prove a fresh start with SessionStart's source,
// which /clear sets too; Cursor proves it from the transcript, so only a new
// chat counts there. What the user types is in backquotes, painted as a
// command when printed.
var hookNextStep = map[string]string{
	"codex":  "Codex: run `/hooks` and approve the archive hooks; they capture sessions that start after that (or after `/clear`).",
	"claude": "Claude Code: nothing to approve; hooks capture sessions that start from now on (or after `/clear`).",
	"cursor": "Cursor: nothing to approve; hooks capture Agent chats that start from now on.",
}

// printNextSteps ends a committed setup with one line per app on what to do
// next, then how to check progress. Sessions already open in included
// projects need no step: setup imported them (importRecentSessions).
func printNextSteps(p *prompter, cfg config.Config, paused bool) {
	if paused {
		p.renderer().block("Next: run " + p.style.cmd("agent-archive resume") + " when you’re ready to start archiving.\n")
	} else {
		p.setupHeading("Next, in each app")
		for _, app := range cfg.Harnesses {
			if app == "codex" && cfg.Discovery != nil && cfg.Discovery.Enabled {
				location := "an included project"
				if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
					location = "any non-excluded current or future project"
				}
				terminal.Println(p.out, p.style.hang("  ", "Codex: automatic discovery captures supported tasks that start from now on in "+location+"; no hook approval needed."))
				terminal.Println(p.out, p.style.hang("  ", "Optional hook capture: "+paintCommands(p.style, hookNextStep[app])))
				continue
			}
			if step, ok := hookNextStep[app]; ok {
				terminal.Println(p.out, p.style.hang("  ", paintCommands(p.style, step)))
			}
		}
		if containsString(cfg.Harnesses, "codex") && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			// The discovery line above names the scope; hook capture's does not.
			if cfg.Discovery == nil || !cfg.Discovery.Enabled {
				terminal.Println(p.out, "Codex hooks capture supported tasks in any non-excluded project.")
			}
			if len(cfg.Harnesses) > 1 {
				terminal.Println(p.out, "Other apps still require an included project.")
			}
		}
		terminal.Println(p.out, "Check progress with "+p.style.cmd("agent-archive status")+".")
	}
}

// printAnotherMachine ends a committed setup with the command that sets up
// another machine with the same storage.
func printAnotherMachine(p *prompter, cfg config.Config, userHome string, environments ...Env) {
	if len(cfg.Archive.Projects) > maxProjectScopeRules {
		p.warn("Scope transfer accepts at most 4096 rules and refuses larger saved or resulting destination scopes. Review a larger scope before transferring; equal source aliases may coalesce.")
	}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		terminal.Printf(p.out, "\nTo set up another machine with this storage, set %s and\n%s there, then run:\n", envR2AccessKeyID, envR2SecretAccessKey)
	} else {
		terminal.Println(p.out, "\nTo set up another machine with this storage, run there:")
	}
	terminal.Println(p.out, "  "+p.style.cmd(anotherMachineCommand(cfg, userHome, environments...)))
}

// anotherMachineCommand is the setup --yes command that sets up another machine
// like this one: the same storage, capture rules, skills, apps and projects.
// Projects in the home
// folder are written from ~, which setup resolves on that machine. An R2 key is
// never written: setup --yes reads it from its environment variables there.
func anotherMachineCommand(cfg config.Config, userHome string, environments ...Env) string {
	var env Env
	if len(environments) > 0 {
		env = environments[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keys := map[string]string{}
	validatedKeys := map[string]string{}
	args := []string{"agent-archive", "setup", "--yes", "--provider", cfg.Storage.Provider, "--bucket", cfg.Storage.Bucket}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		args = append(args, "--r2-account", firstNonEmpty(cfg.Storage.R2AccountID, cfg.Storage.R2Endpoint))
	} else {
		args = append(args, "--aws-profile", cfg.Storage.AWSProfile)
		if cfg.Storage.Region != "" {
			args = append(args, "--region", cfg.Storage.Region)
		}
	}
	if len(cfg.Harnesses) > 0 {
		args = append(args, "--apps", strings.Join(cfg.Harnesses, ","))
	}
	if containsString(cfg.Harnesses, "codex") {
		discoveryChoice := "off"
		if cfg.Discovery != nil && cfg.Discovery.Enabled {
			discoveryChoice = "on"
		}
		args = append(args, "--codex-discovery", discoveryChoice, "--codex-capture-scope", string(cfg.EffectiveCodexCaptureScope()))
	}
	args = append(args, "--prefix", firstNonEmpty(cfg.Storage.Prefix, defaultPrefix))
	if cfg.RetentionDays == 0 {
		args = append(args, "--keep-forever")
	} else {
		args = append(args, "--retention-days", strconv.Itoa(cfg.RetentionDays))
	}
	if cfg.RequireSkillUse {
		args = append(args, "--require-skill-use")
	} else {
		args = append(args, "--no-require-skill-use")
	}
	args = append(args, "--skill-evidence", string(cfg.EffectiveSkillEvidence()))
	if cfg.NoSkills {
		args = append(args, "--no-skills")
	} else {
		args = append(args, "--skills")
	}

	projectStart := len(args)
	scope := ""
	if hasProjectExclusions(cfg.Archive.Projects) {
		scope = portableProjectScope(cfg.Archive.Projects, userHome, env, ctx)
		args = append(args, "--project-scope-file", "-")
	} else {
		for _, project := range cfg.Archive.Projects {
			if !project.Included {
				continue
			}
			key, checked := keys[project.Root]
			if !checked {
				child, done := context.WithTimeout(ctx, 250*time.Millisecond)
				// A key describes the whole repository. A configured subdirectory
				// must keep its path to avoid widening capture on another machine.
				if child.Err() == nil {
					if info, err := os.Stat(filepath.Join(project.Root, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
						root := local.CanonicalPath(project.Root)
						candidate, top, known := env.projectRepository(child, project.Root)
						if known && archive.IsRepoKey(candidate) && local.CanonicalPath(top) == root {
							key = candidate
							validatedKeys[root] = key
						}
					}
				}
				done()
				keys[project.Root] = key
			}
			if key != "" {
				args = append(args, "--project-repo", key)
			} else {
				args = append(args, "--project", homeRelative(project.Root, userHome))
			}
		}
	}
	if scope == "" && scopeArgumentsNeedStream(args) {
		scope = portableProjectScopeWithKeys(cfg.Archive.Projects, userHome, env, ctx, validatedKeys)
		args = append(args[:projectStart], "--project-scope-file", "-")
	}
	for i, arg := range args {
		args[i] = shellWord(arg)
	}
	command := strings.Join(args, " ")
	if scope != "" {
		// JSON is a single line beginning with [, so it cannot terminate this
		// quoted heredoc. Its paths never become shell expansions or argv.
		command += " <<'AGENT_ARCHIVE_PROJECT_SCOPE'\n" + scope + "\nAGENT_ARCHIVE_PROJECT_SCOPE"
	}
	return command
}

// homeRelative writes path from ~ when it is in the home folder. Project
// roots are saved with symlinks resolved, so the home folder is compared
// resolved too (on macOS a folder under /tmp resolves to /private/tmp).
func homeRelative(path, userHome string) string {
	if userHome == "" {
		return path
	}
	homes := []string{userHome}
	if resolved, err := filepath.EvalSymlinks(userHome); err == nil && resolved != userHome {
		homes = append(homes, resolved)
	}
	for _, home := range homes {
		if !local.PathWithin(path, home) {
			continue
		}
		if rel, err := filepath.Rel(home, path); err == nil {
			return filepath.ToSlash(filepath.Join("~", rel))
		}
	}
	return path
}

// shellWord quotes s for a POSIX shell when it needs quoting. A leading ~
// stays inside the quotes, where setup's --project expands it itself.
func shellWord(s string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("~/._-,:=@+%", r)
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !plain(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// chooseCapture asks for the apps and projects to capture. known, when not
// nil, lists the projects the apps' history mentions.
func chooseCapture(p *prompter, cfg *config.Config, userHome string, env Env, known func(config.Config) []backfill.KnownProject) error {
	if known == nil {
		known = func(config.Config) []backfill.KnownProject { return nil }
	}
	p.setupStep(1, "Choose what to capture")
	detected := env.detectHarnesses(userHome)
	p.reviewHint = ""
	current, refused := currentProject(env, userHome)
	if refused != "" {
		terminal.Println(p.out, p.style.dim(refused))
	}
	done, err := offerFirstCapture(env.setupNames(), p, cfg, detected)
	if err == nil && !done {
		err = chooseHarnesses(env.setupNames(), p, detected, cfg)
	}
	if err != nil {
		return err
	}
	if len(cfg.Harnesses) == 0 {
		return fmt.Errorf("choose at least one application")
	}
	if cfg.CodexCapture == nil {
		if err := promptCodexCaptureScope(p, cfg); err != nil {
			return err
		}
	}
	if codexOnlyAllProjects(*cfg) {
		p.note("Codex captures all current and future projects except explicit exceptions.")
		if err := promptCodexExceptions(p, cfg, userHome); err != nil {
			return err
		}
		if cfg.RetentionDays == 0 && !cfg.RetentionProtection {
			cfg.RetentionDays = defaultRetentionDays
		}
		return nil
	}
	p.projectCurrent = current
	p.projectConfig = *cfg
	p.projectBackfilled = backfilledProjects(env)
	cfg.Archive.Projects, err = selectSetupProjects(p, cfg.Archive.Projects, cfg.Archive.Projects, known(*cfg), current, userHome, p.projectBackfilled)
	if err != nil {
		return err
	}
	if cfg.RetentionDays == 0 && !cfg.RetentionProtection {
		cfg.RetentionDays = defaultRetentionDays
	}
	return nil
}

// offerFirstCapture offers detected applications while leaving project consent
// to the shared selector, inside and outside a repository.
func offerFirstCapture(available []string, p *prompter, cfg *config.Config, detected []string) (bool, error) {
	if len(cfg.Harnesses) > 0 || len(cfg.DeclinedHarnesses) > 0 {
		return false, nil
	}
	var apps []string
	for _, app := range available {
		if containsString(detected, app) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return false, nil
	}
	yes, err := p.setupYesNo("Capture sessions from "+appList(apps)+"?", true)
	if err != nil || !yes {
		return false, err
	}
	cfg.Harnesses = apps
	return true, nil
}

// currentProject is the Git repository setup was run from, which heads the
// project list already included, or "" when there is none. A repository too
// broad to archive on one Enter is no project, and refused says why, as one
// line to print: it is the home folder or a folder that holds it (a dotfiles
// checkout, or a stray .git above it), or a temporary folder or a folder that
// holds one. Setup then asks for the projects and pre-selects none. A
// repository inside a temporary folder is offered: setup running there is
// the person's own choice, unlike backfill, which treats such a repository
// as temporary. Only the folder itself is too broad.
func currentProject(env Env, userHome string) (current, refused string) {
	dir, err := os.Getwd()
	if env.WorkingDir != nil {
		dir, err = env.WorkingDir()
	}
	if err != nil {
		return "", ""
	}
	repo := suggestedProject(dir)
	if repo == "" {
		return "", ""
	}
	if reason := broadFolder(repo, userHome, env.backfillTempDirs()); reason != "" {
		return "", "Not offering " + displayPath(repo, local.CanonicalPath(userHome)) + " as a project: " + reason + ". Enter the projects you want."
	}
	return repo, ""
}

// broadFolder says why archiving dir on the strength of one Enter would take
// in too much, or returns "": dir is the home folder or holds it, or is one
// of the temporary folders (the ones backfill skips, see backfillTempDirs)
// or holds one. Folders are compared by CanonicalPath, which resolves
// symlinks and, on a case-insensitive volume, spells each folder as its
// directory lists it, as a project root is saved.
func broadFolder(dir, userHome string, temps []string) string {
	dir = local.CanonicalPath(dir)
	if home := local.CanonicalPath(userHome); userHome != "" {
		switch {
		case dir == home:
			return "it is your home folder"
		case local.PathWithin(home, dir):
			return "it holds your home folder"
		}
	}
	for _, temp := range temps {
		if temp = strings.TrimSpace(temp); temp == "" {
			continue
		}
		temp = local.CanonicalPath(temp)
		switch {
		case dir == temp:
			return "it is a temporary folder"
		case local.PathWithin(temp, dir):
			return "it holds a temporary folder"
		}
	}
	return ""
}

// storedCredentialReadable reports whether the credential saved under ref
// can be loaded now, without any Keychain prompt. It asks about what setup
// saved, not about what could be loaded: a key in the environment is not
// stored (credentials.LoadStored).
func storedCredentialReadable(env Env, ref string) bool {
	kc, err := env.credentialStore()
	if err != nil {
		return false
	}
	_, err = credentials.LoadStored(context.Background(), kc, ref)
	return err == nil
}

// bucketDocURL is the guide to creating a bucket by hand, which the storage
// menu's instructions point at.
const bucketDocURL = "https://github.com/wangjohn/agent-archive/blob/main/docs/getting-started/bucket.md"

// storageMenuOptions lists only the two storage providers.
func storageMenuOptions() []option {
	return []option{{"r2", "Cloudflare R2"}, {"s3", "Amazon S3"}}
}

// promptR2Location asks for the R2 account ID, or a URL: the bucket URL
// Cloudflare's dashboard shows fills in the bucket as well (see
// credentials.ParseR2Location), and fromURL says it did.
func promptR2Location(p *prompter, cfg *credentials.Config) (fromURL bool, err error) {
	for {
		answer, err := p.guidedText(promptModel{Question: "R2 account ID or bucket URL", Helpers: []string{"Find the account ID or S3 endpoint in Cloudflare Dashboard > Storage & databases > R2 > Overview.", "Example: https://<account-id>.r2.cloudflarestorage.com/<bucket>", "[:back] Back to storage options"}, Default: firstNonEmpty(cfg.R2AccountID, cfg.R2Endpoint), Label: "Account", Validate: func(value string) error {
			if value == "" {
				return fmt.Errorf("this value is required")
			}
			if value == ":back" {
				return nil
			}
			loc, e := credentials.ParseR2Location(value)
			if e == nil {
				_, e = credentials.R2Endpoint(loc.Endpoint, loc.AccountID)
			}
			if e != nil {
				return fmt.Errorf("that isn't an R2 account ID or bucket URL (%w). Paste the Account ID or a URL for only the bucket", e)
			}
			return nil
		}})
		if err != nil {
			return false, err
		}
		if answer == ":back" {
			return false, errChooseStorageAgain
		}
		loc, err := credentials.ParseR2Location(answer)
		if err == nil {
			var endpoint string
			if endpoint, err = credentials.R2Endpoint(loc.Endpoint, loc.AccountID); err == nil {
				if !loc.Cloudflare() {
					p.warn("That isn't a Cloudflare R2 address; it is used as an S3-compatible endpoint.")
				}
				if loc.Bucket != "" {
					cfg.Bucket = loc.Bucket
					terminal.Printf(p.out, "Bucket: %s\n", cfg.Bucket)
				}
				cfg.R2AccountID, cfg.R2Endpoint = loc.AccountID, endpoint
				return loc.Bucket != "", nil
			}
		}
		terminal.Printf(p.out, "That isn't an R2 account ID or bucket URL (%v).\n", err)
		terminal.Println(p.out, "Paste the Account ID from the R2 overview page, or the bucket's URL from its settings, such as https://<account-id>.r2.cloudflarestorage.com/<bucket>.")
	}
}

// chooseHarnesses asks which apps to include and keeps cfg.DeclinedHarnesses
// in step. On reconfiguration, an app the user leaves out after it was
// offered as found, or removes from the saved selection, is remembered so
// later runs do not offer it again (detection only sees a config directory,
// which stays after an app is excluded on purpose). An app that ends up
// included is no longer declined.
func chooseHarnesses(available []string, p *prompter, detected []string, cfg *config.Config) error {
	previous := cfg.Harnesses
	var offered, found []string
	for _, app := range detected {
		if containsString(cfg.DeclinedHarnesses, app) {
			continue
		}
		offered = append(offered, app)
		if len(cfg.Harnesses) > 0 && !containsString(cfg.Harnesses, app) {
			found = append(found, app)
		}
	}
	harnesses, err := promptHarnesses(available, p, offered, cfg.Harnesses)
	if err != nil {
		return err
	}
	var declined []string
	for _, app := range available {
		if !containsString(harnesses, app) && (containsString(cfg.DeclinedHarnesses, app) || containsString(found, app) || containsString(previous, app)) {
			declined = append(declined, app)
		}
	}
	cfg.Harnesses, cfg.DeclinedHarnesses = harnesses, declined
	return nil
}

func promptHarnesses(available []string, p *prompter, detected, existing []string) ([]string, error) {
	// Preserve an existing selection on reconfiguration. Detection supplies
	// defaults for first-time setup and, on reconfiguration, offers apps the
	// selection leaves out; it never proves capture is working.
	defaults := detected
	if len(existing) > 0 {
		defaults = existing
	}
	var suggested, others, found []string
	for _, app := range available {
		switch {
		case containsString(defaults, app):
			suggested = append(suggested, app)
		case containsString(detected, app):
			// Only reachable on reconfiguration: detected but not included.
			found = append(found, app)
			others = append(others, app)
		default:
			others = append(others, app)
		}
	}
	switch {
	case len(suggested) == 0:
		terminal.Println(p.out, "No apps found automatically.")
	case len(existing) > 0 && len(others) > 0:
		if len(found) > 0 {
			// Detected apps the saved selection leaves out are offered on
			// their own, defaulting to yes. Declining still allows other
			// changes below.
			terminal.Printf(p.out, "Included: %s.\nAlso found on this computer: %s.\n", appList(suggested), appList(found))
			add, err := p.setupYesNo(addPrompt(found), true)
			if err != nil {
				return nil, err
			}
			if add {
				var result []string
				for _, app := range available {
					if containsString(suggested, app) || containsString(found, app) {
						result = append(result, app)
					}
				}
				return result, nil
			}
		} else {
			// Name the apps left out so it is clear how to add them; "Keep X?"
			// reads as if declining would remove X.
			terminal.Printf(p.out, "Included: %s. Not included: %s.\n", appList(suggested), appList(others))
		}
		change, err := p.setupYesNo("Change which apps are included?", false)
		if err != nil {
			return nil, err
		}
		if !change {
			return suggested, nil
		}
	default:
		verb := "Include "
		if len(existing) > 0 {
			verb = "Keep "
		}
		yes, err := p.setupYesNo(verb+appList(suggested)+"?", true)
		if err != nil {
			return nil, err
		}
		if yes {
			return suggested, nil
		}
	}
	terminal.Println(p.out, "Choose which apps to include:")
	for {
		var result []string
		for _, app := range available {
			yes, err := p.setupYesNo("Include "+appName(app)+"?", containsString(suggested, app))
			if err != nil {
				return nil, err
			}
			if yes {
				result = append(result, app)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
		terminal.Println(p.out, "Choose at least one app to continue.")
	}
}

// addPrompt asks to add newly found apps: "Add it?" or "Add them?".
func addPrompt(apps []string) string {
	if len(apps) == 1 {
		return "Add it?"
	}
	return "Add them?"
}

// appList names apps in prose: "Codex", "Codex and Cursor", or
// "Codex, Claude Code, and Cursor".
func appList(apps []string) string {
	names := make([]string, len(apps))
	for i, app := range apps {
		names[i] = appName(app)
	}
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

// promptProjects asks which included projects to keep, then for new ones.
// Projects backfill added (backfilled holds their project IDs) are kept or
// excluded together with one question, since an import can add hundreds.
// An excluded project stays in the list as excluded, so its exclusion keeps
// holding: its imported sessions stop uploading and later backfills skip it.
// A project that was not imported and is not kept is dropped, as before.
// known are the projects the apps' history mentions, offered by number.
func promptProjects(p *prompter, existing []archive.ProjectActivation, backfilled map[string]bool, known []backfill.KnownProject, userHomes ...string) ([]archive.ProjectActivation, error) {
	home := ""
	if len(userHomes) > 0 {
		home = userHomes[0]
	}
	return selectSetupProjects(p, existing, existing, known, p.projectCurrent, home, backfilled)
}

// maxKnownProjects limits each display page; selection includes every candidate.
const maxKnownProjects = 12

// projectDetails describes a listed project: whether it is the folder setup
// runs in, how many sessions the apps' history holds for it, and when one
// was last used.
func projectDetails(project backfill.KnownProject, current string, now time.Time) string {
	var details []string
	if project.Root == current {
		details = append(details, "this folder")
	}
	switch {
	case project.Sessions == 1:
		details = append(details, "1 session")
	case project.Sessions > 1:
		details = append(details, fmt.Sprintf("%d sessions", project.Sessions))
	}
	if used := lastUsed(project.LastUsed, now); used != "" {
		details = append(details, used)
	}
	return strings.Join(details, " · ")
}

// projectDir resolves a project directory as typed: ~ is home (the
// process's when home is ""), and the result is absolute with symlinks
// resolved, as hooks match projects.
func projectDir(path, home string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return "", errors.New("your home directory is unknown; use the full path")
			}
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	root, err := filepath.Abs(path)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return "", fmt.Errorf("%s does not exist", path)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return root, nil
}

// parseNumbers reads a list of numbers from 1 to limit, such as "1 3",
// "1,3", or "2-4". ok is false for anything else, such as a path; inRange is
// false when a number is outside 1 to limit, and no range is expanded then.
func parseNumbers(answer string, limit int) (numbers []int, ok, inRange bool) {
	inRange = true
	for _, field := range strings.FieldsFunc(answer, func(r rune) bool { return r == ' ' || r == ',' }) {
		first, last, isRange := strings.Cut(field, "-")
		from, err := strconv.Atoi(first)
		if err != nil {
			return nil, false, false
		}
		to := from
		if isRange {
			if to, err = strconv.Atoi(last); err != nil || to < from {
				return nil, false, false
			}
		}
		if from < 1 || to > limit {
			inRange = false
			continue
		}
		for n := from; n <= to; n++ {
			numbers = append(numbers, n)
		}
	}
	return numbers, len(numbers) > 0 || !inRange, inRange
}

// lastUsed says how long ago a project was last used, in days.
func lastUsed(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	y1, m1, d1 := at.In(now.Location()).Date()
	y2, m2, d2 := now.Date()
	days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", days)
}

// knownProjectsOnce reuses bounded first-record evidence and coverage within a
// setup run. Apps, source roots, and project rules form the cache key; explicit
// Retry invalidates it. Partial usable roots remain available for consent.
func knownProjectsOnce(env Env, userHome string, searches ...*setupProjectSearch) func(config.Config) []backfill.KnownProject {
	search := &setupProjectSearch{env: env, home: userHome}
	if len(searches) > 0 {
		search = searches[0]
	}
	return search.projects
}

// includedProjects counts the projects capture is on for.
func includedProjects(projects []archive.ProjectActivation) int {
	n := 0
	for _, project := range projects {
		if project.Included {
			n++
		}
	}
	return n
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

func appName(app string) string {
	if d, ok := productionAgents.Catalog().Lookup(app); ok {
		return d.DisplayName
	}
	return app
}

func friendlyApps(apps []string) string {
	if len(apps) == 0 {
		return "none"
	}
	names := []string{}
	for _, a := range apps {
		names = append(names, appName(a))
	}
	return strings.Join(names, ", ")
}

func suggestedProject(dir string) string {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for p := root; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		if filepath.Dir(p) == p {
			return ""
		}
	}
}

// withBackfilledProjects adds to a resumed draft's projects the committed
// projects an import added that the draft does not mention.
func withBackfilledProjects(draft, committed []archive.ProjectActivation, backfilled map[string]bool) []archive.ProjectActivation {
	mentioned := map[string]bool{}
	for _, project := range draft {
		mentioned[project.Root] = true
	}
	for _, project := range committed {
		if backfilled[project.ProjectID] && !mentioned[project.Root] {
			draft = append(draft, project)
		}
	}
	return draft
}

// backfilledProjects is the set of project IDs any backfill import added:
// the projects it imported into, and the folders inside an added plain
// folder it added excluded (ProjectsKeptOut). A kept-out entry must never
// be dropped while the folder around it is included: that folder would
// then capture it. So a resumed draft carries it (withBackfilledProjects),
// and declining it in setup excludes it rather than removing it. An
// unreadable batch file leaves its projects out, so they are asked about
// one by one, as before imports existed.
func backfilledProjects(env Env) map[string]bool {
	out := map[string]bool{}
	home, err := env.home()
	if err != nil {
		return out
	}
	batches, _ := backfill.LoadBatches(home)
	for _, b := range batches {
		for _, id := range slices.Concat(b.ProjectsAdded, b.ProjectsKeptOut) {
			out[id] = true
		}
	}
	return out
}

// temporaryExecutableProblem says why exe cannot be what the hooks and the
// LaunchAgent run, or "" when it can: go run and go test build into a
// go-build directory that Go deletes on exit, and macOS clears the
// temporary folder.
func (e Env) temporaryExecutableProblem(exe string) string {
	paths := []string{filepath.Clean(exe)}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		paths = append(paths, resolved)
	}
	for _, path := range paths {
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if isGoBuildDir(filepath.Base(dir)) {
				return exe + " is a temporary build (from go run or go test) that Go deletes when it exits, so the hooks and background collector would stop working."
			}
		}
	}
	temp := e.tempDir()
	if temp == "" {
		return ""
	}
	temps := []string{filepath.Clean(temp)}
	if resolved, err := filepath.EvalSymlinks(temp); err == nil {
		temps = append(temps, resolved)
	}
	for _, path := range paths {
		for _, dir := range temps {
			if local.PathWithin(path, dir) {
				return fmt.Sprintf("%s is in the temporary folder %s, which is cleared automatically, so the hooks and background collector would stop working.", exe, temp)
			}
		}
	}
	return ""
}

// isGoBuildDir reports whether name is a directory Go builds a go run or go
// test binary in: go-build followed by digits, and nothing else, so a
// directory like go-builder is not mistaken for one.
func isGoBuildDir(name string) bool {
	digits, ok := strings.CutPrefix(name, "go-build")
	return ok && digits != "" && strings.Trim(digits, "0123456789") == ""
}

func runPairingSetupCommand(opts setupOptions, refresh, abandon bool, fs *commandFlags, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if refresh || abandon || opts.storageFlagsSupplied || opts.prefixSupplied || opts.keepForever || opts.retentionSupplied || opts.requireSkillSupplied || opts.noRequireSkillSupplied || opts.apps != "" || opts.skillEvidence != "" || opts.noSkills || opts.skills || len(opts.projectRepos) > 0 || opts.hasProjectScope() {
		return fs.usageError("pairing accepts --yes, --verbose, --project, --codex-discovery, --codex-capture-scope and one bundle input; other settings are reviewed interactively")
	}
	if err := setupPairing(opts, stdin, stdout, stderr, env.choosingBackend()); err != nil {
		terminal.Printf(stderr, "Pairing incomplete: %v\n", err)
		return 1
	}
	return 0
}
