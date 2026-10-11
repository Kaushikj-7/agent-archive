package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupresources"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

var errResourceRecoveryStopped = errors.New("setup stopped; saved progress and resources retained")

// resourceUsed is deliberately conservative for S3: profile names can be rebound.
func resourceUsed(r setupresources.Receipt, cfg credentials.Config) bool {
	if string(r.Provider) != cfg.Provider || r.Bucket != cfg.Bucket {
		return false
	}
	if r.Provider == setupresources.S3 {
		return true
	}
	account, bucket, err := providerDestination(cfg)
	return err != nil || account == r.Account && bucket.Jurisdiction == r.Jurisdiction
}

func resourceStorage(r setupresources.Receipt) credentials.Config {
	region, endpoint := r.Region, ""
	if r.Provider == setupresources.R2 {
		region = "auto"
		host := r.Account
		if r.Jurisdiction != "" {
			host += "." + r.Jurisdiction
		}
		endpoint = "https://" + host + ".r2.cloudflarestorage.com"
	}
	return credentials.Config{Provider: string(r.Provider), Bucket: r.Bucket, Region: region, AWSProfile: r.Profile, R2AccountID: r.Account, R2Endpoint: endpoint}
}

type resourceRecoveryChoice string

const (
	resourceRecoveryConnect resourceRecoveryChoice = "connect"
	resourceRecoveryOther   resourceRecoveryChoice = "other"
	resourceRecoveryDetails resourceRecoveryChoice = "details"
	resourceRecoveryStop    resourceRecoveryChoice = "stop"
)

// recoverSetupResources never interprets connectivity as proof of ownership.
// A recovered bucket goes through the ordinary existing-storage credential flow.
func recoverSetupResources(p *prompter, home string, saved setupDraft, have bool, initial config.Config, env Env) (setupDraft, bool, error) {
	receipts, err := setupresources.List(home)
	if err != nil {
		return saved, have, fmt.Errorf("read setup resource receipts: %w", err)
	}
	slots, err := issuance.List(home)
	if err != nil {
		return saved, have, err
	}
	for _, r := range receipts {
		if r.State == setupresources.Refused || r.State == setupresources.Deleted || resourceUsed(r, initial.Storage) {
			continue
		}
		// A valid saved destination already has a safe, repeatable resume path.
		if have && resourceUsed(r, saved.Config.Storage) {
			continue
		}
		for {
			p.setupHeading("Setup paused · Storage resources")
			terminal.Printf(p.out, "Bucket %s · %s\n", r.Bucket, r.Provider)
			if r.State == setupresources.RequestSent {
				p.warn("Creation outcome unknown. The bucket may exist; setup will not retry its creation.")
			} else {
				p.note("A previous setup created this bucket. Capture has not been started for this destination.")
			}
			choice, e := p.setupMenu("What would you like to do?", "connect",
				option{"connect", "Connect this as an existing bucket"}, option{"other", "Choose other storage"}, option{"details", "Resource details and cleanup"}, option{"stop", "Stop; keep saved progress"})
			if e != nil {
				return saved, have, e
			}
			switch resourceRecoveryChoice(choice) {
			case resourceRecoveryConnect:
				if !have {
					saved = setupDraft{Version: draftFormat, Config: initial, NewInstallation: initial.Storage.Provider == ""}
				}
				// Retain draft staging references for eventual safe cleanup.
				saved.Config.Storage = resourceStorage(r)
				saved.Step = 1
				if !have && len(saved.Config.Harnesses) == 0 {
					saved.Step = 0
				}
				if e = local.Write(draftPath(home), saved); e != nil {
					return saved, have, e
				}
				return saved, true, nil
			case resourceRecoveryOther:
				return saved, have, nil
			case resourceRecoveryStop:
				return saved, have, errResourceRecoveryStopped
			case resourceRecoveryDetails:
				if e = setupResourceDetails(p, home, r, slots, saved, have, initial, env); e != nil {
					return saved, have, e
				}
			}
		}
	}
	// A crash before bucket receipts existed can still leave guided keys.
	for _, s := range slots {
		if !cleanupGuidedEligible(s, saved, have, initial) {
			continue
		}
		p.warn("An unused archive key remains tracked for cleanup.")
		choice, e := p.setupMenu("Unused archive key", "keep", option{"keep", "Keep it tracked and continue"}, option{"cleanup", "Review and revoke this key"})
		if e != nil {
			return saved, have, e
		}
		if choice == "cleanup" {
			if e = cleanupSetupKey(p, home, s, env); e != nil {
				return saved, have, e
			}
		}
	}
	return saved, have, nil
}

func cleanupGuidedEligible(s issuance.Slot, saved setupDraft, have bool, active config.Config) bool {
	if s.Origin != issuance.Guided || (s.State != issuance.CreationIntent && s.State != issuance.SecretIntent && s.State != issuance.CleanupPending) {
		return false
	}
	if active.Storage.R2CredentialRef == s.SecretRef || containsString(active.SpareCredentialRefs, s.SecretRef) || committedGuidedSlot(active, s) {
		return false
	}
	if have && (saved.GuidedSlotID == s.SlotID || saved.CredentialRef == s.SecretRef || saved.Config.Storage.R2CredentialRef == s.SecretRef || containsString(saved.StagedRefs, s.SecretRef) || containsString(saved.Config.SpareCredentialRefs, s.SecretRef)) {
		return false
	}
	return true
}

func setupResourceDetails(p *prompter, home string, r setupresources.Receipt, slots []issuance.Slot, saved setupDraft, have bool, active config.Config, env Env) error {
	terminal.Printf(p.out, "Resource receipt %s\nBucket %s\nOutcome %s\n", r.ID, r.Bucket, r.State)
	if r.Provider == setupresources.S3 {
		terminal.Printf(p.out, "Creation profile %s · Region %s\n", r.Profile, r.Region)
		p.note("A profile name is not proof of the current AWS account. Check the bucket and account in the AWS console before deleting an empty unused bucket.")
	} else {
		terminal.Printf(p.out, "Cloudflare account %s · Jurisdiction %s\n", r.Account, r.Jurisdiction)
		p.note("Open this account in Cloudflare → R2 → this bucket. Check its contents and all machines using it before deleting it.")
	}
	for _, s := range slots {
		if s.AccountID != r.Account || s.Bucket != r.Bucket || s.Jurisdiction != r.Jurisdiction {
			continue
		}
		terminal.Printf(p.out, "Archive key slot %s · %s\n", s.SlotID, s.State)
		if cleanupGuidedEligible(s, saved, have, active) {
			choice, err := p.setupMenu("Unused archive key", "keep", option{"keep", "Keep it tracked"}, option{"cleanup", "Review and revoke this key"})
			if err != nil {
				return err
			}
			if choice == "cleanup" {
				if err = cleanupSetupKey(p, home, s, env); err != nil {
					return err
				}
			}
		}
	}
	if r.State == setupresources.Created || r.State == setupresources.Secured || r.State == setupresources.CleanupPending {
		choice, err := p.setupMenu("Bucket cleanup", "keep", option{"keep", "Keep this resource"}, option{"manual", "Track manual cleanup as pending"})
		if err != nil {
			return err
		}
		if choice == "manual" {
			if err = setupresources.Finish(home, r, setupresources.CleanupPending); err != nil {
				return err
			}
			p.note("Manual cleanup remains pending. Setup cannot verify ownership or deletion and will not delete this bucket.")
		}
	} else {
		p.note("Ownership remains uncertain. Check the provider account manually; setup will not delete this bucket.")
	}
	return nil
}

func cleanupSetupKey(p *prompter, home string, s issuance.Slot, env Env) error {
	terminal.Printf(p.out, "Revoke unused archive key slot %s in Cloudflare account %s for bucket %s?\n", s.SlotID, s.AccountID, s.Bucket)
	choice, err := p.setupMenu("This permanently revokes this key", "keep", option{"keep", "Keep this key"}, option{"revoke", "Revoke this unused key"})
	if err != nil || choice != "revoke" {
		return err
	}
	release, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		return err
	}
	defer release()
	slots, err := issuance.List(home)
	if err != nil {
		return err
	}
	// setup.lock is held by the wizard; issued.lock now excludes ledger changes.
	// Refresh both protected snapshots before any provider mutation.
	active, _, err := config.Load(home)
	if err != nil {
		return err
	}
	saved, have, problem, err := readDraft(home)
	if err != nil {
		return err
	}
	if problem != "" {
		return errors.New("saved setup cannot be read; no key was revoked")
	}
	for _, current := range slots {
		if current.SlotID != s.SlotID {
			continue
		}
		if !cleanupGuidedEligible(current, saved, have, active) {
			return fmt.Errorf("key is now referenced or no longer eligible; no key was revoked")
		}
		return cleanupCurrentSetupKey(p, home, current, saved, have, active, env)
	}
	return fmt.Errorf("key receipt no longer exists; no key was revoked")
}

func cleanupCurrentSetupKey(p *prompter, home string, current issuance.Slot, saved setupDraft, have bool, active config.Config, env Env) error {
	token, _, _, err := readManagementToken(context.Background(), p, env, active.CloudflareTokenCommand, true)
	if err != nil {
		return err
	}
	api := env.cloudflareAPI(token)
	defer api.Discard()
	var protectionErr error
	issuer := keyIssuer{home: home, env: env, p: p, api: api, account: current.AccountID, bucket: cloudflare.BucketRef{Name: current.Bucket, Jurisdiction: current.Jurisdiction}, beforeCleanupDelete: func(ctx context.Context, providerID string) error {
		protectionErr = protectSetupKeyCredentials(ctx, providerID, active, saved, have, env)
		return protectionErr
	}}
	issuer.cleanup(&current)
	if current.State == issuance.Deleted {
		p.note("Unused key revoked. Credential cleanup is tracked separately if needed.")
	} else if protectionErr != nil {
		p.warn(protectionErr.Error())
	} else {
		p.warn("Key cleanup remains pending. Check account API token permissions and retry; no deletion was confirmed.")
	}
	return nil
}

// refuseUncertainCreation prevents retries across process restarts without ownership evidence.
func (p *prompter) refuseUncertainCreation(home string, candidate setupresources.Receipt) error {
	receipts, err := setupresources.List(home)
	if err != nil {
		return err
	}
	for _, r := range receipts {
		if p.resourceRequestsThisRun[r.ID] || r.State != setupresources.RequestSent || r.Provider != candidate.Provider || r.Bucket != candidate.Bucket {
			continue
		}
		if r.Provider == setupresources.S3 || r.Account == candidate.Account && r.Jurisdiction == candidate.Jurisdiction {
			return fmt.Errorf("creation outcome for bucket %s remains unknown; connect it as existing storage or choose a different bucket; check receipt %s in the provider account", r.Bucket, r.ID)
		}
	}
	return nil
}

func (p *prompter) rememberResourceRequest(id string) {
	if p.resourceRequestsThisRun == nil {
		p.resourceRequestsThisRun = make(map[string]bool)
	}
	p.resourceRequestsThisRun[id] = true
}

// protectSetupKeyCredentials protects a provider key even when it was entered
// again under a different opaque reference. Unreadable references fail closed.
func protectSetupKeyCredentials(ctx context.Context, providerID string, active config.Config, saved setupDraft, have bool, env Env) error {
	refs := append([]string{active.Storage.R2CredentialRef}, active.SpareCredentialRefs...)
	if have {
		refs = append(refs, saved.Config.Storage.R2CredentialRef, saved.CredentialRef)
		refs = append(refs, saved.StagedRefs...)
		refs = append(refs, saved.Config.SpareCredentialRefs...)
	}
	var store credentials.CredentialStore
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if store == nil {
			var err error
			store, err = env.credentialStore()
			if err != nil {
				return errors.New("cannot inspect protected credentials; no key was revoked")
			}
		}
		key, err := credentials.LoadStored(ctx, store, ref)
		if err != nil {
			return errors.New("cannot inspect protected credentials; no key was revoked")
		}
		if key.AccessKeyID == providerID {
			return errors.New("key is used by active or saved setup credentials; no key was revoked")
		}
	}
	return nil
}
