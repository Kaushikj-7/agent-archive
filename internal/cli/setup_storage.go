package cli

import (
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// storageNavigationError carries an explicit destination, rather than a failure.
type storageNavigationError string

func (n storageNavigationError) Error() string { return string(n) }

const errUseExistingStorage storageNavigationError = "use existing storage"

// storageProviderChoice includes explicit new and existing storage routes.
type storageProviderChoice string

const (
	storageProviderR2Existing storageProviderChoice = "r2-existing"
	storageProviderS3Existing storageProviderChoice = "s3-existing"
	storageProviderR2Create   storageProviderChoice = guidedR2Choice
	storageProviderS3Create   storageProviderChoice = storageChoiceS3New
)

// storageStage names the work currently being performed; it is not a counter.
type storageStage string

const (
	storageAccount      storageStage = "Account"
	storageDetails      storageStage = "Storage"
	storageVerification storageStage = "Verification"
)

func (p *prompter) storageStage(stage storageStage) {
	p.setupHeading("Storage · " + string(stage))
	terminal.Println(p.out, p.style.dim("Account → Storage → Verification"))
}

func storageHelp(p *prompter) {
	terminal.Println(p.out, "Step by step, for Cloudflare R2 and Amazon S3: "+bucketDocURL)
}

func promptStorage(p *prompter, existing credentials.Config, env Env, failedRegion string, keepInstalled ...bool) (credentials.Config, credentials.R2Credentials, bool, error) {
	offerKeep := len(keepInstalled) == 0 || keepInstalled[0]
	var secret credentials.R2Credentials
	answers := map[string]credentials.Config{}
	if offerKeep && existing.Bucket != "" {
		cfg, key, saved, kept, err := promptKeepStorage(p, existing, env)
		if kept || err != nil {
			return cfg, key, saved, err
		}
		answers[cfg.Provider+":existing"] = cfg
	}
	def := existing.Provider
	if def == "" {
		def = defaultStorageProvider(env)
	}
	for {
		p.storageStage(storageAccount)
		aliases, secondary := storageProviderActions(existing)
		if existing.Bucket != "" {
			terminal.Println(p.out, "Continue connecting the saved bucket, or choose a replacement.")
		}
		choice, err := p.guidedChoice(promptModel{Question: "Where should your archive live?", Default: def, Primary: storageMenuOptions(), Secondary: secondary, Aliases: aliases, Receipt: "Provider", ResolveReceipt: storageProviderLabel})
		if err != nil {
			return existing, secret, false, err
		}
		if choice == "help" {
			storageHelp(p)
			continue
		}
		provider := strings.Split(choice, "-")[0]
		mode := "new"
		switch storageProviderChoice(choice) {
		case storageProviderR2Existing, storageProviderS3Existing:
			mode = "existing"
		case storageProviderR2Create, storageProviderS3Create:
		default:
			if provider == existing.Provider && existing.Bucket != "" {
				mode = "existing"
				break
			}
			mode, err = storageIntroduction(p, provider)
			if err != nil {
				return existing, secret, false, err
			}
			if mode == "back" {
				continue
			}
		}
		cfg := existing
		if cfg.Provider != provider || mode == "new" {
			cfg = credentials.Config{Provider: provider}
		}
		answerKey := provider + ":" + mode
		if saved, ok := answers[answerKey]; ok {
			cfg = saved
		}
		if mode == "new" && provider == credentials.ProviderR2 {
			var saved bool
			cfg, secret, saved, err = createR2Bucket(p, env)
			if err == nil {
				return cfg, secret, saved, nil
			}
			if errors.Is(err, errChooseStorageAgain) {
				continue
			}
			if !errors.Is(err, errUseExistingStorage) {
				return cfg, secret, false, err
			}
			cfg.Provider = provider
			mode = "existing"
			answerKey = provider + ":" + mode
		}
		if provider == credentials.ProviderR2 {
			connected, key, save, e := promptExistingR2(p, cfg, env)
			if errors.Is(e, errChooseStorageAgain) {
				answers[answerKey] = connected
				continue
			}
			return connected, key, save, e
		}
		err = promptS3Bucket(p, &cfg, env, failedRegion, mode == "new")
		if errors.Is(err, errChooseStorageAgain) {
			answers[answerKey] = cfg
			continue
		}
		if err != nil {
			return cfg, secret, false, err
		}
		cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)
		return cfg, secret, false, nil
	}
}

// storageProviderActions exposes replacement creation only for a saved destination.
func storageProviderActions(existing credentials.Config) ([]option, []actionOption) {
	aliases := []option{{"r2-existing", ""}, {"s3-existing", ""}, {storageChoiceS3New, ""}}
	aliases = append(aliases, option{guidedR2Choice, ""})
	secondary := []actionOption{{"help", "h", "Setup instructions"}}
	if existing.Bucket != "" {
		if existing.Provider == credentials.ProviderS3 {
			secondary = append(secondary, actionOption{storageChoiceS3New, "n", "Create a replacement S3 bucket"})
		} else {
			secondary = append(secondary, actionOption{guidedR2Choice, "n", "Create a replacement R2 bucket"})
		}
	}
	return aliases, secondary
}

func storageIntroduction(p *prompter, provider string) (string, error) {
	for {
		title := "Set up Amazon S3"
		explanation := "Setup will create a bucket with Block Public Access using an AWS profile."
		def := "new"
		if provider == credentials.ProviderR2 {
			title = "Set up Cloudflare R2"
			explanation = "Setup will create a private bucket and archive key. You'll provide a setup token once; it won't be saved."
		}
		choice, err := p.guidedChoice(promptModel{Question: title, Helpers: []string{explanation}, Default: def, Primary: []option{{"new", "Create a new bucket"}, {"existing", "Use an existing bucket"}}, Secondary: []actionOption{{"back", "b", "Back to storage options"}, {"help", "h", "Setup instructions"}}})
		if err != nil {
			return "", err
		}
		if choice == "help" {
			storageHelp(p)
			continue
		}
		return choice, nil
	}
}

func promptExistingR2(p *prompter, cfg credentials.Config, env Env) (credentials.Config, credentials.R2Credentials, bool, error) {
	var secret credentials.R2Credentials
	var err error
	p.storageStage(storageAccount)
	previous := cfg
	// The account comes first, so a pasted bucket URL can fill in the
	// bucket too.
	fromURL, e := promptR2Location(p, &cfg)
	if e != nil {
		return cfg, secret, false, e
	}
	if cfg.R2Endpoint != previous.R2Endpoint || cfg.R2AccountID != previous.R2AccountID || cfg.Bucket != previous.Bucket {
		cfg.R2CredentialRef = ""
	}
	p.storageStage(storageDetails)
	if !fromURL {
		bucket, e := p.setupStorageRequired("Bucket name", cfg.Bucket)
		if e != nil {
			return cfg, secret, false, e
		}
		cfg.Bucket = bucket
	}
	if cfg.Bucket != previous.Bucket || cfg.R2Endpoint != previous.R2Endpoint || cfg.R2AccountID != previous.R2AccountID {
		cfg.R2CredentialRef = ""
	}
	reuse := false
	if cfg.R2CredentialRef != "" {
		// A rebuilt or reinstalled binary can lose access to the item it
		// stored; offering to keep it would only fail after the
		// questions, so ask for the key again right away.
		if storedCredentialReadable(env, cfg.R2CredentialRef) {
			reuse, err = p.setupYesNo("Keep stored R2 credentials?", true)
			if err != nil {
				return cfg, secret, false, err
			}
		} else {
			terminal.Println(p.out, "The stored R2 credentials can't be read from the "+credentials.StoreName(credentialOS)+"; enter them again.")
		}
	}
	if !reuse {
		secret.AccessKeyID, err = p.setupStorageField("Access key ID (hidden)", "", true)
		if err != nil {
			return cfg, secret, false, err
		}
		for secret.SecretAccessKey == "" {
			secret.SecretAccessKey, err = p.guidedText(promptModel{Question: "Secret access key (hidden)", Label: "Credential", Secret: true})
			if err != nil {
				return cfg, secret, false, err
			}
		}
	}

	cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)
	return cfg, secret, secret.SecretAccessKey != "", err
}

// storageProviderLabel resolves named compatibility answers to visible labels.
func storageProviderLabel(key string) string {
	if strings.HasPrefix(key, "r2") {
		return "Cloudflare R2"
	}
	if strings.HasPrefix(key, "s3") {
		return "Amazon S3"
	}
	if key == "help" {
		return "Setup instructions"
	}
	return key
}

// promptKeepStorage returns kept only when current storage can be reused or the
// replacement credential field completed. Back returns to the provider menu.
func promptKeepStorage(p *prompter, existing credentials.Config, env Env) (credentials.Config, credentials.R2Credentials, bool, bool, error) {
	var key credentials.R2Credentials
	p.storageStage(storageAccount)
	terminal.Println(p.out, "Current storage: "+existing.Provider+" / "+existing.Bucket)
	choice, err := p.setupActions("Keep your current storage?", "keep", nil, []actionOption{{"keep", "", "Keep current storage"}, {"change", "c", "Change storage"}})
	if err != nil {
		return existing, key, false, false, err
	}
	if choice != "keep" {
		return existing, key, false, false, nil
	}
	if existing.Provider != credentials.ProviderR2 || storedCredentialReadable(env, existing.R2CredentialRef) {
		return existing, key, false, true, nil
	}
	cfg, key, saved, err := promptExistingR2(p, existing, env)
	if errors.Is(err, errChooseStorageAgain) {
		return cfg, key, false, false, nil
	}
	return cfg, key, saved, true, err
}
