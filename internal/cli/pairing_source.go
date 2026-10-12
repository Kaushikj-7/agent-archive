package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func pairingAgentRefusal(env Env) error {
	for _, marker := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CURSOR_AGENT"} {
		if _, present := env.lookupEnv(marker); present {
			return errors.New("pairing refuses to run inside a coding agent; run it in a separate terminal")
		}
	}
	return nil
}

func pairingInvocation(args []string) bool {
	if len(args) > 1 && args[0] == "machines" && args[1] == "add" {
		return true
	}
	if len(args) > 0 && args[0] == "setup" {
		for _, arg := range args[1:] {
			//lint:ignore LV1001 These are public flag spellings rather than domain states.
			if arg == "--pair" || arg == "-pair" || arg == "--pair-file" || arg == "-pair-file" || strings.HasPrefix(arg, "--pair=") || strings.HasPrefix(arg, "--pair-file=") || strings.HasPrefix(arg, "-pair=") || strings.HasPrefix(arg, "-pair-file=") {
				return true
			}
		}
	}
	return false
}

func runPairingAdd(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	fs := env.newCommandFlags("machines add", errOut)
	spares := fs.Int("spares", 2, "save target unused dedicated R2 keys, 0..5 (default 2)")
	name := fs.String("name", "", "new machine name")
	share := fs.Bool("share-key", false, "explicitly share this R2 key (cannot revoke the recipient independently)")
	expires := fs.Duration("expires", 15*time.Minute, "pairing expiry, 5m through 24h")
	printBundle := fs.Bool("print", false, "print the encrypted bundle instead of using the clipboard")
	file := fs.String("file", "", "write a new private bundle file, refusing overwrite")
	yes := fs.Bool("yes", false, "scripted delivery: print bundle and code separately without questions")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	sparesSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "spares" {
			sparesSet = true
		}
	})
	if *spares < 0 || *spares > 5 || *expires < 5*time.Minute || *expires > 24*time.Hour || (*yes && *name == "") || (*file != "" && *printBundle) {
		return fs.usageError("use expiry 5m..24h, --name with --yes, and one of --print or --file")
	}
	if err := pairingAgentRefusal(env); err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	if !*yes && (!env.interactive(stdin) || !env.interactive(out)) {
		terminal.Println(errOut, "machines add needs terminal input and output, or deliberate --yes scripted delivery")
		return 1
	}
	p := newPrompter(stdin, out)
	defer p.close()
	out = p.out
	if !*yes {
		terminal.Println(out, "Add another machine")
		terminal.Println(out)
	}
	if *name == "" {
		value, err := p.guidedText(promptModel{Question: "Name for the new machine", Helpers: []string{"Use 1 to 40 lowercase letters, digits, or hyphens."}, Validate: func(value string) error {
			if !pairing.ValidName(value) {
				return errors.New("machine names use 1 to 40 lowercase letters, digits, or hyphens")
			}
			return nil
		}})
		if err != nil {
			terminal.Println(errOut, "no name supplied")
			return 1
		}
		*name = value
	}
	if !pairing.ValidName(*name) {
		return fs.usageError("machine names use 1..40 lowercase letters, digits, or hyphens")
	}
	cfg, home, userHome, err := preparePairingSource(*name, *share, env)
	if err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	payload, err := sourcePairingPayload(cfg, *name, userHome, *expires, env)
	if err != nil {
		terminal.Println(errOut, "cannot prepare portable settings: "+err.Error())
		return 1
	}
	if !sparesSet {
		*spares = -1
	}
	if *file != "" {
		*file, err = pairingFilePath(*file, userHome)
		if err != nil {
			terminal.Println(errOut, "cannot resolve pairing file path")
			return 1
		}
	}
	return executePairingAdd(home, cfg, payload, p, env, out, errOut, pairingAddOptions{prompt: p, userHome: userHome, name: *name, share: *share, spares: *spares, yes: *yes, printBundle: *printBundle, file: *file})
}

type pairingAddOptions struct {
	prompt      *prompter
	userHome    string
	name        string
	share       bool
	spares      int
	yes         bool
	printBundle bool
	file        string
}

func (o *pairingAddOptions) chooseDelivery(p *prompter) error {
	if o.yes || o.printBundle || o.file != "" {
		return nil
	}
	return choosePairingDelivery(p, o)
}

func printPairingAccessReady(out io.Writer, cfg config.Config, slot issuance.Slot) {
	if cfg.RetentionDays == 0 {
		terminal.Println(out, "Keep forever is copied at pairing time. Upgrade the receiving machine to a version that supports Keep forever; older receivers reject this bundle.")
	}
	if slot.SlotID != "" {
		terminal.Println(out, guidedOK(out)+" Separate access ready. You can revoke this machine independently.")
	} else if cfg.Storage.Provider == credentials.ProviderR2 {
		terminal.Println(out, guidedOK(out)+" Shared access ready. Revoking it affects every machine using this key.")
	} else {
		terminal.Println(out, guidedOK(out)+" Archive settings ready.")
	}
}

func executePairingAdd(home string, cfg config.Config, payload pairing.Payload, p *prompter, env Env, out, errOut io.Writer, opts pairingAddOptions) int {
	release, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		terminal.Println(errOut, "another issuance operation is running")
		return 1
	}
	defer release()
	// A purge can unlink the lock after source preparation. Recheck the
	// committed configuration before issuing against an old in-memory snapshot.
	if err := validatePairingSourceSnapshot(home, cfg); err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	// Repeat local name checks under the same lock used by every delivery.
	prior, err := readPairingLedgers(home)
	if err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	for _, l := range prior {
		if l.Name == opts.name && l.State != pairingCancelled && l.State != pairingExpired && !env.now().After(l.ExpiresAt) {
			terminal.Println(errOut, "that name already has an outstanding pairing")
			return 1
		}
	}
	if opts.spares >= 0 && cfg.Storage.Provider != credentials.ProviderR2 {
		return pairingAddError(errOut, "--spares is for R2")
	}
	var target *int
	if opts.spares >= 0 {
		target = &opts.spares
		cfg.SpareKeys = &opts.spares
		if err = saveSpareIndex(home, cfg, target); err != nil {
			terminal.Println(errOut, "cannot save spare target")
			return 1
		}
	}
	slot, issuer, err := choosePairingKey(home, cfg, p, env, opts.yes, opts.share, &payload)
	if err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	if issuer != nil {
		defer issuer.api.Discard()
	}
	if slot.SlotID != "" {
		defer func() {
			releaseUntouchedSlot(home, &slot, issuer)
			discardDeliveredSecret(home, &slot, env, errOut)
			if e := saveSpareIndex(home, cfg, nil); e != nil {
				terminal.Println(errOut, "Could not update saved spare-key status; key records are preserved.")
			}
		}()
	}
	printPairingAccessReady(out, cfg, slot)
	if err := opts.chooseDelivery(p); err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	code, err := pairing.NewCode()
	if err != nil {
		terminal.Println(errOut, "cannot generate pairing code")
		return 1
	}
	bundle, err := pairing.Seal(payload, code)
	if err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	kind := pairingAWSProfile
	keyID, credentialRef := "", ""
	if cfg.Storage.Provider == credentials.ProviderR2 {
		kind = pairingSharedR2
		if slot.SlotID != "" {
			kind = pairingOwnR2
		}
		keyID = payload.AccessKeyID
		credentialRef = cfg.Storage.R2CredentialRef
		if slot.SlotID != "" {
			credentialRef = slot.SecretRef
		}
	}
	ledger := pairingLedger{Version: 1, SlotID: slot.SlotID, PairingID: payload.PairingID, RecipientID: payload.RecipientID, IssuerID: payload.IssuerID, Name: opts.name, DestinationID: config.DestinationID(cfg.Storage), Kind: kind, State: pairingPrepared, AccessKeyID: keyID, CredentialRef: credentialRef, CreatedAt: payload.CreatedAt, ExpiresAt: payload.ExpiresAt}
	if err = savePairingLedger(home, ledger); err != nil {
		terminal.Println(errOut, "cannot persist pairing preparation")
		return 1
	}
	if !opts.yes && !opts.printBundle && opts.file == "" {
		defer env.clearPairClipboard(bundle)
	}
	terminal.Println(out, "Pairing · Transfer prepared")
	if result := deliverPairingBundle(home, bundle, &ledger, &slot, env, out, errOut, opts); result != 0 {
		return result
	}
	return finishPairingDelivery(p, code, ledger, home, opts.yes, out, errOut, env, &slot, issuer)
}

func pairingFilePath(path, userHome string) (string, error) {
	if path == "~" {
		path = userHome
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(userHome, path[2:])
	}
	return filepath.Abs(path)
}

func choosePairingDelivery(p *prompter, opts *pairingAddOptions) error {
	choice, err := p.guidedMenu("How would you like to transfer the pairing file?", "file",
		option{"file", "Save a file (recommended)"}, option{"clipboard", "Copy to clipboard"}, option{"cancel", "Cancel"})
	if err != nil {
		return errors.New("no transfer method selected; rerun machines add")
	}
	if choice == "cancel" {
		return errors.New("pairing cancelled before delivery; no bundle was delivered; any prepared dedicated key remains tracked for reuse or cleanup")
	}
	if choice == "clipboard" {
		return nil
	}
	dir := filepath.Join(opts.userHome, "Downloads")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir = opts.userHome
	}
	path, err := p.guidedDefault("Save pairing file", filepath.Join(dir, "agent-archive-pairing-"+opts.name+".txt"))
	if err != nil {
		return errors.New("no pairing file selected; rerun machines add")
	}
	opts.file, err = pairingFilePath(path, opts.userHome)
	return err
}

// validatePairingSourceSnapshot is called with issued.lock held before key effects.
func validatePairingSourceSnapshot(home string, cfg config.Config) error {
	current, found, err := config.Load(home)
	if err != nil || !found || !current.Archive.Enabled {
		return errors.New("source setup changed; run setup before creating a pairing")
	}
	checked := cfg
	// The source's storage probe refreshes only these observations in memory.
	checked.StorageVerifiedAt = current.StorageVerifiedAt
	checked.BucketPrivacy = current.BucketPrivacy
	if !reflect.DeepEqual(checked, current) {
		return errors.New("source configuration changed; rerun machines add")
	}
	return nil
}

func finishPairingDelivery(p *prompter, code string, ledger pairingLedger, home string, yes bool, out, errOut io.Writer, env Env, slot *issuance.Slot, issuer *keyIssuer) int {
	if yes {
		words, err := pairing.CodeWords(code)
		if err != nil {
			terminal.Println(errOut, "code delivery uncertain; pairing remains tracked")
			return 1
		}
		if _, err := fmt.Fprintln(out, "Pairing code (deliver separately): "+strings.Join(words, " ")); err != nil {
			terminal.Println(errOut, "code delivery uncertain; pairing remains tracked")
			return 1
		}
		terminal.Println(out, "Pairing · Code available · Receiver completion unconfirmed")
		prepareFuturePairingAccess(issuer, out, errOut)
		return 0
	}
	out = p.out
	if _, err := p.guidedText(promptModel{Question: "Press Enter when the pairing file or text has arrived on the other machine"}); err != nil {
		terminal.Println(errOut, "Transfer unfinished. The pairing remains valid until expiry; the code is discarded on exit.")
		return 1
	}
	if err := showPairingCode(p, code, env); err != nil {
		terminal.Println(errOut, "Could not show the code. The pairing remains valid until expiry; rerun machines add for a new pairing.")
		return 1
	}
	terminal.Println(out, "Pairing · Code available · Receiver completion unconfirmed")
	terminal.Printf(out, "Finish setup on %s. That machine confirms when its configuration is saved.\n", ledger.Name)
	for {
		choice, err := p.guidedMenu("Pairing", "done", option{"show", "Show code again"}, option{"done", "Close transfer instructions"}, option{"cancel", "Cancel pairing"})
		if err != nil {
			terminal.Println(errOut, "pairing remains delivered; code discarded on exit")
			return 1
		}
		if choice == "done" {
			terminal.Println(out, "Transfer instructions closed. Receiver completion is unconfirmed on this machine.")
			terminal.Println(out, "Delete any saved pairing files when no longer needed.")
			prepareFuturePairingAccess(issuer, out, errOut)
			return 0
		}
		if choice == "cancel" {
			terminal.Println(out, "Cancellation does not delete transferred files or invalidate the encrypted bundle; delete your copies. Bundle expiry does not revoke storage access.")
			ledger.State = pairingCancelled
			if err = savePairingLedger(home, ledger); err != nil {
				terminal.Println(errOut, "cannot record cancellation")
				return 1
			}
			if slot.SlotID != "" {
				if issuer != nil {
					issuer.cancelDelivered(slot)
				} else {
					slot.State = issuance.CleanupPending
					slot.CleanupReason = "explicit-cancellation-token-needed"
					_ = issuance.Save(home, *slot)
				}
				if slot.State == issuance.Deleted {
					terminal.Println(out, "Dedicated token deletion confirmed; provider propagation remains unverified.")
				} else {
					terminal.Println(out, "Pairing cancelled locally; dedicated cleanup pending, access may remain. Use explicit revoke with management access.")
				}
			} else if ledger.Kind == pairingAWSProfile {
				terminal.Println(out, "Pairing cancelled locally. Local AWS profiles on the receiving machine are unchanged; no AWS credential was issued by pairing.")
			} else {
				terminal.Println(out, "Pairing cancelled locally. Shared R2 access remains active; replace the shared key on every user to revoke it.")
			}
			return 0
		}
		if err = showPairingCode(p, code, env); err != nil {
			terminal.Println(errOut, "pairing remains delivered; code discarded on exit")
			return 1
		}
	}
}

func prepareFuturePairingAccess(issuer *keyIssuer, out, errOut io.Writer) {
	if issuer == nil || issuer.cfg.SpareTarget() == 0 {
		return
	}
	terminal.Println(out, "Preparing access for future machines…")
	if err := issuer.refill(); err != nil {
		terminal.Println(errOut, "This pairing is ready. Preparing access for future machines failed; retry on the next add.")
	}
}

func sourcePairingPayload(cfg config.Config, name, userHome string, expiry time.Duration, env Env) (pairing.Payload, error) {
	if !pairing.ValidID(cfg.MachineID) {
		return pairing.Payload{}, errors.New("source machine identity is invalid; run setup first")
	}
	id, err := local.ID()
	if err != nil {
		return pairing.Payload{}, err
	}
	recipient, err := local.ID()
	if err != nil {
		return pairing.Payload{}, err
	}
	now := env.now().UTC().Truncate(time.Second)
	storage, key, err := pairingSourceStorage(cfg, env)
	if err != nil {
		return pairing.Payload{}, err
	}
	issuerName := cfg.MachineName
	if issuerName == "" {
		issuerName = "unnamed-" + cfg.MachineID[:4]
	}
	inclusions, exclusions, err := exportPairingScope(context.Background(), cfg, userHome, env)
	if err != nil {
		return pairing.Payload{}, err
	}
	p := pairing.Payload{Version: 1, PairingID: id, RecipientID: recipient, IssuerID: cfg.MachineID, IssuerName: issuerName, Name: name, CreatedAt: now, ExpiresAt: now.Add(expiry), Apps: cfg.Harnesses, RetentionDays: cfg.RetentionDays, RequireSkillUse: cfg.RequireSkillUse, SkillEvidence: string(cfg.EffectiveSkillEvidence()), NoSkills: cfg.NoSkills, HandoffArgs: cfg.Handoff.Args, HandoffDefault: cfg.Handoff.DefaultTo, Storage: storage, AccessKeyID: key.AccessKeyID, SecretAccessKey: key.SecretAccessKey, Inclusions: inclusions, Exclusions: exclusions, HistoricalProjects: exportHistoricalPairing(cfg)}
	return p, p.Validate()
}

func pairingNameAvailable(store storage.ObjectStore, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), machines.Timeout)
	defer cancel()
	result := machines.List(ctx, store)
	if result.Partial || len(result.Unreadable) > 0 {
		return errors.New("machine-name discovery is incomplete; resolve omitted records before pairing")
	}
	for _, record := range result.Records {
		if record.Name == name {
			return errors.New("machine name is already observed; choose another name")
		}
	}
	return nil
}

func writePairingFile(path, bundle string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = io.WriteString(f, bundle+"\n"); err != nil {
		return err
	}
	return f.Sync()
}

func showPairingCode(p *prompter, code string, env Env) error {
	words, err := pairing.CodeWords(code)
	if err != nil {
		return err
	}
	if !env.interactive(underlyingWriter(p.out)) {
		return errors.New("pairing code display needs terminal output")
	}
	release := p.suspendPrompts()
	defer release()
	// Use checked writes for secret-bearing output. Always restore the screen.
	if _, err := io.WriteString(p.out, "\x1b[?1049h\x1b[2J\x1b[H"); err != nil {
		return err
	}
	signals, stop := env.interrupts()
	screen := &altScreen{out: p.out, active: true, stop: stop, done: make(chan struct{}), exit: env.exit}
	screen.restoreOnLeave(func() { _, _ = io.WriteString(p.out, "\x1b[2J\x1b[H") })
	defer screen.leave()
	go func() {
		select {
		case sig := <-signals:
			if sig != nil {
				screen.exitForSignal(sig)
			}
		case <-screen.done:
		}
	}()
	display := "2. Enter the pairing code on the other machine\n\n" + strings.Join(words, " ") + "\n\nKeep this screen open while entering the code there.\nThe first three characters of each word work (yo- for yo-yo).\nDeliver the code separately from the pairing file. Avoid recording or screen sharing.\nPress Enter to hide.\n"
	resumeErrors := make(chan error, 1)
	if p.lineGuard != nil {
		restoreLifecycle := p.lineGuard.screenLifecycle(screen.hide, func() {
			screen.mu.Lock()
			defer screen.mu.Unlock()
			if !screen.active {
				return
			}
			// Never repaint a secret if returning to the alternate screen failed.
			control := "\x1b[?1049h\x1b[2J\x1b[H"
			n, err := io.WriteString(p.out, control)
			if err == nil && n != len(control) {
				err = io.ErrShortWrite
			}
			if err == nil {
				_, err = io.WriteString(p.out, display)
			}
			if err != nil {
				select {
				case resumeErrors <- err:
				default:
				}
			}
		})
		defer restoreLifecycle()
	}
	screen.mu.Lock()
	if !screen.active {
		screen.mu.Unlock()
		return errors.New("pairing code display interrupted")
	}
	_, err = io.WriteString(p.out, display)
	screen.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = boundedPairingLine(p.in, 512)
	if err == nil {
		select {
		case err = <-resumeErrors:
		default:
		}
	}
	return err
}

func (e Env) pairClipboardWrite(data []byte) error {
	if err := e.clipboard(data); err != nil {
		return errors.New("clipboard unavailable; use --print or --file")
	}
	return nil
}

func (e Env) clearPairClipboard(bundle string) {
	var data []byte
	var err error
	if e.PairingClipboardRead != nil {
		data, err = e.PairingClipboardRead()
	} else if e.Clipboard == nil {
		data, err = e.pairClipboardRead()
	} else {
		return
	}
	if err == nil && string(data) == bundle {
		_ = e.pairClipboardWrite(nil)
	}
}

func (e Env) pairClipboardRead() ([]byte, error) {
	provider, _, err := e.clipboardCommand()
	if err != nil {
		return nil, err
	}
	name := filepath.Base(provider)
	var args []string
	//lint:ignore LV1001 These are names of external clipboard executables selected by clipboardCommand.
	switch name {
	case "pbcopy":
		name = "pbpaste"
	case "wl-copy":
		name = "wl-paste"
		args = []string{"--no-newline", "--type", "text/plain"}
	case "xclip":
		args = []string{"-selection", "clipboard", "-o"}
	case "xsel":
		args = []string{"--clipboard", "--output"}
	default:
		return nil, errors.New("clipboard provider cannot be checked")
	}
	program, err := e.lookPath(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = e.environ()
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var output pairingClipboardOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil || output.over {
		return nil, errors.New("clipboard could not be checked")
	}
	return output.buffer.Bytes(), nil
}

type pairingClipboardOutput struct {
	buffer bytes.Buffer
	over   bool
}

func (o *pairingClipboardOutput) Write(p []byte) (int, error) {
	room := pairing.MaxBundle - o.buffer.Len()
	if len(p) > room {
		o.over = true
	}
	if room > 0 {
		o.buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func pairingInvocationError(args []string, env Env) error {
	if pairingInvocation(args) {
		return pairingAgentRefusal(env)
	}
	return nil
}

func runMachinesWithInput(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	if len(args) > 0 && args[0] == "add" {
		return runPairingAdd(args[1:], stdin, out, errOut, env)
	}
	return runMachinesCommand(args, stdin, out, errOut, env)
}

func preparePairingSource(name string, share bool, env Env) (config.Config, string, string, error) {
	bad := func(message string) (config.Config, string, string, error) {
		return config.Config{}, "", "", errors.New(message)
	}
	home, err := env.readHome()
	if err != nil {
		return bad("cannot locate archive configuration")
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled {
		return bad("run setup on this machine before creating a pairing")
	}

	if cfg.Storage.Provider == credentials.ProviderS3 && share {
		return bad("--share-key is for R2; S3 transfers settings and a profile name only")
	}
	if cfg.MachineName == name {
		return bad("choose a name different from the source machine")
	}
	prior, err := readPairingLedgers(home)
	if err != nil {
		return bad(err.Error())
	}
	for _, claim := range prior {
		if claim.Name == name && claim.State != pairingCancelled && claim.State != pairingExpired && !env.now().After(claim.ExpiresAt) {
			return bad("that name already has an outstanding pairing; choose another name")
		}
	}
	if connect, access := verifyStorage(&cfg, env); connect != nil || access != nil {
		return bad("source storage check failed; no pairing was created")
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return bad("cannot inspect machine names")
	}
	if err = pairingNameAvailable(store, name); err != nil {
		return bad(err.Error())
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return bad("cannot resolve project hints")
	}
	return cfg, home, userHome, nil
}

func pairingSourceStorage(cfg config.Config, env Env) (pairing.Storage, credentials.R2Credentials, error) {
	storage := pairing.Storage{Provider: cfg.Storage.Provider, Bucket: cfg.Storage.Bucket, Prefix: cfg.Storage.Prefix, Region: cfg.Storage.Region, R2Account: cfg.Storage.R2AccountID, R2Endpoint: cfg.Storage.R2Endpoint, AWSProfile: cfg.Storage.AWSProfile}
	if cfg.Storage.Provider != credentials.ProviderR2 {
		return storage, credentials.R2Credentials{}, nil
	}
	kc, err := env.credentialStore()
	if err != nil {
		return storage, credentials.R2Credentials{}, errors.New("cannot read source R2 credential")
	}
	key, err := credentials.LoadStored(context.Background(), kc, cfg.Storage.R2CredentialRef)
	if err != nil {
		return storage, key, errors.New("cannot read source R2 credential")
	}
	return storage, key, nil
}

func pairingAddError(out io.Writer, message string) int { terminal.Println(out, message); return 2 }

func deliverPairingBundle(home, bundle string, ledger *pairingLedger, slot *issuance.Slot, env Env, out, errOut io.Writer, opts pairingAddOptions) int {
	var err error
	savedPath := opts.file
	printed := opts.printBundle || opts.yes
	if slot.SlotID != "" {
		intent := *slot
		intent.State = issuance.DeliveryIntent
		if err = issuance.Save(home, intent); err != nil {
			terminal.Println(errOut, "cannot persist dedicated delivery intent")
			return 1
		}
		*slot = intent
	}
	ledger.State = pairingDeliveryIntent
	if err = savePairingLedger(home, *ledger); err != nil {
		terminal.Println(errOut, "cannot persist delivery intent")
		return 1
	}
	// Any failure after durable intent is uncertain delivery, never cancellation.
	if opts.file != "" {
		err = writePairingFile(opts.file, bundle)
		for err != nil && !opts.yes && opts.prompt != nil {
			terminal.Printf(errOut, "Cannot save pairing file: %v. Choose a new path; existing files are never overwritten.\n", err)
			path, e := opts.prompt.guidedText(promptModel{Question: "New pairing file path (Enter to stop)"})
			if e != nil || path == "" {
				break
			}
			savedPath, err = pairingFilePath(path, opts.userHome)
			if err == nil {
				err = writePairingFile(savedPath, bundle)
			}
		}
	} else if opts.printBundle || opts.yes {
		_, err = fmt.Fprintln(out, bundle)
	} else {
		for {
			err = env.pairClipboardWrite([]byte(bundle))
			if err == nil {
				savedPath = ""
				printed = false
				break
			}
			if opts.prompt == nil {
				break
			}
			terminal.Println(errOut, "Clipboard unavailable. The key remains tracked; choose deliberate delivery or retry.")
			choice, e := opts.prompt.guidedMenu("Bundle delivery", "cancel", option{"retry", "Retry clipboard"}, option{"file", "Write a new private file"}, option{"print", "Print encrypted bundle in this terminal"}, option{"cancel", "Stop; retain tracked key"})
			if e != nil || choice == "cancel" {
				break
			}
			if choice == "retry" {
				continue
			}
			if choice == "print" {
				savedPath = ""
				printed = true
				_, err = fmt.Fprintln(out, bundle)
				break
			}
			path, e := opts.prompt.guidedDefault("New private bundle file", "")
			if e != nil {
				err = e
				break
			}
			savedPath, err = pairingFilePath(path, opts.userHome)
			if err == nil {
				err = writePairingFile(savedPath, bundle)
			}
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		terminal.Println(errOut, "pairing delivery failed or is uncertain; its key remains valid and tracked")
		return 1
	}
	if slot.SlotID != "" {
		slot.State = issuance.Delivered
		if err = issuance.Save(home, *slot); err != nil {
			terminal.Println(errOut, "delivery occurred; dedicated ledger update pending")
			return 1
		}
	}
	ledger.State = pairingDelivered
	ledger.DeliveredAt = env.now().UTC()
	if err = savePairingLedger(home, *ledger); err != nil {
		terminal.Println(errOut, "delivery occurred; ledger update pending (delivery intent retained)")
		return 1
	}
	if !opts.yes {
		if opts.prompt != nil {
			opts.prompt.renderer().block("")
		}
		printPairingTransfer(out, savedPath, printed, *ledger, env.now())
	}

	return 0
}

func printPairingTransfer(out io.Writer, path string, printed bool, ledger pairingLedger, now time.Time) {
	terminal.Println(out, "Pairing · File delivered locally · Receiver completion unconfirmed")
	terminal.Println(out, "1. Transfer the pairing file")
	if path != "" {
		terminal.Printf(out, "Saved to: %s\n", path)
	} else if printed {
		terminal.Println(out, "The encrypted pairing text is printed above. Transfer it to the other machine.")
	} else {
		terminal.Println(out, "Copied to this machine's clipboard. Paste the pairing text into a file or message and transfer it.")
		terminal.Println(out, "The clipboard is cleared on exit if it still contains this text. Clipboard managers may retain it.")
	}
	terminal.Printf(out, "Expires in %.0f minutes.\n", ledger.ExpiresAt.Sub(now).Minutes())
	terminal.Println(out, "Expiry prevents opening this bundle; it does not revoke storage access or delete transferred files.")
	terminal.Println(out, "Deliver the code separately from the file or text.")
	if path != "" {
		terminal.Printf(out, "Send this file to %s (for example using AirDrop). Save it in Downloads there, then run:\n", ledger.Name)
		terminal.Printf(out, "  agent-archive setup --pair-file \"$HOME\"/Downloads/%s\n", shellQuote(filepath.Base(path)))
		terminal.Println(out, "If you save it elsewhere, use that path with --pair-file.")
	} else {
		terminal.Printf(out, "On %s, run:\n  agent-archive setup --pair\n", ledger.Name)
		terminal.Println(out, "Paste the pairing text when asked for a pairing file or text.")
	}
}

func exportHistoricalPairing(cfg config.Config) []pairing.HistoricalProject {
	var out []pairing.HistoricalProject
	for _, project := range cfg.HistoricalProjects {
		if enabled, found := project.HistoricalPermission(cfg.DestinationID()); found {
			out = append(out, pairing.HistoricalProject{ID: project.ID, Label: project.Label, Enabled: enabled})
		}
	}
	return out
}
