package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupresources"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Guided R2 creation: setup makes the bucket and a key that reaches only that
// bucket, from one Cloudflare API token the person pastes. The design, and
// what it leaves unconfirmed, is Part 2a of
// dev/proposals/implemented/portable-handoff-and-onboarding.md.
//
// The pasted "bootstrap" token is the only value here that can do more than
// read and write one bucket. It exists only in local variables of
// createR2Bucket and in the cloudflare.Client made from it; it is never put
// in the setup draft, the journal, the configuration, the Keychain, the
// environment of any program setup starts, a log, or an error. The key that
// is stored is the derived, bucket-scoped one, and it goes through the same
// staging (advanceSetupDraft) as a key pasted by hand.
//
// The flow verifies that key against the new bucket itself, with the bucket
// creation still in reach, so a key that does not work can be revoked at
// once. The bootstrap client then stays alive, held by the prompter, until
// setup has staged the key: if staging fails, setup revokes the new token
// (rollbackGuidedCreation); when it succeeds, the client is discarded
// (finishGuidedCreation). The ordinary storage check then runs on the stored
// key as it does for any R2 key.
//
// Guided creation is available in ordinary setup. Live provider coverage is
// tracked in dev/contributing/testing.md.

// guidedR2Choice is the explicit interactive shortcut for R2 creation.
const guidedR2Choice = "r2-create"

// errChooseStorageAgain ends guided creation without a bucket: the person
// asked for another storage option, or gave nothing to create one with.
var errChooseStorageAgain = errors.New("choose another storage option")

// errNeedAnotherName is what an attempt returns when the bucket name it has
// cannot be used and only the person can pick another: the prompt is asked by
// createUntilVerified, outside the attempt's signal handling, so Ctrl-C at it
// ends setup as it does at any other prompt.
var errNeedAnotherName = errors.New("choose another bucket name")

// errConfirmR2Creation asks the retry loop to confirm creation after the
// permission lookup, outside the request signal handler.
var errConfirmR2Creation = errors.New("confirm R2 creation")

// guidedInterruptedError is what guided creation returns when a signal
// stopped it, so setup exits with the shell's status for the signal.
type guidedInterruptedError struct{ sig os.Signal }

func (e *guidedInterruptedError) Error() string { return "guided bucket creation was interrupted" }

// r2VerifyAttempts and r2VerifyPause bound how long setup waits for
// Cloudflare to start honoring a key it just created. How long that takes is
// not documented (see "Live acceptance" in dev/contributing/testing.md).
const (
	r2VerifyAttempts = 5
	r2VerifyPause    = 3 * time.Second
)

// r2Creator is one run of guided creation.
type r2Creator struct {
	releaseIssued func()
	keyStorage    *credentials.Config
	slot          *issuance.Slot
	home          string
	issuerID      string
	p             *prompter
	env           Env
	api           cloudflare.API
	account       string
	bucket        cloudflare.BucketSpec
	// tokenFromEnv is whether the bootstrap token came from
	// CLOUDFLARE_API_TOKEN rather than the prompt.
	tokenFromEnv bool
	// tokenRemovedFromEnv is whether setup then removed the variable from its
	// own environment, so programs it starts do not inherit the token.
	tokenRemovedFromEnv bool
	// defaultName allows one replacement of the initially generated name
	// if it collides; replacements still require confirmation.
	defaultName       bool
	bucketCreated     bool
	creationConfirmed bool
	// maybeCreated holds the names of buckets whose creation got no clear
	// answer, so Cloudflare may have made them.
	maybeCreated map[string]bool
	groupID      string
	// token and tokenName are the key's token, once one passed its check.
	token     cloudflare.Token
	tokenName string
	privacy   storage.PrivacyReport
	// revoking is set while a revoke request is out, so a signal that
	// arrives then is answered instead of ignored. printMu keeps that answer
	// from interleaving with the revoke's own output.
	revoking atomic.Bool
	printMu  sync.Mutex
}

// r2Handoff is a finished creation waiting for setup to stage its key. The
// bootstrap client stays alive in it, so a failure to stage can revoke the
// token; setup ends it with finishGuidedCreation or rollbackGuidedCreation.
type r2Handoff struct{ c *r2Creator }

// r2Created is what guided creation left in the person's Cloudflare account,
// for the storage failure text: names only, nothing secret.
type r2Created struct {
	bucket    string
	tokenName string
	storage   credentials.Config
	privacy   storage.PrivacyReport
	// reported is set once setup has said they are still there, so the note
	// at the end of setup does not say it again.
	reported bool
}

// createR2Bucket runs guided creation and returns the storage settings and
// the derived key for setup to stage and check, as promptStorage does for a
// key typed in. On success the prompter holds the bootstrap client until
// setup calls finishGuidedCreation or rollbackGuidedCreation.
func createR2Bucket(p *prompter, env Env) (credentials.Config, credentials.R2Credentials, bool, error) {
	p.storageStage(storageAccount)
	var none credentials.R2Credentials
	token, fromEnv, removed, err := askBootstrapToken(p, env)
	if err != nil {
		return credentials.Config{}, none, false, err
	}
	c := &r2Creator{p: p, env: env, api: env.cloudflareAPI(token), tokenFromEnv: fromEnv, tokenRemovedFromEnv: removed, maybeCreated: map[string]bool{}}
	kept := false
	defer func() {
		if !kept {
			c.releaseIssuance()
			c.api.Discard()
		}
	}()
	if err = c.connect(); err != nil {
		return credentials.Config{}, none, false, err
	}
	c.askBucket()
	key, err := c.createUntilVerified()
	if err != nil {
		if errors.Is(err, errUseExistingStorage) && c.bucketCreated {
			return c.storageConfig(), none, false, err
		}
		return credentials.Config{}, none, false, err
	}
	// From here to staging, in setup.go, no signal handler is installed: a
	// Ctrl-C then ends setup with the key's token still live, and its name was
	// printed when it was created (docs/security/privacy.md says so).
	if err = c.confirmPublicAccess(); err != nil {
		return credentials.Config{}, none, false, err
	}
	kept = true
	p.guided = &r2Handoff{c: c}
	return c.storageConfig(), key, true, nil
}

// r2ConnectChoice is the recovery action after a bootstrap permission failure.
type r2ConnectChoice string

const (
	r2ConnectToken    r2ConnectChoice = "token"
	r2ConnectRetry    r2ConnectChoice = "retry"
	r2ConnectOther    r2ConnectChoice = "other"
	r2ConnectStop     r2ConnectChoice = "stop"
	r2ConnectExisting r2ConnectChoice = "existing"
)

// connect checks the account and archive-key permission before asking for
// bucket settings. Recovery prompts run outside the request signal handler.
func (c *r2Creator) connect() error {
	for {
		var err error
		if c.account == "" {
			err = c.chooseAccount()
		}
		if err == nil {
			_, err = c.attempt()
			if errors.Is(err, errConfirmR2Creation) {
				terminal.Println(c.p.out, c.p.style.okMark()+" Archive-key permission lookup succeeded.")
				return nil
			}
		}
		var interrupted *guidedInterruptedError
		if errors.As(err, &interrupted) {
			return err
		}
		if !errors.Is(err, errChooseStorageAgain) && err != nil && c.account == "" {
			return err
		}
		choice, err := c.p.setupActions("What next?", string(r2ConnectToken),
			[]option{{string(r2ConnectToken), "Paste a different token"}, {string(r2ConnectRetry), "Retry after updating permissions"}},
			[]actionOption{{string(r2ConnectExisting), "e", "Use an existing bucket"}, {string(r2ConnectOther), "b", "Back"}, {string(r2ConnectStop), "q", "Stop setup"}})
		if err != nil {
			return err
		}
		switch r2ConnectChoice(choice) {
		case r2ConnectToken:
			token, err := c.p.guidedText(promptModel{Question: "Cloudflare API token (hidden; Enter to go back)", Label: "Credential", Secret: true, Validate: func(value string) error {
				if value == "" {
					return nil
				}
				return validateManagementToken(value)
			}})
			if err != nil {
				return err
			}
			if token == "" {
				return errChooseStorageAgain
			}
			if err := validateManagementToken(token); err != nil {
				return err
			}
			c.api.Discard()
			c.api = c.env.cloudflareAPI(token)
			c.account, c.groupID = "", ""
			c.tokenFromEnv = false
		case r2ConnectRetry:
			continue
		case r2ConnectOther:
			return errChooseStorageAgain
		case r2ConnectExisting:
			return errUseExistingStorage
		case r2ConnectStop:
			return errors.New("guided bucket creation stopped")
		default:
			return errors.New("guided bucket creation stopped")
		}
	}
}

// releaseIssuance ends the guided creation/staging critical section.
func (c *r2Creator) releaseIssuance() {
	if c.releaseIssued != nil {
		c.releaseIssued()
		c.releaseIssued = nil
	}
}

// finishGuidedCreation ends guided creation once setup has staged the key:
// it discards the bootstrap client, and says so.
func (p *prompter) finishGuidedCreation(draft *setupDraft, save func() error) {
	h := p.guided
	if h == nil {
		return
	}
	if h.c.slot != nil {
		issuer := keyIssuer{home: h.c.home, cfg: draft.Config, env: h.c.env, p: p, api: h.c.api, group: h.c.groupID, account: h.c.account, bucket: h.c.bucket.BucketRef}
		if h.c.releaseIssued != nil {
			if e := issuer.refill(); e != nil {
				terminal.Println(p.out, "Setup key remains valid; spare refill failed or is pending.")
			}
			refs, e := spareRefs(h.c.home, draft.Config)
			if e == nil {
				draft.Config.SpareCredentialRefs = refs
			}
			if e = save(); e != nil {
				terminal.Println(p.out, "Spare index draft update pending; ledger remains authoritative.")
			}
		} else {
			terminal.Println(p.out, "Spare refill deferred: issuance operation is running.")
		}
	}
	h.c.releaseIssuance()
	p.guided = nil
	h.c.api.Discard()
	p.created = append(p.created, &r2Created{bucket: h.c.bucket.Name, tokenName: h.c.tokenName, storage: h.c.storageConfig(), privacy: h.c.privacy})
	terminal.Println(p.out, p.style.okMark()+" Archive key saved.")
	if h.c.tokenFromEnv {
		if h.c.tokenRemovedFromEnv {
			terminal.Println(p.out, "Setup did not save the Cloudflare API token from CLOUDFLARE_API_TOKEN, and has dropped it; it also removed the variable from setup's own environment. Your shell still has it.")
			return
		}
		terminal.Println(p.out, "Setup did not save the Cloudflare API token from CLOUDFLARE_API_TOKEN, and has dropped it.")
		return
	}
	terminal.Println(p.out, "The Cloudflare API token you pasted is not saved anywhere. You can delete it in the dashboard; the archive doesn't need it again.")
}

// guidedR2Privacy returns the last management-API check for the bucket setup
// just created. The ordinary storage check uses the bucket's object key,
// which cannot read these settings, so it cannot replace this evidence.
func (p *prompter) guidedR2Privacy(cfg config.Config) *storage.PrivacyReport {
	destination := cfg.Storage
	destination.R2CredentialRef = ""
	for i := len(p.created) - 1; i >= 0; i-- {
		made := p.created[i]
		if made.storage != destination || made.privacy.CheckedAt == nil {
			continue
		}
		report := made.privacy
		report.ConfigurationID = privacyConfigurationID(cfg)
		return &report
	}
	if report := cfg.BucketPrivacy; report != nil && report.ConfigurationID == privacyConfigurationID(cfg) && report.CheckedAt != nil {
		//lint:ignore LV1001 storage.PrivacyReport.Reason is an untyped string owned by package storage
		switch report.Reason {
		case "r2_public_domains_disabled", "r2_public_access_enabled", "r2_public_access_not_fully_checked":
			if age, ok := privacyEvidenceAge(cfg, p.clock()); ok && age <= bucketPrivacyStaleAfter {
				savedReport := *report
				return &savedReport
			}
		}
	}
	return nil
}

// rollbackGuidedCreation is for a failure to stage the new key: the key was
// never stored, so it revokes the key's token, tells the person what is left,
// and discards the bootstrap client. It returns err.
func (p *prompter) rollbackGuidedCreation(err error) error {
	h := p.guided
	if h == nil {
		return err
	}
	p.guided = nil
	terminal.Println(p.out, "Setup couldn't store the new key, so it is revoking the key's token.")
	h.c.revoke(context.Background(), h.c.token, h.c.tokenName)
	h.c.reportBucketLeftBehind()
	h.c.releaseIssuance()
	h.c.api.Discard()
	return err
}

// printGuidedLeftovers is for a storage check that failed on a key guided
// creation made: the bucket and the key's token are still in the person's
// Cloudflare account, and the bootstrap token that could revoke the key is
// gone.
func printGuidedLeftovers(p *prompter, cfg credentials.Config) {
	var c *r2Created
	for _, made := range p.created {
		if made.bucket == cfg.Bucket {
			c = made
		}
	}
	if c == nil {
		return
	}
	c.reported = true
	terminal.Println(p.out, "Setup created the bucket "+c.bucket+" and an API token named \""+c.tokenName+"\" for it; both are still in your Cloudflare account.")
	terminal.Println(p.out, "If you stop or use other storage, delete the bucket and revoke that token in the dashboard (Manage account > Account API tokens).")
	terminal.Println(p.out, "")
}

// noteUnusedCreatedR2 says, once setup is over, what guided creation left in
// the person's Cloudflare account when the saved configuration does not use
// it, however setup ended (the storage check was interrupted, the review was
// cancelled, an error). Nothing is said when the bucket is in use, or when a
// failed storage check already said it. A saved setup draft that uses the
// bucket is named, since running setup again resumes with it.
func noteUnusedCreatedR2(p *prompter, home string) {
	for _, c := range p.created {
		if c.reported {
			continue
		}
		committed, drafted := savedStorageUses(home, credentials.ProviderR2, c.bucket)
		if committed {
			continue
		}
		c.reported = true
		what := "Setup created the bucket " + c.bucket + " (it is empty) and an API token named \"" + c.tokenName + "\" for it."
		if drafted {
			terminal.Println(p.out, what+" Your saved setup draft uses them, so running setup again will resume with them. To not use them, choose other storage there, then delete the bucket and revoke the token in the Cloudflare dashboard (Manage account > Account API tokens).")
			continue
		}
		terminal.Println(p.out, what+" Neither is used by your saved setup. To not keep them, delete the bucket and revoke the token in the Cloudflare dashboard (Manage account > Account API tokens).")
	}
}

// storageConfig is what a pasted account ID or bucket URL would have made:
// the account ID with its endpoint, or, in a jurisdiction, only the
// jurisdiction's endpoint, as credentials.ParseR2Location reads it.
func (c *r2Creator) storageConfig() credentials.Config {
	endpoint, _ := credentials.R2Endpoint(cloudflare.Endpoint(c.account, c.bucket.Jurisdiction), "")
	accountID := c.account
	if c.bucket.Jurisdiction != "" {
		accountID = ""
	}
	return credentials.Config{Provider: credentials.ProviderR2, Bucket: c.bucket.Name, R2Endpoint: endpoint, R2AccountID: accountID, Prefix: defaultPrefix}
}

func r2BootstrapInstructions() []string {
	return []string{
		"Setup can create a new Cloudflare R2 bucket (Cloudflare buckets have no public access by default), and a key that reaches only that bucket.",
		"Setup needs a Cloudflare API token to create the bucket and key, and won't save it.",
		"Get your token: " + cloudflare.TokenDashboardURL,
		"1. Sign in, select your account, and open Manage account > Account API tokens.",
		"   If you see Create Account API token and Object Read & Write, return to Manage account: that is the R2-specific form.",
		"2. Choose Create Token and use the custom token form. Name it agent-archive setup.",
		"3. Add these two permission rows (choose Edit for each):",
		"   Account > Workers R2 Storage > Edit",
		"   Account > Account API Tokens > Edit",
		"4. Limit access to this account only, review the summary, and create the token.",
		"5. Copy the API token value and paste it below (not the S3 Access Key ID or Secret Access Key).",
		"If these permissions aren't available, ask an account administrator or choose existing R2 storage and follow the manual steps: " + bucketDocURL,
	}
}

// askBootstrapToken reads the bootstrap token from CLOUDFLARE_API_TOKEN, as
// Cloudflare's own tools do, or asks for it without echoing it, and says
// which. A token from the environment is then removed from setup's own
// environment (removed says so), so a program setup starts, such as launchctl
// or a profile's credential_process, does not inherit it. An empty answer goes
// back to the storage menu.
func askBootstrapToken(p *prompter, env Env) (token string, fromEnv, removed bool, err error) {
	oldHelpers := p.tokenHelpers
	p.tokenHelpers = r2BootstrapInstructions()
	defer func() { p.tokenHelpers = oldHelpers }()
	token, fromEnv, removed, err = readManagementToken(context.Background(), p, env, p.tokenCommand, len(p.tokenCommand) == 0 || env.interactive(p.source))
	if fromEnv && err == nil {
		terminal.Println(p.out, "Using the API token in CLOUDFLARE_API_TOKEN.")
	}
	return token, fromEnv, removed, err
}

// chooseAccount settles which Cloudflare account owns the bucket: the one
// in CLOUDFLARE_ACCOUNT_ID, else the token's only account, else the one the
// person names. Whether the token may list accounts is not documented, so a
// refusal is a reason to ask, not a failure; only a token Cloudflare does
// not accept ends the flow.
func (c *r2Creator) chooseAccount() error {
	if value, ok := c.env.lookupEnv("CLOUDFLARE_ACCOUNT_ID"); ok && strings.TrimSpace(value) != "" {
		if account, err := parseR2AccountID(value); err == nil {
			terminal.Println(c.p.out, "Using the account ID in CLOUDFLARE_ACCOUNT_ID.")
			c.account = account
			return nil
		}
		c.p.warn("CLOUDFLARE_ACCOUNT_ID isn't a Cloudflare account ID (32 characters, 0-9 and a-f); ignoring it.")
	}
	accounts, err := c.api.Accounts(context.Background())
	var apiErr *cloudflare.Error
	switch {
	case errors.As(err, &apiErr) && apiErr.Unauthorized():
		terminal.Println(c.p.out, c.p.style.failMark()+" "+explainCloudflare(err, ""))
		return errChooseStorageAgain
	case err != nil:
		terminal.Println(c.p.out, "Couldn't list your Cloudflare accounts, so setup needs the account ID. "+explainCloudflare(err, "The token isn't allowed to list accounts."))
	case len(accounts) == 1:
		if account, e := parseR2AccountID(accounts[0].ID); e == nil {
			terminal.Printf(c.p.out, "Using Cloudflare account: %s (%s)\n", printableText(accounts[0].Name), account)
			c.account = account
			return nil
		}
	case len(accounts) > 1:
		terminal.Println(c.p.out, "The token can see more than one Cloudflare account:")
		for _, a := range accounts {
			terminal.Printf(c.p.out, "  %s  %s\n", printableText(a.ID), printableText(a.Name))
		}
	}
	terminal.Println(c.p.out, "Find the Account ID in the Cloudflare dashboard > Storage & databases > R2 > Overview.")
	answer, err := c.p.guidedText(promptModel{Question: "Cloudflare account ID", Label: "Answer", Validate: func(value string) error {
		_, err := parseR2AccountID(value)
		if err != nil {
			return fmt.Errorf("that isn't a Cloudflare account ID (%w)", err)
		}
		return nil
	}, ResolveReceipt: func(value string) string {
		account, _ := parseR2AccountID(value)
		return "Cloudflare account ID " + account
	}})
	if err != nil {
		return err
	}
	c.account, err = parseR2AccountID(answer)
	return err
}

// parseR2AccountID reads a 32-character account ID.
func parseR2AccountID(input string) (string, error) {
	loc, err := credentials.ParseR2Location(input)
	if err != nil {
		return "", err
	}
	if loc.AccountID == "" || loc.Bucket != "" {
		return "", errors.New("an R2 account ID is 32 characters, 0-9 and a-f")
	}
	return loc.AccountID, nil
}

// askBucket prepares a suggested name for the creation summary.
func (c *r2Creator) askBucket() {
	c.bucket.Name, c.defaultName = newBucketName(), true
}

func (c *r2Creator) askBucketName(label, def string) (string, error) {
	name, err := c.p.guidedText(promptModel{Question: label, Label: "Name", Default: def, ResolveReceipt: func(name string) string { return "Bucket " + strings.ToLower(name) }, Validate: func(name string) error { return cloudflare.ValidateBucketName(strings.ToLower(name)) }})
	return strings.ToLower(name), err
}

func (c *r2Creator) askLocation() error {
	p := c.p
	change, err := p.setupYesNo("Customize storage location? Leave this off for automatic placement.", false)
	if err != nil {
		return err
	}
	if !change {
		c.bucket.Jurisdiction, c.bucket.LocationHint = "", ""
		return nil
	}
	jurisdictionLabel := "Jurisdiction (" + strings.Join(cloudflare.Jurisdictions, ", ") + "; Enter for none)"
	jurisdiction, err := p.guidedText(promptModel{Question: jurisdictionLabel, Label: "Answer", Validate: func(value string) error {
		if value != "" && !cloudflare.ValidJurisdiction(strings.ToLower(value)) {
			return fmt.Errorf("choose one of %s, or press Enter for none", strings.Join(cloudflare.Jurisdictions, ", "))
		}
		return nil
	}, ResolveReceipt: func(value string) string {
		return "Jurisdiction " + firstNonEmpty(strings.ToLower(value), "none")
	}})
	if err != nil {
		return err
	}
	c.bucket.Jurisdiction = strings.ToLower(jurisdiction)
	if c.bucket.Jurisdiction != "" {
		terminal.Println(p.out, "A jurisdiction can't be changed later, and the bucket is reached only at "+cloudflare.Endpoint(c.account, c.bucket.Jurisdiction)+".")
	}
	locationLabel := "Location hint (" + strings.Join(cloudflare.LocationHints, ", ") + "; Enter for automatic)"
	location, err := p.guidedText(promptModel{Question: locationLabel, Label: "Answer", Validate: func(value string) error {
		if value != "" && !cloudflare.ValidLocationHint(strings.ToLower(value)) {
			return fmt.Errorf("choose one of %s, or press Enter for automatic", strings.Join(cloudflare.LocationHints, ", "))
		}
		return nil
	}, ResolveReceipt: func(value string) string {
		return "Location hint " + firstNonEmpty(strings.ToLower(value), "automatic")
	}})
	if err != nil {
		return err
	}
	c.bucket.LocationHint = strings.ToLower(location)
	return nil
}

// createUntilVerified creates the bucket, mints the key, and checks it, and
// after a failure offers to try again (reusing the bucket), to pick another
// storage option, or to stop. A key that failed its check is revoked before
// any of those. An interrupt ends it at once.
func (c *r2Creator) createUntilVerified() (credentials.R2Credentials, error) {
	for {
		if c.creationConfirmed {
			c.p.storageStage(storageDetails)
		}
		key, err := c.attempt()
		if err == nil {
			return key, nil
		}
		if errors.Is(err, errChooseStorageAgain) {
			return credentials.R2Credentials{}, err
		}
		if errors.Is(err, errConfirmR2Creation) {
			c.p.storageStage(storageDetails)
			terminal.Println(c.p.out, "Account: "+c.account)
			terminal.Println(c.p.out, "Bucket: "+c.bucket.Name)
			location := "automatic"
			if c.bucket.Jurisdiction != "" {
				location = c.bucket.Jurisdiction
			}
			if c.bucket.LocationHint != "" {
				location += " (hint: " + c.bucket.LocationHint + ")"
			}
			terminal.Println(c.p.out, "Location: "+location)
			terminal.Println(c.p.out, "Setup will leave public access off and create a key for this bucket only.")
			choice, e := c.p.setupActions("Your archive storage", "create", nil, []actionOption{{"create", "", "Create"}, {"customize", "c", "Customize name or location"}, {"existing", "e", "Use an existing bucket"}, {"back", "b", "Back"}})
			if e != nil {
				return credentials.R2Credentials{}, e
			}
			if choice == "back" {
				return credentials.R2Credentials{}, errChooseStorageAgain
			}
			if choice == "existing" {
				return credentials.R2Credentials{}, errUseExistingStorage
			}
			if choice == "customize" {
				name, e := c.askBucketName("Bucket name", c.bucket.Name)
				if e != nil {
					return credentials.R2Credentials{}, e
				}
				c.bucket.Name, c.defaultName = name, false
				if e := c.askLocation(); e != nil {
					return credentials.R2Credentials{}, e
				}
				continue
			}
			c.creationConfirmed = true
			continue
		}
		var interrupted *guidedInterruptedError
		if errors.As(err, &interrupted) {
			c.reportBucketLeftBehind()
			return credentials.R2Credentials{}, err
		}
		if errors.Is(err, errNeedAnotherName) {
			name, e := c.askBucketName("Another bucket name", "")
			if e != nil {
				return credentials.R2Credentials{}, e
			}
			c.bucket.Name, c.defaultName = name, false
			c.creationConfirmed = false
			continue
		}
		retry := "Try again"
		if c.bucketCreated {
			retry = "Try again with the same bucket (" + c.bucket.Name + ")"
		}
		choice, e := c.p.setupActions("What next?", "retry", []option{{"retry", retry}}, []actionOption{{"existing", "e", "Use an existing bucket"}, {"other", "b", "Back"}, {"stop", "q", "Stop setup"}})
		if e != nil {
			return credentials.R2Credentials{}, e
		}
		if choice == "retry" {
			continue
		}
		c.reportBucketLeftBehind()
		if choice == "other" {
			return credentials.R2Credentials{}, errChooseStorageAgain
		}
		if choice == "existing" {
			return credentials.R2Credentials{}, errUseExistingStorage
		}
		return credentials.R2Credentials{}, errors.New("guided bucket creation stopped")
	}
}

// reportBucketLeftBehind says so when a bucket was created and will not be
// used: it is empty, and it stays in the person's Cloudflare account.
func (c *r2Creator) reportBucketLeftBehind() {
	if c.bucketCreated {
		terminal.Printf(c.p.out, "The bucket %s was created and is empty. It stays in your Cloudflare account; delete it in the dashboard (R2 > %s > Settings) if you don't want it.\n", c.bucket.Name, c.bucket.Name)
	}
}

// attempt is one pass through creating the bucket (once), the key, and its
// check. It prints why it failed and returns the failure. A signal during it
// cancels its requests and waits; the key's token, if one was made, is
// revoked, and the error says the flow was interrupted.
func (c *r2Creator) attempt() (credentials.R2Credentials, error) {
	interrupts, stop := c.env.interrupts()
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	caught := make(chan os.Signal, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		first := true
		for {
			select {
			case sig := <-interrupts:
				c.stillRevoking()
				if first {
					first = false
					caught <- sig
					cancel()
				}
			case <-done:
				return
			}
		}
	}()
	key, err := c.attemptWith(ctx)
	if err != nil && ctx.Err() != nil {
		select {
		case sig := <-caught:
			terminal.Println(c.p.out, c.p.style.failMark()+" Stopped.")
			return credentials.R2Credentials{}, &guidedInterruptedError{sig: sig}
		default:
		}
	}
	return key, err
}

func (c *r2Creator) attemptWith(ctx context.Context) (key credentials.R2Credentials, err error) {
	defer func() {
		if err != nil {
			c.releaseIssuance()
		}
	}()
	if c.slot != nil && c.slot.State != issuance.Deleted {
		release, e := local.NamedLock(c.home, "issued.lock")
		if e != nil {
			return key, e
		}
		c.releaseIssued = release
		cfg := config.Config{MachineID: c.slot.IssuerID, Storage: c.storageConfig()}
		issuer := keyIssuer{home: c.home, cfg: cfg, env: c.env, p: c.p, api: c.api, account: c.account, bucket: c.bucket.BucketRef}
		issuer.cleanupWithContext(ctx, c.slot)
		c.releaseIssuance()
		if c.slot.State != issuance.Deleted {
			return key, errors.New("prior key creation remains unresolved; cleanup must be confirmed before retrying")
		}
	}
	if c.groupID == "" {
		terminal.Println(c.p.out, "Checking archive-key permissions with Cloudflare...")
		id, err := c.lookUpPermissionGroup(ctx)
		if err != nil {
			if !c.bucketCreated {
				terminal.Println(c.p.out, "No bucket or key has been created.")
			}
			return credentials.R2Credentials{}, err
		}
		c.groupID = id
	}
	if !c.creationConfirmed {
		return credentials.R2Credentials{}, errConfirmR2Creation
	}
	if !c.bucketCreated {
		if err := c.createBucket(ctx); err != nil {
			return credentials.R2Credentials{}, err
		}
	}
	token, name, key, err := c.mintKey(ctx)
	if err != nil {
		return credentials.R2Credentials{}, err
	}
	if err = c.checkKey(ctx, key); err != nil {
		c.revoke(ctx, token, name)
		return credentials.R2Credentials{}, err
	}
	c.token, c.tokenName = token, name
	return key, nil
}

// stillRevoking answers a signal that arrives while a revoke request is out:
// stopping now would leave the token, so setup finishes the revoke first.
func (c *r2Creator) stillRevoking() {
	c.printMu.Lock()
	defer c.printMu.Unlock()
	if c.revoking.Load() {
		terminal.Println(c.p.out, "Still revoking the key's token; please wait.")
	}
}

// answerLost reports whether err leaves it unknown whether Cloudflare did
// what was asked: no answer, an answer that could not be read, or a failure
// on Cloudflare's side. A clear refusal (a 4xx answer) did nothing.
func answerLost(err error) bool {
	var apiErr *cloudflare.Error
	return !errors.As(err, &apiErr) || apiErr.Err != nil || apiErr.ServerError()
}

// createBucket makes the bucket. A name that is taken is replaced once if
// setup chose it, and otherwise asked for again. A name whose earlier
// creation got no answer is never replaced silently: Cloudflare may have
// made that bucket.
func (c *r2Creator) createBucket(ctx context.Context) error {
	p := c.p
	home, err := c.env.readHome()
	if err != nil {
		return err
	}
	receipt, err := setupresources.Begin(home, setupresources.Receipt{Provider: setupresources.R2, Account: c.account, Bucket: c.bucket.Name, Jurisdiction: c.bucket.Jurisdiction, CreatedAt: c.env.now()})
	if err != nil {
		return err
	}
	err = c.api.CreateBucket(ctx, c.account, c.bucket)
	if err == nil {
		if receiptErr := setupresources.Finish(home, receipt, setupresources.Created); receiptErr != nil {
			return receiptErr
		}
	} else if !answerLost(err) {
		if receiptErr := setupresources.Finish(home, receipt, setupresources.Refused); receiptErr != nil {
			return receiptErr
		}
	}
	if err == nil {
		c.bucketCreated = true
		terminal.Println(p.out, p.style.okMark()+" Created bucket "+c.bucket.Name+".")
		return nil
	}
	var apiErr *cloudflare.Error
	if !errors.As(err, &apiErr) || !apiErr.AlreadyExists() {
		terminal.Println(p.out, p.style.failMark()+" Couldn't create the bucket. "+explainCloudflare(err, "The token needs the "+cloudflare.PermissionR2Write+" permission to create buckets; add it to the token in the dashboard. If R2 isn't enabled on the account yet, enable it in the dashboard first (Cloudflare may ask for a payment method)."))
		if answerLost(err) {
			c.maybeCreated[c.bucket.Name] = true
			terminal.Println(p.out, "Cloudflare may have made the bucket "+c.bucket.Name+" before the answer was lost.")
		}
		return err
	}
	switch {
	case c.maybeCreated[c.bucket.Name]:
		terminal.Printf(p.out, "A bucket named %s may have been created by the earlier request, which got no answer. Setup can't tell it from a bucket that was already yours, so it won't use it: delete it in the dashboard if it is empty and new, or choose another name.\n", c.bucket.Name)
	case c.defaultName:
		name := newBucketName()
		terminal.Printf(p.out, "The name %s is taken; preparing %s.\n", c.bucket.Name, name)
		c.bucket.Name = name
		c.defaultName, c.creationConfirmed = false, false
		return errConfirmR2Creation
	default:
		terminal.Printf(p.out, "The name %s is taken in your Cloudflare account.\n", c.bucket.Name)
	}
	return errNeedAnotherName
}

// lookUpPermissionGroup finds the ID of the bucket-item-write group. It is
// asked for every time and never written down: Cloudflare documents the ID,
// not the name, as the stable key.
func (c *r2Creator) lookUpPermissionGroup(ctx context.Context) (string, error) {
	p := c.p
	groups, err := c.api.PermissionGroups(ctx, c.account, cloudflare.PermissionBucketItemWrite)
	if err == nil {
		var id string
		if id, err = cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite); err == nil {
			return id, nil
		}
		terminal.Println(p.out, p.style.failMark()+" Couldn't find the permission for the bucket's key: "+err.Error()+".")
		terminal.Println(p.out, "The token needs the "+cloudflare.PermissionTokensWrite+" permission, and its owner must be allowed to grant "+cloudflare.PermissionBucketItemWrite+".")
		return "", err
	}
	terminal.Println(p.out, p.style.failMark()+" Couldn't look up the permission for the bucket's key. "+explainCloudflare(err, tokenWriteHint))
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) && (apiErr.Unauthorized() || apiErr.Forbidden()) {
		terminal.Println(p.out, "This checks access to create a separate archive key, not permission to create the bucket. In Manage account > Account API tokens, add Account > Account API Tokens > Edit to this setup token before retrying.")
	}
	return "", err
}

// tokenWriteHint is what a refused token step needs. Cloudflare lets a member
// grant only permissions the member has, so a token made by a member who is
// not an administrator can be refused here even with the permission.
const tokenWriteHint = "The token needs the " + cloudflare.PermissionTokensWrite + " permission. Account members can grant a token only a subset of their own permissions, so an account administrator may need to create it."

// mintKey creates the bucket-scoped token and derives its S3 key. The
// token's name is printed first: a crash between its creation and the key
// being saved loses the token's value, which Cloudflare shows only once, and
// the name is how the person recognizes what to revoke. Whenever the answer
// leaves it unknown whether the token exists, the name is printed again with
// what to do.
func (c *r2Creator) mintKey(ctx context.Context) (token cloudflare.Token, name string, key credentials.R2Credentials, err error) {
	p := c.p
	defer func() {
		if err != nil {
			c.releaseIssuance()
		}
	}()
	// The default bucket name's random part tells this Mac's tokens apart.
	home, e := c.env.readHome()
	if e != nil {
		return token, name, key, e
	}
	c.home = home
	cfg, _, e := config.Load(home)
	if e != nil {
		return token, name, key, e
	}
	issuer := c.issuerID
	if issuer == "" {
		issuer = cfg.MachineID
	}
	if issuer == "" {
		issuer, e = local.ID()
		if e != nil {
			return token, name, key, e
		}
	}
	c.issuerID = issuer
	cfg.Storage = c.storageConfig()
	cfg.MachineID = issuer
	slot, e := issuance.New(issuer, cfg.DestinationID(), c.account, c.bucket.BucketRef, c.groupID, issuance.Guided, c.env.now())
	if e != nil {
		return token, name, key, e
	}
	release, e := local.NamedLock(home, "issued.lock")
	if e != nil {
		return token, name, key, e
	}
	c.releaseIssued = release
	if e = issuance.Save(home, slot); e != nil {
		return token, name, key, e
	}
	c.slot = &slot
	name = slot.ProviderName
	resource, err := cloudflare.BucketResource(c.account, c.bucket.BucketRef)
	if err != nil {
		return token, name, key, err
	}
	terminal.Println(p.out, "Creating the bucket's key, an API token named \""+name+"\".")
	terminal.Println(p.out, "If setup stops before it finishes, revoke that token in the dashboard (Manage account > Account API tokens).")
	spec := cloudflare.TokenSpec{
		Name:     name,
		Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{c.groupID}, Resources: map[string]string{resource: "*"}}},
	}
	token, err = c.api.CreateToken(ctx, c.account, spec)
	slot.ProviderID = token.ID
	if err != nil {
		slot.State = issuance.CleanupPending
		slot.CleanupReason = "guided-creation-response-uncertain"
		if !answerLost(err) {
			slot.State = issuance.Deleted
			slot.CleanupReason = "creation-refused"
		}
		if saveErr := issuance.Save(home, slot); saveErr != nil {
			// Preserve the failed durability result while still revoking any token
			// whose identifier came back without a usable secret.
			err = errors.Join(err, saveErr)
		}
	} else {
		slot.State = issuance.SecretIntent
		if e = issuance.Save(home, slot); e != nil {
			c.revoke(ctx, token, name)
			return token, name, key, e
		}
	}
	c.slot = &slot
	if err != nil {
		terminal.Println(p.out, p.style.failMark()+" Couldn't create the key. "+explainCloudflare(err, tokenWriteHint))
		if answerLost(err) {
			terminal.Println(p.out, "If Cloudflare did create it, revoke the token named \""+name+"\" in the dashboard.")
		}
		if token.ID != "" {
			// The token exists but its value didn't come back.
			c.revoke(ctx, token, name)
		}
		return token, name, key, err
	}
	derived := cloudflare.DeriveS3Credentials(token)
	key = credentials.R2Credentials{AccessKeyID: derived.AccessKeyID, SecretAccessKey: derived.SecretAccessKey}
	terminal.Println(p.out, p.style.okMark()+" Created a key for "+c.bucket.Name+" only.")
	return token, name, key, nil
}

// wait pauses for d, ending early when ctx does.
func (c *r2Creator) wait(ctx context.Context, d time.Duration) {
	if c.env.Pause != nil {
		c.env.Pause(d)
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// checkKey runs setup's storage check (a listing, then the write, read,
// list, delete round trip) on the new key, waiting a little while Cloudflare
// starts honoring it, and prints why it failed. A canceled ctx ends it
// without a diagnosis.
func (c *r2Creator) checkKey(ctx context.Context, key credentials.R2Credentials) error {
	p := c.p
	p.storageStage(storageVerification)
	cfg := c.storageConfig()
	if c.keyStorage != nil {
		cfg = *c.keyStorage
	}
	var err error
	for attempt := 1; attempt <= r2VerifyAttempts; attempt++ {
		if err = c.env.verifyR2Key(ctx, cfg, key); err == nil {
			terminal.Println(p.out, p.style.okMark()+" The new key can read and write "+c.bucket.Name+".")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause := storage.Diagnose(err).Cause; cause != storage.CauseNoCredentials && cause != storage.CauseAccessDenied {
			break
		}
		if attempt == 1 {
			terminal.Println(p.out, "Waiting for the key to activate (up to ~15s)…")
		}
		if attempt < r2VerifyAttempts {
			c.wait(ctx, r2VerifyPause)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	d := storageDiagnosis(cfg, err)
	terminal.Println(p.out, p.style.failMark()+" The new key didn't pass the storage check.")
	terminal.Println(p.out, "  "+storageFailureHeadline(cfg, d))
	terminal.Println(p.out, "  "+d.Explanation)
	return err
}

// revoke deletes a token whose key will not be used, so no working
// credential is left behind, in a request of its own (a canceled setup
// still revokes). A 404 is reported as Cloudflare saying the token does not
// exist, with what to check, because what Cloudflare means by it is
// unconfirmed. If Cloudflare refuses, the person is told which token to
// remove.
func (c *r2Creator) revoke(ctx context.Context, token cloudflare.Token, name string) {
	p := c.p
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	terminal.Println(p.out, "Revoking the key that wasn't used…")
	c.revoking.Store(true)
	err := c.api.DeleteToken(ctx, c.account, token.ID)
	c.printMu.Lock()
	c.revoking.Store(false)
	c.printMu.Unlock()
	var apiErr *cloudflare.Error
	switch {
	case err == nil:
		if c.slot != nil && c.slot.ProviderID == token.ID {
			c.slot.State = issuance.Deleted
			c.slot.CleanupReason = "provider-delete-confirmed"
			_ = issuance.Save(c.home, *c.slot)
		}
		terminal.Println(p.out, p.style.okMark()+" Revoked the key that wasn't used.")
	case errors.As(err, &apiErr) && apiErr.NotFound():
		p.note("Cloudflare says that token doesn't exist.", "If the token named \""+name+"\" still appears in the dashboard, revoke it there (Manage account > Account API tokens).")
	default:
		p.warn("Couldn't revoke the key that wasn't used. "+explainCloudflare(err, "The token needs the "+cloudflare.PermissionTokensWrite+" permission to revoke tokens."),
			"Revoke the API token named \""+name+"\" in the dashboard (Manage account > Account API tokens).")
	}
}

// confirmPublicAccess reports what the bootstrap token can see of the bucket's
// public access, and stops when it sees the bucket is publicly readable: the
// person checks again (after turning it off in the dashboard), chooses another
// storage option, or continues knowing it. Choosing another storage option
// revokes the key's token (it is made and checked but not stored), reports the
// empty bucket, and ends with errChooseStorageAgain; so does a prompt that
// fails, after the same revoke, with its own error. A read that failed does
// not stop anything: it was reported as "not checked", as before.
func (c *r2Creator) confirmPublicAccess() error {
	for {
		if !c.reportPublicAccess() {
			return nil
		}
		choice, err := c.p.setupMenu("What now?", "other",
			option{"again", "Check again"},
			option{"other", "Choose another storage option"},
			option{"continue", "Continue anyway (the bucket is publicly readable)"})
		if err == nil && choice == "again" {
			continue
		}
		if err == nil && choice == "continue" {
			return nil
		}
		c.revoke(context.Background(), c.token, c.tokenName)
		c.reportBucketLeftBehind()
		if err != nil {
			return err
		}
		return errChooseStorageAgain
	}
}

// reportPublicAccess records what the bootstrap token could see of the
// bucket's native public access: the r2.dev URL and custom domains. Setup
// does not repeat these reads after discarding that token.
//
// Which token permissions these two reads need is not documented; a refusal
// is reported as "not checked", and does not hide what the other read found.
func (c *r2Creator) reportPublicAccess() (public bool) {
	p := c.p
	ctx := context.Background()
	domain, managedErr := c.api.ManagedDomain(ctx, c.account, c.bucket.BucketRef)
	custom, customErr := c.api.CustomDomains(ctx, c.account, c.bucket.BucketRef)
	report := storage.UnknownPrivacy(credentials.ProviderR2)
	checked := c.env.now().UTC()
	report.CheckedAt = &checked
	if managedErr == nil {
		report.Checks = append(report.Checks, "r2_dev_domain")
	}
	if customErr == nil {
		report.Checks = append(report.Checks, "custom_domains")
	}
	var enabled []string
	for _, d := range custom {
		if d.Enabled {
			enabled = append(enabled, printableText(d.Domain))
		}
	}
	switch {
	case managedErr == nil && domain.Enabled:
		public = true
		p.warn(p.style.fail("The bucket's public r2.dev URL is ON: anyone with the link can read what is stored."),
			"Turn it off before archiving: Cloudflare dashboard > R2 > "+c.bucket.Name+" > Settings > Public access.")
	case managedErr == nil:
		terminal.Println(p.out, p.style.okMark()+" r2.dev public access: off (checked at setup).")
	default:
		p.warn("Couldn't check whether the bucket's public r2.dev URL is on. "+explainCloudflare(managedErr, "The token may need the Workers R2 Storage Read permission to read it."),
			"Check it in the dashboard: R2 > "+c.bucket.Name+" > Settings > Public access.")
	}
	switch {
	case customErr == nil && len(enabled) > 0:
		public = true
		p.warn(p.style.fail("The bucket serves these custom domains publicly: "+strings.Join(enabled, ", ")+"."), "Remove them if the archive shouldn't be publicly readable.")
	case customErr == nil:
		terminal.Println(p.out, p.style.okMark()+" Custom domains: none enabled (checked at setup).")
	default:
		p.warn("Couldn't check whether the bucket has custom domains. " + explainCloudflare(customErr, "The token may need the Workers R2 Storage Read permission to read them."))
	}
	terminal.Println(p.out, p.style.dim("  These were read just now; setup doesn't check again."))
	switch {
	case public:
		report.State, report.Reason = "public_or_risky", "r2_public_access_enabled"
	case managedErr == nil && customErr == nil:
		report.State, report.Reason = "verified_private", "r2_public_domains_disabled"
	default:
		report.Reason = "r2_public_access_not_fully_checked"
	}
	c.privacy = report
	return public
}

// maxMessageRunes bounds how much of Cloudflare's own message is shown.
const maxMessageRunes = 200

// cutMessage shortens text to maxMessageRunes characters without splitting
// one, after making it safe to print.
func cutMessage(text string) string {
	text = printableText(text)
	if utf8.RuneCountInString(text) <= maxMessageRunes {
		return text
	}
	return string([]rune(text)[:maxMessageRunes]) + "…"
}

// printableText is text from Cloudflare (an account or domain name, a
// message) made safe to print: a character that is not printable, such as a
// terminal escape or a line break, becomes a space, so what Cloudflare sends
// can never move the cursor or restyle setup's own lines. What the person
// typed is validated instead (bucket names, account IDs).
func printableText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, text)
}

// explainCloudflare says, in one or two sentences, what a failed Cloudflare
// call needs from the person. forbidden is what to say for a 403, which names
// the permission that step uses. Cloudflare's own message follows: it never
// holds the token (the client removes it).
func explainCloudflare(err error, forbidden string) string {
	var apiErr *cloudflare.Error
	if !errors.As(err, &apiErr) {
		return "Setup couldn't complete the request: " + err.Error() + "."
	}
	var text string
	switch {
	case apiErr.Status == 0:
		text = "Couldn't reach Cloudflare. Check your connection, then try again."
	case apiErr.Err != nil && apiErr.Status/100 == 2:
		text = "Cloudflare answered, but setup couldn't read the answer."
	case apiErr.Unauthorized():
		text = "Cloudflare didn't accept the API token. Check that it was copied whole and hasn't expired or been deleted."
	case apiErr.Forbidden():
		text = forbidden
		if text == "" {
			text = "The token doesn't have the permission this step needs."
		}
	case apiErr.NotFound():
		text = "Cloudflare doesn't know that account or bucket. Check the account ID, and that the token belongs to that account."
	case apiErr.AlreadyExists():
		text = "Cloudflare says it already exists."
	case apiErr.RateLimited():
		text = "Cloudflare is limiting this token's requests"
		if apiErr.RetryAfter > 0 {
			text += fmt.Sprintf("; it asked for a wait of %d seconds", int(apiErr.RetryAfter/time.Second))
		}
		text += ". Try again in a few minutes."
	case apiErr.ServerError():
		text = fmt.Sprintf("Cloudflare had a problem (HTTP %d). Try again in a few minutes.", apiErr.Status)
	default:
		text = fmt.Sprintf("Cloudflare refused the request (HTTP %d).", apiErr.Status)
	}
	if len(apiErr.Messages) > 0 {
		text += " Cloudflare said: " + cutMessage(apiErr.Messages[0])
	}
	if apiErr.Err != nil {
		text += " (" + apiErr.Err.Error() + ")"
	}
	return text
}

// cloudflareAPI is the API client for a bootstrap token: Env.Cloudflare, or the
// real API.
func (e Env) cloudflareAPI(token string) cloudflare.API {
	if e.Cloudflare != nil {
		return e.Cloudflare(token)
	}
	return openCloudflare(token)
}

// openCloudflare is Env.Cloudflare's default. The package's tests replace it
// with one that stops the test, so a test that leaves Env.Cloudflare unset
// never reaches Cloudflare.
var openCloudflare = func(token string) cloudflare.API {
	return cloudflare.New(token, cloudflare.Options{})
}

// verifyR2Key runs setup's storage check on a key that is not stored yet:
// the same listing and round trip as verifyStorage, without the privacy
// inspection.
func (e Env) verifyR2Key(ctx context.Context, cfg credentials.Config, key credentials.R2Credentials) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var store storage.ObjectStore
	if e.OpenStore != nil {
		opened, err := e.OpenStore(config.Config{Storage: cfg})
		if err != nil {
			return fmt.Errorf("connect storage: %w", err)
		}
		store = opened
	} else {
		cfg.R2CredentialRef = "guided-new-key"
		opened, err := storage.NewConfiguredStore(ctx, cfg, singleCredential{key})
		if err != nil {
			return fmt.Errorf("connect storage: %w", err)
		}
		store = opened
	}
	if err := storage.Probe(ctx, store); err != nil {
		return err
	}
	return storage.VerifyAccess(ctx, store)
}

// singleCredential is a credential store holding one key in memory, for
// checking a key before it is stored.
type singleCredential struct{ value credentials.R2Credentials }

func (s singleCredential) Save(context.Context, string, credentials.R2Credentials) error {
	return credentials.ErrUnavailable
}

func (s singleCredential) Load(context.Context, string) (credentials.R2Credentials, error) {
	return s.value, nil
}

func (s singleCredential) Delete(context.Context, string) error { return credentials.ErrUnavailable }
