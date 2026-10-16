package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// PreparationAuthority freezes the exact original selection and inputs.
// Progress and retired cleanup are deliberately outside its digest.
type PreparationAuthority struct {
	Retention               *RetentionRestoration     `json:"retention,omitempty"`
	NativeTarget            *PublicationNativeTarget  `json:"native_target,omitempty"`
	PrivacyPreviousMetadata []byte                    `json:"privacy_previous_metadata,omitempty"`
	Migration               *OrdinaryHistoryMigration `json:"migration,omitempty"`
	Version                 int                       `json:"version"`
	Kind                    PreparationKind           `json:"kind"`
	SessionID               string                    `json:"session_id"`
	OwnerSHA256             string                    `json:"owner_sha256"`
	DestinationID           string                    `json:"destination_id"`
	AdmissionContext        string                    `json:"admission_context"`
	PolicyContext           string                    `json:"policy_context"`
	Purpose                 PublicationPurpose        `json:"purpose"`
	Predecessor             PredecessorState          `json:"predecessor"`
	PredecessorSHA256       string                    `json:"predecessor_sha256,omitempty"`
	OriginMetadata          []byte                    `json:"origin_metadata"`
	OriginMetadataSHA256    string                    `json:"origin_metadata_sha256"`
	OriginSetSHA256         string                    `json:"origin_set_sha256"`
	Inputs                  []PreparationInput        `json:"inputs"`
	OriginalEvidenceSHA256  string                    `json:"original_evidence_sha256,omitempty"`
	SHA256                  string                    `json:"sha256"`
}

// PreparationInput is one immutable input in the original ordered selection.
type PreparationInput struct {
	ParentSessionID  *string                      `json:"parent_session_id,omitempty"`
	NativeChild      *bool                        `json:"native_child,omitempty"`
	HookObservations *PublicationHookObservations `json:"hook_observations,omitempty"`
	Reference        archive.SourceReference      `json:"reference"`
	Selection        PublicationSelection         `json:"selection"`
	FilterVersion    string                       `json:"filter_version"`
	AdapterVersion   string                       `json:"adapter_version"`
	SkillPolicy      string                       `json:"skill_policy"`
	Payload          PublicationPayload           `json:"payload"`
}

// PreparedOutput binds a completed sequential transform to its original index.
type PreparedOutput struct {
	Privacy    *PrivacySource    `json:"privacy,omitempty"`
	InputIndex int               `json:"input_index"`
	Source     PublicationSource `json:"source"`
}

// PreparationProgress is the independently checksummed mutable checkpoint.
type PreparationProgress struct {
	Version         int              `json:"version"`
	AuthoritySHA256 string           `json:"authority_sha256"`
	Cursor          int              `json:"cursor"`
	Outputs         []PreparedOutput `json:"outputs"`
	WorkingMetadata []byte           `json:"working_metadata"`
	SHA256          string           `json:"sha256"`
}

// CleanupProgress keeps retirement outside immutable selecting authority.
type CleanupProgress struct {
	Version                 int             `json:"version"`
	SelectingMetadataSHA256 string          `json:"selecting_metadata_sha256"`
	SelectingSetSHA256      string          `json:"selecting_set_sha256"`
	Retired                 []RetiredSource `json:"retired"`
	SHA256                  string          `json:"sha256"`
}

func preparationSHAWithFacts(a PreparationAuthority, facts *payloadDigestFacts) string {
	type inputBinding struct {
		ParentSessionID *string                 `json:"ParentSessionID,omitempty"`
		NativeChild     *bool                   `json:"NativeChild,omitempty"`
		Hook            *PublicationHookFacts   `json:"Hook"`
		Reference       archive.SourceReference `json:"Reference"`
		Selection       PublicationSelection    `json:"Selection"`
		FilterVersion   string                  `json:"FilterVersion"`
		AdapterVersion  string                  `json:"AdapterVersion"`
		SkillPolicy     string                  `json:"SkillPolicy"`
		PayloadSHA256   string                  `json:"PayloadSHA256"`
	}
	inputs := make([]inputBinding, len(a.Inputs))
	for i, input := range a.Inputs {
		inputs[i] = inputBinding{input.ParentSessionID, input.NativeChild, publicationHookFacts(input.HookObservations), input.Reference, input.Selection, input.FilterVersion, input.AdapterVersion, input.SkillPolicy, payloadSetSHAWithFacts([]PublicationSource{{Reference: input.Reference, Selection: input.Selection, Payload: input.Payload}}, facts)}
	}
	raw, _ := json.Marshal(struct {
		NativeTarget           *PublicationNativeTarget `json:"NativeTarget,omitempty"`
		Version                int                      `json:"Version"`
		Kind                   PreparationKind          `json:"Kind"`
		SessionID              string                   `json:"SessionID"`
		OwnerSHA256            string                   `json:"OwnerSHA256"`
		DestinationID          string                   `json:"DestinationID"`
		AdmissionContext       string                   `json:"AdmissionContext"`
		PolicyContext          string                   `json:"PolicyContext"`
		Purpose                PublicationPurpose       `json:"Purpose"`
		Predecessor            PredecessorState         `json:"Predecessor"`
		PredecessorSHA256      string                   `json:"PredecessorSHA256"`
		OriginMetadataSHA256   string                   `json:"OriginMetadataSHA256"`
		OriginSetSHA256        string                   `json:"OriginSetSHA256"`
		OriginalEvidenceSHA256 string                   `json:"OriginalEvidenceSHA256"`
		Inputs                 []inputBinding           `json:"Inputs"`
	}{a.NativeTarget, a.Version, a.Kind, a.SessionID, a.OwnerSHA256, a.DestinationID, a.AdmissionContext, a.PolicyContext, a.Purpose, a.Predecessor, a.PredecessorSHA256, a.OriginMetadataSHA256, a.OriginSetSHA256, a.OriginalEvidenceSHA256, inputs})
	previousPrivacySHA := publicationSHA256(a.PrivacyPreviousMetadata)
	migration := ""
	if a.Migration != nil {
		migration = a.Migration.SHA256
	}
	retention, _ := json.Marshal(a.Retention)
	return publicationSHA256(append(append(append([]byte("preparation-authority/v1\x00"), raw...), []byte("\x00"+migration+"\x00"+previousPrivacySHA)...), retentionBinding(a.Retention, retention)...))
}

func progressSHAWithFacts(p PreparationProgress, facts *payloadDigestFacts) string {
	type outputBinding struct {
		Index         int    `json:"Index"`
		PayloadSHA256 string `json:"PayloadSHA256"`
		PrivacySHA256 string `json:"PrivacySHA256"`
	}
	outputs := make([]outputBinding, len(p.Outputs))
	for i, o := range p.Outputs {
		proofSHA := ""
		if o.Privacy != nil {
			proofSHA = o.Privacy.SHA256
		}
		outputs[i] = outputBinding{o.InputIndex, payloadSetSHAWithFacts([]PublicationSource{o.Source}, facts), proofSHA}
	}
	raw, _ := json.Marshal(struct {
		Version               int             `json:"Version"`
		AuthoritySHA256       string          `json:"AuthoritySHA256"`
		Cursor                int             `json:"Cursor"`
		Outputs               []outputBinding `json:"Outputs"`
		WorkingMetadataSHA256 string          `json:"WorkingMetadataSHA256"`
	}{p.Version, p.AuthoritySHA256, p.Cursor, outputs, publicationSHA256(p.WorkingMetadata)})
	return publicationSHA256(append([]byte("preparation-progress/v1\x00"), raw...))
}

func cleanupSHA(c CleanupProgress) string {
	c.SHA256 = ""
	raw, _ := json.Marshal(c)
	return publicationSHA256(append([]byte("cleanup-progress/v1\x00"), raw...))
}

// PreparePublicationV2 freezes a preparing adjunct or seals a ready transaction.
// It grants no storage capability; the guarded writer persists source-set17
// protection before writing the distinct outer5 envelope and inner commit2.
func PreparePublicationV2(p PendingPublication, prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) (PendingPublication, error) {
	return PreparePublicationV2Budgeted(context.Background(), nil, p, prior, destination, admission, policy, purpose)
}

// PreparePublicationV2Budgeted borrows invocation-only digest scratch from the
// existing source ledger; it grants no persistence or storage capability.
func PreparePublicationV2Budgeted(ctx context.Context, budget *agentapi.NativeReadBudget, p PendingPublication, prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) (prepared PendingPublication, err error) {
	preparing := p.History != nil && p.History.Preparing
	if p.JournalVersion == 5 && p.Commit != nil {
		if err := p.ValidatePublicationBudgeted(ctx, budget); err != nil {
			return p, fmt.Errorf("existing selecting seal: %w", err)
		}
		return p, nil
	}
	if p.JournalVersion != 0 && p.JournalVersion != 5 {
		return p, ErrDurableStorageRecovery
	}
	p.Commit = nil
	sources, err := selectedPublicationSources(p, destination, admission)
	if err != nil {
		return p, err
	}
	count := 2 * len(sources)
	if p.Preparation != nil {
		count = len(sources) + len(p.Preparation.Inputs)
	}
	facts, release, err := newPayloadDigestFacts(ctx, budget, count)
	if err != nil {
		return p, err
	}
	defer release()
	defer func() { err = facts.result(err) }()
	p.Sources = sources
	var m archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return p, err
	}
	if p.Preparation == nil {
		if err := p.initializePreparation(prior, m, destination, admission, policy, purpose, sources, facts); err != nil {
			return p, err
		}
	}
	p.JournalVersion = 5
	p.Phase = PublicationReady
	if preparing {
		p.Phase = PublicationPreparing
	}
	if p.History != nil {
		if err := p.initializeHistoryProgress(sources, facts, preparing); err != nil {
			return p, err
		}
	} else if purpose == PublicationPrivacyRewrite {
		if err := p.initializeOrdinaryPrivacyProgress(sources, facts); err != nil {
			return p, err
		}
	}
	digest, _, err := archive.PublicationIdentity(p.MetadataBytes, destination, admission, policy, string(purpose))
	if err != nil {
		return p, err
	}
	var retired []RetiredSource
	if p.History != nil {
		retired = slices.Clone(p.History.Retired)
	}
	c := &CleanupProgress{Version: 1, SelectingMetadataSHA256: publicationSHA256(p.MetadataBytes), SelectingSetSHA256: digest, Retired: retired}
	c.SHA256 = cleanupSHA(*c)
	p.Cleanup = c
	if !preparing {
		if err = p.validatePublicationPredecessor(prior, destination, admission, policy, purpose); err != nil {
			return p, fmt.Errorf("selecting predecessor correspondence: %w", err)
		}
		p.Commit = &PublicationCommit{Version: 2, MetadataSHA256: publicationSHA256(p.MetadataBytes), SourceSetSHA256: digest, PayloadSetSHA256: payloadSetSHAWithFacts(sources, facts), PreparationSHA256: p.Preparation.SHA256, PrivacySHA256: privacyReceiptsSHA(p.privacyReceipts()), Predecessor: prior.State, DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, Purpose: purpose}
		if prior.State == PredecessorPresent {
			p.Commit.PredecessorSHA256 = publicationSHA256(prior.Body)
		}
	}
	if err := p.validatePublicationEnvelopeWithFacts(facts); err != nil {
		return p, fmt.Errorf("new selecting preparation envelope: %w", err)
	}
	return p, nil
}

func (owned *PendingPublication) validatePublicationEnvelope() error {
	return owned.validatePublicationEnvelopeWithFacts(nil)
}

func (owned *PendingPublication) validatePublicationEnvelopeWithFacts(facts *payloadDigestFacts) (err error) {
	defer func() { err = facts.result(err) }()
	p := *owned
	if p.JournalVersion != 5 || (p.Phase != PublicationPreparing && p.Phase != PublicationReady) || p.Preparation == nil || p.Cleanup == nil {
		return ErrDurableStorageRecovery
	}
	a := p.Preparation
	if a.Retention != nil && (p.RequestToken != "" || p.ScanSignature != nil || p.MetadataOnly || publicationSHA256(p.MetadataBytes) != a.OriginMetadataSHA256) {
		return ErrDurableStorageRecovery
	}
	if p.Commit != nil && p.Commit.SettledPrivacySHA256 != "" {
		return ErrDurableStorageRecovery
	}
	if len(p.MetadataBytes) > 32<<20 || len(a.OriginMetadata) > 32<<20 {
		return ErrDurableStorageCapacity
	}
	if err := a.validateWithFacts(facts); err != nil {
		return fmt.Errorf("frozen preparation authority: %w", err)
	}
	if a.Migration != nil && p.Phase == PublicationReady && a.Migration.NextMetadataSHA256 != publicationSHA256(p.MetadataBytes) {
		return ErrDurableStorageRecovery
	}
	var working archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &working); err != nil {
		return err
	}
	expected, err := selectedPublicationSources(p, a.DestinationID, a.AdmissionContext)
	if err != nil {
		return err
	}
	if len(expected) != len(p.Sources) {
		return ErrDurableStorageRecovery
	}
	if err := p.validatePreparationPayloads(expected, working, facts); err != nil {
		return err
	}
	if err := p.validatePreparationPhase(); err != nil {
		return err
	}
	if err := p.validatePreparationProgress(facts); err != nil {
		return err
	}
	c := p.Cleanup
	if c.Version != 1 || c.SHA256 != cleanupSHA(*c) || c.SelectingMetadataSHA256 != publicationSHA256(p.MetadataBytes) {
		return ErrDurableStorageRecovery
	}
	if p.History != nil && !slices.Equal(c.Retired, p.History.Retired) {
		return ErrDurableStorageRecovery
	}
	if p.Phase == PublicationReady {
		return p.validatePreparationCommit(facts)
	}
	return nil
}

// publicationWire clears compatibility byte fields; the union is the only wire payload.
func publicationWire(p PendingPublication) PendingPublication {
	if p.JournalVersion == 5 {
		p.SourceBytes = nil
		for i := range p.Sources {
			if len(p.Sources[i].Bytes) != 0 {
				p.Sources = slices.Clone(p.Sources)
				p.Sources[i].Bytes = nil
			}
		}
	}
	return p
}

// validate checks immutable preparation authority without a mutable cursor.
func (a PreparationAuthority) validateWithFacts(facts *payloadDigestFacts) (err error) {
	defer func() { err = facts.result(err) }()
	if len(a.OriginMetadata) == 0 || len(a.OriginMetadata) > 32<<20 {
		return ErrDurableStorageCapacity
	}
	if a.Version != 1 || (a.Kind != PreparationCapture && a.Kind != PreparationPrivacyCommitted && a.Kind != PreparationPrivacyPending && a.Kind != PreparationPrivacyPendingAbsent) || a.SHA256 != preparationSHAWithFacts(a, facts) || a.OriginMetadataSHA256 != publicationSHA256(a.OriginMetadata) || len(a.Inputs) == 0 || len(a.Inputs) > 65 {
		return ErrDurableStorageRecovery
	}
	if err := a.validateRetention(); err != nil {
		return err
	}
	if err := a.validateInputFacts(); err != nil {
		return err
	}
	if err := a.validatePurpose(); err != nil {
		return err
	}
	if err := a.validateMigration(); err != nil {
		return err
	}
	var origin archive.Metadata
	if err := json.Unmarshal(a.OriginMetadata, &origin); err != nil {
		return err
	}
	if a.SessionID != origin.SessionID || a.OwnerSHA256 != publicationOwner(origin, a.DestinationID, a.AdmissionContext) {
		return ErrDurableStorageRecovery
	}
	digest, refs, err := archive.PublicationIdentity(a.OriginMetadata, a.DestinationID, a.AdmissionContext, a.PolicyContext, string(a.Purpose))
	if err != nil || digest != a.OriginSetSHA256 || len(refs) != len(a.Inputs) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	for i, input := range a.Inputs {
		if input.Reference != refs[i] {
			return ErrDurableStorageRecovery
		}
		if err := validatePublicationPayloadWithFacts(PublicationSource{Reference: input.Reference, Selection: input.Selection, Payload: input.Payload}, origin, a.DestinationID, a.AdmissionContext, facts); err != nil {
			return err
		}
	}
	return nil
}

func (p *PendingPublication) validatePreparationProgress(facts *payloadDigestFacts) error {
	a := p.Preparation
	if p.History != nil || a.Purpose == PublicationPrivacyRewrite {
		cursor := len(a.Inputs)
		if p.History != nil {
			cursor = p.History.PrivacyCursor
		}
		g := p.Progress
		if g == nil || g.Version != 1 || g.AuthoritySHA256 != a.SHA256 || g.SHA256 != progressSHAWithFacts(*g, facts) || g.Cursor != cursor || len(g.Outputs) != g.Cursor || g.Cursor < 0 || g.Cursor > len(a.Inputs) || publicationSHA256(g.WorkingMetadata) != publicationSHA256(p.MetadataBytes) {
			return ErrDurableStorageRecovery
		}
		for i, output := range g.Outputs {
			if output.Privacy != nil {
				if err := output.Privacy.validate(*a, i, a.Inputs[i], output.Source); err != nil {
					return err
				}
			} else if output.Source.Reference != a.Inputs[i].Reference {
				return fmt.Errorf("changed frozen output without transformation receipt: %w", ErrDurableStorageRecovery)
			}
			if output.InputIndex != i || output.Source.Selection.RevisionID != a.Inputs[i].Selection.RevisionID {
				return ErrDurableStorageRecovery
			}
		}
	}
	return nil
}

func (p *PendingPublication) validatePreparationPayloads(expected []PublicationSource, working archive.Metadata, facts *payloadDigestFacts) error {
	a := p.Preparation
	inline, compressed := 0, 0
	for _, input := range a.Inputs {
		if h := input.HookObservations; h != nil {
			inline += len(h.Body)
			compressed += len(h.Body)
		}
	}
	for i, source := range p.Sources {
		if source.Reference != expected[i].Reference || source.Selection != expected[i].Selection || source.Reference.CompressedBytes <= 0 || source.Reference.CompressedBytes > 128<<20 || len(source.Bytes) != 0 {
			return ErrDurableStorageRecovery
		}
		if err := validatePublicationPayloadWithFacts(source, working, a.DestinationID, a.AdmissionContext, facts); err != nil {
			return err
		}
		inline += len(source.Payload.Inline)
		if source.Reference.CompressedBytes > (128<<20)-compressed && p.History != nil {
			return ErrDurableStorageCapacity
		}
		compressed += source.Reference.CompressedBytes
	}
	if inline > 128<<20 {
		return ErrDurableStorageCapacity
	}
	return nil
}

func (a PreparationAuthority) validatePurpose() error {
	switch a.Kind {
	case PreparationCapture:
		if a.Purpose != PublicationCapture && a.Purpose != PublicationMetadata || len(a.PrivacyPreviousMetadata) != 0 {
			return ErrDurableStorageRecovery
		}
	case PreparationPrivacyPendingAbsent:
		if a.Purpose != PublicationPrivacyRewrite || a.Predecessor != PredecessorAbsent || a.PredecessorSHA256 != "" || len(a.PrivacyPreviousMetadata) != 0 || a.Migration != nil {
			return ErrDurableStorageRecovery
		}
	case PreparationPrivacyCommitted:
		if a.Purpose != PublicationPrivacyRewrite || a.Predecessor != PredecessorPresent || a.PredecessorSHA256 != a.OriginMetadataSHA256 || len(a.PrivacyPreviousMetadata) != 0 || a.Migration != nil {
			return ErrDurableStorageRecovery
		}
	case PreparationPrivacyPending:
		if a.Purpose != PublicationPrivacyRewrite || a.Predecessor != PredecessorPresent || len(a.PrivacyPreviousMetadata) == 0 || len(a.PrivacyPreviousMetadata) > 32<<20 || publicationSHA256(a.PrivacyPreviousMetadata) != a.PredecessorSHA256 || a.Migration != nil {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func (p *PendingPublication) initializePreparation(prior PublicationPredecessor, m archive.Metadata, destination, admission, policy string, purpose PublicationPurpose, sources []PublicationSource, facts *payloadDigestFacts) error {
	digest, _, e := archive.PublicationIdentity(p.MetadataBytes, destination, admission, policy, string(purpose))
	if e != nil {
		return e
	}
	kind := PreparationCapture
	var previousPrivacy []byte
	if purpose == PublicationPrivacyRewrite {
		kind = PreparationPrivacyCommitted
		if prior.State == PredecessorAbsent {
			kind = PreparationPrivacyPendingAbsent
		}
		if prior.State == PredecessorPresent && !bytes.Equal(prior.Body, p.MetadataBytes) {
			kind = PreparationPrivacyPending
			previousPrivacy = slices.Clone(prior.Body)
		}
	}
	predecessorSHA := ""
	if prior.State == PredecessorPresent {
		predecessorSHA = publicationSHA256(prior.Body)
	}
	a := &PreparationAuthority{NativeTarget: p.nativeTarget, Version: 1, Kind: kind, PrivacyPreviousMetadata: previousPrivacy, PredecessorSHA256: predecessorSHA, SessionID: m.SessionID, OwnerSHA256: publicationOwner(m, destination, admission), DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, Purpose: purpose, Predecessor: prior.State, OriginMetadata: slices.Clone(p.MetadataBytes), OriginMetadataSHA256: publicationSHA256(p.MetadataBytes), OriginSetSHA256: digest}
	for _, source := range sources {
		input := PreparationInput{Reference: source.Reference, Selection: source.Selection, FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SkillPolicy: p.SkillEvidence, Payload: source.Payload}
		if p.History != nil {
			for _, h := range p.History.Inputs {
				if h.Reference == input.Reference {
					input.ParentSessionID = h.ParentSessionID
					input.NativeChild = h.NativeChild
					input.FilterVersion = h.FilterVersion
					input.AdapterVersion = h.AdapterVersion
					if input.AdapterVersion == "" && input.Selection.Role == PublicationCurrent {
						input.AdapterVersion = p.Bundle.Capture.AdapterVersion
					}
					input.Selection.CapturedAt = h.CapturedAt.UTC()
					input.Selection.SourceSchemaVersion = h.SourceSchemaVersion
				}
			}
		}
		if input.Selection.Role == PublicationCurrent {
			input.HookObservations = p.hookObservations
		}
		a.Inputs = append(a.Inputs, input)
	}
	if p.migration != nil {
		receipt := p.migration.receipt
		a.Migration = &receipt
	}
	a.SHA256 = preparationSHAWithFacts(*a, facts)
	p.Preparation = a
	return nil
}

func (p *PendingPublication) validatePreparationCommit(facts *payloadDigestFacts) error {
	a := p.Preparation
	if a.NativeTarget != nil {
		var next archive.Metadata
		if err := json.Unmarshal(p.MetadataBytes, &next); err != nil {
			return err
		}
		if next.ParentSessionID != a.NativeTarget.ParentSessionID || next.NativeChild != a.NativeTarget.NativeChild || p.Bundle.ParentSessionID != a.NativeTarget.ParentSessionID || p.Bundle.NativeChild != a.NativeTarget.NativeChild {
			return ErrDurableStorageRecovery
		}
	}
	if p.Commit.PreparationSHA256 != a.SHA256 || p.Commit.PayloadSetSHA256 != payloadSetSHAWithFacts(p.Sources, facts) {
		return ErrDurableStorageRecovery
	}
	if p.Commit.PrivacySHA256 != privacyReceiptsSHA(p.privacyReceipts()) {
		return ErrDurableStorageRecovery
	}
	if p.Commit.Purpose == PublicationPrivacyRewrite {
		if err := validatePrivacyCorrespondence(*a, p.Sources, p.privacyReceipts(), p.MetadataBytes, privacyPreviousBody(*a)); err != nil {
			return err
		}
	}
	return p.validateReadyPublicationWithFacts(facts)
}

func (a PreparationAuthority) validateMigration() error {
	if a.Migration != nil {
		r := a.Migration
		if len(r.PreviousMetadata) == 0 || len(r.PreviousMetadata) > 32<<20 {
			return ErrDurableStorageRecovery
		}
		var previous, next archive.Metadata
		if err := json.Unmarshal(r.PreviousMetadata, &previous); err != nil {
			return err
		}
		if err := json.Unmarshal(a.OriginMetadata, &next); err != nil {
			return err
		}
		if err := r.validate(previous, next, r.PreviousMetadata, a.OriginMetadata, a.DestinationID, a.AdmissionContext, a.PolicyContext); err != nil {
			return err
		}

	}
	return nil
}

func (p *PendingPublication) validatePreparationPhase() error {
	if p.Phase == PublicationPreparing {
		if p.Commit != nil || p.History == nil || !p.History.Preparing || p.Attempted {
			return ErrDurableStorageRecovery
		}
	} else if p.Commit == nil || p.Commit.Version != 2 || p.History != nil && p.History.Preparing {
		return fmt.Errorf("ready preparation commit: %w", ErrDurableStorageRecovery)
	}
	return nil
}

func (p *PendingPublication) initializeHistoryProgress(sources []PublicationSource, facts *payloadDigestFacts, preparing bool) error {
	p.History.Version = 2
	if !preparing && p.Progress == nil {
		if len(p.History.Inputs) == 0 {
			for _, input := range p.Preparation.Inputs {
				p.History.Inputs = append(p.History.Inputs, HistoryInput{Reference: input.Reference, RevisionID: input.Selection.RevisionID, CapturedAt: input.Selection.CapturedAt, SourceSchemaVersion: input.Selection.SourceSchemaVersion, FilterVersion: input.FilterVersion, AdapterVersion: input.AdapterVersion})
			}
		}
		p.History.PrivacyCursor = len(p.Preparation.Inputs)
	}
	cursor := p.History.PrivacyCursor
	progress := &PreparationProgress{Version: 1, AuthoritySHA256: p.Preparation.SHA256, Cursor: cursor, WorkingMetadata: slices.Clone(p.MetadataBytes)}
	for i := range cursor {
		input := p.Preparation.Inputs[i]
		var output *PublicationSource
		for j := range sources {
			if sources[j].Selection.RevisionID == input.Selection.RevisionID {
				output = &sources[j]
				break
			}
		}
		if output == nil {
			return errors.New("completed preparation input has no selected output")
		}
		var privacy *PrivacySource
		if proof, ok := p.privacyOutputs[i]; ok {
			factCopy := proof
			privacy = &factCopy
		} else if p.Progress != nil && i < len(p.Progress.Outputs) {
			privacy = p.Progress.Outputs[i].Privacy
		}
		progress.Outputs = append(progress.Outputs, PreparedOutput{InputIndex: i, Source: *output, Privacy: privacy})
	}
	progress.SHA256 = progressSHAWithFacts(*progress, facts)
	p.Progress = progress
	return nil
}

func (a PreparationAuthority) validateInputFacts() error {
	if a.NativeTarget != nil && a.NativeTarget.validate() != nil {
		return ErrDurableStorageRecovery
	}
	for _, input := range a.Inputs {
		if input.ParentSessionID != nil && len(*input.ParentSessionID) > 4096 {
			return ErrDurableStorageRecovery
		}
		if h := input.HookObservations; h != nil {
			if validatePublicationHookFacts(&h.Facts, input, a.OwnerSHA256, a.DestinationID, a.AdmissionContext, a.PolicyContext) != nil || len(h.Body) != h.Facts.BodySize || publicationSHA256(h.Body) != h.Facts.BodySHA256 {
				return ErrDurableStorageRecovery
			}
		}
	}
	return nil
}
