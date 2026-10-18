package state

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"time"
)

// SettledPrivacyPreparation is a non-replay projection minted only at a full
// guarded privacy save. It carries no original metadata or source bytes.
type SettledPrivacyPreparation struct {
	NativeTarget              *PublicationNativeTarget `json:"native_target,omitempty"`
	Version                   int                      `json:"version"`
	OriginalPreparationSHA256 string                   `json:"original_preparation_sha256"`
	Kind                      PreparationKind          `json:"kind"`
	SessionID                 string                   `json:"session_id"`
	NativeSessionID           string                   `json:"native_session_id"`
	ProjectID                 string                   `json:"project_id"`
	MachineID                 string                   `json:"machine_id"`
	Harness                   archive.Harness          `json:"harness"`
	Origin                    archive.SessionOrigin    `json:"origin,omitempty"`
	ImportedAt                *time.Time               `json:"imported_at,omitempty"`
	StartedAtSource           archive.StartedAtSource  `json:"started_at_source,omitempty"`
	PreviousGenerationID      string                   `json:"previous_generation_id,omitempty"`
	StartedAt                 time.Time                `json:"started_at"`
	OwnerSHA256               string                   `json:"owner_sha256"`
	DestinationID             string                   `json:"destination_id"`
	AdmissionContext          string                   `json:"admission_context"`
	PolicyContext             string                   `json:"policy_context"`
	Purpose                   PublicationPurpose       `json:"purpose"`
	Predecessor               PredecessorState         `json:"predecessor"`
	PredecessorSHA256         string                   `json:"predecessor_sha256"`
	PreviousSetSHA256         string                   `json:"previous_set_sha256"`
	OriginMetadataSHA256      string                   `json:"origin_metadata_sha256"`
	OriginSetSHA256           string                   `json:"origin_set_sha256"`
	Inputs                    []SettledPrivacyInput    `json:"inputs"`
	MetadataSHA256            string                   `json:"metadata_sha256"`
	SourceSetSHA256           string                   `json:"source_set_sha256"`
	PayloadSetSHA256          string                   `json:"payload_set_sha256"`
	PrivacySHA256             string                   `json:"privacy_sha256"`
	SHA256                    string                   `json:"sha256"`
}

// SettledPrivacyInput keeps original selection and payload facts without sensitive raw bodies.
type SettledPrivacyInput struct {
	ParentSessionID *string                 `json:"parent_session_id,omitempty"`
	NativeChild     *bool                   `json:"native_child,omitempty"`
	Hook            *PublicationHookFacts   `json:"hook,omitempty"`
	Reference       archive.SourceReference `json:"reference"`
	Selection       PublicationSelection    `json:"selection"`
	FilterVersion   string                  `json:"filter_version"`
	AdapterVersion  string                  `json:"adapter_version"`
	SkillPolicy     string                  `json:"skill_policy"`
	PayloadKind     PublicationPayloadKind  `json:"payload_kind"`
	InlineSHA256    string                  `json:"inline_sha256,omitempty"`
	InlineSize      int                     `json:"inline_size,omitempty"`
	Stage           *ImmutableStageHandle   `json:"stage,omitempty"`
	PayloadSHA256   string                  `json:"payload_sha256"`
}

func settledPrivacySHA(p SettledPrivacyPreparation) string {
	p.SHA256 = ""
	raw, _ := json.Marshal(p)
	return publicationSHA256(append([]byte("settled-privacy-preparation/v1\x00"), raw...))
}

func (s *Store) mintSettledPrivacy(p publishedState) (publishedState, error) {
	reservation := s.readReservation()
	release, err := s.publicationValidationLoan(p)
	if err != nil {
		return p, err
	}
	defer release()

	if p.Commit == nil || p.Commit.Purpose != PublicationPrivacyRewrite || p.Preparation == nil || p.SettledPrivacy != nil {
		return p, ErrDurableStorageRecovery
	}
	if err := p.validateSelectingPublished(); err != nil {
		return p, err
	}
	a := p.Preparation
	var origin archive.Metadata
	if err := json.Unmarshal(a.OriginMetadata, &origin); err != nil {
		return p, err
	}
	previousSet := ""
	if a.Predecessor == PredecessorPresent {
		var e error
		previousSet, _, e = archive.PublicationIdentity(privacyPreviousBody(*a), a.DestinationID, a.AdmissionContext, a.PolicyContext, string(a.Purpose))
		if e != nil {
			return p, e
		}
	}

	// Strings and handles borrow the already-owned immutable preparation. The
	// new fact cells and digest strings have their own scope-long ownership.
	factCharge := int64(16<<10)*int64(len(a.Inputs)+1) + int64(len(origin.SessionID)+len(origin.NativeSessionID)+len(origin.ProjectID)+len(origin.MachineID)+len(origin.Harness.Name)+len(origin.Harness.Version)+len(origin.Harness.Mode)+len(origin.Origin)+len(origin.StartedAtSource)+len(origin.PreviousGenerationID))
	// The decoded identity strings are newly owned facts, rather than aliases
	// into the preparation. Retain their exact bytes after the decoding loan ends.
	if !reservation.Reserve(factCharge) {
		return p, errStateBudget
	}
	keepFacts := false
	defer func() {
		if !keepFacts {
			reservation.Release(factCharge)
		}
	}()
	projection := &SettledPrivacyPreparation{NativeTarget: a.NativeTarget, Inputs: make([]SettledPrivacyInput, 0, len(a.Inputs)), Version: 1, OriginalPreparationSHA256: a.SHA256, Kind: a.Kind, SessionID: origin.SessionID, NativeSessionID: origin.NativeSessionID, ProjectID: origin.ProjectID, MachineID: origin.MachineID, Harness: origin.Harness, Origin: origin.Origin, ImportedAt: origin.ImportedAt, StartedAtSource: origin.StartedAtSource, PreviousGenerationID: origin.PreviousGenerationID, StartedAt: origin.StartedAt.UTC(), OwnerSHA256: a.OwnerSHA256, DestinationID: a.DestinationID, AdmissionContext: a.AdmissionContext, PolicyContext: a.PolicyContext, Purpose: a.Purpose, Predecessor: a.Predecessor, PredecessorSHA256: a.PredecessorSHA256, PreviousSetSHA256: previousSet, OriginMetadataSHA256: a.OriginMetadataSHA256, OriginSetSHA256: a.OriginSetSHA256, MetadataSHA256: p.Commit.MetadataSHA256, SourceSetSHA256: p.Commit.SourceSetSHA256, PayloadSetSHA256: p.Commit.PayloadSetSHA256, PrivacySHA256: p.Commit.PrivacySHA256}
	for _, input := range a.Inputs {
		inlineSHA, inlineSize := "", 0
		if input.Payload.Kind == PublicationInline {
			inlineSHA = publicationSHA256(input.Payload.Inline)
			inlineSize = len(input.Payload.Inline)
		}
		fact := SettledPrivacyInput{ParentSessionID: input.ParentSessionID, NativeChild: input.NativeChild, InlineSHA256: inlineSHA, InlineSize: inlineSize, Hook: publicationHookFacts(input.HookObservations), Reference: input.Reference, Selection: input.Selection, FilterVersion: input.FilterVersion, AdapterVersion: input.AdapterVersion, SkillPolicy: input.SkillPolicy, PayloadKind: input.Payload.Kind, Stage: input.Payload.Stage, PayloadSHA256: payloadSetSHA([]PublicationSource{{Reference: input.Reference, Selection: input.Selection, Payload: input.Payload}})}
		projection.Inputs = append(projection.Inputs, fact)
	}
	projection.SHA256 = settledPrivacySHA(*projection)
	commit := *p.Commit
	commit.SettledPrivacySHA256 = projection.SHA256
	p.Commit = &commit
	p.SettledPrivacy = projection
	p.Preparation = nil
	if err := p.validateSelectingPublished(); err != nil {
		return p, err
	}
	if s.resourceBudget != nil {
		*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(factCharge) })
		keepFacts = true
	}
	return p, nil
}

func (p SettledPrivacyPreparation) validate(published publishedState) error {
	c := published.Commit
	if settledPrivacyCommitInvalid(p, c, published) {
		return ErrDurableStorageRecovery
	}
	if p.Kind == PreparationPrivacyPendingAbsent {
		if p.Predecessor != PredecessorAbsent || p.PredecessorSHA256 != "" || p.PreviousSetSHA256 != "" {
			return ErrDurableStorageRecovery
		}
	} else if p.Predecessor != PredecessorPresent || !validPublicationDigest(p.PredecessorSHA256) || !validPublicationDigest(p.PreviousSetSHA256) {
		return ErrDurableStorageRecovery
	}
	var next archive.Metadata
	if err := json.Unmarshal(published.MetadataBytes, &next); err != nil {
		return err
	}
	if p.NativeTarget != nil && (p.NativeTarget.validate() != nil || next.ParentSessionID != p.NativeTarget.ParentSessionID || next.NativeChild != p.NativeTarget.NativeChild) {
		return ErrDurableStorageRecovery
	}
	if settledPrivacyOwnerChanged(p, next) {
		return ErrDurableStorageRecovery
	}
	receipts := make(map[int]PrivacySource, len(published.PrivacyReceipts))
	previous := -1
	for _, receipt := range published.PrivacyReceipts {
		if receipt.InputIndex <= previous || receipt.InputIndex >= len(p.Inputs) {
			return ErrDurableStorageRecovery
		}
		receipts[receipt.InputIndex] = receipt
		previous = receipt.InputIndex
	}
	return p.validateInputs(published, next, receipts)
}

func sameOptionalTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// publicationValidationLoan covers metadata decode views, binding slices/maps,
// and overlapping encoder buffers before any selecting validation allocates.
// Source payload bytes remain borrowed; validation hashes them without cloning.
func (s *Store) publicationValidationLoan(p publishedState) (func(), error) {
	reservation := s.readReservation()
	if err := s.durableContext().Err(); err != nil {
		return nil, err
	}
	if s.resourceBudget == nil {
		return func() {}, nil
	}
	if len(p.MetadataBytes) > 32<<20 {
		return nil, ErrDurableStorageCapacity
	}
	metadataBytes := int64(len(p.MetadataBytes))
	facts := len(p.Payloads) + len(p.PrivacyReceipts)
	if p.Preparation != nil {
		a := p.Preparation
		if len(a.OriginMetadata) > 32<<20 || len(a.PrivacyPreviousMetadata) > 32<<20 {
			return nil, ErrDurableStorageCapacity
		}
		metadataBytes += int64(len(a.OriginMetadata) + len(a.PrivacyPreviousMetadata))
		facts += len(a.Inputs)
		for _, input := range a.Inputs {
			if h := input.HookObservations; h != nil {
				if len(h.Body) > 32<<20 {
					return nil, ErrDurableStorageCapacity
				}
				metadataBytes += int64(len(h.Body))
			}
		}
		if a.Migration != nil {
			if len(a.Migration.PreviousMetadata) > 32<<20 {
				return nil, ErrDurableStorageCapacity
			}
			metadataBytes += int64(len(a.Migration.PreviousMetadata))
		}
	}
	if p.SettledPrivacy != nil {
		facts += len(p.SettledPrivacy.Inputs)
	}
	// Every metadata body is bounded separately before multiplication; these
	// loans cover repeated nested decode/identity validation plus JSON digests.
	if metadataBytes > 4*(32<<20) || facts > 4*65 {
		return nil, ErrDurableStorageCapacity
	}
	n := 8*metadataBytes + int64(facts+1)*(16<<10)
	if !reservation.Reserve(n) {
		return nil, errStateBudget
	}
	return func() { reservation.Release(n) }, nil
}

func (s *Store) validateSelectingPublishedBudgeted(ctx context.Context, p publishedState) error {
	s, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	release, err := s.publicationValidationLoan(p)
	if err != nil {
		return err
	}
	defer release()
	count := len(p.Payloads)
	if p.Preparation != nil {
		count += len(p.Preparation.Inputs)
	}
	facts, end, e := newPayloadDigestFacts(ctx, s.resourceBudget, count)
	if e != nil {
		return e
	}
	defer end()
	return p.validateSelectingPublishedWithFacts(facts)
}

func settledPrivacyOwnerChanged(p SettledPrivacyPreparation, next archive.Metadata) bool {
	return next.SessionID != p.SessionID || next.NativeSessionID != p.NativeSessionID || next.ProjectID != p.ProjectID || next.MachineID != p.MachineID || next.Harness != p.Harness || next.Origin != p.Origin || !sameOptionalTime(next.ImportedAt, p.ImportedAt) || next.StartedAtSource != p.StartedAtSource || next.PreviousGenerationID != p.PreviousGenerationID || !next.StartedAt.Equal(p.StartedAt) || publicationOwner(next, p.DestinationID, p.AdmissionContext) != p.OwnerSHA256
}

func settledPrivacyReceiptChanged(receipt PrivacySource, input SettledPrivacyInput, output PublicationSource, p SettledPrivacyPreparation) bool {
	return settledPrivacyNativeChanged(receipt, input, p) || receipt.Version != 1 || receipt.SHA256 != privacySourceSHA(receipt) || receipt.Previous != input.Reference || receipt.Next != output.Reference || receipt.Selection != input.Selection || receipt.SessionID != p.SessionID || receipt.NativeSessionID != p.NativeSessionID || receipt.ProjectID != p.ProjectID || receipt.MachineID != p.MachineID || receipt.Harness != p.Harness || receipt.Origin != p.Origin || !sameOptionalTime(receipt.ImportedAt, p.ImportedAt) || receipt.StartedAtSource != p.StartedAtSource || receipt.PreviousGenerationID != p.PreviousGenerationID || !receipt.StartedAt.Equal(p.StartedAt) || receipt.OwnerSHA256 != p.OwnerSHA256 || receipt.DestinationID != p.DestinationID || receipt.AdmissionContext != p.AdmissionContext || receipt.OriginMetadataSHA256 != p.OriginMetadataSHA256 || receipt.PreviousPolicy.FilterVersion != input.FilterVersion || receipt.PreviousPolicy.AdapterVersion != input.AdapterVersion || string(receipt.PreviousPolicy.SkillEvidence) != input.SkillPolicy || receipt.NextPolicy.Context() != p.PolicyContext || receipt.PreviousPolicy.validate() != nil || receipt.NextPolicy.validate() != nil
}

func (p SettledPrivacyPreparation) validateInputs(published publishedState, next archive.Metadata, receipts map[int]PrivacySource) error {
	for i, input := range p.Inputs {
		output := published.Payloads[i]
		if input.Reference.CompressedBytes <= 0 || !validSourceReference(input.Reference) || input.Selection != output.Selection || input.Selection.CapturedAt.IsZero() {
			return ErrDurableStorageRecovery
		}
		binding := payloadBinding{Reference: input.Reference, Selection: input.Selection, Kind: input.PayloadKind, InlineSHA256: input.InlineSHA256, InlineSize: input.InlineSize, Stage: input.Stage}
		switch input.PayloadKind {
		case PublicationInline:
			if input.Stage != nil || input.InlineSize != input.Reference.CompressedBytes || input.InlineSHA256 != input.Reference.SHA256 {
				return ErrDurableStorageRecovery
			}
		case PublicationRemote:
			if input.Stage != nil || input.InlineSize != 0 || input.InlineSHA256 != "" {
				return ErrDurableStorageRecovery
			}
		case PublicationAdmissionStage:
			return ErrDurableStorageRecovery
		case PublicationHistoryStage:
			fake := PublicationSource{Reference: input.Reference, Selection: input.Selection, Payload: PublicationPayload{Kind: input.PayloadKind, Stage: input.Stage}}
			if input.InlineSize != 0 || input.InlineSHA256 != "" || validatePublicationPayload(fake, next, p.DestinationID, p.AdmissionContext) != nil {
				return ErrDurableStorageRecovery
			}
		default:
			return ErrDurableStorageRecovery
		}
		raw, _ := json.Marshal([]payloadBinding{binding})
		if input.PayloadSHA256 != publicationSHA256(append([]byte("payload-map/v1\x00"), raw...)) {
			return ErrDurableStorageRecovery
		}
		if validatePublicationHookFacts(input.Hook, PreparationInput{Selection: input.Selection}, p.OwnerSHA256, p.DestinationID, p.AdmissionContext, p.PolicyContext) != nil {
			return ErrDurableStorageRecovery
		}
		receipt, found := receipts[i]
		if input.Hook != nil && (!found || !reflect.DeepEqual(receipt.Hook, input.Hook)) {
			return ErrDurableStorageRecovery
		}
		if input.Reference != output.Reference && !found {
			return ErrDurableStorageRecovery
		}
		if found && validateCoveredPrivacyFacts(receipt) != nil {
			return ErrDurableStorageRecovery
		}
		if found && settledPrivacyReceiptChanged(receipt, input, output, p) {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func settledPrivacyCommitInvalid(p SettledPrivacyPreparation, c *PublicationCommit, published publishedState) bool {
	return c == nil || p.Version != 1 || p.SHA256 != settledPrivacySHA(p) || c.SettledPrivacySHA256 != p.SHA256 || c.PreparationSHA256 != p.OriginalPreparationSHA256 || p.Purpose != PublicationPrivacyRewrite || c.Purpose != p.Purpose || p.Kind != PreparationPrivacyCommitted && p.Kind != PreparationPrivacyPending && p.Kind != PreparationPrivacyPendingAbsent || c.Predecessor != p.Predecessor || c.PredecessorSHA256 != p.PredecessorSHA256 || !validPublicationDigest(p.OriginMetadataSHA256) || !validPublicationDigest(p.OriginSetSHA256) || !validPublicationDigest(p.OriginalPreparationSHA256) || c.MetadataSHA256 != p.MetadataSHA256 || c.SourceSetSHA256 != p.SourceSetSHA256 || c.PayloadSetSHA256 != p.PayloadSetSHA256 || c.PrivacySHA256 != p.PrivacySHA256 || c.DestinationID != p.DestinationID || c.AdmissionContext != p.AdmissionContext || c.PolicyContext != p.PolicyContext || len(p.Inputs) == 0 || len(p.Inputs) > 65 || len(p.Inputs) != len(published.Payloads)
}

func settledPrivacyNativeChanged(receipt PrivacySource, input SettledPrivacyInput, p SettledPrivacyPreparation) bool {
	return input.ParentSessionID != nil && !reflect.DeepEqual(receipt.OriginalParentSessionID, input.ParentSessionID) || input.NativeChild != nil && !reflect.DeepEqual(receipt.OriginalNativeChild, input.NativeChild) || p.NativeTarget != nil && (receipt.NativeTargetSHA256 != nativeTargetDigest(p.NativeTarget) || receipt.OutputParentSessionID != p.NativeTarget.ParentSessionID || receipt.OutputNativeChild != p.NativeTarget.NativeChild)
}
