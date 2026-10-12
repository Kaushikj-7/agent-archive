package catalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func exactHex(value string, bytes int) bool {
	if len(value) != bytes*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

var errCoordinatorDescriptor = errors.New("invalid catalog coordinator descriptor")

func (state admissions) validate() error {
	if state.Protocol != 10 || state.Generation == 0 || state.Owners == nil || len(state.Owners) > 256 {
		return errCoordinatorDescriptor
	}
	if state.Mode != admissionCandidate && state.Mode != admissionActive && state.Mode != admissionRollback {
		return errCoordinatorDescriptor
	}
	if state.Mode == admissionActive && state.Proof == "" {
		return errCoordinatorDescriptor
	}
	if state.Seal != "" && !exactHex(state.Seal, 24) {
		return errCoordinatorDescriptor
	}
	if state.Hold != "" && (state.Seal == "" || !exactHex(state.Hold, 24) || len(state.Owners) != 0) {
		return errCoordinatorDescriptor
	}
	if err := validateAdmissions(state.Owners); err != nil {
		return err
	}
	if len(state.Completed) > 256 || validateAdmissions(state.Completed) != nil {
		return errCoordinatorDescriptor
	}
	for key, receipt := range state.Completed {
		if receipt.Journal == nil || key != receipt.Journal.Owner || !exactHex(receipt.Digest, 32) {
			return errCoordinatorDescriptor
		}
	}
	if receipt := state.GCReceipt; receipt != nil && (!exactHex(receipt.Owner, 24) || !exactHex(receipt.ReleasedSHA256, 32)) {
		return errCoordinatorDescriptor
	}
	if err := state.Intent.validate(state); err != nil {
		return err
	}
	if state.GCLink == nil {
		return nil
	}
	return state.GCLink.validate(state)
}

func validateAdmissions(owners map[string]admission) error {
	for owner, claim := range owners {
		if owner == "" || len(owner) > 512 || claim.Digest == "" || len(claim.Refs) > 256 {
			return errCoordinatorDescriptor
		}
		if claim.Journal != nil && claim.Journal.Validate() != nil {
			return errCoordinatorDescriptor
		}
		for _, ref := range claim.Refs {
			if ref.Key == "" || ref.SHA256 == "" || len(ref.Key) > 4096 {
				return errCoordinatorDescriptor
			}
		}
	}
	return nil
}

func (link gcLink) validate(state admissions) error {
	if !exactHex(link.Owner, 24) || link.StateGeneration != state.Generation || link.Witness.Generation == 0 || link.Witness.Generation >= state.Generation {
		return errCoordinatorDescriptor
	}
	if link.Witness.Seal != state.Seal || link.Witness.Hold != state.Hold || state.Hold == "" || len(state.Owners) != 0 {
		return errCoordinatorDescriptor
	}
	if link.PriorETag == "" || len(link.PriorETag) > 4096 || !exactHex(link.PriorSHA256, 32) || !exactHex(link.LeasedSHA256, 32) || inventoryHash(link.Inventory) != link.Witness.InventorySHA256 {
		return errCoordinatorDescriptor
	}
	switch link.Phase {
	case gcPrepared:
		if link.ReleasedSHA256 != "" || len(link.ReleasedHead) != 0 {
			return errCoordinatorDescriptor
		}
		return nil
	case gcReleasing:
		return link.validateReleasedHead()
	default:
		return errCoordinatorDescriptor
	}
}

func (link gcLink) validateReleasedHead() error {
	if !exactHex(link.ReleasedSHA256, 32) || len(link.ReleasedHead) == 0 || len(link.ReleasedHead) > 16<<10 || !exactHex(link.Witness.InventorySHA256, 32) {
		return errCoordinatorDescriptor
	}
	if storage.SHA256Hex(link.ReleasedHead) != link.ReleasedSHA256 {
		return errCoordinatorDescriptor
	}
	var h CatalogHead
	decoder := json.NewDecoder(bytes.NewReader(link.ReleasedHead))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&h) != nil || decoder.Decode(new(any)) != io.EOF {
		return errCoordinatorDescriptor
	}
	if h.GCLease != "" || h.GCCoordinator != (gcCoordinatorWitness{}) || h.Protocol != 10 || h.Schema != 4 || h.Generation == 0 || !exactHex(h.Epoch, 24) || h.validateReferences() != nil {
		return errCoordinatorDescriptor
	}
	return nil
}
