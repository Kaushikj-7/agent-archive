package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// keyIssuer owns no management token persistence. Caller holds issued.lock.
type keyIssuer struct {
	home    string
	cfg     config.Config
	env     Env
	p       *prompter
	api     cloudflare.API
	group   string
	account string
	bucket  cloudflare.BucketRef
	// beforeCleanupDelete can veto deletion after exact provider lineage is established.
	beforeCleanupDelete func(context.Context, string) error
}

func newKeyIssuer(home string, cfg config.Config, env Env, p *prompter, api cloudflare.API) (*keyIssuer, error) {
	account, bucket, err := providerDestination(cfg.Storage)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	groups, err := api.PermissionGroups(ctx, account, cloudflare.PermissionBucketItemWrite)
	if err != nil {
		return nil, errors.New("cannot read key permission; management token needs Account API Tokens Write")
	}
	group, err := cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite)
	if err != nil {
		return nil, err
	}
	return &keyIssuer{home: home, cfg: cfg, env: env, p: p, api: api, group: group, account: account, bucket: bucket}, nil
}

func (i *keyIssuer) create(origin issuance.Origin) (issuance.Slot, credentials.R2Credentials, error) {
	return i.createWithIntent(origin, "", nil)
}

// createWithIntent binds a chosen recipient before the immutable provider name
// is journaled, and lets an owning transaction persist that exact slot before API.
func (i *keyIssuer) createWithIntent(origin issuance.Origin, recipientID string, beforeProvider func(issuance.Slot) error) (issuance.Slot, credentials.R2Credentials, error) {
	if recipientID != "" && recipientID == i.cfg.MachineID && beforeProvider == nil {
		return issuance.Slot{}, credentials.R2Credentials{}, errors.New("own-key creation requires durable transaction intent callback")
	}
	s, err := issuance.New(i.cfg.MachineID, i.cfg.DestinationID(), i.account, i.bucket, i.group, origin, i.env.now())
	if err != nil {
		return s, credentials.R2Credentials{}, err
	}
	if recipientID != "" {
		if !config.ValidMachineID(recipientID) {
			return s, credentials.R2Credentials{}, errors.New("invalid recipient identity")
		}
		s.RecipientID = recipientID
		s.ProviderName = issuance.ProviderName(recipientID, s.IssuerID, s.SlotID)
	}
	if err = issuance.Save(i.home, s); err != nil {
		return s, credentials.R2Credentials{}, err
	}
	if beforeProvider != nil {
		if err = beforeProvider(s); err != nil {
			s.State = issuance.Deleted
			s.CleanupReason = "transaction-refused-before-provider"
			_ = issuance.Save(i.home, s)
			return s, credentials.R2Credentials{}, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	resource, err := cloudflare.BucketResource(i.account, i.bucket)
	if err != nil {
		return s, credentials.R2Credentials{}, err
	}
	spec := cloudflare.TokenSpec{Name: s.ProviderName, Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{i.group}, Resources: map[string]string{resource: "*"}}}}
	token, err := i.api.CreateToken(ctx, i.account, spec)
	if err != nil {
		s.ProviderID = token.ID
		s.State = issuance.CleanupPending
		s.CleanupReason = "creation-response-uncertain"
		if !answerLost(err) {
			s.State = issuance.Deleted
			s.CleanupReason = "creation-refused"
		}
		_ = issuance.Save(i.home, s)
		if s.State != issuance.Deleted {
			i.cleanup(&s)
		}
		return s, credentials.R2Credentials{}, errors.New("key creation failed; check Account API Tokens Write; uncertain creation remains tracked for cleanup")
	}
	s.ProviderID = token.ID
	s.State = issuance.SecretIntent
	if err = issuance.Save(i.home, s); err != nil {
		i.cleanup(&s)
		return s, credentials.R2Credentials{}, errors.New("cannot persist created key; cleanup tracked")
	}
	derived := cloudflare.DeriveS3Credentials(token)
	key := credentials.R2Credentials{AccessKeyID: derived.AccessKeyID, SecretAccessKey: derived.SecretAccessKey}
	verifier := r2Creator{keyStorage: &i.cfg.Storage, env: i.env, p: i.p, account: i.account, bucket: cloudflare.BucketSpec{BucketRef: i.bucket}}
	if err = verifier.checkKey(ctx, key); err != nil {
		i.cleanup(&s)
		return s, credentials.R2Credentials{}, errors.New("dedicated key did not pass the storage check; cleanup tracked")
	}
	kc, err := i.env.credentialStore()
	if err == nil {
		err = kc.Save(ctx, s.SecretRef, key)
	}
	if err != nil {
		i.cleanup(&s)
		return s, credentials.R2Credentials{}, errors.New("cannot stage dedicated key; cleanup tracked")
	}
	s.State = issuance.Spare
	if recipientID != "" && recipientID == i.cfg.MachineID {
		s.State = issuance.OwnIntent
	}
	if err = issuance.Save(i.home, s); err != nil {
		i.cleanup(&s)
		return s, credentials.R2Credentials{}, err
	}
	return s, key, nil
}

// cleanup is used only for keys whose ledger proves no delivery was attempted.
// A lost reply is matched by the whole immutable name and exact resource policy.
func (i *keyIssuer) cleanup(s *issuance.Slot) {
	i.cleanupWithContext(context.Background(), s)
}

// cleanupWithContext detaches cancellation only for bounded orphan cleanup.
func (i *keyIssuer) cleanupWithContext(ctx context.Context, s *issuance.Slot) {
	if s.State == issuance.Deleted || s.State == issuance.DeliveryIntent || s.State == issuance.Delivered || s.State == issuance.Own || s.State == issuance.OwnIntent {
		return
	}
	s.State = issuance.CleanupPending
	if issuance.Save(i.home, *s) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudflare.InventoryTimeout)
	defer cancel()
	reader, ok := i.api.(cloudflare.InventoryAPI)
	if !ok {
		return
	}
	var metadata cloudflare.TokenMetadata
	var err error
	if s.ProviderID != "" {
		metadata, err = reader.TokenDetails(ctx, i.account, s.ProviderID)
	} else {
		var inventory cloudflare.TokenInventory
		inventory, err = reader.TokenInventory(ctx, i.account)
		matches := 0
		for _, token := range inventory.Tokens {
			if token.Name == s.ProviderName {
				matches++
				metadata = token
			}
		}
		if err != nil || !inventory.PaginationComplete || matches != 1 {
			return
		}
	}
	if err != nil || metadata.Name != s.ProviderName || !cloudflare.ExactBucketPolicy(metadata, i.account, i.bucket, s.PermissionID) || !config.ValidMachineID(metadata.ID) || (s.ProviderID != "" && s.ProviderID != metadata.ID) {
		return
	}
	if i.beforeCleanupDelete != nil && i.beforeCleanupDelete(ctx, metadata.ID) != nil {
		return
	}
	s.ProviderID = metadata.ID
	if issuance.Save(i.home, *s) != nil {
		return
	}
	if i.api.DeleteToken(ctx, i.account, s.ProviderID) != nil {
		return
	}
	s.State = issuance.Deleted
	s.CleanupReason = "provider-delete-confirmed"
	kc, err := i.env.credentialStore()
	if err == nil {
		err = kc.Delete(ctx, s.SecretRef)
	}
	s.SecretRemovalPending = err != nil
	_ = issuance.Save(i.home, *s)
}

func (i *keyIssuer) reconcile() error {
	slots, err := issuance.List(i.home)
	if err != nil {
		return err
	}
	for _, s := range slots {
		if s.DestinationID != i.cfg.DestinationID() || s.IssuerID != i.cfg.MachineID {
			continue
		}
		if s.Origin == issuance.Guided && s.State == issuance.OwnIntent && committedGuidedSlot(i.cfg, s) {
			s.State = issuance.Own
			if err := issuance.Save(i.home, s); err != nil {
				return err
			}
		}
		// Explicit self-recipient fresh slots belong to own-key's durable
		// operation, including interrupted creation and cleanup boundaries.
		if s.Origin == issuance.Fresh && s.RecipientID == i.cfg.MachineID {
			continue
		}
		if s.State == issuance.CreationIntent || s.State == issuance.SecretIntent || s.State == issuance.CleanupPending || (s.State == issuance.Reserved && s.Origin != issuance.Precreated) {
			if i.cfg.Storage.R2CredentialRef == s.SecretRef || (i.cfg.MachineAssignment != nil && i.cfg.MachineAssignment.AccessKeyID == s.ProviderID) {
				continue
			}
			i.cleanup(&s)
		}
	}
	return nil
}

func reserveSpare(home string, cfg config.Config, pairID, name string, expiry time.Time, env Env) (issuance.Slot, credentials.R2Credentials, error) {
	if cfg.SpareTarget() == 0 {
		return issuance.Slot{}, credentials.R2Credentials{}, nil
	}
	account, bucket, err := providerDestination(cfg.Storage)
	if err != nil {
		return issuance.Slot{}, credentials.R2Credentials{}, err
	}
	slots, err := issuance.List(home)
	if err != nil {
		return issuance.Slot{}, credentials.R2Credentials{}, err
	}
	for _, s := range slots {
		if s.State != issuance.Spare || s.DestinationID != cfg.DestinationID() || s.IssuerID != cfg.MachineID || s.AccountID != account || s.Bucket != bucket.Name || s.Jurisdiction != bucket.Jurisdiction {
			continue
		}
		// The reference is read only after durable reservation, never from the config index.
		s.State = issuance.Reserved
		s.PairingID = pairID
		s.Label = name
		s.ExpiresAt = expiry
		if err = issuance.Save(home, s); err != nil {
			return s, credentials.R2Credentials{}, err
		}
		kc, err := env.credentialStore()
		var key credentials.R2Credentials
		if err == nil {
			key, err = credentials.LoadStored(context.Background(), kc, s.SecretRef)
		}
		if err != nil || key.AccessKeyID != s.ProviderID {
			s.State = issuance.CleanupPending
			s.CleanupReason = "spare-secret-unavailable"
			_ = issuance.Save(home, s)
			continue
		}
		verifier := r2Creator{keyStorage: &cfg.Storage, env: env, p: newPrompter(nil, io.Discard), account: s.AccountID, bucket: cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: s.Bucket, Jurisdiction: s.Jurisdiction}}}
		if verifier.checkKey(context.Background(), key) != nil {
			s.State = issuance.CleanupPending
			s.CleanupReason = "spare-storage-check-failed"
			_ = issuance.Save(home, s)
			continue
		}
		return s, key, nil
	}
	return issuance.Slot{}, credentials.R2Credentials{}, nil
}

func spareRefs(home string, cfg config.Config) ([]string, error) {
	slots, err := issuance.List(home)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, s := range slots {
		if s.State == issuance.Spare && s.DestinationID == cfg.DestinationID() && s.IssuerID == cfg.MachineID {
			refs = append(refs, s.SecretRef)
		}
	}
	return refs, nil
}

func (i *keyIssuer) refill() error {
	refs, err := spareRefs(i.home, i.cfg)
	if err != nil {
		return err
	}
	for len(refs) < i.cfg.SpareTarget() {
		s, _, err := i.create(issuance.Precreated)
		if err != nil {
			return err
		}
		refs = append(refs, s.SecretRef)
	}
	return nil
}

// saveSpareIndex reloads under the collector lock so no unrelated config is lost.
// The ledger remains authoritative even if this advisory update fails.
func saveSpareIndex(home string, cfg config.Config, target *int) error {
	release, err := local.Lock(home)
	if err != nil {
		return err
	}
	defer release()
	current, found, err := config.Load(home)
	if err != nil || !found {
		return errors.New("cannot reload spare configuration")
	}
	if current.DestinationID() != cfg.DestinationID() || current.MachineID != cfg.MachineID {
		return errors.New("destination changed; spare index update deferred")
	}
	refs, err := spareRefs(home, current)
	if err != nil {
		return err
	}
	if len(refs) > 5 {
		return errors.New("spare ledger contains more than five eligible keys")
	}
	current.SpareCredentialRefs = refs
	if target != nil {
		n := *target
		current.SpareKeys = &n
	}
	return config.Save(home, current)
}

func discardDeliveredSecret(home string, s *issuance.Slot, env Env, out io.Writer) {
	if s.State != issuance.DeliveryIntent && s.State != issuance.Delivered && s.State != issuance.CleanupPending && s.State != issuance.Deleted {
		return
	}
	kc, err := env.credentialStore()
	if err == nil {
		err = kc.Delete(context.Background(), s.SecretRef)
	}
	s.SecretRemovalPending = err != nil
	if issuance.Save(home, *s) != nil || err != nil {
		terminal.Println(out, "Issuer-local secret cleanup pending; issuance lineage retained.")
	}
}

func releaseUntouchedSlot(home string, s *issuance.Slot, issuer *keyIssuer) {
	if s.State != issuance.Reserved {
		return
	}
	if s.Origin == issuance.Precreated {
		s.State = issuance.Spare
		s.PairingID = ""
		s.Label = ""
		s.ExpiresAt = time.Time{}
		_ = issuance.Save(home, *s)
	} else if issuer != nil {
		issuer.cleanup(s)
	}
}

func dedicatedWarnings(home string) []string {
	slots, err := issuance.List(home)
	if err != nil {
		return []string{err.Error()}
	}
	var warnings []string
	for _, s := range slots {
		if s.State == issuance.CreationIntent || s.State == issuance.SecretIntent || s.State == issuance.Reserved || s.State == issuance.OwnIntent || s.State == issuance.CleanupPending || s.SecretRemovalPending {
			warnings = append(warnings, fmt.Sprintf("Dedicated slot %s: cleanup pending; provider access may remain.", s.SlotID))
		}
	}
	return warnings
}

func choosePairingKey(home string, cfg config.Config, p *prompter, env Env, yes, share bool, payload *pairing.Payload) (issuance.Slot, *keyIssuer, error) {
	if cfg.Storage.Provider != credentials.ProviderR2 || share {
		return issuance.Slot{}, nil, nil
	}
	if _, _, err := providerDestination(cfg.Storage); err != nil {
		return issuance.Slot{}, nil, err
	}
	if err := recoverUntouchedSpares(home, cfg); err != nil {
		return issuance.Slot{}, nil, err
	}
	available := false
	if _, ok := env.lookupEnv("CLOUDFLARE_API_TOKEN"); ok {
		available = true
	}
	if !yes && len(cfg.CloudflareTokenCommand) > 0 {
		available = true
	}
	var issuer *keyIssuer
	var selected issuance.Slot
	var key credentials.R2Credentials
	if !available {
		var err error
		selected, key, err = reserveSpare(home, cfg, payload.PairingID, payload.Name, payload.ExpiresAt, env)
		if err != nil {
			return selected, nil, err
		}
		if selected.SlotID != "" {
			payload.RecipientID = selected.RecipientID
			payload.SlotID = selected.SlotID
			payload.Kind = config.MachineAssignmentR2Own
			payload.AccessKeyID = key.AccessKeyID
			payload.SecretAccessKey = key.SecretAccessKey
			return selected, nil, nil
		}
		if yes {
			return selected, nil, errors.New("no eligible spare: set CLOUDFLARE_API_TOKEN or deliberately use --share-key; no key was shared")
		}
		choice, err := p.guidedChoice(promptModel{Question: "Create separate access for this machine?", Helpers: []string{"Separate access lets you revoke this machine later without affecting other machines."}, Default: "create", Primary: []option{{"create", "Create a Cloudflare key (recommended)"}, {"share", "Share existing access (revoking it affects all machines using it)"}}, Secondary: []actionOption{{"cancel", "q", "Cancel"}}})
		if err != nil {
			return selected, nil, err
		}
		if choice == "share" {
			return selected, nil, nil
		}
		if choice != "create" {
			return selected, nil, errors.New("pairing cancelled")
		}
	}
	var err error
	issuer, err = acquirePairingIssuer(home, cfg, p, env, yes)
	if err == nil {
		if err = issuer.reconcile(); err == nil {
			selected, key, err = issuer.create(issuance.Fresh)
		}
	}
	if err != nil {
		if issuer != nil {
			issuer.api.Discard()
		}
		spare, spareKey, spareErr := reserveSpare(home, cfg, payload.PairingID, payload.Name, payload.ExpiresAt, env)
		if spareErr != nil {
			return spare, nil, spareErr
		}
		if spare.SlotID == "" {
			return spare, nil, err
		}
		setDedicatedPayload(payload, spare, spareKey)
		terminal.Println(p.out, "Fresh creation unavailable; checked and reserved an eligible spare instead.")
		return spare, nil, nil
	}
	selected.State = issuance.Reserved
	selected.PairingID = payload.PairingID
	selected.Label = payload.Name
	selected.ExpiresAt = payload.ExpiresAt
	if err = issuance.Save(home, selected); err != nil {
		issuer.cleanup(&selected)
		issuer.api.Discard()
		return selected, nil, err
	}
	setDedicatedPayload(payload, selected, key)
	return selected, issuer, nil
}

// cancelDelivered is an explicit user request, never automatic delivery cleanup.
func (i *keyIssuer) cancelDelivered(s *issuance.Slot) {
	if s.State != issuance.DeliveryIntent && s.State != issuance.Delivered {
		return
	}
	s.State = issuance.CleanupPending
	s.CleanupReason = "explicit-cancellation"
	i.cleanup(s)
}

// Reserved precedes the separately durable delivery intent, so an interrupted
// pre-created slot is provably untouched by this command and can be released.
func recoverUntouchedSpares(home string, cfg config.Config) error {
	slots, err := issuance.List(home)
	if err != nil {
		return err
	}
	for _, s := range slots {
		if s.State == issuance.Reserved && s.Origin == issuance.Precreated && s.DestinationID == cfg.DestinationID() && s.IssuerID == cfg.MachineID {
			s.State = issuance.Spare
			s.PairingID = ""
			s.Label = ""
			s.ExpiresAt = time.Time{}
			if err = issuance.Save(home, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func acquirePairingIssuer(home string, cfg config.Config, p *prompter, env Env, yes bool) (*keyIssuer, error) {
	token, _, _, err := readManagementToken(context.Background(), p, env, cfg.CloudflareTokenCommand, !yes)
	if err != nil {
		return nil, err
	}
	api := env.cloudflareAPI(token)
	issuer, err := newKeyIssuer(home, cfg, env, p, api)
	if err != nil {
		api.Discard()
		return nil, err
	}
	return issuer, nil
}

func setDedicatedPayload(payload *pairing.Payload, slot issuance.Slot, key credentials.R2Credentials) {
	payload.RecipientID = slot.RecipientID
	payload.SlotID = slot.SlotID
	payload.Kind = config.MachineAssignmentR2Own
	payload.AccessKeyID = key.AccessKeyID
	payload.SecretAccessKey = key.SecretAccessKey
}

// reconcileCommittedGuidedSlot promotes only the exact locally committed binding.
// A crash between config commit and this advisory update is safe to retry.
func reconcileCommittedGuidedSlot(home string) error {
	release, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		return err
	}
	defer release()
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		return err
	}
	slots, err := issuance.List(home)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		if slot.Origin == issuance.Guided && slot.State == issuance.OwnIntent && committedGuidedSlot(cfg, slot) {
			slot.State = issuance.Own
			if err = issuance.Save(home, slot); err != nil {
				return err
			}
		}
	}
	return nil
}

func committedGuidedSlot(cfg config.Config, slot issuance.Slot) bool {
	a := cfg.MachineAssignment
	return a != nil && a.Kind == config.MachineAssignmentR2Own && cfg.MachineID == slot.IssuerID && cfg.DestinationID() == slot.DestinationID && cfg.Storage.R2CredentialRef == slot.SecretRef && a.SlotID == slot.SlotID && a.AccessKeyID == slot.ProviderID && a.RecipientID == slot.RecipientID && a.IssuerID == slot.IssuerID && a.DestinationID == slot.DestinationID
}

// abandonGuidedStage retains provider cleanup ownership after explicit discard.
func abandonGuidedStage(home string, draft setupDraft, active config.Config) error {
	if draft.GuidedSlotID == "" {
		return nil
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
	for _, slot := range slots {
		if slot.SlotID != draft.GuidedSlotID || slot.Origin != issuance.Guided || slot.State != issuance.OwnIntent || committedGuidedSlot(active, slot) {
			continue
		}
		if slot.SecretRef != draft.CredentialRef && !containsString(draft.StagedRefs, slot.SecretRef) {
			return errors.New("guided draft credential does not match its slot")
		}
		slot.State = issuance.CleanupPending
		slot.CleanupReason = "guided-draft-discarded"
		if err = issuance.Save(home, slot); err != nil {
			return err
		}
	}
	return nil
}
