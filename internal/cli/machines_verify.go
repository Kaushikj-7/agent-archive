package cli

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type providerObservationState string

const (
	observationLegacy        providerObservationState = "legacy_or_unknown_binding"
	observationMissing       providerObservationState = "missing_or_not_visible"
	observationScope         providerObservationState = "scope_unknown_or_mismatch"
	observationInactive      providerObservationState = "provider_key_not_active"
	observationIssuance      providerObservationState = "issuance_unknown_or_mismatch"
	observationMatches       providerObservationState = "provider_metadata_matches_claim"
	observationLocalMismatch providerObservationState = "local_binding_mismatch"
)

type providerObservationRole string

const (
	observationCurrent           providerObservationRole = "current"
	observationUnusedSpare       providerObservationRole = "unused_spare_claim"
	observationRetiredCredential providerObservationRole = "retired_credential_claim"
)

type machineProviderObservation struct {
	Role        providerObservationRole  `json:"role,omitempty"`
	MachineID   string                   `json:"machine_id"`
	AccessKeyID string                   `json:"access_key_id,omitempty"`
	State       providerObservationState `json:"state"`
	Binding     string                   `json:"binding"`
}

type providerVerification struct {
	CheckedAt                time.Time                    `json:"checked_at"`
	PaginationComplete       bool                         `json:"pagination_complete"`
	AccountInventoryComplete bool                         `json:"account_inventory_complete"`
	Visibility               string                       `json:"visibility"`
	Partial                  bool                         `json:"partial"`
	Diagnostic               string                       `json:"diagnostic,omitempty"`
	Observations             []machineProviderObservation `json:"observations"`
	Unclaimed                []string                     `json:"claim_not_observed,omitempty"`
}

type verifiedMachinesResult struct {
	machines.ListResult
	Verification providerVerification `json:"verification"`
}

func providerDestination(cfg credentials.Config) (string, cloudflare.BucketRef, error) {
	var ref cloudflare.BucketRef
	if cfg.Provider != credentials.ProviderR2 || cloudflare.ValidateBucketName(cfg.Bucket) != nil {
		return "", ref, errors.New("provider verification requires a Cloudflare R2 bucket")
	}
	endpoint := cfg.R2Endpoint
	if endpoint == "" {
		endpoint = cloudflare.Endpoint(cfg.R2AccountID, "")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Port() != "" {
		return "", ref, errors.New("provider verification refuses this storage endpoint")
	}
	host := strings.Split(u.Host, ".")
	if len(host) != 4 && len(host) != 5 {
		return "", ref, errors.New("provider verification requires a Cloudflare endpoint")
	}
	account := host[0]
	if !config.ValidMachineID(account) || (cfg.R2AccountID != "" && cfg.R2AccountID != account) {
		return "", ref, errors.New("provider account does not match the configured endpoint")
	}
	jurisdiction := ""
	if len(host) == 5 {
		jurisdiction = host[1]
		if !cloudflare.ValidJurisdiction(jurisdiction) {
			return "", ref, errors.New("unsupported Cloudflare jurisdiction")
		}
	}
	if strings.TrimSuffix(endpoint, "/") != cloudflare.Endpoint(account, jurisdiction) {
		return "", ref, errors.New("provider verification refuses this storage endpoint")
	}
	return account, cloudflare.BucketRef{Name: cfg.Bucket, Jurisdiction: jurisdiction}, nil
}

func runMachinesVerify(cfg config.Config, listing machines.ListResult, stdin io.Reader, out, errOut io.Writer, env Env, asJSON, unattended bool) int {
	account, bucket, err := providerDestination(cfg.Storage)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	p := newPrompter(stdin, errOut)
	defer p.close()
	interactive := !unattended && !asJSON && env.interactive(stdin)
	if interactive {
		p.tokenHelpers = []string{"Explicit read-only provider check. Management token stays in memory; use Account API Tokens Read or Write.", cloudflare.TokenDashboardURL}
	}
	token, _, _, err := readManagementToken(context.Background(), p, env, cfg.CloudflareTokenCommand, interactive)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	api := env.cloudflareAPI(token)
	defer api.Discard()
	inventoryAPI, ok := api.(cloudflare.InventoryAPI)
	if !ok {
		return machineCommandError(errOut, errors.New("provider client cannot read token inventory"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	home, homeErr := env.readHome()
	var slots []issuance.Slot
	if homeErr == nil {
		slots, homeErr = issuance.List(home)
	}
	report := verifyProvider(ctx, cfg, listing, api, inventoryAPI, account, bucket, env.now(), slots)
	if homeErr != nil {
		report.Partial = true
		report.Diagnostic = "local_issuance_unreadable"
	}
	listing.ProviderVerified = !listing.Partial && len(listing.Unreadable) == 0 && !report.Partial && report.PaginationComplete
	result := verifiedMachinesResult{ListResult: listing, Verification: report}
	if asJSON {
		if err := writeCompactJSON(out, result); err != nil {
			return machineCommandError(errOut, err)
		}
	} else {
		terminal.Printf(out, "Provider check at %s.\n", report.CheckedAt.Format(time.RFC3339))
		for _, omitted := range listing.Unreadable {
			terminal.Printf(out, "Omitted %s: %s.\n", omitted.Key, omitted.Reason)
		}
		if listing.Partial {
			terminal.Println(out, "Bucket machine listing is incomplete; provider observations cover only readable records.")
		}
		rows := [][]string{}
		for _, observation := range report.Observations {
			rows = append(rows, []string{observation.MachineID, observation.AccessKeyID, string(observation.Role), string(observation.State), string(observation.Binding)})
		}
		guidedRows(out, []string{"Machine ID", "Provider key", "Role", "State", "Binding"}, rows)
		terminal.Println(out)
		for _, id := range report.Unclaimed {
			terminal.Printf(out, "Provider key %s: claim not observed.\n", id)
		}
		if report.Diagnostic != "" {
			terminal.Printf(out, "Provider check incomplete: %s.\n", report.Diagnostic)
		}
		terminal.Println(out, "Account inventory completeness is unknown: the token may list only keys it created. Not observed can mean missing or not visible.")
		terminal.Println(out, "Bucket claims are untrusted; matching provider metadata alone does not establish machine ownership or access removal.")
	}
	if report.Partial || listing.Partial || len(listing.Unreadable) > 0 {
		return 1
	}
	return 0
}

func verifyProvider(ctx context.Context, cfg config.Config, listing machines.ListResult, api cloudflare.API, reader cloudflare.InventoryAPI, account string, bucket cloudflare.BucketRef, now time.Time, localSlots ...[]issuance.Slot) providerVerification {
	inventory, err := reader.TokenInventory(ctx, account)
	report := newProviderVerification(now, inventory.PaginationComplete)
	if err != nil {
		report.Partial = true
		report.Diagnostic = "token_listing_failed_or_incomplete"
	}
	tokens := map[string]cloudflare.TokenMetadata{}
	for _, token := range inventory.Tokens {
		tokens[token.ID] = token
	}
	groups, groupErr := api.PermissionGroups(ctx, account, cloudflare.PermissionBucketItemWrite)
	permissionID := ""
	if groupErr == nil {
		permissionID, groupErr = verificationPermissionGroup(groups)
	}
	if groupErr != nil {
		report.Partial = true
		report.Diagnostic = "permission_evidence_unavailable"
	}
	claimed := map[string]bool{}
	details := 0
	for _, record := range listing.Records {
		if record.CredentialHistoryPartial {
			report.Partial = true
			report.Diagnostic = "credential_history_claim_incomplete"
		}
		bindings := append([]machines.CredentialBinding{record.Credential}, record.UnusedSpares...)
		bindings = append(bindings, record.RetiredCredentials...)
		for index, binding := range bindings {
			role := observationCurrent
			if index > 0 && index <= len(record.UnusedSpares) {
				role = observationUnusedSpare
			} else if index > 0 {
				role = observationRetiredCredential
			}
			claimed[binding.AccessKeyID] = true
			if !config.ValidMachineID(binding.AccessKeyID) {
				report.Observations = append(report.Observations, machineProviderObservation{Role: role, MachineID: record.MachineID, State: observationLegacy, Binding: "untrusted_bucket_claim"})
				continue
			}
			token, found := tokens[binding.AccessKeyID]
			if (!found || len(token.Policies) == 0) && details < 16 && ctx.Err() == nil {
				details++
				detail, detailErr := reader.TokenDetails(ctx, account, binding.AccessKeyID)
				if detailErr == nil {
					token = detail
					found = true
					tokens[token.ID] = token
				}
			}
			state, complete := providerBindingState(token, found, binding, account, bucket, permissionID, now)
			if !complete {
				report.Partial = true
			}

			locallyCommitted, mismatch := locallyCommittedProviderBinding(cfg, record.MachineID, binding, role, localSlots)
			if mismatch {
				state = observationLocalMismatch
				report.Partial = true
			}
			report.Observations = append(report.Observations, machineProviderObservation{Role: role, MachineID: record.MachineID, AccessKeyID: binding.AccessKeyID, State: state, Binding: providerBindingTrust(locallyCommitted)})
		}
	}
	for _, token := range inventory.Tokens {
		if _, known := cloudflare.ParseProviderName(token.Name); known && !claimed[token.ID] && cloudflare.ExactBucketPolicy(token, account, bucket, permissionID) {
			report.Unclaimed = append(report.Unclaimed, token.ID)
		}
	}
	if ctx.Err() != nil {
		report.Partial = true
		report.Diagnostic = "provider_budget_exhausted"
	}
	return report
}

// verificationPermissionGroup reads identity, not the caller's right to mint it.
// Conflicting or malformed identifiers cannot establish exact policy evidence.
func verificationPermissionGroup(groups []cloudflare.PermissionGroup) (string, error) {
	id := ""
	for _, group := range groups {
		if group.Name != cloudflare.PermissionBucketItemWrite {
			continue
		}
		if !config.ValidMachineID(group.ID) || id != "" && id != group.ID {
			return "", errors.New("permission group identity is invalid or ambiguous")
		}
		id = group.ID
	}
	if id == "" {
		return "", errors.New("permission group identity is unavailable")
	}
	return id, nil
}

func newProviderVerification(now time.Time, paginationComplete bool) providerVerification {
	return providerVerification{CheckedAt: now.UTC(), Visibility: "unknown_may_be_creator_only", Observations: []machineProviderObservation{}, PaginationComplete: paginationComplete}
}

func providerBindingTrust(locallyCommitted bool) string {
	if locallyCommitted {
		return "local_committed_binding"
	}
	return "untrusted_bucket_claim"
}

func providerBindingState(token cloudflare.TokenMetadata, found bool, binding machines.CredentialBinding, account string, bucket cloudflare.BucketRef, permissionID string, now time.Time) (providerObservationState, bool) {
	if !found {
		return observationMissing, false
	}
	if !cloudflare.ExactBucketPolicy(token, account, bucket, permissionID) {
		return observationScope, false
	}
	if !providerTokenActive(token, now) {
		return observationInactive, false
	}
	if binding.Kind != config.MachineAssignmentR2Own {
		return observationLegacy, true
	}
	id, known := cloudflare.ParseProviderName(token.Name)
	if !known || id.RecipientID != binding.RecipientID || id.IssuerID != binding.IssuerID || id.SlotID != binding.SlotID {
		return observationIssuance, false
	}
	return observationMatches, true
}

func providerTokenActive(token cloudflare.TokenMetadata, now time.Time) bool {
	if token.Status != cloudflare.TokenStatusActive {
		return false
	}
	if token.ExpiresOn != "" {
		expiry, err := time.Parse(time.RFC3339, token.ExpiresOn)
		if err != nil || !now.Before(expiry) {
			return false
		}
	}
	if token.NotBefore != "" {
		start, err := time.Parse(time.RFC3339, token.NotBefore)
		if err != nil || now.Before(start) {
			return false
		}
	}
	return true
}

func locallyCommittedProviderBinding(cfg config.Config, machineID string, binding machines.CredentialBinding, role providerObservationRole, localSlots [][]issuance.Slot) (bool, bool) {
	if machineID != cfg.MachineID {
		return false, false
	}
	switch role {
	case observationCurrent:
		local := cfg.MachineAssignment
		if local == nil || local.DestinationID != cfg.DestinationID() {
			return false, false
		}
		matches := providerAssignmentMatches(binding, *local)
		return matches, !matches
	case observationRetiredCredential:
		for _, retired := range cfg.RetiredMachineAssignments {
			if retired.DestinationID == cfg.DestinationID() && providerAssignmentMatches(binding, retired) {
				return true, false
			}
		}
	case observationUnusedSpare:
		for _, slots := range localSlots {
			for _, slot := range slots {
				if providerSpareMatches(cfg, binding, slot) {
					return true, false
				}
			}
		}
	}
	return false, true
}

func providerAssignmentMatches(binding machines.CredentialBinding, a config.MachineAssignment) bool {
	return a.AccessKeyID == binding.AccessKeyID && a.RecipientID == binding.RecipientID && a.IssuerID == binding.IssuerID && a.SlotID == binding.SlotID && a.Kind == binding.Kind
}

func providerSpareMatches(cfg config.Config, binding machines.CredentialBinding, slot issuance.Slot) bool {
	return slot.State == issuance.Spare && slot.DestinationID == cfg.DestinationID() && slot.IssuerID == cfg.MachineID && slot.ProviderID == binding.AccessKeyID && slot.RecipientID == binding.RecipientID && slot.SlotID == binding.SlotID
}
