package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// The environment variables setup --yes reads R2 keys from. The secret is
// never taken from a command argument, which other processes can read.
const (
	envR2AccessKeyID     = "AGENT_ARCHIVE_R2_ACCESS_KEY_ID"
	envR2SecretAccessKey = "AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY" //nolint:gosec // G101: a variable's name, not a credential.
)

type discoverySetting string

const (
	discoveryOn  discoverySetting = "on"
	discoveryOff discoverySetting = "off"
)

// setupOptions are setup's answers given as flags, for setup --yes.
type setupOptions struct {
	// pairingInput preserves answers buffered by first-run onboarding. The original
	// stdin remains the terminal identity used for private code entry.
	pairingInput             io.Reader
	codexDiscovery           string
	codexCaptureScope        string
	pair                     bool
	pairFile                 string
	prefix                   string
	prefixSupplied           bool
	keepForever              bool
	retentionDays            int
	retentionSupplied        bool
	requireSkillUse          bool
	noRequireSkillUse        bool
	requireSkillSupplied     bool
	noRequireSkillSupplied   bool
	provider                 string
	bucket                   string
	r2Account                string
	r2KeyID                  string
	awsProfile               string
	region                   string
	apps                     string
	projects                 []string
	projectRepos             []string
	projectScope             string
	projectScopeFile         string
	projectScopeSupplied     bool
	projectScopeFileSupplied bool
	projectScopeInputRead    bool
	projectMatches           *projectMatchResult
	yes                      bool
	verbose                  bool
	skillEvidence            string
	noSkills                 bool
	skills                   bool
	allowNetworkHome         bool
	storageFlagsSupplied     bool
}

// skillsChoice is what the person asked of the agent skills on this run:
// nothing (keep what the saved configuration says), --no-skills, or
// --skills.
type skillsChoice int

const (
	skillsUnchanged skillsChoice = iota
	skillsOff
	skillsOn
)

// skillsChoice is the choice the flags make; setup refuses both before it
// gets here.
func (o setupOptions) skillsChoice() skillsChoice {
	switch {
	case o.noSkills:
		return skillsOff
	case o.skills:
		return skillsOn
	}
	return skillsUnchanged
}

// flag is the flag that made c.
func (c skillsChoice) flag() string {
	if c == skillsOff {
		return "--no-skills"
	}
	return "--skills"
}

// noSkills is Config.NoSkills after this run: the choice when one was made,
// else what the saved configuration has.
func (c skillsChoice) noSkills(saved bool) bool {
	switch c {
	case skillsOff:
		return true
	case skillsOn:
		return false
	case skillsUnchanged:
	}
	return saved
}

// projectList is a repeatable --project.
type projectList []string

func (l *projectList) String() string { return strings.Join(*l, ",") }

func (l *projectList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// setupFlags adds setup's answer flags to fs and parses args.
func setupFlags(fs *commandFlags, args []string) (setupOptions, bool) {
	var opts setupOptions
	fs.BoolVar(&opts.pair, "pair", false, "import an encrypted pairing bundle")
	fs.StringVar(&opts.pairFile, "pair-file", "", "read a pairing bundle from PATH, or - for stdin")
	var projects, projectRepos projectList
	fs.StringVar(&opts.prefix, "prefix", "", "folder inside the bucket")
	fs.BoolVar(&opts.keepForever, "keep-forever", false, "disable automatic whole-session expiry")
	fs.IntVar(&opts.retentionDays, "retention-days", 0, "keep sessions for 1 to 36500 days")
	fs.BoolVar(&opts.requireSkillUse, "require-skill-use", false, "capture only sessions that use skills")
	fs.BoolVar(&opts.noRequireSkillUse, "no-require-skill-use", false, "capture sessions with or without skills")
	fs.StringVar(&opts.provider, "provider", "", "storage provider: r2 or s3")
	fs.StringVar(&opts.bucket, "bucket", "", "bucket name")
	fs.StringVar(&opts.r2Account, "r2-account", "", "R2 account ID, or the bucket URL")
	fs.StringVar(&opts.r2KeyID, "r2-access-key-id", "", "R2 access key ID")
	fs.StringVar(&opts.awsProfile, "aws-profile", "", "AWS profile for S3")
	fs.StringVar(&opts.region, "region", "", "S3 bucket region")
	fs.Func("codex-discovery", "on or off; automatically discover supported Codex tasks", func(value string) error {
		if discoverySetting(value) != discoveryOn && discoverySetting(value) != discoveryOff {
			return fmt.Errorf("--codex-discovery requires on or off")
		}
		opts.codexDiscovery = value
		return nil
	})
	fs.Func("codex-capture-scope", "included-projects or all-projects; Codex only", func(value string) error {
		scope := config.CodexCaptureScope(value)
		if scope != config.CodexIncludedProjects && scope != config.CodexAllProjects {
			return fmt.Errorf("--codex-capture-scope requires included-projects or all-projects")
		}
		opts.codexCaptureScope = value
		return nil
	})
	fs.StringVar(&opts.apps, "apps", "", "apps to capture, comma-separated")
	fs.StringVar(&opts.skillEvidence, "skill-evidence", "", "none, metadata, or body")
	fs.BoolVar(&opts.noSkills, "no-skills", false, "install no agent skills, and remove those setup wrote")
	fs.BoolVar(&opts.skills, "skills", false, "install the agent skills again after --no-skills")
	fs.BoolVar(&opts.allowNetworkHome, "allow-network-home", false, "allow a data directory or systemd unit directory on a network filesystem (Linux), when only one machine uses this home")
	fs.Var(&projectRepos, "project-repo", "repository key to capture (repeatable; unresolved or ambiguous keys are skipped)")
	fs.StringVar(&opts.projectScope, "project-scope", "", "portable JSON capture rules, including exclusions")
	fs.StringVar(&opts.projectScopeFile, "project-scope-file", "", "read portable capture rules from PATH, or - for stdin")
	fs.Var(&projects, "project", "project directory to capture (repeatable)")
	fs.BoolVar(&opts.yes, "yes", false, "apply without questions")
	fs.BoolVar(&opts.verbose, "verbose", false, "show a failed storage check's full error")
	if !fs.parseFlagsOnly(args) {
		return opts, false
	}
	opts.projects = projects
	opts.projectRepos = projectRepos
	fs.Visit(func(f *flag.Flag) {
		//lint:ignore LV1001 flag names are the ones defined just above
		switch f.Name {
		case "project-scope":
			opts.projectScopeSupplied = true
		case "project-scope-file":
			opts.projectScopeFileSupplied = true
		case "prefix":
			opts.prefixSupplied = true
		case "retention-days":
			opts.retentionSupplied = true
		case "require-skill-use":
			opts.requireSkillSupplied = true
		case "no-require-skill-use":
			opts.noRequireSkillSupplied = true
		case "provider", "bucket", "r2-account", "r2-access-key-id", "aws-profile", "region":
			opts.storageFlagsSupplied = true
		}
	})
	if opts.keepForever && opts.retentionSupplied {
		fs.usageError("--keep-forever and --retention-days are mutually exclusive")
		return opts, false
	}
	if err := validateProjectScopeOptions(opts); err != nil {
		fs.usageError("%s", err)
		return opts, false
	}
	return opts, true
}

// given reports whether any answer flag was passed.
func (o setupOptions) given() bool {
	return o.keepForever || o.codexCaptureScope != "" || o.codexDiscovery != "" || o.prefixSupplied || o.retentionSupplied || o.requireSkillSupplied || o.noRequireSkillSupplied || o.storageFlagsSupplied || o.apps != "" || len(o.projects) > 0 || len(o.projectRepos) > 0 || o.hasProjectScope() || o.skillEvidence != ""
}

// setupWithoutQuestions is setup --yes: the answers come from opts, the
// saved configuration, and what setup detects. It runs the same storage
// check and the same transaction as interactive setup, and refuses before
// changing anything when an answer is missing.
func setupWithoutQuestions(opts setupOptions, stdin io.Reader, out, errOut io.Writer, env Env) error {
	opts, err := readProjectScopeInput(opts, stdin)
	if err != nil {
		return err
	}
	return applySetupWithoutQuestions(opts, stdin, out, errOut, env)
}

// applySetupWithoutQuestions applies answers after explicit scope input is read.
func applySetupWithoutQuestions(opts setupOptions, stdin io.Reader, out, errOut io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
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
	// After uninstall the configuration stays with archiving disabled: its
	// answers are defaults, and no app's hooks are installed.
	installed := found && existing.Archive.Enabled
	if _, saved, _, e := readDraft(home); e != nil || saved {
		return fmt.Errorf("an unfinished setup is saved in %s; run agent-archive setup to finish or discard it first", draftPath(home))
	}
	p := newPrompter(stdin, out)
	defer p.close()
	p.now = env.now
	var matches projectMatchResult
	opts.projectMatches = &matches
	cfg, secret, err := setupAnswers(existing, opts, home, userHome, installed, env)
	// A key-only command may leave no project included. Explain its skips
	// before returning that validation error; invalid keys are never echoed.
	printSetupProjectMatches(p, opts.projectRepos, matches)
	if err != nil {
		return err
	}
	if err := validateScriptCodexChoices(cfg, opts, found && containsString(existing.Harnesses, "codex")); err != nil {
		return err
	}
	prepareDiscoveryHomes(&cfg, env, userHome)
	if secret.SecretAccessKey, err = scopeR2Secret(secret, p, stdin, env, opts); err != nil {
		return err
	}
	// The checks interactive setup makes before its first question, for the
	// apps these answers install and, when they store in R2, the Keychain.
	// They follow the answers' own checks, so a script learns of every
	// mistake in its flags without launchctl or the Keychain being asked.
	var kept []string
	if installed {
		kept = existing.Harnesses
	}
	scope := preflightScope{apps: cfg.Harnesses, kept: kept, r2: cfg.Storage.Provider == credentials.ProviderR2, credentialRef: cfg.Storage.R2CredentialRef}
	checks := preflight(env, home, userHome, scope)
	checks.print(p)
	if checks.blocked() {
		return &preflightError{checks: checks, yes: true}
	}
	if cfg.Storage.Provider == credentials.ProviderR2 && !(credentials.R2Location{Endpoint: cfg.Storage.R2Endpoint}).Cloudflare() {
		p.warn("--r2-account isn't a Cloudflare R2 address; it is used as an S3-compatible endpoint.")
	}
	// Only --provider r2 gives a new access key ID.
	if secret.AccessKeyID != "" && secret.SecretAccessKey == "" {
		if secret.SecretAccessKey, err = readR2Secret(p, stdin, env); err != nil {
			return err
		}
	}
	// The apps' version commands and launchctl run below; they must not
	// inherit the R2 key.
	env.forgetR2Variables()
	discoveries := env.discoverApplications(userHome)
	discoveredAt := env.now()

	// A new R2 key is staged under a draft, as interactive setup stages it,
	// so an interrupted run leaves a setup to finish or discard. A run that
	// fails before the transaction starts removes the key again; once it has
	// started, the key stays with the draft, as in interactive setup, since
	// an incomplete rollback can leave the configuration naming it.
	draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2}
	discard := func(failure error) error {
		if e := discardDraft(home, draft, existing, env); e != nil {
			return fmt.Errorf("%w (and the staged R2 key could not be removed: %w)", failure, e)
		}
		return failure
	}
	if err = stageScriptR2Credential(home, &cfg, &draft, secret, env); err != nil {
		return discard(err)
	}

	accessErr := runStorageCheck(p, &cfg, env)
	if errors.Is(accessErr, errStorageCheckInterrupted) {
		return discard(accessErr)
	}
	if accessErr != nil {
		// On standard error, with setup's last line, so a script that keeps
		// only errors still learns why.
		printStorageFailure(&prompter{out: errOut, style: styleFor(errOut)}, cfg.Storage, accessErr, opts.verbose, "run again with --verbose")
		return discard(&storageCheckError{err: accessErr, outcome: "nothing was changed"})
	}
	cfg.ImportedHarnesses = carriedImportedHarnesses(existing.ImportedHarnesses, cfg.Harnesses, nil)
	if err = reviewWithoutQuestions(home, existing, cfg, p, env); err != nil {
		return discard(err)
	}
	warnCollectorEnvironment(p, cfg.Storage, userHome, env)
	skills := planSkillOptOut(env, home, userHome, exe, existing, cfg)
	if err = applySetup(home, userHome, exe, existing, &cfg, nil, env); err != nil {
		if len(draft.StagedRefs) > 0 {
			return fmt.Errorf("%w; the new R2 key is kept with the unfinished setup: run agent-archive setup to finish or discard it", err)
		}
		return err
	}
	return finishSetup(p, errOut, home, cfg, existing.Paused, discoveries, discoveredAt, setupFinish{env: env, userHome: userHome, skills: skills})
}

// reviewWithoutQuestions is setup --yes's review: a reconfiguration's
// warnings, or its refusal (sessions pending at the old destination), then
// what will be saved and the bucket's privacy. A public bucket is refused,
// as its ✗ row blocks starting from interactive setup's review; privacy
// that could not be read only warns.
func reviewWithoutQuestions(home string, existing, cfg config.Config, p *prompter, env Env) error {
	if err := reviewChanges(home, existing, cfg, p, env); err != nil {
		return err
	}
	printDiscoveryConsent(p, cfg)
	terminal.Printf(p.out, "Apps: %s. Projects: %d. Storage: %s bucket %s.\n", appList(cfg.Harnesses), includedProjects(cfg.Archive.Projects), providerName(cfg.Storage.Provider), cfg.Storage.Bucket)
	policy := string(cfg.EffectiveSkillEvidence())
	if cfg.SkillEvidence == "" {
		policy += " (kept from previous setup)"
	}
	terminal.Printf(p.out, "Skill evidence: %s. User-level skill roots outside selected projects may be scanned.\n", policy)
	if check := privacyCheck(cfg, p.clock()); check.mark == symbolFail {
		printReviewChecklist(p, []reviewCheck{check})
		return fmt.Errorf("bucket %s allows public access (%s); nothing was changed. Fix its access (%s), then run the same agent-archive setup --yes command again", cfg.Storage.Bucket, check.detail, check.link)
	}
	printReviewPrivacy(p, cfg)
	return nil
}

func (opts setupOptions) applyRetentionAnswer(cfg *config.Config) error {
	if opts.keepForever && opts.retentionSupplied {
		return errors.New("--keep-forever and --retention-days are mutually exclusive")
	}
	if opts.keepForever {
		cfg.RetentionDays = 0
		cfg.RetentionProtection = true
	}
	if opts.retentionSupplied {
		if opts.retentionDays < 1 || opts.retentionDays > 36500 {
			return errors.New("--retention-days must be between 1 and 36500; use --keep-forever for no automatic expiry")
		}
		cfg.RetentionDays = opts.retentionDays
	}
	return nil
}

// setupAnswers is the configuration setup --yes saves, before its storage
// is checked: existing with the flags' answers. secret carries a new R2
// access key ID, if one was given; its secret is read once the answers
// check out. Every missing or wrong answer is reported together.
func setupAnswers(existing config.Config, opts setupOptions, home, userHome string, installed bool, env Env) (config.Config, credentials.R2Credentials, error) {
	cfg := existing
	if err := validateProjectScopeOptions(opts); err != nil {
		return cfg, credentials.R2Credentials{}, err
	}
	if err := opts.applyRetentionAnswer(&cfg); err != nil {
		return cfg, credentials.R2Credentials{}, err
	}
	if opts.prefixSupplied {
		if strings.TrimSpace(opts.prefix) == "" {
			return cfg, credentials.R2Credentials{}, errors.New("--prefix must name a folder inside the bucket")
		}
		if _, err := storage.Prefix(opts.prefix, "test"); err != nil {
			return cfg, credentials.R2Credentials{}, fmt.Errorf("--prefix: %w", err)
		}
	}
	if opts.requireSkillSupplied {
		cfg.RequireSkillUse = opts.requireSkillUse
	}
	if opts.noRequireSkillSupplied {
		cfg.RequireSkillUse = !opts.noRequireSkillUse
	}
	if cfg.SkillEvidence == "" && cfg.SchemaVersion == 0 {
		cfg.SkillEvidence = config.SkillEvidenceMetadata
	}
	if opts.skillEvidence != "" {
		config.SetSkillEvidence(&cfg, config.SkillEvidence(opts.skillEvidence))
	}
	cfg.NoSkills = opts.skillsChoice().noSkills(existing.NoSkills)
	cfg.AllowNetworkHome = env.networkHomeOptIn(home, userHome, opts.allowNetworkHome, existing)
	if !config.ValidSkillEvidence(cfg.EffectiveSkillEvidence()) {
		return cfg, credentials.R2Credentials{}, fmt.Errorf("--skill-evidence must be none, metadata, or body")
	}
	cfg.Archive.Projects = slices.Clone(existing.Archive.Projects)
	problems := setupApps(env.setupNames(), &cfg, opts.apps, env.detectHarnesses(userHome), installed)
	if err := configureCodexCaptureScope(&cfg, opts.codexCaptureScope); err != nil {
		problems = append(problems, err)
	}
	if err := configureDiscovery(&cfg, discoverySetting(opts.codexDiscovery)); err != nil {
		problems = append(problems, err)
	}
	if len(problems) == 0 {
		if other := env.installation(home, userHome).otherInstallationProblems(env.hookFiles(userHome), cfg.Harnesses); len(other) > 0 {
			return cfg, credentials.R2Credentials{}, &otherInstallationError{problems: other}
		}
	}
	if len(opts.projectRepos) > 0 {
		requests := make([]projectMatchRequest, 0, len(opts.projectRepos))
		for _, key := range opts.projectRepos {
			if !archive.IsRepoKey(key) {
				problems = append(problems, fmt.Errorf("--project-repo must be repo- followed by 16 lowercase hex digits"))
				continue
			}
			requests = append(requests, projectMatchRequest{RepoKey: key})
		}
		matched := matchProjects(context.Background(), env, userHome, existing, requests)
		if opts.projectMatches != nil {
			*opts.projectMatches = matched
		}
		if !matched.Incomplete {
			for _, roots := range matched.Roots {
				if len(roots) == 1 {
					problems = append(problems, setupProjects(&cfg, roots, userHome)...)
				}
			}
		}
	}
	if opts.projectScope != "" {
		problems = append(problems, setupProjectScope(&cfg, opts.projectScope, userHome, env)...)
	}
	problems = append(problems, setupProjects(&cfg, opts.projects, userHome)...)
	secret, storageProblems := setupStorageFromFlags(&cfg, opts, env)
	problems = append(problems, storageProblems...)
	if cfg.RetentionDays == 0 && !cfg.RetentionProtection {
		cfg.RetentionDays = defaultRetentionDays
	}
	return cfg, secret, answersError(problems)
}

// scopeR2Secret reads a script's R2 secret for the new key in secret, set
// in the environment or piped in. setup --yes calls it once every other
// answer checks out, so nothing is read from standard input for a run that
// is refused, and a missing secret is reported before launchctl or the
// Keychain is asked. A terminal is asked for it only after the preflight
// checks, so it returns nothing then.
func scopeR2Secret(secret credentials.R2Credentials, p *prompter, stdin io.Reader, env Env, opts setupOptions) (string, error) {
	if opts.projectScopeFile == "-" && secret.AccessKeyID != "" && lookupEnvTrimmed(env, envR2SecretAccessKey) == "" {
		return "", errors.New("--project-scope-file - owns stdin; set AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY for the R2 secret, or read scope from a file")
	}
	return scriptR2Secret(secret, p, stdin, env)
}

func scriptR2Secret(secret credentials.R2Credentials, p *prompter, stdin io.Reader, env Env) (string, error) {
	if secret.AccessKeyID == "" || (env.interactive(stdin) && lookupEnvTrimmed(env, envR2SecretAccessKey) == "") {
		return "", nil
	}
	return readR2Secret(p, stdin, env)
}

// answersError is setup --yes refused for its answers: a single problem as
// it is, or several listed one per line, each naming the flag that fixes
// it. A problem found twice, such as one --project given twice, is listed
// once.
func answersError(errs []error) error {
	var problems []error
	seen := map[string]bool{}
	for _, err := range errs {
		if !seen[err.Error()] {
			seen[err.Error()] = true
			problems = append(problems, err)
		}
	}
	switch len(problems) {
	case 0:
		return nil
	case 1:
		return problems[0]
	}
	lines := make([]string, 0, len(problems))
	for _, err := range problems {
		lines = append(lines, "\n  - "+err.Error())
	}
	return fmt.Errorf("%d answers are missing or wrong; nothing was changed:%s", len(problems), strings.Join(lines, ""))
}

// stageR2Key saves a new R2 key in the Keychain under a fresh reference,
// which it sets in cfg. The reference is written to draft's file first, so
// a run stopped in between leaves a setup that can be discarded.
func stageR2Key(home string, cfg *config.Config, draft *setupDraft, secret credentials.R2Credentials, env Env) error {
	keychain, err := env.credentialStore()
	if err != nil {
		return openCredentialStoreError(credentialOS, err)
	}
	id, err := local.ID()
	if err != nil {
		return err
	}
	cfg.Storage.R2CredentialRef = "setup-" + id
	draft.Config.Storage = cfg.Storage
	draft.CredentialRef, draft.StagedRefs = cfg.Storage.R2CredentialRef, []string{cfg.Storage.R2CredentialRef}
	if err = local.Write(draftPath(home), draft); err != nil {
		return err
	}
	if err = keychain.Save(context.Background(), cfg.Storage.R2CredentialRef, secret); err != nil {
		return fmt.Errorf("save the R2 key: %w", err)
	}
	return nil
}

// providerName is how setup names a storage provider.
func providerName(provider string) string {
	if provider == credentials.ProviderS3 {
		return "Amazon S3"
	}
	return "Cloudflare R2"
}

// setupApps sets cfg's apps from --apps, or else keeps the saved apps, or
// else takes the detected ones. An app it includes is no longer declined.
// It never removes an app: when installed, --apps must name every app whose
// hooks are installed, since taking them out is a choice for interactive
// setup, which shows the hooks it removes. It returns every problem with
// --apps.
func setupApps(available []string, cfg *config.Config, apps string, detected []string, installed bool) []error {
	var chosen, unknown []string
	switch {
	case apps != "":
		for app := range strings.SplitSeq(apps, ",") {
			app = strings.TrimSpace(app)
			if !containsString(available, app) {
				unknown = append(unknown, strconv.Quote(app))
			} else if !containsString(chosen, app) {
				chosen = append(chosen, app)
			}
		}
		if len(unknown) > 0 {
			return []error{fmt.Errorf("--apps takes %s, not %s", strings.Join(available, ", "), strings.Join(unknown, " or "))}
		}
	case len(cfg.Harnesses) > 0:
		chosen = cfg.Harnesses
	default:
		for _, app := range detected {
			if !containsString(cfg.DeclinedHarnesses, app) {
				chosen = append(chosen, app)
			}
		}
		if len(chosen) == 0 {
			return []error{fmt.Errorf("no apps were found on this machine; pass --apps (%s)", strings.Join(available, ", "))}
		}
	}
	if installed {
		var dropped []string
		for _, app := range cfg.Harnesses {
			if !containsString(chosen, app) {
				dropped = append(dropped, app)
			}
		}
		if len(dropped) > 0 {
			return []error{fmt.Errorf("--apps leaves out %s, which this machine captures now; --yes never removes an app's hooks, so name every app in --apps, or run agent-archive setup to remove one", appList(dropped))}
		}
	}
	var ordered, declined []string
	for _, app := range available {
		if containsString(chosen, app) {
			ordered = append(ordered, app)
		} else if containsString(cfg.DeclinedHarnesses, app) {
			declined = append(declined, app)
		}
	}
	cfg.Harnesses, cfg.DeclinedHarnesses = ordered, declined
	return nil
}

// setupProjects includes each --project directory in cfg, beside the
// projects already saved. It returns every problem with --project.
func setupProjects(cfg *config.Config, paths []string, userHome string) []error {
	var problems []error
	for _, path := range paths {
		root, err := projectDir(path, userHome)
		if err != nil {
			problems = append(problems, fmt.Errorf("--project %w", err))
			continue
		}
		included := false
		for i := range cfg.Archive.Projects {
			if cfg.Archive.Projects[i].Root == root {
				cfg.Archive.Projects[i].Included, included = true, true
			}
		}
		if !included {
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true})
		}
	}
	if len(problems) > 0 {
		return problems
	}
	if setupNeedsProject(*cfg) {
		return []error{errors.New("no project is included; pass --project DIR")}
	}
	return nil
}

// setupStorageFromFlags sets cfg's storage from the flags, or keeps the
// saved storage when no storage flag was passed. For R2, the returned
// credentials carry the access key ID when a new key is to be stored; the
// secret is read later, once everything else checks out. Every problem with
// the flags is returned.
func setupStorageFromFlags(cfg *config.Config, opts setupOptions, env Env) (credentials.R2Credentials, []error) {
	var secret credentials.R2Credentials
	if !opts.storageFlagsSupplied {
		if opts.prefixSupplied {
			cfg.Storage.Prefix = opts.prefix
		}
		if cfg.Storage.Provider == "" {
			return secret, []error{errors.New("storage is not set up yet; pass --provider r2 or --provider s3 and the bucket's details")}
		}
		return secret, nil
	}
	previous := cfg.Storage
	next := credentials.Config{Provider: opts.provider, Bucket: opts.bucket}
	if next.Provider == previous.Provider {
		next.Prefix = previous.Prefix
	}
	next.Prefix = firstNonEmpty(next.Prefix, defaultPrefix)
	if opts.prefixSupplied {
		next.Prefix = opts.prefix
	}
	var problems []error
	checkBucket := true
	//lint:ignore LV1001 --provider is raw user input; anything else is refused below
	switch opts.provider {
	case credentials.ProviderR2:
		secret, problems, checkBucket = r2FromFlags(&next, opts, previous, env)
	case credentials.ProviderS3:
		problems = s3FromFlags(&next, opts, env)
	case "":
		return secret, []error{errors.New("--bucket and the other storage flags need --provider r2 or --provider s3")}
	default:
		return secret, []error{fmt.Errorf("--provider must be r2 or s3, not %q", opts.provider)}
	}
	if checkBucket && next.Bucket == "" {
		problems = append(problems, errors.New("--bucket is required"))
	}
	if len(problems) > 0 {
		return secret, problems
	}
	cfg.Storage = next
	return secret, nil
}

// r2FromFlags sets next from the R2 flags. It returns the new key's access
// key ID, if one is given, and every problem with the flags. checkBucket is
// false when --r2-account is missing or doesn't parse, since its URL may
// name the bucket.
func r2FromFlags(next *credentials.Config, opts setupOptions, previous credentials.Config, env Env) (secret credentials.R2Credentials, problems []error, checkBucket bool) {
	if opts.awsProfile != "" || opts.region != "" {
		problems = append(problems, errors.New("--aws-profile and --region are for --provider s3"))
	}
	if opts.r2Account == "" {
		problems = append(problems, errors.New("--provider r2 needs --r2-account (the account ID, or the bucket URL from the dashboard)"))
	} else if loc, err := credentials.ParseR2Location(opts.r2Account); err != nil {
		problems = append(problems, fmt.Errorf("--r2-account: %w", err))
	} else if next.R2Endpoint, err = credentials.R2Endpoint(loc.Endpoint, loc.AccountID); err != nil {
		problems = append(problems, fmt.Errorf("--r2-account: %w", err))
	} else if loc.Bucket != "" && next.Bucket != "" && loc.Bucket != next.Bucket {
		problems = append(problems, fmt.Errorf("--bucket %s differs from the bucket in --r2-account's URL (%s)", next.Bucket, loc.Bucket))
	} else {
		checkBucket = true
		next.Bucket = firstNonEmpty(next.Bucket, loc.Bucket)
		next.R2AccountID = loc.AccountID
	}
	secret.AccessKeyID = firstNonEmpty(opts.r2KeyID, lookupEnvTrimmed(env, envR2AccessKeyID))
	if secret.AccessKeyID == "" && previous.Provider == credentials.ProviderR2 && previous.R2CredentialRef != "" {
		next.R2CredentialRef = previous.R2CredentialRef
		return secret, problems, checkBucket
	}
	if secret.AccessKeyID == "" {
		problems = append(problems, fmt.Errorf("--provider r2 needs the access key ID: pass --r2-access-key-id or set %s", envR2AccessKeyID))
	}
	return secret, problems, checkBucket
}

// s3FromFlags sets next from the S3 flags, with the profile's region when
// --region isn't given, and returns every problem with them.
func s3FromFlags(next *credentials.Config, opts setupOptions, env Env) []error {
	var problems []error
	if opts.r2Account != "" || opts.r2KeyID != "" {
		problems = append(problems, errors.New("--r2-account and --r2-access-key-id are for --provider r2"))
	}
	next.AWSProfile, next.Region = opts.awsProfile, opts.region
	if opts.awsProfile == "" {
		problems = append(problems, errors.New("--provider s3 needs --aws-profile"))
	} else if next.Region == "" {
		profiles, _ := env.awsProfiles()
		for _, profile := range profiles {
			if profile.Name == next.AWSProfile {
				next.Region = profile.Region
			}
		}
		if next.Region == "" {
			problems = append(problems, fmt.Errorf("AWS profile %s names no region; pass --region", next.AWSProfile))
		} else if !validRegion(next.Region) {
			problems = append(problems, fmt.Errorf("AWS profile %s names region %q, which isn't an AWS region; pass --region, such as --region us-east-1", next.AWSProfile, next.Region))
		}
	}
	if opts.region != "" && !validRegion(opts.region) {
		problems = append(problems, fmt.Errorf("--region %q isn't an AWS region; use a lowercase name such as us-east-1", opts.region))
	}
	return problems
}

// readR2Secret reads the R2 secret access key from its environment variable,
// or else from standard input: hidden when that is a terminal, otherwise its
// first line.
func readR2Secret(p *prompter, stdin io.Reader, env Env) (string, error) {
	if value := lookupEnvTrimmed(env, envR2SecretAccessKey); value != "" {
		return value, nil
	}
	// A terminal that interaction is switched off for is never read from: a
	// run inside an agent would wait there for a key nobody will type.
	if reason, blocked := env.blockedByNonInteractive(stdin); blocked {
		return "", fmt.Errorf("the R2 secret access key is needed: set %s (standard input is a terminal, which is not read because %s; %s=0 allows it)", envR2SecretAccessKey, reason, envNonInteractive)
	}
	label := ""
	if env.interactive(stdin) {
		label = "Secret access key (hidden): "
	}
	value, err := p.secret(label)
	if err != nil || value == "" {
		return "", fmt.Errorf("the R2 secret access key is needed: set %s or pass it on standard input", envR2SecretAccessKey)
	}
	return value, nil
}

// forgetR2Variables removes the R2 key's environment variables from this
// process, so no command setup runs inherits them. An injected LookupEnv
// (tests) reads no process environment, so there is nothing to remove.
func (e Env) forgetR2Variables() {
	if e.LookupEnv == nil {
		_ = os.Unsetenv(envR2AccessKeyID)
		_ = os.Unsetenv(envR2SecretAccessKey)
	}
}

func lookupEnvTrimmed(env Env, key string) string {
	value, _ := env.lookupEnv(key)
	return strings.TrimSpace(value)
}

func stageScriptR2Credential(home string, cfg *config.Config, draft *setupDraft, secret credentials.R2Credentials, env Env) error {
	if secret.SecretAccessKey != "" {
		return stageR2Key(home, cfg, draft, secret, env)
	}
	if cfg.Storage.Provider == credentials.ProviderR2 && !storedCredentialReadable(env, cfg.Storage.R2CredentialRef) {
		return fmt.Errorf("the stored R2 key can't be read from the %s; pass --r2-access-key-id and the secret (see agent-archive setup --help)", credentials.StoreName(credentialOS))
	}
	return nil
}
