package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// MaxPublicationReplayBytes caps inline compressed replay bytes, not remote references.
const MaxPublicationReplayBytes = 128 << 20

// PublicationPurpose identifies the mutation authorized by a pending journal.
type PublicationPurpose string

const (
	// PublicationCapture publishes a validated selected source snapshot.
	PublicationCapture PublicationPurpose = "capture"
	// PublicationMetadata refreshes metadata over unchanged retained sources.
	PublicationMetadata PublicationPurpose = "metadata"
	// PublicationPrivacyRewrite binds a complete typed privacy transformation.
	PublicationPrivacyRewrite PublicationPurpose = "privacy-rewrite"
)

// PredecessorState distinguishes absent metadata from unavailable evidence.
type PredecessorState string

const (
	// PredecessorAbsent authorizes the first publication only when remote metadata is absent.
	PredecessorAbsent PredecessorState = "absent"
	// PredecessorPresent authorizes replacement of an exact recorded body.
	PredecessorPresent PredecessorState = "present"
	// PredecessorUnknown authorizes only completion of the exact next body already remote.
	PredecessorUnknown PredecessorState = "unknown"
)

// PublicationPredecessor is evidence from retained local committed state.
// Body is required only for Present; it must never be guessed from a remote read.
type PublicationPredecessor struct {
	State                  PredecessorState
	Body                   []byte
	Bundle                 archive.SourceBundle
	SameRevisionContinuity *PublicationContinuity
}

// PublicationContinuity binds a provider-approved continuation to exact source digests.
// Filtered equality alone cannot supply this evidence.
type PublicationContinuity struct {
	PreviousSourceSHA256 string
	NextSourceSHA256     string
}

// PublicationSource carries one exact source reference and optional replay bytes.
// The active source uses legacy SourceBytes instead, avoiding a duplicate payload.
// A future admitted stage port must resolve one bounded checksum/size-bound object
// at a time and release only after local commit and covered request completion.
type PublicationSource struct {
	Reference archive.SourceReference `json:"reference"`
	Selection PublicationSelection    `json:"selection,omitempty"`
	Payload   PublicationPayload      `json:"payload,omitempty"`
	Bytes     []byte                  `json:"bytes,omitempty"`
}

// PublicationCommit binds replay to the complete source set and admitted context.
type PublicationCommit struct {
	SettledPrivacySHA256 string             `json:"settled_privacy_sha256,omitempty"`
	PrivacySHA256        string             `json:"privacy_sha256,omitempty"`
	PayloadSetSHA256     string             `json:"payload_set_sha256,omitempty"`
	PreparationSHA256    string             `json:"preparation_sha256,omitempty"`
	Version              int                `json:"version"`
	MetadataSHA256       string             `json:"metadata_sha256"`
	SourceSetSHA256      string             `json:"source_set_sha256"`
	Predecessor          PredecessorState   `json:"predecessor"`
	PredecessorSHA256    string             `json:"predecessor_sha256,omitempty"`
	DestinationID        string             `json:"destination_id,omitempty"`
	AdmissionContext     string             `json:"admission_context,omitempty"`
	PolicyContext        string             `json:"policy_context"`
	Purpose              PublicationPurpose `json:"purpose"`
}

func publicationSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (owned *PendingPublication) validatePublicationPredecessor(prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) error {
	p := *owned
	switch prior.State {
	case PredecessorPresent:
		if len(prior.Body) == 0 {
			return errors.New("publication predecessor body is unavailable")
		}
		var previous, next archive.Metadata
		if err := json.Unmarshal(prior.Body, &previous); err != nil {
			return err
		}
		if err := json.Unmarshal(p.MetadataBytes, &next); err != nil {
			return err
		}
		if _, _, err := archive.PublicationIdentity(prior.Body, destination, admission, policy, string(purpose)); err != nil {
			return fmt.Errorf("invalid publication predecessor: %w", err)
		}
		if previous.SessionID != next.SessionID || previous.NativeSessionID != next.NativeSessionID || previous.ProjectID != next.ProjectID || previous.MachineID != next.MachineID {
			return errors.New("publication cannot change committed ownership")
		}
		if purpose == PublicationPrivacyRewrite {
			if p.Preparation == nil {
				return ErrDurableStorageRecovery
			}
			return validatePrivacyCorrespondence(*p.Preparation, p.Sources, p.privacyReceipts(), p.MetadataBytes, prior.Body)
		}
		if previous.History != nil && next.History != nil && previous.History.CurrentRevision == next.History.CurrentRevision && previous.SourceBundle != next.SourceBundle {
			proof := prior.SameRevisionContinuity
			if proof == nil || proof.PreviousSourceSHA256 != previous.SourceBundle.SHA256 || proof.NextSourceSHA256 != next.SourceBundle.SHA256 {
				return errors.New("same revision update requires provider-approved continuity; retry selection validation")
			}
		}
		if previous.History == nil && next.History != nil && p.Preparation != nil && p.Preparation.Migration != nil {
			if err := p.Preparation.Migration.validate(previous, next, prior.Body, p.MetadataBytes, destination, admission, policy); err != nil {
				return err
			}
			return nil
		}
		if err := archive.ValidateRevisionTransition(previous, next, prior.Bundle, p.Bundle); err != nil {
			return err
		}
	case PredecessorAbsent, PredecessorUnknown:
		if len(prior.Body) != 0 {
			return errors.New("unexpected publication predecessor body")
		}
	default:
		return errors.New("publication predecessor evidence is required")
	}
	return nil
}

// ValidatePublication checks every local replay payload and exact metadata binding.
func (owned *PendingPublication) ValidatePublication() error {
	p := *owned
	if p.JournalVersion == 5 {
		return p.validatePublicationEnvelope()
	}
	return p.validateReadyPublication()
}

// ValidatePublicationBudgeted shares hashes only within this complete validation
// call, borrowing the caller's existing ledger without extending proof lifetime.
func (owned *PendingPublication) ValidatePublicationBudgeted(ctx context.Context, budget *agentapi.NativeReadBudget) error {
	if owned.JournalVersion != 5 {
		return owned.ValidatePublication()
	}
	count := len(owned.Sources)
	if owned.Preparation != nil {
		count += len(owned.Preparation.Inputs)
	}
	facts, release, err := newPayloadDigestFacts(ctx, budget, count)
	if err != nil {
		return err
	}
	defer release()
	return owned.validatePublicationEnvelopeWithFacts(facts)
}

func (owned *PendingPublication) validateReadyPublication() error {
	return owned.validateReadyPublicationWithFacts(nil)
}

func (owned *PendingPublication) validateReadyPublicationWithFacts(facts *payloadDigestFacts) (err error) {
	defer func() { err = facts.result(err) }()
	p := *owned
	c := p.Commit
	if c == nil || (c.Version != 1 && c.Version != 2) || (c.Purpose != PublicationCapture && c.Purpose != PublicationMetadata && c.Purpose != PublicationPrivacyRewrite) {
		return errors.New("pending source-set journal is incomplete")
	}
	if c.Predecessor != PredecessorAbsent && c.Predecessor != PredecessorPresent && c.Predecessor != PredecessorUnknown {
		return errors.New("invalid publication predecessor state")
	}
	if (c.Predecessor == PredecessorPresent && !validPublicationDigest(c.PredecessorSHA256)) || (c.Predecessor != PredecessorPresent && c.PredecessorSHA256 != "") {
		return errors.New("invalid publication predecessor digest")
	}
	digest, refs, err := archive.PublicationIdentity(p.MetadataBytes, c.DestinationID, c.AdmissionContext, c.PolicyContext, string(c.Purpose))
	if err != nil {
		return err
	}
	if digest != c.SourceSetSHA256 || publicationSHA256(p.MetadataBytes) != c.MetadataSHA256 || len(refs) != len(p.Sources) || len(refs) > archive.MaxPreservedRevisions+1 {
		return errors.New("pending publication identity mismatch")
	}
	if err := p.validatePublicationOwnership(refs[0]); err != nil {
		return err
	}
	if p.JournalVersion == 5 {
		var metadata archive.Metadata
		if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
			return err
		}
		total := 0
		expected, err := selectedPublicationSources(p, c.DestinationID, c.AdmissionContext)
		if err != nil {
			return err
		}
		for i, source := range p.Sources {
			if source.Selection != expected[i].Selection {
				return ErrDurableStorageRecovery
			}
			if source.Reference != refs[i] || len(source.Bytes) != 0 {
				return ErrDurableStorageRecovery
			}
			if err := validatePublicationPayloadWithFacts(source, metadata, c.DestinationID, c.AdmissionContext, facts); err != nil {
				return err
			}
			total += len(source.Payload.Inline)
		}
		if total > MaxPublicationReplayBytes {
			return ErrDurableStorageCapacity
		}
		return nil
	}
	return p.validatePublicationPayloads(refs)
}

func (owned *PendingPublication) validatePublicationOwnership(active archive.SourceReference) error {
	p := *owned
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return err
	}
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil || p.MetadataKey != key || active != p.SourceReference() || p.Bundle.ArchiveSessionID != m.SessionID || p.Bundle.NativeSessionID != m.NativeSessionID || p.Bundle.ProjectID != m.ProjectID || p.Bundle.Capture.Harness != m.Harness || !p.Bundle.Capture.CapturedAt.Equal(m.CapturedAt) {
		return errors.New("pending publication source or ownership mismatch")
	}
	if p.Bundle.Capture.FilterVersion != m.FilterVersion || p.Bundle.Capture.AdapterName != m.Adapter.Name || p.Bundle.Capture.AdapterVersion != m.Adapter.Version {
		return errors.New("pending source filter provenance differs from metadata")
	}
	if (p.Bundle.History == nil) != (m.History == nil) {
		return errors.New("pending source selection differs from metadata")
	}
	if m.History != nil {
		if err := p.Bundle.ValidateHistory(); err != nil {
			return err
		}
		if p.Bundle.History.ActiveRolloutID != m.History.CurrentRevision {
			return errors.New("pending active revision differs from metadata")
		}
	}
	return nil
}

func (owned *PendingPublication) validatePublicationPayloads(refs []archive.SourceReference) error {
	p := *owned
	total := len(p.SourceBytes)
	for i, source := range p.Sources {
		if source.Reference != refs[i] || source.Reference.CompressedBytes > 128<<20 {
			return errors.New("pending publication source set differs from metadata")
		}
		payload := source.Bytes
		if i == 0 {
			if len(payload) > 0 {
				return errors.New("active replay bytes must use the legacy payload field")
			}
			payload = p.SourceBytes
		} else {
			total += len(payload)
		}
		if len(payload) > 0 && (len(payload) != source.Reference.CompressedBytes || publicationSHA256(payload) != source.Reference.SHA256) {
			return errors.New("pending source checksum or size does not match persisted bytes")
		}
	}
	if total > MaxPublicationReplayBytes {
		return errors.New("pending replay exceeds 128 MiB; publish bounded groups without dropping retained references")
	}
	return nil
}

func validPublicationDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// SaveCommittedPublication atomically records the complete remotely verified
// set and exact metadata before any request acknowledgement or stage release.
func (p *Published) SaveCommittedPublication(pending PendingPublication, at time.Time) error {
	if pending.Commit == nil {
		return errors.New("committed publication requires a sealed source set")
	}
	if pending.MetadataOnly && p.found {
		return p.SaveRepublishedMetadata(pending, at)
	}
	return p.SavePublication(pending.Bundle, at, pending.SourceReference(), pending.MetadataBytes, pending)

}

// CommittedSources returns every exact retained reference of the last publication.
// Legacy state uses cached metadata, or its single recorded source when available.
func (p *Published) CommittedSources() ([]archive.SourceReference, error) {
	if p.state.Commit != nil {
		digest, refs, err := archive.PublicationIdentity(p.state.MetadataBytes, p.state.Commit.DestinationID, p.state.Commit.AdmissionContext, p.state.Commit.PolicyContext, string(p.state.Commit.Purpose))
		if err != nil {
			return nil, err
		}
		if digest != p.state.Commit.SourceSetSHA256 || publicationSHA256(p.state.MetadataBytes) != p.state.Commit.MetadataSHA256 || !slices.Equal(refs, p.state.Sources) {
			return nil, errors.New("committed publication identity is damaged; reconcile retained state")
		}
		return refs, nil
	}
	if len(p.state.MetadataBytes) > 0 {
		var m archive.Metadata
		if err := json.Unmarshal(p.state.MetadataBytes, &m); err != nil {
			return nil, err
		}
		return m.SourceReferences()
	}
	if ref, found := p.LastPublishedSource(); found {
		return []archive.SourceReference{ref}, nil
	}
	return nil, nil
}

// PublicationPredecessor returns replacement authority from durable local state.
// A remote-only legacy cache migration remains unknown even after restart.
func (p *Published) PublicationPredecessor() PublicationPredecessor {
	bundle, _, published := p.LastPublished()
	if !published {
		return PublicationPredecessor{State: PredecessorAbsent}
	}
	if p.state.PredecessorUnknown || len(p.state.MetadataBytes) == 0 || (p.state.Commit != nil && publicationSHA256(p.state.MetadataBytes) != p.state.Commit.MetadataSHA256) {
		return PublicationPredecessor{State: PredecessorUnknown}
	}
	return PublicationPredecessor{State: PredecessorPresent, Body: p.state.MetadataBytes, Bundle: bundle}
}

func attachPublication(next publishedState, pending PendingPublication, ctx context.Context, budget *agentapi.NativeReadBudget) (publishedState, error) {
	if err := pending.ValidatePublicationBudgeted(ctx, budget); err != nil {
		return next, err
	}
	commit := *pending.Commit
	next.Commit = &commit
	if commit.Version == 2 {
		next.PublicationVersion = 5
		next.PrivacyReceipts = pending.privacyReceipts()
		next.Preparation = pending.Preparation
		next.SettledPrivacy = nil
		next.settlePrivacy = pending.History == nil || !pending.History.MaintenanceOwed
		next.Payloads = pending.Sources
		next.Cleanup = pending.Cleanup
	}
	next.PredecessorUnknown = false
	next.Sources = make([]archive.SourceReference, len(pending.Sources))
	for i, source := range pending.Sources {
		next.Sources[i] = source.Reference
	}
	return next, nil
}
