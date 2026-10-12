package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func boundedPairingLine(r *bufio.Reader, limit int) (string, error) {
	line, err := boundedPairingAnswer(r, limit)
	if errors.Is(err, io.EOF) && line != "" {
		err = nil
	}
	return strings.TrimSpace(line), err
}

// boundedPairingAnswer preserves the raw answer and EOF for guidedRead, which
// distinguishes an explicit blank line from exhausted private input.
func boundedPairingAnswer(r *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return "", fmt.Errorf("pairing input exceeds its size limit")
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("pairing input could not be read")
		}
		return string(line), err
	}
}

func readPairingBundle(p *prompter, opts setupOptions, stdin io.Reader, env Env) (string, error) {
	if !opts.yes && opts.pairFile == "" {
		input, err := p.guidedText(promptModel{Question: "Pairing file path or text", Helpers: []string{"On your configured machine, run agent-archive machines add.", "Transfer the pairing file here, then enter its path. You can also paste copied pairing text."}, Secret: true, ReadAnswer: func(in *bufio.Reader) (string, error) { return boundedPairingAnswer(in, pairing.MaxBundle+1) }})
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(input, pairing.Prefix) {
			// A regular file can share the wire prefix; prefer its contents.
			if info, err := os.Stat(input); err != nil || !info.Mode().IsRegular() {
				return input, nil
			}
		}
		if input == "" || input == "-" {
			return "", errors.New("enter a pairing file path or paste pairing text; use --pair-file - to read stdin")
		}
		opts.pairFile = input
	}
	if opts.pairFile != "" && opts.pairFile != "-" {
		userHome, err := env.userHomeDir()
		if err != nil {
			return "", errors.New("cannot locate home directory for pairing file")
		}
		opts.pairFile, err = pairingFilePath(opts.pairFile, userHome)
		if err != nil {
			return "", errors.New("cannot resolve pairing file path")
		}
	}
	if opts.pairFile != "" {
		reader := stdin
		var f *os.File
		if opts.pairFile != "-" {
			var err error
			f, err = os.Open(opts.pairFile)
			if err != nil {
				return "", fmt.Errorf("cannot open pairing file %s; transfer it here and rerun agent-archive setup --pair-file %s", opts.pairFile, shellQuote(opts.pairFile))
			}
			defer func() { _ = f.Close() }()
			reader = f
		}
		data, err := io.ReadAll(io.LimitReader(reader, pairing.MaxBundle+2))
		if err != nil || len(data) > pairing.MaxBundle+1 {
			return "", fmt.Errorf("pairing file is oversized or unreadable")
		}
		return strings.TrimSpace(string(data)), nil
	}
	if opts.yes {
		return "", fmt.Errorf("--yes pairing needs --pair-file PATH or --pair-file -")
	}
	return "", errors.New("a pairing file or pairing text is required")
}

func setupPairing(opts setupOptions, stdin io.Reader, out, errOut io.Writer, env Env) error {
	if err := pairingAgentRefusal(env); err != nil {
		return err
	}
	p, bundle, closeInput, err := pairingReceiverInput(opts, stdin, out, env)
	if err != nil {
		return err
	}
	defer closeInput()
	defer p.close()
	code, err := readPairingCode(opts, p, env)
	if err != nil {
		return err
	}
	payload, err := pairing.Open(bundle, code, env.now())
	if err != nil {
		return fmt.Errorf("%w; for a wrong code, retry this bundle with the code delivered separately; for an expired bundle, ask the source for a new pairing. Expiry does not revoke storage access", err)
	}
	// Validation above is direct: setup's transaction does not call Config.Save.
	home, err := env.home()
	if err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	if err = recoverSetup(home, env); err != nil {
		return err
	}
	existing, found, err := config.Load(home)
	if err != nil {
		return err
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	if problem := env.temporaryExecutableProblem(exe); problem != "" {
		return fmt.Errorf("%s; install a lasting executable before pairing", problem)
	}
	if result, refused := refuseNetworkHome(opts, out, errOut, env); refused {
		return fmt.Errorf("network home refused (exit %d)", result)
	}
	terminal.Println(p.out, "Pairing · Review settings")
	cfg, err := reviewPairingDestination(p, payload, existing, found, opts, env)
	if err != nil {
		return err
	}
	cfg, err = pairingCaptureSettings(p, payload, cfg, existing, userHome, opts, env)
	if err != nil {
		return err
	}
	if err := configurePairedDiscovery(p, &cfg, opts, existing, found); err != nil {
		return err
	}
	prepareDiscoveryHomes(&cfg, env, userHome)
	cfg, err = reviewPairingSettings(p, payload, cfg, existing, found, userHome, opts, env)
	if err != nil {
		return err
	}
	cfg, err = assignPairedMachine(cfg, payload, env.now())
	if err != nil {
		return err
	}

	cfg, err = stagePairingCredential(home, cfg, payload, env)
	if err != nil {
		return err
	}
	checks := preflight(env, home, userHome, preflightScope{apps: cfg.Harnesses, kept: existing.Harnesses, r2: cfg.Storage.Provider == credentials.ProviderR2, credentialRef: cfg.Storage.R2CredentialRef})
	checks.print(p)
	if checks.blocked() {
		return &preflightError{checks: checks, yes: opts.yes}
	}
	terminal.Println(p.out, "Pairing · Check storage")
	if err = runStorageCheck(p, &cfg, env); err != nil {
		return fmt.Errorf("paired storage check failed; existing capture is unchanged, retry this pairing or finish/discard the saved setup")
	}
	if err = reviewWithoutQuestions(home, existing, cfg, p, env); err != nil {
		return err
	}
	cfg.ImportedHarnesses = carriedImportedHarnesses(existing.ImportedHarnesses, cfg.Harnesses, nil)
	skills := planSkillOptOut(env, home, userHome, exe, existing, cfg)
	discoveries := env.discoverApplications(userHome)
	terminal.Println(p.out, "Pairing · Save configuration")
	if err = applySetup(home, userHome, exe, existing, &cfg, nil, env); err != nil {
		return fmt.Errorf("pairing setup did not commit: %w; retry the same pairing or finish/discard the saved setup", err)
	}
	printPairingConnected(out, cfg.MachineName, payload, opts.yes)
	if err = finishSetup(p, errOut, home, cfg, existing.Paused, discoveries, env.now(), setupFinish{env: env, userHome: userHome, interactive: !opts.yes, skills: skills}); err != nil {
		return err
	}
	return nil
}

func assignPairedMachine(cfg config.Config, payload pairing.Payload, now time.Time) (config.Config, error) {
	pairedAt := now.UTC()
	kind := config.MachineAssignmentAWSProfile
	sharedWith := ""
	if cfg.Storage.Provider == credentials.ProviderR2 {
		kind = config.MachineAssignmentR2Shared
		sharedWith = payload.IssuerID
		if payload.Kind == config.MachineAssignmentR2Own {
			kind = payload.Kind
			sharedWith = ""
		}
	}
	cfg.MachineName = payload.Name
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: kind, SlotID: payload.SlotID, AccessKeyID: payload.AccessKeyID, RecipientID: payload.RecipientID, IssuerID: payload.IssuerID, SharedWith: sharedWith, PairingID: payload.PairingID, PairedFrom: payload.IssuerID, PairedAt: &pairedAt}
	if err := cfg.ValidateMachine(); err != nil {
		return cfg, fmt.Errorf("invalid paired machine assignment")
	}

	return cfg, nil
}

func printPairingConnected(out io.Writer, name string, payload pairing.Payload, yes bool) {
	terminal.Printf(out, "%s Paired successfully · %s is connected. Paired with %s.\n%s.\n", guidedOK(out), name, payload.IssuerName, pairingCredentialDescription(payload))
	if !yes {
		terminal.Println(out, "Configuration saved here. The source does not receive automatic confirmation; close its transfer instructions and delete your pairing file copies.")
	}
}

func readPairingCode(opts setupOptions, p *prompter, env Env) (string, error) {
	var err error
	var code string
	if opts.yes {
		var present bool
		code, present = env.lookupEnv("AGENT_ARCHIVE_PAIRING_CODE")
		unsetErr := env.unsetEnv("AGENT_ARCHIVE_PAIRING_CODE")
		if unsetErr != nil {
			return "", fmt.Errorf("cannot remove AGENT_ARCHIVE_PAIRING_CODE from this process")
		}
		if !present {
			return "", fmt.Errorf("--yes needs AGENT_ARCHIVE_PAIRING_CODE; deliver it separately from the bundle")
		}
	} else {
		if env.PairingCode != nil {
			code, err = env.PairingCode()
		} else if env.interactive(p.source) {
			code, err = p.guidedText(promptModel{Question: "Pairing code (hidden)", Label: "Credential", Secret: true})
		} else {
			return "", fmt.Errorf("pairing code needs a terminal, or --yes with AGENT_ARCHIVE_PAIRING_CODE")
		}
		if err != nil {
			return "", fmt.Errorf("pairing code could not be read")
		}
	}
	return code, nil
}

func reviewPairingDestination(p *prompter, payload pairing.Payload, existing config.Config, found bool, opts setupOptions, env Env) (config.Config, error) {
	var err error
	cfg := existing
	cfg.Storage = credentials.Config{Provider: payload.Storage.Provider, Bucket: payload.Storage.Bucket, Prefix: payload.Storage.Prefix, Region: payload.Storage.Region, AWSProfile: payload.Storage.AWSProfile, R2AccountID: payload.Storage.R2Account, R2Endpoint: payload.Storage.R2Endpoint}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		cfg.Storage.R2Endpoint, err = credentials.R2Endpoint(cfg.Storage.R2Endpoint, cfg.Storage.R2AccountID)
		if err != nil {
			return cfg, fmt.Errorf("paired R2 endpoint is invalid")
		}
	}
	guidedExplanation(p.out, "Pairing destination", fmt.Sprintf("%s from %s. Proposed machine: %s.", pairingCredentialDescription(payload), payload.IssuerName, payload.Name), fmt.Sprintf("Sessions will upload to %s bucket %s, folder %s, account %s, endpoint %s.", cfg.Storage.Provider, cfg.Storage.Bucket, cfg.Storage.Prefix, cfg.Storage.R2AccountID, cfg.Storage.R2Endpoint))
	if found {
		terminal.Printf(p.out, "Prior destination: %s bucket %s, folder %s.\n", existing.Storage.Provider, existing.Storage.Bucket, existing.Storage.Prefix)
	}
	changed := found && existing.Storage.Provider != "" && !destinationEqual(existing.Storage, cfg.Storage)
	if changed && opts.yes {
		return cfg, fmt.Errorf("paired destination differs from this machine's destination; run interactive pairing to approve the exact change")
	}
	if changed {
		consent, e := p.guidedYesNo("Explicitly replace the prior destination with the displayed destination?")
		if e != nil {
			return cfg, e
		}
		if !consent {
			return cfg, fmt.Errorf("destination change declined; nothing was changed")
		}
	}
	if cfg.Storage.Provider == credentials.ProviderS3 {
		if !validRegion(cfg.Storage.Region) {
			return cfg, fmt.Errorf("paired AWS region is invalid; configure the source and create a new pairing")
		}
		profiles, e := env.awsProfiles()
		if e != nil {
			return cfg, fmt.Errorf("cannot read local AWS profiles")
		}
		present := false
		for _, profile := range profiles {
			if profile.Name == cfg.Storage.AWSProfile {
				present = true
			}
		}
		if !present {
			return cfg, fmt.Errorf("AWS profile %s is missing; configure it with aws configure --profile %s or aws configure sso --profile %s, then retry", cfg.Storage.AWSProfile, shellWord(cfg.Storage.AWSProfile), shellWord(cfg.Storage.AWSProfile))
		}
	}
	return cfg, nil
}

func pairingCaptureSettings(p *prompter, payload pairing.Payload, cfg, existing config.Config, userHome string, opts setupOptions, env Env) (config.Config, error) {
	var err error
	cfg.RetentionDays = payload.RetentionDays
	if payload.RetentionDays == 0 {
		cfg.RetentionProtection = true
	}
	cfg.RequireSkillUse = payload.RequireSkillUse
	cfg.SkillEvidence = config.SkillEvidence(payload.SkillEvidence)
	cfg.NoSkills = payload.NoSkills
	cfg.Handoff = config.HandoffConfig{Args: payload.HandoffArgs, DefaultTo: payload.HandoffDefault}
	for _, agent := range []string{"claude", "codex", "cursor"} {
		if args := cfg.Handoff.Args[agent]; len(args) > 0 {
			quoted := make([]string, len(args))
			for i, arg := range args {
				quoted[i] = shellWord(arg)
			}
			terminal.Printf(p.out, "Handoff %s arguments: %s\n", agent, strings.Join(quoted, " "))
		}
	}
	var historical []config.HistoricalProject
	for _, project := range payload.HistoricalProjects {
		historical = append(historical, config.HistoricalProject{ID: project.ID, Label: project.Label, Permissions: []config.HistoricalProjectPermission{{DestinationID: cfg.DestinationID(), Enabled: project.Enabled}}})
		terminal.Printf(p.out, "Historical project %s [%s]: import-only permission for this destination. Local decisions are preserved.\n", project.Label, project.ID)
	}
	cfg.HistoricalProjects = config.ReconcileHistoricalProjects(existing.HistoricalProjects, historical)
	if len(cfg.HistoricalProjects) > 0 {
		cfg.HistoricalProjectProtection = true
	}
	cfg.Archive.Projects, err = pairScope(p, payload, existing, userHome, env, opts.yes)
	if err != nil {
		return cfg, err
	}
	detected := env.detectHarnesses(userHome)
	var apps []string
	for _, app := range payload.Apps {
		if slices.Contains(detected, app) || slices.Contains(existing.Harnesses, app) {
			apps = append(apps, app)
		} else {
			terminal.Printf(p.out, "Source app %s is not found here. See https://github.com/wangjohn/agent-archive/blob/main/docs/getting-started/setup.md for app installation and permissions.\n", app)
			if !opts.yes {
				choice, e := p.guidedMenu("Missing app "+app, "skip", option{"retry", "Recheck after installing the app"}, option{"skip", "Continue without this app"}, option{"cancel", "Cancel pairing setup"})
				if e != nil {
					return cfg, e
				}
				if choice == "cancel" {
					return cfg, fmt.Errorf("pairing setup cancelled before commit; existing capture is unchanged. Delete your bundle copies; issued storage access is not revoked here")
				}
				if choice == "retry" && slices.Contains(env.detectHarnesses(userHome), app) {
					apps = append(apps, app)
				} else {
					terminal.Printf(p.out, "Skipped %s; rerun setup after installing it.\n", app)
				}
			} else {
				terminal.Printf(p.out, "Skipped %s; rerun setup after installing it.\n", app)
			}
		}
	}
	for _, app := range existing.Harnesses {
		if !slices.Contains(apps, app) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return cfg, fmt.Errorf("none of the source apps is installed; install an app and retry")
	}
	cfg.Harnesses = apps
	// Pairing transfers settings and project suggestions, never live permission
	// from the other machine. Existing local consent remains the default.
	if err := configureCodexCaptureScope(&cfg, opts.codexCaptureScope); err != nil {
		return cfg, err
	}
	if !opts.yes && opts.codexCaptureScope == "" {
		if err := promptCodexCaptureScope(p, &cfg); err != nil {
			return cfg, err
		}
	}
	if problems := setupProjects(&cfg, opts.projects, userHome); len(problems) > 0 {
		return cfg, answersError(problems)
	}
	return cfg, nil
}

func reviewPairingSettings(p *prompter, payload pairing.Payload, cfg, existing config.Config, found bool, userHome string, opts setupOptions, env Env) (config.Config, error) {
	var err error
	approvedStorage := cfg.Storage
	if !opts.yes {
		p.projectCurrent, _ = currentProject(env, userHome)
		for {
			showSetupReview(p, cfg, setupReview{existing: existing, reconfiguring: found, hookFiles: env.hookFiles(userHome), installedHookFiles: env.installedHookFiles(userHome, existing), userHome: userHome, storageUnchecked: true, sourceRoots: env.nativeSessionDirectories(userHome, cfg)})
			choice, e := pairingReviewAction(p)
			for e == nil && choice == "details" {
				if e = showSetupReviewDetails(p, env, p.out); e != nil {
					break
				}
				renderSetupReview(p, p.reviewModel, false)
				choice, e = pairingReviewAction(p)
			}
			if e != nil {
				return cfg, e
			}
			if choice == "cancel" {
				return cfg, fmt.Errorf("pairing cancelled before commit; existing capture is unchanged. The transferred bundle remains usable until expiry; delete your copies. Issued storage access is not revoked here")
			}
			if choice == "save" {
				if setupNeedsProject(cfg) {
					p.warn("Include a project with Edit settings → Projects before saving these capture settings.")
					continue
				}
				break
			}
			draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2}
			if err = editSetupReview(env.setupNames(), p, &draft, userHome, backfilledProjects(env), func(config.Config) []backfill.KnownProject { return nil }); err != nil {
				return cfg, err
			}
			cfg = draft.Config
			if !destinationEqual(cfg.Storage, approvedStorage) {
				terminal.Printf(p.out, "Edited destination: %s bucket %s, folder %s, account %s, endpoint %s.\n", cfg.Storage.Provider, cfg.Storage.Bucket, cfg.Storage.Prefix, cfg.Storage.R2AccountID, cfg.Storage.R2Endpoint)
				consent, e := p.guidedYesNo("Explicitly approve this edited destination?")
				if e != nil {
					return cfg, e
				}
				if !consent {
					return cfg, fmt.Errorf("destination change declined; nothing was changed")
				}
				if cfg.Storage.Provider != payload.Storage.Provider || cfg.Storage.R2AccountID != payload.Storage.R2Account {
					return cfg, fmt.Errorf("credential source edited; configure it with ordinary setup before pairing")
				}
				approvedStorage = cfg.Storage
			}
		}
	}
	return cfg, nil
}

func pairingReviewAction(p *prompter) (string, error) {
	return p.guidedChoice(promptModel{Question: "Review pairing settings", Default: "cancel",
		Primary:   []option{{"save", "Save the displayed destination and capture settings"}, {"edit", "Edit settings"}},
		Secondary: []actionOption{{"details", "d", "Full settings and privacy"}, {"cancel", "q", "Cancel"}}})
}

func stagePairingCredential(home string, cfg config.Config, payload pairing.Payload, env Env) (config.Config, error) {
	priorDraft, have, problem, err := readDraft(home)
	if err != nil {
		return cfg, err
	}
	if problem != "" {
		return cfg, fmt.Errorf("unfinished setup cannot be read; finish or discard it before pairing")
	}
	ref := "pairing-" + payload.PairingID
	if have && (priorDraft.PairingID != payload.PairingID || !destinationEqual(priorDraft.Config.Storage, cfg.Storage)) {
		return cfg, fmt.Errorf("another unfinished setup exists; finish or discard it before pairing")
	}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		kc, e := env.credentialStore()
		if e != nil {
			return cfg, openCredentialStoreError(credentialOS, e)
		}
		cfg.Storage.R2CredentialRef = ref
		draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2, PairingID: payload.PairingID, CredentialRef: ref, StagedRefs: []string{ref}}
		if err = local.Write(draftPath(home), draft); err != nil {
			return cfg, err
		}
		value := credentials.R2Credentials{AccessKeyID: payload.AccessKeyID, SecretAccessKey: payload.SecretAccessKey}
		prior, e := credentials.LoadStored(context.Background(), kc, ref)
		if e != nil && !errors.Is(e, credentials.ErrMissingCredential) {
			return cfg, fmt.Errorf("cannot read staged pairing credential; retry or discard unfinished setup")
		}
		if e == nil && prior != value {
			return cfg, fmt.Errorf("staged pairing identity mismatched; discard unfinished setup before retrying")
		}
		if errors.Is(e, credentials.ErrMissingCredential) {
			if err = kc.Save(context.Background(), ref, value); err != nil {
				return cfg, fmt.Errorf("cannot stage paired credential; retry the same pairing")
			}
		}
	}
	payload.SecretAccessKey = ""
	return cfg, nil
}

func (e Env) openPairingTerminal() (io.ReadWriteCloser, error) {
	if e.PairingTerminal != nil {
		return e.PairingTerminal()
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func pairingReceiverInput(opts setupOptions, stdin io.Reader, out io.Writer, env Env) (*prompter, string, func(), error) {
	noClose := func() {}
	if mode, err := env.nonInteractive(); err != nil {
		return nil, "", noClose, err
	} else if mode.on && !opts.yes {
		return nil, "", noClose, fmt.Errorf("pairing prompts are off; use --yes with --pair-file and AGENT_ARCHIVE_PAIRING_CODE")
	}
	if !opts.yes && opts.pairFile == "" && !env.interactive(stdin) {
		return nil, "", noClose, fmt.Errorf("pasting a pairing bundle needs a terminal; use --pair-file for redirected input")
	}
	p := newPrompter(stdin, out)
	closePrompt := func() { p.close(); noClose() }
	if opts.pairingInput != nil {
		p.in = bufio.NewReader(opts.pairingInput)
	}
	p.now = env.now
	bundle, err := readPairingBundle(p, opts, stdin, env)
	if err != nil {
		p.close()
		return nil, "", noClose, err
	}
	if _, err = pairing.Inspect(bundle); err != nil {
		p.close()
		return nil, "", noClose, err
	}
	// Bundle stdin is consumed separately; all subsequent private input and
	// destination consent must use the same interactive terminal.
	if !opts.yes && !env.interactive(stdin) {
		p.close()
		tty, e := env.openPairingTerminal()
		if e != nil {
			return nil, "", noClose, fmt.Errorf("pairing review needs a terminal, or deliberate --yes")
		}
		noClose = func() { _ = tty.Close() }
		if !env.interactive(tty) {
			noClose()
			return nil, "", noClose, fmt.Errorf("pairing review needs an interactive terminal")
		}
		p = newPrompter(tty, tty)
		p.now = env.now
	}
	return p, bundle, closePrompt, nil
}

func pairingCredentialDescription(payload pairing.Payload) string {
	if payload.Storage.Provider != credentials.ProviderR2 {
		return "AWS profile/settings transfer"
	}
	if payload.Kind == config.MachineAssignmentR2Own {
		return "Dedicated R2 key; recipient can be revoked independently"
	}
	return "Shared-key R2 pairing; recipient cannot be revoked independently"
}
