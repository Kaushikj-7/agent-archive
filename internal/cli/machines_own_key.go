package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const ownKeyFile = "own-key.json"

type ownKeyCheckpoint struct {
	DestinationID  string `json:"destination_id"`
	OldRef         string `json:"old_ref"`
	SlotID         string `json:"slot_id,omitempty"`
	Committed      bool   `json:"committed"`
	CleanupPending bool   `json:"cleanup_pending"`
}

func runMachinesOwnKey(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	fs := env.newCommandFlags("machines own-key", errOut)
	yes := fs.Bool("yes", false, "use an environment management token without prompting")
	cancelOwn := fs.Bool("cancel", false, "cancel a proven uncommitted dedicated-key stage")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if err := pairingAgentRefusal(env); err != nil {
		return machineCommandError(errOut, err)
	}
	home, err := env.readHome()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer release()
	cfg, err := ownKeyConfiguration(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	issuedRelease, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer issuedRelease()
	checkpoint, done, code := prepareOwnKeyCheckpoint(home, cfg, *cancelOwn, out, errOut)
	if done {
		return code
	}
	if checkpoint.Committed || checkpoint.SlotID != "" && cfg.Storage.R2CredentialRef == "issued-"+checkpoint.SlotID {
		if *cancelOwn {
			return machineCommandError(errOut, errors.New("dedicated key already committed; cancellation refused"))
		}
		checkpoint.Committed = true
		return finishOwnKey(home, checkpoint, out, errOut, env)
	}
	if cfg.MachineAssignment != nil && cfg.MachineAssignment.DestinationID == cfg.DestinationID() && cfg.MachineAssignment.Kind == config.MachineAssignmentR2Own && checkpoint.SlotID == "" {
		if *cancelOwn {
			return machineCommandError(errOut, errors.New("dedicated key already committed; cancellation refused"))
		}
		terminal.Println(out, "This machine already has locally committed dedicated ownership; no key created.")
		publishOwnKeyRegistration(home, out, env)
		return 0
	}
	p := newPrompter(stdin, out)
	defer p.close()
	interactive := !*yes && env.interactive(stdin)
	if err = confirmOwnKey(p, *yes, interactive); err != nil {
		return machineCommandError(errOut, err)
	}
	token, _, _, err := readManagementToken(context.Background(), p, env, cfg.CloudflareTokenCommand, interactive)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	api := env.cloudflareAPI(token)
	defer api.Discard()
	issuer, err := newKeyIssuer(home, cfg, env, p, api)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if *cancelOwn {
		return cancelStagedOwnKey(home, checkpoint, issuer, out, errOut, env)
	}
	if err = local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
		return machineCommandError(errOut, err)
	}
	slot, err := ownKeySlot(home, &checkpoint, issuer, api, env)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	next := cfg
	next.Storage.R2CredentialRef = slot.SecretRef
	next.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Own, AccessKeyID: slot.ProviderID, RecipientID: slot.RecipientID, IssuerID: slot.IssuerID, SlotID: slot.SlotID}
	if err = next.ValidateMachine(); err != nil {
		return machineCommandError(errOut, err)
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	executable, err := env.executable()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if err = applySetup(home, userHome, executable, cfg, &next, nil, env); err != nil {
		return machineCommandError(errOut, errors.New("dedicated key remains staged; setup did not commit, retry own-key after recovery; shared access unchanged"))
	}
	checkpoint.Committed = true
	checkpoint.CleanupPending = true
	if err = local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
		return machineCommandError(errOut, errors.New("dedicated key committed; cleanup checkpoint pending, retry own-key"))
	}
	return finishOwnKey(home, checkpoint, out, errOut, env)
}

// Caller holds setup.lock and issued.lock, including local retirement/cancel.
func prepareOwnKeyCheckpoint(home string, cfg config.Config, cancelStage bool, out, errOut io.Writer) (ownKeyCheckpoint, bool, int) {
	path := filepath.Join(home, ownKeyFile)
	_, statErr := os.Stat(path)
	if cancelStage && statErr == nil {
		var prior ownKeyCheckpoint
		if err := local.Read(path, &prior); err != nil {
			return prior, true, machineCommandError(errOut, errors.New("own-key checkpoint unreadable; cancellation refused"))
		}
		if prior.Committed {
			return prior, true, machineCommandError(errOut, errors.New("dedicated key already committed; cancellation refused"))
		}
	}
	checkpoint, err := ownKeyCheckpointForCommand(home, cfg, cancelStage)
	if err != nil {
		return checkpoint, true, machineCommandError(errOut, err)
	}
	if cancelStage && checkpoint.SlotID == "" && !checkpoint.Committed && !checkpoint.CleanupPending {
		if statErr != nil {
			return checkpoint, true, machineCommandError(errOut, errors.New("no staged own-key operation to cancel"))
		}
		terminal.Println(out, "Pre-slot own-key checkpoint cancelled. Provider and local access unchanged.")
		return checkpoint, true, 0
	}
	return checkpoint, false, 0
}

func retirePreSlotOwnKeyCheckpoint(home string, cfg config.Config, checkpoint ownKeyCheckpoint) (ownKeyCheckpoint, error) {
	// Before the owning callback records a slot, no provider call is allowed.
	// Still fail closed on orphaned intent or any remaining cleanup proof.
	if checkpoint.CleanupPending || checkpoint.OldRef == "" || checkpoint.DestinationID == "" || !config.SafeMachineText(checkpoint.DestinationID, 128) {
		return checkpoint, errors.New("pre-slot own-key cleanup proof unavailable; checkpoint retained")
	}
	slots, err := issuance.List(home)
	if err != nil {
		return checkpoint, err
	}
	for _, slot := range slots {
		if slot.IssuerID == cfg.MachineID && slot.RecipientID == cfg.MachineID && (slot.SecretRemovalPending || slot.State != issuance.Own && slot.State != issuance.Deleted) {
			return checkpoint, errors.New("unbound own-key issuance remains; checkpoint retained")
		}
	}
	if err = os.Remove(filepath.Join(home, ownKeyFile)); err != nil {
		return checkpoint, err
	}
	return ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}, nil
}

// A staged migration belongs to its exact pre-migration reference. Ordinary
// setup can replace that reference without changing the destination. Refuse a
// stale resume before token acquisition, but preserve safe cancellation and
// recovery after the exact staged slot has already committed.
func ownKeyCheckpointForCommand(home string, cfg config.Config, cancelStage bool) (ownKeyCheckpoint, error) {
	checkpoint, err := readOwnKeyCheckpoint(home, cfg)
	if err != nil || cancelStage || checkpoint.Committed || cfg.Storage.R2CredentialRef == checkpoint.OldRef {
		return checkpoint, err
	}
	if checkpoint.SlotID != "" && cfg.Storage.R2CredentialRef == "issued-"+checkpoint.SlotID {
		return checkpoint, nil
	}
	return checkpoint, errors.New("staged own-key source credential changed; stage retained, cancel it safely with own-key --cancel before starting a new migration")
}

func readOwnKeyCheckpoint(home string, cfg config.Config) (ownKeyCheckpoint, error) {
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	path := filepath.Join(home, ownKeyFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return checkpoint, nil
	}
	// Existing journals must supply their own proof, not inherit current config.
	checkpoint = ownKeyCheckpoint{}
	if err := local.Read(path, &checkpoint); err != nil {
		return checkpoint, errors.New("own-key checkpoint unreadable; no new key created")
	}
	if checkpoint.SlotID != "" && !config.ValidMachineID(checkpoint.SlotID) || !config.SafeMachineText(checkpoint.OldRef, 128) {
		return checkpoint, errors.New("own-key checkpoint is invalid")
	}
	// Older completed checkpoints may survive a later setup. Retire only after
	// the exact old slot proves committed ownership; pending cleanup/stages stay.
	if checkpoint.Committed && !checkpoint.CleanupPending {
		slots, err := issuance.List(home)
		if err != nil {
			return checkpoint, err
		}
		for _, slot := range slots {
			if slot.SlotID == checkpoint.SlotID && slot.DestinationID == checkpoint.DestinationID && slot.State == issuance.Own && slot.IssuerID == cfg.MachineID && slot.RecipientID == cfg.MachineID {
				if err = os.Remove(path); err != nil {
					return checkpoint, err
				}
				return ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}, nil
			}
		}
		return checkpoint, errors.New("completed own-key ownership unavailable; checkpoint retained")
	}
	if checkpoint.SlotID == "" && !checkpoint.Committed {
		return retirePreSlotOwnKeyCheckpoint(home, cfg, checkpoint)
	}
	if checkpoint.DestinationID != cfg.DestinationID() {
		return checkpoint, errors.New("own-key checkpoint does not match this destination")
	}
	return checkpoint, nil
}

func ownKeySlot(home string, checkpoint *ownKeyCheckpoint, issuer *keyIssuer, api cloudflare.API, env Env) (issuance.Slot, error) {
	var slot issuance.Slot
	if checkpoint.SlotID == "" {
		created, _, err := issuer.createWithIntent(issuance.Fresh, issuer.cfg.MachineID, func(intent issuance.Slot) error {
			checkpoint.SlotID = intent.SlotID
			checkpoint.CleanupPending = true
			return local.Write(filepath.Join(home, ownKeyFile), checkpoint)
		})
		if err != nil {
			return slot, err
		}
		slot = created
		checkpoint.SlotID = slot.SlotID
		checkpoint.CleanupPending = true
		if err = local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
			return slot, errors.New("own key staged; checkpoint write failed, inspect issuance before retrying")
		}
	} else {
		slots, err := issuance.List(home)
		if err != nil {
			return slot, err
		}
		for _, candidate := range slots {
			if candidate.SlotID == checkpoint.SlotID {
				slot = candidate
				break
			}
		}
		if slot.SlotID == "" || slot.DestinationID != issuer.cfg.DestinationID() || slot.IssuerID != issuer.cfg.MachineID || slot.RecipientID != issuer.cfg.MachineID {
			return slot, errors.New("own-key staged lineage mismatched")
		}
	}
	if err := issuer.recoverOwnKeyStage(&slot); err != nil {
		return slot, err
	}
	if slot.State == issuance.Deleted {
		if slot.SecretRemovalPending {
			return slot, errors.New("own-key secret removal remains pending; stage retained")
		}
		checkpoint.SlotID = ""
		checkpoint.CleanupPending = false
		if err := local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
			return slot, err
		}
		return ownKeySlot(home, checkpoint, issuer, api, env)
	}
	if slot.State != issuance.OwnIntent {
		return slot, errors.New("own-key creation or cleanup remains uncertain; no duplicate key created")
	}
	reader, ok := api.(cloudflare.InventoryAPI)
	if !ok {
		return slot, errors.New("provider cannot verify dedicated ownership")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	metadata, err := reader.TokenDetails(ctx, issuer.account, slot.ProviderID)
	if err != nil || metadata.ID != slot.ProviderID || metadata.Name != slot.ProviderName || !cloudflare.ExactBucketPolicy(metadata, issuer.account, issuer.bucket, issuer.group) {
		return slot, errors.New("staged dedicated metadata not independently verified")
	}
	kc, err := env.credentialStore()
	if err != nil {
		return slot, err
	}
	key, err := credentials.LoadStored(ctx, kc, slot.SecretRef)
	if err != nil || key.AccessKeyID != slot.ProviderID {
		return slot, errors.New("staged dedicated credential missing")
	}
	verifier := r2Creator{env: env, p: issuer.p, account: issuer.account, bucket: cloudflare.BucketSpec{BucketRef: issuer.bucket}}
	if err = verifier.checkKey(ctx, key); err != nil {
		return slot, errors.New("staged dedicated key did not pass storage check; previous credential unchanged")
	}
	return slot, nil
}

func finishOwnKey(home string, checkpoint ownKeyCheckpoint, out, errOut io.Writer, env Env) int {
	// Commit has completed. Only now may the old local secret be removed.
	release, err := lockCollector(home, "own-key cleanup", env.now())
	if err != nil {
		return machineCommandError(errOut, err)
	}
	hooksRelease, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		release()
		return machineCommandError(errOut, err)
	}
	searchRelease, err := local.NamedLockWait(home, "search-worker.lock", 2*time.Second)
	if err != nil {
		hooksRelease()
		release()
		return machineCommandError(errOut, errors.New("search indexing is finishing; old access retained"))
	}
	searchRelease = releaseOnce(searchRelease)
	defer searchRelease()
	current, found, err := config.Load(home)
	if err != nil || !found || current.Storage.R2CredentialRef != "issued-"+checkpoint.SlotID || current.DestinationID() != checkpoint.DestinationID {
		searchRelease()
		hooksRelease()
		release()
		return machineCommandError(errOut, errors.New("committed own-key destination changed; old access retained"))
	}
	if err = promoteCommittedOwnKey(home, current, checkpoint); err != nil {
		searchRelease()
		hooksRelease()
		release()
		return machineCommandError(errOut, err)
	}
	retained := oldSharedAccessNeeded(current, checkpoint.OldRef, env)
	checkpoint.Committed = true
	checkpoint.CleanupPending = retained
	if !retained && checkpoint.OldRef != "" && checkpoint.OldRef != current.Storage.R2CredentialRef {
		kc, e := env.credentialStore()
		if e == nil {
			e = kc.Delete(context.Background(), checkpoint.OldRef)
		}
		checkpoint.CleanupPending = e != nil
		if e == nil {
			current.RetiredCredentialRefs = slices.DeleteFunc(current.RetiredCredentialRefs, func(ref string) bool { return ref == checkpoint.OldRef })
			err = config.Save(home, current)
		}
	}
	if err == nil {
		if checkpoint.CleanupPending {
			err = local.Write(filepath.Join(home, ownKeyFile), checkpoint)
		} else {
			err = os.Remove(filepath.Join(home, ownKeyFile))
		}
	}
	searchRelease()
	hooksRelease()
	release()
	if err != nil {
		return machineCommandError(errOut, errors.New("dedicated key committed; old access cleanup pending"))
	}
	terminal.Println(out, "Dedicated key committed through setup's transaction. The shared provider key was not deleted.")
	if checkpoint.CleanupPending {
		terminal.Println(out, "Old shared access remains locally present or needed by another local destination; cleanup pending.")
	} else {
		terminal.Println(out, "Old shared local secret removed after commit.")
	}
	publishOwnKeyRegistration(home, out, env)
	return 0
}

func publishOwnKeyRegistration(home string, out io.Writer, env Env) {
	if err := publishMachineAfterSetup(home, env); err != nil {
		terminal.Println(out, "Machine registration pending; committed dedicated ownership is retained.")
	}
}

// References can alias the same provider credential. Preserve truthful retained
// access reporting, and keep cleanup pending when another binding is unknown.
func oldSharedAccessNeeded(cfg config.Config, oldRef string, env Env) bool {
	if oldRef == "" {
		return false
	}
	// Retired references can keep the same shared provider key usable even when
	// setup merely changed its local reference at the current destination.
	for _, ref := range cfg.RetiredCredentialRefs {
		if ref == oldRef || ref == cfg.Storage.R2CredentialRef {
			continue
		}
		kc, err := env.credentialStore()
		if err != nil {
			return true
		}
		old, err := credentials.LoadStored(context.Background(), kc, oldRef)
		if err != nil || old.AccessKeyID == "" {
			return true
		}
		retired, err := credentials.LoadStored(context.Background(), kc, ref)
		if err != nil || retired.AccessKeyID == "" || retired.AccessKeyID == old.AccessKeyID {
			return true
		}
	}
	for _, destination := range cfg.PreviousDestinations {
		if destination.Provider != credentials.ProviderR2 {
			continue
		}
		if destination.R2CredentialRef == oldRef {
			return true
		}
		kc, err := env.credentialStore()
		if err != nil {
			return true
		}
		old, err := credentials.LoadStored(context.Background(), kc, oldRef)
		if err != nil || old.AccessKeyID == "" {
			return true
		}
		other, err := credentials.LoadStored(context.Background(), kc, destination.R2CredentialRef)
		if err != nil || other.AccessKeyID == "" || other.AccessKeyID == old.AccessKeyID {
			return true
		}
	}
	return false
}

// Caller holds issued.lock and the collector lock, so config and ledger proof
// remain stable across the final promotion. A crash before this step leaves
// OwnIntent ineligible for spare delivery or ordinary automatic cleanup.
func promoteCommittedOwnKey(home string, cfg config.Config, checkpoint ownKeyCheckpoint) error {
	slots, err := issuance.List(home)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		if slot.SlotID != checkpoint.SlotID {
			continue
		}
		assignment := cfg.MachineAssignment
		if assignment == nil || assignment.Kind != config.MachineAssignmentR2Own || assignment.DestinationID != checkpoint.DestinationID || assignment.SlotID != slot.SlotID || assignment.AccessKeyID != slot.ProviderID || assignment.RecipientID != cfg.MachineID || assignment.IssuerID != cfg.MachineID || slot.RecipientID != cfg.MachineID || slot.IssuerID != cfg.MachineID || slot.DestinationID != checkpoint.DestinationID || cfg.Storage.R2CredentialRef != slot.SecretRef {
			return errors.New("committed own-key lineage mismatch; old access retained")
		}
		if slot.State == issuance.Own {
			return nil
		}
		if slot.State != issuance.OwnIntent {
			return errors.New("dedicated key is not a verified own intent; old access retained")
		}
		slot.State = issuance.Own
		return issuance.Save(home, slot)
	}
	return errors.New("committed own-key issuance missing; old access retained")
}

func cancelStagedOwnKey(home string, checkpoint ownKeyCheckpoint, issuer *keyIssuer, out, errOut io.Writer, env Env) int {
	if checkpoint.SlotID == "" {
		return machineCommandError(errOut, errors.New("no staged own-key operation to cancel"))
	}
	release, err := lockCollector(home, "own-key cancellation", env.now())
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer release()
	searchRelease, err := local.NamedLockWait(home, "search-worker.lock", 2*time.Second)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer searchRelease()
	slots, err := issuance.List(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	for _, slot := range slots {
		if slot.SlotID != checkpoint.SlotID {
			continue
		}
		if slot.DestinationID != checkpoint.DestinationID || slot.IssuerID != issuer.cfg.MachineID || slot.RecipientID != issuer.cfg.MachineID {
			return machineCommandError(errOut, errors.New("staged ownership mismatch; cancellation refused"))
		}
		switch slot.State {
		case issuance.OwnIntent:
			err = issuer.cleanupUncommittedOwnIntent(&slot)
		case issuance.CreationIntent, issuance.SecretIntent, issuance.CleanupPending:
			err = issuer.cleanupOwnKeyStage(&slot)
		case issuance.Spare, issuance.Reserved, issuance.DeliveryIntent, issuance.Delivered, issuance.Own:
			return machineCommandError(errOut, errors.New("slot is not owned by an uncommitted own-key transaction; cancellation refused"))
		case issuance.Deleted:
			err = issuer.retryDeletedOwnKeySecret(&slot)
		}
		if err != nil || slot.State != issuance.Deleted || slot.SecretRemovalPending {
			return machineCommandError(errOut, errors.New("staged cancellation remains pending; shared access unchanged"))
		}
		if err = os.Remove(filepath.Join(home, ownKeyFile)); err != nil {
			return machineCommandError(errOut, err)
		}
		terminal.Println(out, "Uncommitted dedicated key removed. Shared provider and local access retained.")
		return 0
	}
	return machineCommandError(errOut, errors.New("staged issuance unavailable; cancellation refused"))
}

func ownKeyConfiguration(home string) (config.Config, error) {
	if setupjournal.TransactionPending(home) {
		return config.Config{}, errors.New("setup recovery pending; run setup before own-key")
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return config.Config{}, err
	}
	if !found {
		return config.Config{}, errNotSetUp
	}
	if _, _, err = providerDestination(cfg.Storage); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func confirmOwnKey(p *prompter, yes, interactive bool) error {
	if yes {
		return nil
	}
	if !interactive {
		return errors.New("own-key needs an interactive terminal or deliberate --yes")
	}
	consent, err := p.guidedYesNo("Create and commit a dedicated key? The shared provider key stays valid for other users")
	if err != nil || !consent {
		return errors.New("own-key cancelled; nothing changed")
	}
	return nil
}

// cleanupUncommittedOwnIntent is only for an owning transaction whose durable
// journal proves this slot was never exposed before its config commit. The
// caller holds issued.lock and serializes config writers with collector.lock.
func (i *keyIssuer) cleanupUncommittedOwnIntent(s *issuance.Slot) error {
	if s.State != issuance.OwnIntent {
		return errors.New("own transaction cleanup requires own-intent")
	}
	if err := i.ownKeyCleanupSafe(*s); err != nil {
		return err
	}
	s.State = issuance.CleanupPending
	s.CleanupReason = "uncommitted-own-transaction"
	if err := issuance.Save(i.home, *s); err != nil {
		return err
	}
	i.cleanup(s)
	if s.State != issuance.Deleted {
		return errors.New("own-key cleanup pending; provider access may remain")
	}
	return nil
}

// Every recovery cleanup rechecks all committed local destinations under the
// collector lock, even after a crash has advanced the stage to CleanupPending.
func (i *keyIssuer) ownKeyCleanupSafe(s issuance.Slot) error {
	if setupjournal.TransactionPending(i.home) {
		return errors.New("setup recovery pending; own-key cleanup refused")
	}
	cfg, found, err := config.Load(i.home)
	if err != nil || !found || cfg.MachineID != s.IssuerID || cfg.DestinationID() != s.DestinationID {
		return errors.New("committed ownership unavailable; own-key cleanup pending")
	}
	if cfg.Storage.R2CredentialRef == s.SecretRef || (cfg.MachineAssignment != nil && cfg.MachineAssignment.AccessKeyID == s.ProviderID) {
		return errors.New("committed own-key binding retained; cleanup refused")
	}
	if cfg.Storage.Provider != credentials.ProviderR2 || cfg.Storage.R2CredentialRef == "" {
		return errors.New("active credential binding unavailable; own-key cleanup pending")
	}
	kc, err := i.env.credentialStore()
	if err != nil {
		return errors.New("active credential unavailable; own-key cleanup pending")
	}
	for _, destination := range append([]credentials.Config{cfg.Storage}, cfg.PreviousDestinations...) {
		if destination.Provider != credentials.ProviderR2 {
			continue
		}
		if destination.R2CredentialRef == "" || destination.R2CredentialRef == s.SecretRef {
			return errors.New("retained credential binding unavailable or uses staged key; cleanup refused")
		}
		active, loadErr := credentials.LoadStored(context.Background(), kc, destination.R2CredentialRef)
		if loadErr != nil || active.AccessKeyID == "" {
			return errors.New("retained credential unavailable; own-key cleanup pending")
		}
		if s.ProviderID != "" && active.AccessKeyID == s.ProviderID {
			return errors.New("active own-key binding retained; cleanup refused")
		}
	}
	return nil
}

// An uncertain creation may have no recorded provider ID. Resolve and persist
// that exact slot first, then compare every active/retained credential before
// cleanup. Generic orphan cleanup alone cannot rule out an aliased active key.
func (i *keyIssuer) cleanupOwnKeyStage(s *issuance.Slot) error {
	if s.ProviderID == "" {
		reader, ok := i.api.(cloudflare.InventoryAPI)
		if !ok {
			return errors.New("staged provider identity unavailable; own-key cleanup pending")
		}
		ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
		defer cancel()
		inventory, err := reader.TokenInventory(ctx, i.account)
		if err != nil || !inventory.PaginationComplete {
			return errors.New("staged provider inventory incomplete; own-key cleanup pending")
		}
		matches := 0
		for _, token := range inventory.Tokens {
			if token.Name == s.ProviderName {
				if !config.ValidMachineID(token.ID) || !cloudflare.ExactBucketPolicy(token, i.account, i.bucket, s.PermissionID) {
					return errors.New("staged provider scope mismatched; own-key cleanup refused")
				}
				s.ProviderID = token.ID
				matches++
			}
		}
		if matches != 1 {
			return errors.New("staged provider identity ambiguous; own-key cleanup pending")
		}
		if err = issuance.Save(i.home, *s); err != nil {
			return err
		}
	}
	if err := i.ownKeyCleanupSafe(*s); err != nil {
		return err
	}
	i.cleanup(s)
	return nil
}

// Recovery serializes local binding proof with collection/config mutation.
// The command already holds setup.lock and issued.lock.
func (i *keyIssuer) recoverOwnKeyStage(s *issuance.Slot) error {
	switch s.State {
	case issuance.CreationIntent, issuance.SecretIntent, issuance.CleanupPending:
	case issuance.Deleted:
		if !s.SecretRemovalPending {
			return nil
		}
	case issuance.Spare, issuance.Reserved, issuance.DeliveryIntent, issuance.Delivered, issuance.OwnIntent, issuance.Own:
		return nil
	default:
		return errors.New("unknown own-key stage; recovery refused")
	}
	release, err := lockCollector(i.home, "own-key recovery cleanup", i.env.now())
	if err != nil {
		return err
	}
	defer release()
	searchRelease, err := local.NamedLockWait(i.home, "search-worker.lock", 2*time.Second)
	if err != nil {
		return err
	}
	defer searchRelease()
	if s.State == issuance.Deleted {
		return i.retryDeletedOwnKeySecret(s)
	}
	return i.cleanupOwnKeyStage(s)
}

// Caller holds setup.lock, issued.lock and collector.lock. Provider deletion
// is already confirmed; retry only the exact local secret and persist success.
func (i *keyIssuer) retryDeletedOwnKeySecret(s *issuance.Slot) error {
	if s.State != issuance.Deleted {
		return errors.New("own-key provider deletion is not confirmed; secret retained")
	}
	if !s.SecretRemovalPending {
		return nil
	}
	if s.Validate() != nil || s.CleanupReason != "provider-delete-confirmed" || !config.ValidMachineID(s.ProviderID) || s.IssuerID != i.cfg.MachineID || s.RecipientID != i.cfg.MachineID {
		return errors.New("deleted own-key identity unavailable; secret cleanup pending")
	}
	if err := i.ownKeyCleanupSafe(*s); err != nil {
		return err
	}
	cfg, found, err := config.Load(i.home)
	if err != nil || !found {
		return errors.New("committed ownership unavailable; secret cleanup pending")
	}
	kc, err := i.env.credentialStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	for _, ref := range cfg.RetiredCredentialRefs {
		if ref == s.SecretRef {
			return errors.New("retired own-key reference retained; secret cleanup refused")
		}
		key, loadErr := credentials.LoadStored(ctx, kc, ref)
		if loadErr != nil || key.AccessKeyID == "" || key.AccessKeyID == s.ProviderID {
			return errors.New("retired credential identity unknown or aliases own key; secret retained")
		}
	}
	key, err := credentials.LoadStored(ctx, kc, s.SecretRef)
	if err != nil && !errors.Is(err, credentials.ErrKeychainItemNotFound) && !errors.Is(err, credentials.ErrCredentialFileNotFound) {
		return errors.New("staged credential identity unavailable; secret cleanup pending")
	}
	if err == nil && key.AccessKeyID != s.ProviderID {
		return errors.New("staged credential identity changed; secret cleanup refused")
	}
	if err = kc.Delete(ctx, s.SecretRef); err != nil {
		return err
	}
	next := *s
	next.SecretRemovalPending = false
	if err = issuance.Save(i.home, next); err != nil {
		return err
	}
	*s = next
	return nil
}
