package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// PublicationPolicy identifies a retained transform's exact filter and ceiling.
type PublicationPolicy struct {
	FilterVersion  string               `json:"filter_version"`
	AdapterVersion string               `json:"adapter_version"`
	SkillEvidence  config.SkillEvidence `json:"skill_evidence"`
}

// Context returns the canonical publication policy binding.
func (p PublicationPolicy) Context() string {
	return publicationSHA256([]byte(p.FilterVersion + "\x00" + p.AdapterVersion + "\x00" + string(p.SkillEvidence)))
}

func (p PublicationPolicy) validate() error {
	if p.FilterVersion == "" || p.AdapterVersion == "" || len(p.FilterVersion) > 4096 || len(p.AdapterVersion) > 4096 || p.SkillEvidence != config.SkillEvidenceNone && p.SkillEvidence != config.SkillEvidenceMetadata && p.SkillEvidence != config.SkillEvidenceBody {
		return ErrDurableStorageRecovery
	}
	return nil
}

// PrivacySource is a closed receipt of an executed retained transformation.
type PrivacySource struct {
	OriginalParentSessionID *string                 `json:"original_parent_session_id,omitempty"`
	OriginalNativeChild     *bool                   `json:"original_native_child,omitempty"`
	OutputParentSessionID   string                  `json:"output_parent_session_id,omitempty"`
	OutputNativeChild       bool                    `json:"output_native_child,omitempty"`
	NativeTargetSHA256      string                  `json:"native_target_sha256,omitempty"`
	Hook                    *PublicationHookFacts   `json:"hook,omitempty"`
	Covered                 []CoveredPrivacySource  `json:"covered,omitempty"`
	Version                 int                     `json:"version"`
	InputIndex              int                     `json:"input_index"`
	Previous                archive.SourceReference `json:"previous"`
	Next                    archive.SourceReference `json:"next"`
	Selection               PublicationSelection    `json:"selection"`
	SessionID               string                  `json:"session_id"`
	NativeSessionID         string                  `json:"native_session_id"`
	ProjectID               string                  `json:"project_id"`
	MachineID               string                  `json:"machine_id"`
	Harness                 archive.Harness         `json:"harness"`
	Origin                  archive.SessionOrigin   `json:"origin,omitempty"`
	ImportedAt              *time.Time              `json:"imported_at,omitempty"`
	StartedAtSource         archive.StartedAtSource `json:"started_at_source,omitempty"`
	PreviousGenerationID    string                  `json:"previous_generation_id,omitempty"`
	StartedAt               time.Time               `json:"started_at"`
	OwnerSHA256             string                  `json:"owner_sha256"`
	DestinationID           string                  `json:"destination_id"`
	AdmissionContext        string                  `json:"admission_context"`
	OriginMetadataSHA256    string                  `json:"origin_metadata_sha256"`
	PreviousPolicy          PublicationPolicy       `json:"previous_policy"`
	NextPolicy              PublicationPolicy       `json:"next_policy"`
	SHA256                  string                  `json:"sha256"`
}

func privacySourceSHA(r PrivacySource) string {
	r.SHA256 = ""
	raw, _ := json.Marshal(r)
	return publicationSHA256(append([]byte("privacy-source/v1\x00"), raw...))
}

// ValidatedPrivacySource is minted only by the leased filter and compression factory.
type ValidatedPrivacySource struct {
	receipt     PrivacySource
	budget      *agentapi.NativeReadBudget
	reservation *agentapi.NativeReadReservation
	ctx         context.Context
}

// PublicationContext identifies the frozen destination and admission scope.
type PublicationContext struct {
	NativeTarget     *PublicationNativeTarget `json:"NativeTarget,omitempty"`
	DestinationID    string                   `json:"DestinationID"`
	AdmissionContext string                   `json:"AdmissionContext"`
}

// LimitPublicationSkillEvidence is the one retained policy limiter used before
// refiltering. It never restores evidence removed by an earlier ceiling.
func LimitPublicationSkillEvidence(in []archive.SupplementalEvidence, mode config.SkillEvidence) []archive.SupplementalEvidence {
	if mode == config.SkillEvidenceBody {
		return in
	}
	out := make([]archive.SupplementalEvidence, 0, len(in))
	for _, item := range in {
		if item.Kind == archive.EvidenceKindSkillSnapshot || mode == config.SkillEvidenceNone && item.Kind == archive.EvidenceKindSkillInventory {
			continue
		}
		out = append(out, item)
	}
	return out
}

// RefilterPublicationInput decodes the exact original, runs the injected native
// filter and limiter, and owns the sole canonical compression/ref derivation.
// original is borrowed under the caller's existing source-byte lease.
func RefilterPublicationInput(ctx context.Context, registration archive.SessionRegistration, adapter agentapi.TranscriptFilter, origin archive.Metadata, originBody []byte, input PreparationInput, index int, original []byte, frozenContext PublicationContext, oldPolicy, nextPolicy PublicationPolicy, ceiling config.SkillEvidence, budget *agentapi.NativeReadBudget) (bundle archive.SourceBundle, packed archive.CompressedSource, ref archive.SourceReference, proof ValidatedPrivacySource, release func(), err error) {
	return refilterPublicationInput(ctx, registration, adapter, origin, originBody, input, index, original, frozenContext, oldPolicy, nextPolicy, ceiling, budget, nil, nil)
}

func refilterPublicationInput(ctx context.Context, registration archive.SessionRegistration, adapter agentapi.TranscriptFilter, origin archive.Metadata, originBody []byte, input PreparationInput, index int, original []byte, frozenContext PublicationContext, oldPolicy, nextPolicy PublicationPolicy, ceiling config.SkillEvidence, budget *agentapi.NativeReadBudget, sourceReader PublicationPrivacySourceReader, alternatives []PublicationPrivacyAlternative) (bundle archive.SourceBundle, packed archive.CompressedSource, ref archive.SourceReference, proof ValidatedPrivacySource, release func(), err error) {
	reservation := budget.Reservation()
	if frozenContext.DestinationID != registration.DestinationID || frozenContext.AdmissionContext == "" {
		return bundle, packed, ref, proof, nil, ErrDurableStorageRecovery
	}
	if budget == nil || len(originBody) == 0 || len(originBody) > 32<<20 || len(original) > 128<<20 {
		return bundle, packed, ref, proof, nil, ErrDurableStorageCapacity
	}
	if err = ctx.Err(); err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	admittedRegistration := registration
	registration, err = privacyNativeRegistration(registration, frozenContext.NativeTarget)
	if err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	if privacyFactoryInputInvalid(adapter, oldPolicy, nextPolicy, index, original, input, origin, registration) {
		return bundle, packed, ref, proof, nil, ErrDurableStorageRecovery
	}
	if ceiling != config.SkillEvidenceBody && ceiling != config.SkillEvidenceMetadata && ceiling != config.SkillEvidenceNone {
		return bundle, packed, ref, proof, nil, ErrDurableStorageRecovery
	}
	if err = validatePrivacyFrozenInput(origin, originBody, input, index, registration, frozenContext, nextPolicy, budget); err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	selected := origin
	if origin.History != nil {
		selected.SchemaVersion = archive.HistoryMetadataSchemaVersion
		selected.History = &archive.RevisionHistory{CurrentRevision: input.Selection.RevisionID}
	}
	selected.SourceBundle = input.Reference
	selected.CapturedAt = input.Selection.CapturedAt
	selected.FilterVersion = input.FilterVersion
	var leases []func()
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for i := len(leases) - 1; i >= 0; i-- {
				leases[i]()
			}
		})
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()
	var originalHeader publicationOriginalHeader
	var closeFiltered func()
	bundle, closeFiltered, err = filterPrivacySource(ctx, registration, adapter, selected, input, original, oldPolicy, nextPolicy, ceiling, budget, &originalHeader, admittedRegistration)
	if err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	leases = append(leases, closeFiltered)
	bundle, covered, alternativeLeases, err := filterPrivacyAlternatives(ctx, registration, adapter, origin, input, frozenContext, oldPolicy, nextPolicy, budget, sourceReader, alternatives, bundle, admittedRegistration)
	leases = append(leases, alternativeLeases...)
	if err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	if len(covered) > 0 {
		bundle, closeFiltered, err = agentapi.RefilterRetainedSource(ctx, registration, adapter, bundle, budget)
		if err != nil {
			return bundle, packed, ref, proof, nil, err
		}
		leases = append(leases, closeFiltered)
	}
	var closeCompressed func()
	packed, closeCompressed, err = agentapi.CompressRetainedSource(ctx, bundle, budget)
	if err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	leases = append(leases, closeCompressed)
	key, e := archive.SourceObjectKey(bundle, packed.SHA256)
	if e != nil {
		return bundle, packed, ref, proof, nil, e
	}
	ref = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	if !reservation.Reserve(16 << 10) {
		return bundle, packed, ref, proof, nil, agentapi.ErrReadBudget
	}
	leases = append(leases, func() { reservation.Release(16 << 10) })
	targetSHA, e := nativeTargetSHA(ctx, frozenContext.NativeTarget, budget)
	if e != nil {
		return bundle, packed, ref, proof, nil, e
	}
	originalParent := strings.Clone(originalHeader.ParentSessionID)
	originalMarker := originalHeader.NativeChild
	r := PrivacySource{OriginalParentSessionID: &originalParent, OriginalNativeChild: &originalMarker, OutputParentSessionID: bundle.ParentSessionID, OutputNativeChild: bundle.NativeChild, NativeTargetSHA256: targetSHA, Hook: publicationHookFacts(input.HookObservations), Version: 1, InputIndex: index, Previous: input.Reference, Next: ref, Selection: input.Selection, SessionID: origin.SessionID, NativeSessionID: origin.NativeSessionID, ProjectID: origin.ProjectID, MachineID: origin.MachineID, Harness: origin.Harness, Origin: origin.Origin, ImportedAt: origin.ImportedAt, StartedAtSource: origin.StartedAtSource, PreviousGenerationID: origin.PreviousGenerationID, StartedAt: origin.StartedAt.UTC(), OwnerSHA256: publicationOwner(origin, registration.DestinationID, frozenContext.AdmissionContext), DestinationID: registration.DestinationID, AdmissionContext: frozenContext.AdmissionContext, OriginMetadataSHA256: publicationSHA256(originBody), PreviousPolicy: oldPolicy, NextPolicy: nextPolicy}
	r, err = sealPrivacySourceReceipt(ctx, budget, r, covered)
	if err != nil {
		return bundle, packed, ref, proof, nil, err
	}
	proof = ValidatedPrivacySource{receipt: r, budget: budget, reservation: reservation, ctx: ctx}
	return bundle, packed, ref, proof, closeAll, nil
}

func sealPrivacySourceReceipt(ctx context.Context, budget *agentapi.NativeReadBudget, r PrivacySource, covered []CoveredPrivacySource) (PrivacySource, error) {
	reservation := budget.Reservation()
	const receiptScratch = 32 << 10
	if !reservation.Reserve(receiptScratch) {
		return r, agentapi.ErrReadBudget
	}
	r.SHA256 = r.OriginMetadataSHA256
	for i := range covered {
		covered[i].Next = r.Next
		covered[i].SHA256 = r.OriginMetadataSHA256
	}
	r.Covered = covered
	receiptBound, e := jsonwire.Bound(ctx, r, reservation.Available()/2)
	reservation.Release(receiptScratch)
	if e != nil {
		return r, e
	}
	if !reservation.Reserve(2 * receiptBound) {
		return r, agentapi.ErrReadBudget
	}
	for i := range covered {
		covered[i].Next = r.Next
		covered[i].SHA256 = coveredPrivacySHA(covered[i])
	}
	r.Covered = covered
	r.SHA256 = privacySourceSHA(r)
	reservation.Release(2 * receiptBound)
	return r, nil
}

func (r PrivacySource) validate(a PreparationAuthority, index int, input PreparationInput, output PublicationSource) error {
	var origin archive.Metadata
	if err := json.Unmarshal(a.OriginMetadata, &origin); err != nil {
		return err
	}
	if privacyReceiptBindingsChanged(r, a, index, input, output, origin) || input.ParentSessionID != nil && !reflect.DeepEqual(r.OriginalParentSessionID, input.ParentSessionID) || input.NativeChild != nil && !reflect.DeepEqual(r.OriginalNativeChild, input.NativeChild) || a.NativeTarget != nil && (r.OutputParentSessionID != a.NativeTarget.ParentSessionID || r.OutputNativeChild != a.NativeTarget.NativeChild || r.NativeTargetSHA256 != nativeTargetDigest(a.NativeTarget)) {
		return ErrDurableStorageRecovery
	}
	if !reflect.DeepEqual(r.Hook, publicationHookFacts(input.HookObservations)) {
		return ErrDurableStorageRecovery
	}
	return validateCoveredPrivacyFacts(r)
}

func validatePrivacyFrozenInput(origin archive.Metadata, originBody []byte, input PreparationInput, index int, registration archive.SessionRegistration, frozenContext PublicationContext, nextPolicy PublicationPolicy, budget *agentapi.NativeReadBudget) error {
	// Bind the supplied borrowed metadata view to its exact raw authority,
	// including full ordered manifest membership, before decoding source bytes.
	if err := validatePrivacyFactoryOrigin(origin, originBody, input, index, budget); err != nil {
		return err
	}
	if h := input.HookObservations; h != nil && validatePublicationHookFacts(&h.Facts, input, publicationOwner(origin, registration.DestinationID, frozenContext.AdmissionContext), registration.DestinationID, frozenContext.AdmissionContext, nextPolicy.Context()) != nil {
		return ErrDurableStorageRecovery
	}
	return nil
}

// RecordPrivacyOutput retains an independently charged factory receipt until its returned release.
func (p *PendingPublication) RecordPrivacyOutput(proof ValidatedPrivacySource) (func(), error) {
	r := proof.receipt
	if proof.budget == nil || proof.reservation == nil || r.Version != 1 || !validPublicationDigest(r.SHA256) {
		return nil, errors.New("privacy transform witness unavailable")
	}
	if err := proof.ctx.Err(); err != nil {
		return nil, err
	}
	const scratch = 32 << 10
	if !proof.reservation.Reserve(scratch) {
		return nil, agentapi.ErrReadBudget
	}
	n, err := jsonwire.Bound(proof.ctx, r, proof.reservation.Available()/3)
	proof.reservation.Release(scratch)
	if err != nil {
		return nil, err
	}
	if !proof.reservation.Reserve(3 * n) {
		return nil, agentapi.ErrReadBudget
	}
	if r.SHA256 != privacySourceSHA(r) {
		proof.reservation.Release(3 * n)
		return nil, ErrDurableStorageRecovery
	}
	raw, err := json.Marshal(r)
	var owned PrivacySource
	if err == nil {
		err = json.Unmarshal(raw, &owned)
	}
	if err != nil {
		proof.reservation.Release(3 * n)
		return nil, err
	}
	proof.reservation.Release(2 * n)
	if p.privacyOutputs == nil {
		p.privacyOutputs = make(map[int]PrivacySource)
	}
	p.privacyOutputs[r.InputIndex] = owned
	var once sync.Once
	return func() { once.Do(func() { proof.reservation.Release(n) }) }, nil
}

func (owned *PendingPublication) privacyReceipts() []PrivacySource {
	p := *owned
	var receipts []PrivacySource
	if p.Progress != nil {
		for _, output := range p.Progress.Outputs {
			if output.Privacy != nil {
				receipts = append(receipts, *output.Privacy)
			}
		}
	}
	return receipts
}

func privacyReceiptsSHA(receipts []PrivacySource) string {
	if len(receipts) == 0 {
		return ""
	}
	raw, _ := json.Marshal(receipts)
	return publicationSHA256(append([]byte("publication-privacy-correspondence/v1\x00"), raw...))
}

func privacyPreviousBody(a PreparationAuthority) []byte {
	if a.Kind == PreparationPrivacyCommitted {
		return a.OriginMetadata
	}
	return a.PrivacyPreviousMetadata
}

func validatePrivacyCorrespondence(a PreparationAuthority, sources []PublicationSource, receipts []PrivacySource, nextBody, previousBody []byte) error {
	if privacyPreparationPurposeInvalid(a) {
		return ErrDurableStorageRecovery
	}
	old, next, err := privacyCorrespondenceMetadata(a, nextBody, previousBody)
	if err != nil {
		return err
	}

	if a.NativeTarget != nil && (next.ParentSessionID != a.NativeTarget.ParentSessionID || next.NativeChild != a.NativeTarget.NativeChild) {
		return ErrDurableStorageRecovery
	}
	if privacyMetadataIdentityChanged(old, next, a) {
		return ErrDurableStorageRecovery
	}
	oldRefs, err := old.SourceReferences()
	if err != nil {
		return err
	}
	nextRefs, err := next.SourceReferences()
	if err != nil {
		return err
	}
	if len(oldRefs) != len(a.Inputs) || len(nextRefs) != len(sources) || len(sources) != len(a.Inputs) {
		return ErrDurableStorageRecovery
	}
	byIndex := make(map[int]PrivacySource, len(receipts))
	previousIndex := -1
	for _, receipt := range receipts {
		if receipt.InputIndex <= previousIndex || receipt.InputIndex >= len(a.Inputs) {
			return ErrDurableStorageRecovery
		}
		byIndex[receipt.InputIndex] = receipt
		previousIndex = receipt.InputIndex
	}
	if err := validatePrivacySourceCorrespondence(a, sources, old, oldRefs, nextRefs, byIndex, previousBody); err != nil {
		return err
	}
	if old.History == nil || next.History == nil {
		if old.History != nil || next.History != nil || len(sources) != 1 {
			return ErrDurableStorageRecovery
		}
	} else if old.History.CurrentRevision != next.History.CurrentRevision {
		return ErrDurableStorageRecovery
	}
	return nil
}

// RefilterCoveredPublicationInput runs the same factory with at most two exact
// retained alternatives, borrowing and releasing each before reading the next.
func RefilterCoveredPublicationInput(ctx context.Context, registration archive.SessionRegistration, adapter agentapi.TranscriptFilter, origin archive.Metadata, originBody []byte, input PreparationInput, index int, original []byte, frozenContext PublicationContext, oldPolicy, nextPolicy PublicationPolicy, ceiling config.SkillEvidence, budget *agentapi.NativeReadBudget, sourceReader PublicationPrivacySourceReader, alternatives []PublicationPrivacyAlternative) (archive.SourceBundle, archive.CompressedSource, archive.SourceReference, ValidatedPrivacySource, func(), error) {
	return refilterPublicationInput(ctx, registration, adapter, origin, originBody, input, index, original, frozenContext, oldPolicy, nextPolicy, ceiling, budget, sourceReader, alternatives)
}

func privacyFactoryInputInvalid(adapter agentapi.TranscriptFilter, oldPolicy, nextPolicy PublicationPolicy, index int, original []byte, input PreparationInput, origin archive.Metadata, registration archive.SessionRegistration) bool {
	return adapter == nil || oldPolicy.validate() != nil || nextPolicy.validate() != nil || nextPolicy.FilterVersion != archive.FilterVersion || nextPolicy.AdapterVersion != adapter.Version() || index < 0 || index >= 65 || len(original) != input.Reference.CompressedBytes || publicationSHA256(original) != input.Reference.SHA256 || input.Selection.Role != PublicationCurrent && input.Selection.Role != PublicationPreserved || input.Selection.CapturedAt.IsZero() || origin.SessionID != registration.ArchiveSessionID || origin.NativeSessionID != registration.NativeSessionID || origin.ProjectID != registration.ProjectID || origin.Harness.Name != registration.Harness.Name || origin.PreviousGenerationID != registration.PreviousGenerationID || !origin.StartedAt.Equal(registration.SessionStartedAt)
}

func privacyReceiptBindingsChanged(r PrivacySource, a PreparationAuthority, index int, input PreparationInput, output PublicationSource, origin archive.Metadata) bool {
	return r.Version != 1 || r.SHA256 != privacySourceSHA(r) || r.InputIndex != index || r.Previous != input.Reference || r.Next != output.Reference || r.Selection != input.Selection || output.Selection.Role != input.Selection.Role || output.Selection.RevisionID != input.Selection.RevisionID || !output.Selection.CapturedAt.Equal(input.Selection.CapturedAt) || output.Selection.SourceSchemaVersion != input.Selection.SourceSchemaVersion || privacyReceiptOwnerChanged(r, a, input, origin)
}

func privacyMetadataIdentityChanged(old, next archive.Metadata, a PreparationAuthority) bool {
	return old.SessionID != next.SessionID || old.NativeSessionID != next.NativeSessionID || old.ProjectID != next.ProjectID || old.MachineID != next.MachineID || old.Harness != next.Harness || old.Origin != next.Origin || !reflect.DeepEqual(old.ImportedAt, next.ImportedAt) || old.StartedAtSource != next.StartedAtSource || old.PreviousGenerationID != next.PreviousGenerationID || !old.StartedAt.Equal(next.StartedAt) || publicationOwner(old, a.DestinationID, a.AdmissionContext) != a.OwnerSHA256 || publicationOwner(next, a.DestinationID, a.AdmissionContext) != a.OwnerSHA256
}

func privacyReceiptOwnerChanged(r PrivacySource, a PreparationAuthority, input PreparationInput, origin archive.Metadata) bool {
	return r.SessionID != origin.SessionID || r.NativeSessionID != origin.NativeSessionID || r.ProjectID != origin.ProjectID || r.MachineID != origin.MachineID || r.Harness != origin.Harness || r.Origin != origin.Origin || !reflect.DeepEqual(r.ImportedAt, origin.ImportedAt) || r.StartedAtSource != origin.StartedAtSource || r.PreviousGenerationID != origin.PreviousGenerationID || !r.StartedAt.Equal(origin.StartedAt) || r.OwnerSHA256 != a.OwnerSHA256 || r.DestinationID != a.DestinationID || r.AdmissionContext != a.AdmissionContext || r.OriginMetadataSHA256 != a.OriginMetadataSHA256 || r.PreviousPolicy.FilterVersion != input.FilterVersion || r.PreviousPolicy.AdapterVersion != input.AdapterVersion || r.PreviousPolicy.SkillEvidence != config.SkillEvidence(input.SkillPolicy) || r.NextPolicy.Context() != a.PolicyContext || r.PreviousPolicy.validate() != nil || r.NextPolicy.validate() != nil
}

func validatePrivacyFactoryOrigin(origin archive.Metadata, originBody []byte, input PreparationInput, index int, budget *agentapi.NativeReadBudget) error {
	reservation := budget.Reservation()
	metadataCharge := int64(len(originBody)) + 64<<10
	if !reservation.Reserve(metadataCharge) {
		return agentapi.ErrReadBudget
	}
	var decodedOrigin archive.Metadata
	decodeErr := json.Unmarshal(originBody, &decodedOrigin)
	if decodeErr == nil && !reflect.DeepEqual(decodedOrigin, origin) {
		decodeErr = ErrDurableStorageRecovery
	}
	refs, refsErr := decodedOrigin.SourceReferences()
	if decodeErr == nil && refsErr != nil {
		decodeErr = refsErr
	}
	if decodeErr == nil && (index >= len(refs) || refs[index] != input.Reference) {
		decodeErr = ErrDurableStorageRecovery
	}
	reservation.Release(metadataCharge)
	if decodeErr != nil {
		return errors.Join(ErrDurableStorageRecovery, decodeErr)
	}
	return nil
}

func validatePrivacySourceCorrespondence(a PreparationAuthority, sources []PublicationSource, old archive.Metadata, oldRefs, nextRefs []archive.SourceReference, byIndex map[int]PrivacySource, previousBody []byte) error {
	for i, input := range a.Inputs {
		// A distinct pending original cannot stand in for acknowledged authority.
		// A different acknowledged source requires its own actual transform mapping.
		if sources[i].Reference != nextRefs[i] {
			return ErrDurableStorageRecovery
		}
		receipt, found := byIndex[i]
		var covered *CoveredPrivacySource
		if input.Reference != oldRefs[i] {
			if !found {
				return ErrDurableStorageRecovery
			}
			for j := range receipt.Covered {
				fact := &receipt.Covered[j]
				if fact.Reference == oldRefs[i] && fact.MetadataSHA256 == a.PredecessorSHA256 {
					if covered != nil {
						return ErrDurableStorageRecovery
					}
					covered = fact
				}
			}
			if covered == nil {
				return ErrDurableStorageRecovery
			}
			digest, _, e := archive.PublicationIdentity(previousBody, a.DestinationID, a.AdmissionContext, covered.Policy.Context(), string(PublicationPrivacyRewrite))
			if e != nil || digest != covered.SourceSetSHA256 {
				return ErrDurableStorageRecovery
			}
		}
		selection := input.Selection
		if covered != nil {
			selection = covered.Selection
		}
		expectedRole, expectedRevision, expectedAt := PublicationCurrent, "", old.CapturedAt
		if old.History != nil {
			expectedRevision = old.History.CurrentRevision
		}
		if i > 0 {
			expectedRole = PublicationPreserved
			expectedRevision = old.History.Preserved[i-1].RevisionID
			expectedAt = old.History.Preserved[i-1].CapturedAt
		}
		if selection.Role != expectedRole || selection.RevisionID != expectedRevision || !selection.CapturedAt.Equal(expectedAt) || sources[i].Selection.Role != input.Selection.Role || sources[i].Selection.RevisionID != input.Selection.RevisionID || !sources[i].Selection.CapturedAt.Equal(input.Selection.CapturedAt) {
			return ErrDurableStorageRecovery
		}
		if input.Reference != sources[i].Reference && !found {
			return ErrDurableStorageRecovery
		}
		if found {
			if err := receipt.validate(a, i, input, sources[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func privacyPreparationPurposeInvalid(a PreparationAuthority) bool {
	return a.Purpose != PublicationPrivacyRewrite || a.Kind != PreparationPrivacyCommitted && a.Kind != PreparationPrivacyPending && a.Kind != PreparationPrivacyPendingAbsent || a.Migration != nil
}

func filterPrivacyAlternatives(ctx context.Context, registration archive.SessionRegistration, adapter agentapi.TranscriptFilter, origin archive.Metadata, input PreparationInput, frozenContext PublicationContext, oldPolicy, nextPolicy PublicationPolicy, budget *agentapi.NativeReadBudget, sourceReader PublicationPrivacySourceReader, alternatives []PublicationPrivacyAlternative, bundle archive.SourceBundle, admittedRegistration archive.SessionRegistration) (archive.SourceBundle, []CoveredPrivacySource, []func(), error) {
	reservation := budget.Reservation()
	var leases []func()
	var closeFiltered func()
	var covered []CoveredPrivacySource
	if len(alternatives) > 2 || len(alternatives) > 0 && (sourceReader == nil || sourceReader.NativeReadBudget() != budget) {
		return bundle, covered, leases, ErrDurableStorageRecovery
	}
	for _, alternative := range alternatives {
		if alternative.Input.Reference == input.Reference {
			if alternative.Input.Selection != input.Selection || alternative.Policy != oldPolicy {
				return bundle, covered, leases, ErrDurableStorageRecovery
			}
			if _, e := validatePrivacyAlternative(alternative, origin, input, frozenContext, budget); e != nil {
				return bundle, covered, leases, e
			}
			continue
		}
		duplicate := false
		for _, receipt := range covered {
			if receipt.Reference == alternative.Input.Reference && receipt.Selection == alternative.Input.Selection && receipt.Policy == alternative.Policy && receipt.MetadataSHA256 == publicationSHA256(alternative.MetadataBody) {
				duplicate = true
			}
		}
		if duplicate {
			continue
		}
		setSHA, e := validatePrivacyAlternative(alternative, origin, input, frozenContext, budget)
		if e != nil {
			return bundle, covered, leases, e
		}
		if !reservation.Reserve(16 << 10) {
			return bundle, covered, leases, agentapi.ErrReadBudget
		}
		leases = append(leases, func() { reservation.Release(16 << 10) })
		var receipt CoveredPrivacySource
		bundle, receipt, closeFiltered, e = coverPrivacyAlternative(ctx, registration, adapter, bundle, alternative, setSHA, nextPolicy, sourceReader, budget, admittedRegistration)
		if e != nil {
			return bundle, covered, leases, e
		}
		leases = append(leases, closeFiltered)
		covered = append(covered, receipt)
	}
	return bundle, covered, leases, nil
}

func privacyNativeRegistration(registration archive.SessionRegistration, target *PublicationNativeTarget) (archive.SessionRegistration, error) {
	if target != nil {
		if target.ParentSessionID != "" && registration.ParentSessionID != "" && target.ParentSessionID != registration.ParentSessionID {
			return registration, ErrDurableStorageRecovery
		}
		if err := target.validate(); err != nil {
			return registration, err
		}
		registration = target.registration(registration)
	}
	return registration, nil
}

func privacyCorrespondenceMetadata(a PreparationAuthority, nextBody, previousBody []byte) (old, next archive.Metadata, err error) {
	var origin archive.Metadata
	if json.Unmarshal(nextBody, &next) != nil || json.Unmarshal(a.OriginMetadata, &origin) != nil {
		return old, next, ErrDurableStorageRecovery
	}
	if a.Kind == PreparationPrivacyPendingAbsent {
		if a.Predecessor != PredecessorAbsent || a.PredecessorSHA256 != "" || len(previousBody) != 0 || len(a.PrivacyPreviousMetadata) != 0 {
			return old, next, ErrDurableStorageRecovery
		}
		old = origin
	} else {
		if a.Predecessor != PredecessorPresent || len(previousBody) == 0 || len(previousBody) > 32<<20 || publicationSHA256(previousBody) != a.PredecessorSHA256 || !bytes.Equal(previousBody, privacyPreviousBody(a)) || json.Unmarshal(previousBody, &old) != nil {
			return old, next, ErrDurableStorageRecovery
		}
	}

	return old, next, nil
}
