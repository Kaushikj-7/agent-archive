// Package revocation persists secret-free, monotonic per-key provider outcomes.
// Callers hold revocations.lock for the entire local operation and retry.
package revocation

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Outcome never treats a request or an unverified 404 as confirmed deletion.
type Outcome string

const (
	// Pending means no independently confirmed provider removal.
	Pending Outcome = "pending"
	// Confirmed means this operation received a successful verified delete.
	Confirmed Outcome = "confirmed"
	// Unknown preserves failed or ambiguous provider outcomes, including 404.
	Unknown Outcome = "failed-or-unknown"
)

// Key contains verified immutable metadata, never credential values.
type Key struct {
	ProviderID  string  `json:"provider_id"`
	RecipientID string  `json:"recipient_id"`
	IssuerID    string  `json:"issuer_id"`
	SlotID      string  `json:"slot_id"`
	Outcome     Outcome `json:"outcome"`
}

// SelectorKind identifies the unverified request's form, never its authority.
type SelectorKind string

const (
	// RequestedName is a caller-supplied display name.
	RequestedName SelectorKind = "name"
	// RequestedMachineID is a caller-supplied immutable machine ID.
	RequestedMachineID SelectorKind = "machine_id"
	// RequestedRecipientID is a caller-supplied recipient ID.
	RequestedRecipientID SelectorKind = "recipient_id"
	// RequestedPairingID is a caller-supplied pairing ID.
	RequestedPairingID SelectorKind = "pairing_id"
)

// RequestedSelector records the caller's unverified request, never deletion authority.
type RequestedSelector struct {
	Kind  SelectorKind `json:"kind"`
	Value string       `json:"value"`
}

func (r RequestedSelector) valid() bool {
	switch r.Kind {
	case RequestedName:
		return config.ValidMachineName(r.Value)
	case RequestedMachineID, RequestedRecipientID, RequestedPairingID:
		return config.ValidMachineID(r.Value)
	default:
		return false
	}
}

// Journal separates confirmed provider outcomes from informational publication.
type Journal struct {
	Version            int                `json:"version"`
	OperationID        string             `json:"operation_id"`
	DestinationID      string             `json:"destination_id"`
	RequesterID        string             `json:"requester_id"`
	TargetID           string             `json:"target_id,omitempty"`
	RequestedSelector  *RequestedSelector `json:"requested_selector,omitempty"`
	AccountID          string             `json:"account_id"`
	Bucket             string             `json:"bucket"`
	Jurisdiction       string             `json:"jurisdiction,omitempty"`
	PermissionID       string             `json:"permission_id,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	IncludeIssued      bool               `json:"include_issued"`
	InventoryComplete  bool               `json:"account_inventory_complete"`
	Keys               []Key              `json:"keys"`
	PublicationPending bool               `json:"publication_pending"`
	RequestOnly        bool               `json:"request_only"`
}

// Validate bounds and verifies journal identifiers before retries or publication.
func (j *Journal) Validate() error {
	bad := errors.New("revocation journal is invalid; no provider operation started")
	if j.Version != 1 || j.CreatedAt.IsZero() || len(j.Keys) > 128 || len(j.DestinationID) != 64 || strings.Trim(j.DestinationID, "0123456789abcdef") != "" || !config.ValidMachineID(j.OperationID) || !config.ValidMachineID(j.RequesterID) || !config.SafeMachineText(j.Bucket, 63) || j.InventoryComplete {
		return bad
	}
	if j.Jurisdiction != "" && !cloudflare.ValidJurisdiction(j.Jurisdiction) || j.RequestOnly && len(j.Keys) != 0 || len(j.Keys) > 0 && (!config.ValidMachineID(j.AccountID) || !config.ValidMachineID(j.PermissionID) || cloudflare.ValidateBucketName(j.Bucket) != nil) {
		return bad
	}
	if j.RequestedSelector != nil && !j.RequestedSelector.valid() {
		return bad
	}
	for _, id := range []string{j.TargetID, j.AccountID, j.PermissionID} {
		if id != "" && !config.ValidMachineID(id) {
			return bad
		}
	}
	seen := map[string]bool{}
	for _, key := range j.Keys {
		for _, id := range []string{key.ProviderID, key.RecipientID, key.IssuerID, key.SlotID} {
			if !config.ValidMachineID(id) {
				return bad
			}
		}
		if seen[key.ProviderID] {
			return bad
		}
		seen[key.ProviderID] = true
		switch key.Outcome {
		case Pending, Confirmed, Unknown:
		default:
			return bad
		}
	}
	return nil
}

// Path returns the private local journal path for a canonical operation ID.
func Path(home, id string) string { return filepath.Join(home, "revocations", "operation-"+id+".json") }

// Save persists exact selection and outcomes atomically before provider changes.
func Save(home string, j Journal) error {
	if err := j.Validate(); err != nil {
		return err
	}
	return local.Write(Path(home, j.OperationID), j)
}

// Load reads a previously committed local operation; bucket objects are not loaded.
func Load(home, id string) (Journal, error) {
	var j Journal
	if !config.ValidMachineID(id) {
		return j, errors.New("invalid operation ID")
	}
	f, err := os.Open(Path(home, id))
	if err != nil {
		return j, errors.New("cannot read local revocation journal")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, &j) != nil {
		return j, errors.New("local revocation journal is malformed or oversized")
	}
	if j.OperationID != id {
		return j, errors.New("revocation operation ID mismatch")
	}
	err = j.Validate()
	return j, err
}

// Record changes an outcome while preserving independently confirmed success.
func (j *Journal) Record(id string, outcome Outcome) {
	for n := range j.Keys {
		if j.Keys[n].ProviderID == id && j.Keys[n].Outcome != Confirmed {
			j.Keys[n].Outcome = outcome
			return
		}
	}
}

// Complete is true only for a nonempty verified set of confirmed provider deletes.
func (j *Journal) Complete() bool {
	if len(j.Keys) == 0 || j.RequestOnly {
		return false
	}
	for _, k := range j.Keys {
		if k.Outcome != Confirmed {
			return false
		}
	}
	return true
}

// Summary distinguishes requested, partial and ambiguous results without proving bucket claims.
func (j *Journal) Summary() string {
	if j.RequestOnly || len(j.Keys) == 0 {
		return "requested; access not removed"
	}
	confirmed, unknown := 0, false
	for _, key := range j.Keys {
		if key.Outcome == Confirmed {
			confirmed++
		}
		unknown = unknown || key.Outcome == Unknown
	}
	if confirmed == len(j.Keys) {
		return "provider-confirmed for selected key set; account completeness unknown"
	}
	if confirmed > 0 {
		return "partial; access not verified removed for remaining keys"
	}
	if unknown {
		return "failed or unknown; access not verified removed"
	}
	return "requested; provider results pending"
}

// List reads bounded local operation snapshots for informational status only.
func List(home string) ([]Journal, error) {
	entries, err := os.ReadDir(filepath.Join(home, "revocations"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("revocation progress unreadable")
	}
	if len(entries) > 1000 {
		return nil, errors.New("revocation progress exceeds entry limit")
	}
	var result []Journal
	for _, entry := range entries {
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "operation-"), ".json")
		if entry.Name() != "operation-"+id+".json" || !entry.Type().IsRegular() {
			continue
		}
		j, err := Load(home, id)
		if err != nil {
			return result, err
		}
		result = append(result, j)
	}
	return result, nil
}
