package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// OrdinaryHistoryMigration preserves the frozen semantic proof independently
// of the original-file migration journal. It grants no ancestor content access.
type OrdinaryHistoryMigration struct {
	PreviousMetadata         []byte                     `json:"previous_metadata"`
	Version                  int                        `json:"version"`
	PreviousMetadataSHA256   string                     `json:"previous_metadata_sha256"`
	NextMetadataSHA256       string                     `json:"next_metadata_sha256"`
	PreviousSetSHA256        string                     `json:"previous_set_sha256"`
	NextSetSHA256            string                     `json:"next_set_sha256"`
	PreviousSourceSHA256     string                     `json:"previous_source_sha256"`
	NextSourceSHA256         string                     `json:"next_source_sha256"`
	OwnerSHA256              string                     `json:"owner_sha256"`
	DestinationID            string                     `json:"destination_id"`
	AdmissionContext         string                     `json:"admission_context"`
	PolicyContext            string                     `json:"policy_context"`
	PreviousBindingSHA256    string                     `json:"previous_binding_sha256"`
	NextBindingSHA256        string                     `json:"next_binding_sha256"`
	PreviousRevisionID       string                     `json:"previous_revision_id"`
	NextRevisionID           string                     `json:"next_revision_id"`
	PreviousCapturedAt       time.Time                  `json:"previous_captured_at"`
	Mode                     OrdinaryMigrationMode      `json:"mode"`
	CoveringSourceSHA256     string                     `json:"covering_source_sha256"`
	RetainedPrior            *archive.RevisionReference `json:"retained_prior,omitempty"`
	FilterVersion            string                     `json:"filter_version"`
	AdapterName              string                     `json:"adapter_name"`
	AdapterVersion           string                     `json:"adapter_version"`
	SourceFormat             string                     `json:"source_format"`
	PreviousMeaningfulCount  int                        `json:"previous_meaningful_count"`
	PreviousMeaningfulSHA256 string                     `json:"previous_meaningful_sha256"`
	CoveringMeaningfulSHA256 string                     `json:"covering_meaningful_sha256"`
	SHA256                   string                     `json:"sha256"`
}

// ValidatedOrdinaryMigration can only be minted by actual injected native proof.
type ValidatedOrdinaryMigration struct{ receipt OrdinaryHistoryMigration }

// OwnedRevisionProjection is shared with collector reconciliation. Source2 has
// no ordinal proof and may only cover its same physical source.
func OwnedRevisionProjection(evidence agentapi.RevisionEvidence, bundle archive.SourceBundle) archive.SourceBundle {
	return agentapi.OwnedRevisionProjection(evidence, bundle)
}

func migrationSHA(r OrdinaryHistoryMigration) string {
	r.SHA256 = ""
	r.PreviousMetadata = nil
	raw, _ := json.Marshal(r)
	return publicationSHA256(append([]byte("ordinary-history-migration/v1\x00"), raw...))
}

func bindingSHA(b *archive.CodexSourceBinding) string {
	raw, _ := json.Marshal(b)
	return publicationSHA256(append([]byte("physical-binding/v1\x00"), raw...))
}

func meaningfulSHA(ctx context.Context, b archive.SourceBundle, budget *agentapi.NativeReadBudget) (string, error) {
	owner := budget.Reservation()
	limit := int64(128 << 20)
	if budget != nil {
		limit = min(limit, budget.Available())
	}
	n, err := jsonwire.Bound(ctx, b.NativeRecords, limit)
	if err != nil {
		return "", err
	}
	if budget != nil {
		if !owner.Reserve(n) {
			return "", agentapi.ErrReadBudget
		}
		defer owner.Release(n)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("meaningful-revision-records/v1\x00"))
	if err = json.NewEncoder(hash).Encode(b.NativeRecords); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ValidateOrdinaryHistoryMigration binds exact old/new selections to complete
// frozen registration/physical facts and the actual meaningful native comparator.
func ValidateOrdinaryHistoryMigration(ctx context.Context, previousBody, nextBody []byte, previous, candidate archive.SourceBundle, registration archive.SessionRegistration, previousBinding, nextBinding *archive.CodexSourceBinding, adapter agentapi.TranscriptFilter, admission, policy string, budget *agentapi.NativeReadBudget) (ValidatedOrdinaryMigration, error) {
	owner := budget.Reservation()
	var invalid ValidatedOrdinaryMigration
	if len(previousBody) == 0 || len(previousBody) > 32<<20 || len(nextBody) == 0 || len(nextBody) > 32<<20 {
		return invalid, ErrDurableStorageCapacity
	}
	reservation := int64(len(previousBody)+len(nextBody)) + 8*int64(len(previous.NativeRecords)+len(candidate.NativeRecords)) + (32 << 10)
	if budget != nil {
		if !owner.Reserve(reservation) {
			return invalid, agentapi.ErrReadBudget
		}
		defer owner.Release(reservation)
	}
	evidence, ok := adapter.(agentapi.RevisionEvidence)
	if !ok || previousBinding == nil || nextBinding == nil || previousBinding.Validate() != nil || nextBinding.Validate() != nil || !nextBinding.PreservesFacts(previousBinding) {
		return invalid, errors.New("ordinary history conversion requires frozen physical facts and native evidence")
	}
	var old, next archive.Metadata
	if err := json.Unmarshal(previousBody, &old); err != nil {
		return invalid, err
	}
	if err := json.Unmarshal(nextBody, &next); err != nil {
		return invalid, err
	}
	if ordinaryMigrationOwnershipChanged(old, next, previous, candidate, registration, previousBinding, nextBinding) {
		return invalid, errors.New("ordinary history conversion changes frozen ownership")
	}
	oldSet, _, err := archive.PublicationIdentity(previousBody, registration.DestinationID, admission, policy, string(PublicationCapture))
	if err != nil {
		return invalid, err
	}
	newSet, _, err := archive.PublicationIdentity(nextBody, registration.DestinationID, admission, policy, string(PublicationCapture))
	if err != nil {
		return invalid, err
	}
	left, right := OwnedRevisionProjection(evidence, previous), OwnedRevisionProjection(evidence, candidate)
	leftSHA, err := meaningfulSHA(ctx, left, budget)
	if err != nil {
		return invalid, err
	}
	rightSHA, err := meaningfulSHA(ctx, right, budget)
	if err != nil {
		return invalid, err
	}
	var mode OrdinaryMigrationMode
	var coveringSHA string
	var retainedPrior *archive.RevisionReference
	if previousBinding.PhysicalRolloutID == nextBinding.PhysicalRolloutID && previous.Capture.FilterVersion == candidate.Capture.FilterVersion && previous.Capture.AdapterVersion == candidate.Capture.AdapterVersion && previous.Capture.SourceFormat == candidate.Capture.SourceFormat && adapter.EvidenceExtends(left, right) {
		mode = MigrationSamePhysicalCovered
		coveringSHA = next.SourceBundle.SHA256
	} else {
		mode = MigrationExactPriorRetained
		for _, ref := range next.History.Preserved {
			if ref.RevisionID == previousBinding.PhysicalRolloutID && ref.Source == old.SourceBundle && ref.CapturedAt.Equal(old.CapturedAt) {
				factCopy := ref
				retainedPrior = &factCopy
				break
			}
		}
		if retainedPrior == nil {
			return invalid, errors.New("source2 conversion must preserve exact prior source and original age")
		}
	}
	r := OrdinaryHistoryMigration{Mode: mode, CoveringSourceSHA256: coveringSHA, RetainedPrior: retainedPrior, PreviousMetadata: previousBody, Version: 1, PreviousMetadataSHA256: publicationSHA256(previousBody), NextMetadataSHA256: publicationSHA256(nextBody), PreviousSetSHA256: oldSet, NextSetSHA256: newSet, PreviousSourceSHA256: old.SourceBundle.SHA256, NextSourceSHA256: next.SourceBundle.SHA256, OwnerSHA256: publicationOwner(next, registration.DestinationID, admission), DestinationID: registration.DestinationID, AdmissionContext: admission, PolicyContext: policy, PreviousBindingSHA256: bindingSHA(previousBinding), NextBindingSHA256: bindingSHA(nextBinding), PreviousRevisionID: previousBinding.PhysicalRolloutID, NextRevisionID: nextBinding.PhysicalRolloutID, PreviousCapturedAt: old.CapturedAt.UTC(), FilterVersion: previous.Capture.FilterVersion, AdapterName: previous.Capture.AdapterName, AdapterVersion: previous.Capture.AdapterVersion, SourceFormat: previous.Capture.SourceFormat, PreviousMeaningfulCount: len(left.NativeRecords), PreviousMeaningfulSHA256: leftSHA, CoveringMeaningfulSHA256: rightSHA}
	r.SHA256 = migrationSHA(r)
	return ValidatedOrdinaryMigration{receipt: r}, nil
}

func (r OrdinaryHistoryMigration) validate(old, next archive.Metadata, previousBody, nextBody []byte, destination, admission, policy string) error {
	if len(r.PreviousMetadata) == 0 || len(r.PreviousMetadata) > 32<<20 || publicationSHA256(r.PreviousMetadata) != r.PreviousMetadataSHA256 {
		return ErrDurableStorageRecovery
	}
	if ordinaryMigrationBindingsChanged(r, old, next, previousBody, nextBody, destination, admission, policy) {
		return ErrDurableStorageRecovery
	}
	if ordinaryMigrationIdentityChanged(r, old, next) {
		return ErrDurableStorageRecovery
	}
	oldSet, _, err := archive.PublicationIdentity(previousBody, destination, admission, policy, string(PublicationCapture))
	if err != nil || oldSet != r.PreviousSetSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	nextSet, _, err := archive.PublicationIdentity(nextBody, destination, admission, policy, string(PublicationCapture))
	if err != nil || nextSet != r.NextSetSHA256 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	switch r.Mode {
	case MigrationSamePhysicalCovered:
		if r.PreviousRevisionID != r.NextRevisionID || r.RetainedPrior != nil || r.CoveringSourceSHA256 != next.SourceBundle.SHA256 {
			return ErrDurableStorageRecovery
		}
	case MigrationExactPriorRetained:
		if r.RetainedPrior == nil || r.RetainedPrior.RevisionID != r.PreviousRevisionID || r.RetainedPrior.Source != old.SourceBundle || !r.RetainedPrior.CapturedAt.Equal(old.CapturedAt) {
			return ErrDurableStorageRecovery
		}
		found := false
		for _, ref := range next.History.Preserved {
			if ref == *r.RetainedPrior {
				found = true
			}
		}
		if !found {
			return ErrDurableStorageRecovery
		}
	default:
		return ErrDurableStorageRecovery
	}
	return nil
}

// WithOrdinaryMigration carries only a factory-minted proof until it is frozen.
func (owned *PendingPublication) WithOrdinaryMigration(proof ValidatedOrdinaryMigration) (PendingPublication, error) {
	p := *owned
	if proof.receipt.Version != 1 || proof.receipt.SHA256 != migrationSHA(proof.receipt) {
		return p, ErrDurableStorageRecovery
	}
	p.migration = &proof
	return p, nil
}

func ordinaryMigrationOwnershipChanged(old, next archive.Metadata, previous, candidate archive.SourceBundle, registration archive.SessionRegistration, previousBinding, nextBinding *archive.CodexSourceBinding) bool {
	return old.History != nil || next.History == nil || previous.History != nil || previous.SchemaVersion != archive.SourceSchemaVersion || candidate.History == nil || old.SessionID != registration.ArchiveSessionID || next.SessionID != old.SessionID || old.NativeSessionID != registration.NativeSessionID || next.NativeSessionID != old.NativeSessionID || old.ProjectID != registration.ProjectID || next.ProjectID != old.ProjectID || old.MachineID != next.MachineID || old.Harness != next.Harness || previousBinding.NativeThreadID != old.NativeSessionID || nextBinding.NativeThreadID != old.NativeSessionID || nextBinding.PhysicalRolloutID != next.History.CurrentRevision || previous.NativeSessionID != old.NativeSessionID || previous.ArchiveSessionID != old.SessionID || candidate.NativeSessionID != next.NativeSessionID || candidate.ArchiveSessionID != next.SessionID
}

func ordinaryMigrationBindingsChanged(r OrdinaryHistoryMigration, old, next archive.Metadata, previousBody, nextBody []byte, destination, admission, policy string) bool {
	return r.Version != 1 || r.SHA256 != migrationSHA(r) || old.History != nil || next.History == nil || r.PreviousMetadataSHA256 != publicationSHA256(previousBody) || r.NextMetadataSHA256 != publicationSHA256(nextBody) || r.PreviousSourceSHA256 != old.SourceBundle.SHA256 || r.NextSourceSHA256 != next.SourceBundle.SHA256 || r.OwnerSHA256 != publicationOwner(next, destination, admission) || r.DestinationID != destination || r.AdmissionContext != admission || r.PolicyContext != policy || !r.PreviousCapturedAt.Equal(old.CapturedAt) || !validPublicationDigest(r.PreviousBindingSHA256) || !validPublicationDigest(r.NextBindingSHA256)
}

func ordinaryMigrationIdentityChanged(r OrdinaryHistoryMigration, old, next archive.Metadata) bool {
	return old.SessionID != next.SessionID || old.NativeSessionID != next.NativeSessionID || old.ProjectID != next.ProjectID || old.MachineID != next.MachineID || old.Harness != next.Harness || old.PreviousGenerationID != next.PreviousGenerationID || !old.StartedAt.Equal(next.StartedAt) || r.NextRevisionID != next.History.CurrentRevision || r.PreviousRevisionID == "" || r.PreviousMeaningfulCount < 0 || r.PreviousMeaningfulCount > archive.MaxHistoryRecords || !validPublicationDigest(r.PreviousMeaningfulSHA256) || !validPublicationDigest(r.CoveringMeaningfulSHA256)
}
