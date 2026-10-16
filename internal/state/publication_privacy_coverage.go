package state

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"reflect"
	"slices"
)

// PublicationPrivacySourceReader borrows retained checksum-addressed bytes from
// the existing owner scope. It supplies no native path or coverage authority.
type PublicationPrivacySourceReader interface {
	ReadPublicationPrivacySource(context.Context, PreparationInput) ([]byte, func(), error)
	NativeReadBudget() *agentapi.NativeReadBudget
}

// PublicationPrivacyAlternative borrows an exact owning manifest and selected retained input.
type PublicationPrivacyAlternative struct {
	Metadata     archive.Metadata  `json:"Metadata"`
	MetadataBody []byte            `json:"MetadataBody"`
	Input        PreparationInput  `json:"Input"`
	Policy       PublicationPolicy `json:"Policy"`
}

// CoveredPrivacySource records an alternative actually compared by the factory.
// The parent PrivacySource binds owner/context, original and final output.
type CoveredPrivacySource struct {
	ParentSessionID *string                 `json:"parent_session_id,omitempty"`
	NativeChild     *bool                   `json:"native_child,omitempty"`
	Version         int                     `json:"version"`
	MetadataSHA256  string                  `json:"metadata_sha256"`
	SourceSetSHA256 string                  `json:"source_set_sha256"`
	Reference       archive.SourceReference `json:"reference"`
	Selection       PublicationSelection    `json:"selection"`
	Policy          PublicationPolicy       `json:"policy"`
	Next            archive.SourceReference `json:"next"`
	Direction       string                  `json:"direction"`
	SHA256          string                  `json:"sha256"`
}

func coveredPrivacySHA(r CoveredPrivacySource) string {
	r.SHA256 = ""
	raw, _ := json.Marshal(r)
	return publicationSHA256(append([]byte("covered-privacy-source/v1\x00"), raw...))
}

type publicationOriginalHeader struct {
	ParentSessionID string
	NativeChild     bool
}

func filterPrivacySource(ctx context.Context, registration archive.SessionRegistration, adapter agentapi.TranscriptFilter, selected archive.Metadata, input PreparationInput, original []byte, oldPolicy, nextPolicy PublicationPolicy, ceiling config.SkillEvidence, budget *agentapi.NativeReadBudget, header *publicationOriginalHeader, admittedRegistration archive.SessionRegistration) (archive.SourceBundle, func(), error) {
	reservation := budget.Reservation()
	bundle, closeOriginal, err := decodePrivacyOriginal(ctx, registration, selected, input, original, budget, admittedRegistration)
	if err != nil {
		return bundle, nil, err
	}
	if header != nil {
		header.ParentSessionID = bundle.ParentSessionID
		header.NativeChild = bundle.NativeChild
	}
	if len(bundle.ParentSessionID) > 4096 || input.NativeChild != nil && bundle.NativeChild != *input.NativeChild {
		closeOriginal()
		return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
	}
	if bundle.SchemaVersion != input.Selection.SourceSchemaVersion && input.Selection.SourceSchemaVersion != 0 || !bundle.Capture.CapturedAt.Equal(input.Selection.CapturedAt) || bundle.Capture.FilterVersion != oldPolicy.FilterVersion || bundle.Capture.AdapterVersion != oldPolicy.AdapterVersion {
		closeOriginal()
		return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
	}
	var closeObservations func()
	if h := input.HookObservations; h != nil {
		if validatePublicationHookFacts(&h.Facts, input, publicationOwner(selected, h.Facts.DestinationID, h.Facts.AdmissionContext), h.Facts.DestinationID, h.Facts.AdmissionContext, nextPolicy.Context()) != nil || len(h.Body) != h.Facts.BodySize || publicationSHA256(h.Body) != h.Facts.BodySHA256 {
			closeOriginal()
			return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
		}
		n := 8*int64(len(h.Body)) + 64<<10
		if !reservation.Reserve(n) {
			closeOriginal()
			return archive.SourceBundle{}, nil, errStateBudget
		}
		closeObservations = func() { reservation.Release(n) }
		var observations []archive.SupplementalEvidence
		if err := closedPublicationDecode(h.Body, &observations); err != nil {
			closeObservations()
			closeOriginal()
			return archive.SourceBundle{}, nil, err
		}
		for _, item := range observations {
			if item.Kind != archive.EvidenceKindLinkedSession && item.Kind != archive.EvidenceKindExplicitFeedback {
				closeObservations()
				closeOriginal()
				return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
			}
		}
		bundle.SupplementalEvidence = archive.MergeSupplementalEvidence(bundle.SupplementalEvidence, observations)
	}
	bundle.SupplementalEvidence = LimitPublicationSkillEvidence(bundle.SupplementalEvidence, ceiling)
	bundle.SupplementalEvidence = LimitPublicationSkillEvidence(bundle.SupplementalEvidence, nextPolicy.SkillEvidence)
	filtered, closeFiltered, err := agentapi.RefilterRetainedSource(ctx, registration, adapter, bundle, budget)
	if err != nil {
		if closeObservations != nil {
			closeObservations()
		}
		closeOriginal()
		return archive.SourceBundle{}, nil, err
	}
	return filtered, func() {
		closeFiltered()
		if closeObservations != nil {
			closeObservations()
		}
		closeOriginal()
	}, nil
}

func validatePrivacyAlternative(alternative PublicationPrivacyAlternative, origin archive.Metadata, input PreparationInput, context PublicationContext, budget *agentapi.NativeReadBudget) (string, error) {
	reservation := budget.Reservation()
	if len(alternative.MetadataBody) == 0 || len(alternative.MetadataBody) > 32<<20 || alternative.Policy.validate() != nil || alternative.Input.Selection.Role != input.Selection.Role || alternative.Input.Selection.RevisionID != input.Selection.RevisionID || alternative.Input.Selection.CapturedAt.IsZero() {
		return "", ErrDurableStorageRecovery
	}
	n := int64(len(alternative.MetadataBody)) + (64 << 10)
	if !reservation.Reserve(n) {
		return "", agentapi.ErrReadBudget
	}
	defer reservation.Release(n)
	var metadata archive.Metadata
	if err := json.Unmarshal(alternative.MetadataBody, &metadata); err != nil {
		return "", err
	}
	if privacyAlternativeOwnerChanged(metadata, origin, alternative, context) {
		return "", ErrDurableStorageRecovery
	}
	digest, refs, err := archive.PublicationIdentity(alternative.MetadataBody, context.DestinationID, context.AdmissionContext, alternative.Policy.Context(), string(PublicationPrivacyRewrite))
	if err != nil {
		return "", err
	}
	found := false
	for i, ref := range refs {
		if ref != alternative.Input.Reference {
			continue
		}
		revision := ""
		if metadata.History != nil {
			revision = metadata.History.CurrentRevision
		}
		selection := PublicationSelection{Role: PublicationCurrent, RevisionID: revision, CapturedAt: metadata.CapturedAt.UTC(), SourceSchemaVersion: alternative.Input.Selection.SourceSchemaVersion}
		filter := metadata.FilterVersion
		if i > 0 {
			r := metadata.History.Preserved[i-1]
			selection = PublicationSelection{Role: PublicationPreserved, RevisionID: r.RevisionID, CapturedAt: r.CapturedAt.UTC(), SourceSchemaVersion: r.SourceSchemaVersion}
			filter = r.FilterVersion
		}
		if selection != alternative.Input.Selection || alternative.Input.FilterVersion != filter || alternative.Policy.FilterVersion != filter || alternative.Policy.AdapterVersion != alternative.Input.AdapterVersion || string(alternative.Policy.SkillEvidence) != alternative.Input.SkillPolicy {
			return "", ErrDurableStorageRecovery
		}
		found = true
	}
	if !found {
		return "", errors.New("privacy alternative is absent from owning manifest")
	}
	return digest, nil
}

func coverPrivacyAlternative(ctx context.Context, registration archive.SessionRegistration, adapter agentapi.TranscriptFilter, chosen archive.SourceBundle, alternative PublicationPrivacyAlternative, setSHA string, nextPolicy PublicationPolicy, sourceReader PublicationPrivacySourceReader, budget *agentapi.NativeReadBudget, admittedRegistration archive.SessionRegistration) (archive.SourceBundle, CoveredPrivacySource, func(), error) {
	reservation := budget.Reservation()
	var receipt CoveredPrivacySource
	original, closeBytes, err := sourceReader.ReadPublicationPrivacySource(ctx, alternative.Input)
	if err != nil {
		return archive.SourceBundle{}, receipt, nil, err
	}
	defer closeBytes()
	if len(original) != alternative.Input.Reference.CompressedBytes || len(original) > 128<<20 || publicationSHA256(original) != alternative.Input.Reference.SHA256 {
		return archive.SourceBundle{}, receipt, nil, ErrDurableStorageRecovery
	}
	selected := alternative.Metadata
	selected.SourceBundle = alternative.Input.Reference
	if alternative.Metadata.History != nil {
		selected.SchemaVersion = archive.HistoryMetadataSchemaVersion
		selected.History = &archive.RevisionHistory{CurrentRevision: alternative.Input.Selection.RevisionID}
	}
	selected.CapturedAt = alternative.Input.Selection.CapturedAt
	selected.FilterVersion = alternative.Input.FilterVersion
	var header publicationOriginalHeader
	candidate, closeCandidate, err := filterPrivacySource(ctx, registration, adapter, selected, alternative.Input, original, alternative.Policy, nextPolicy, alternative.Policy.SkillEvidence, budget, &header, admittedRegistration)
	if err != nil {
		return archive.SourceBundle{}, receipt, nil, err
	}
	defer closeCandidate()
	comparisonCharge := int64(len(chosen.NativeRecords)+len(candidate.NativeRecords))*16 + (32 << 10)
	if !reservation.Reserve(comparisonCharge) {
		return archive.SourceBundle{}, receipt, nil, agentapi.ErrReadBudget
	}
	defer reservation.Release(comparisonCharge)
	if !agentapi.OwnedEvidenceCovered(adapter, candidate, chosen) {
		return archive.SourceBundle{}, receipt, nil, errors.New("frozen original does not cover acknowledged revision evidence")
	}
	// Reserve the two auxiliary envelopes before any merge slice allocation.
	envelope := struct {
		A []archive.SupplementalEvidence `json:"A"`
		B []archive.SupplementalEvidence `json:"B"`
		C []archive.CaptureGap           `json:"C"`
		D []archive.CaptureGap           `json:"D"`
	}{chosen.SupplementalEvidence, candidate.SupplementalEvidence, chosen.Capture.Gaps, candidate.Capture.Gaps}
	n, err := jsonwire.Bound(ctx, envelope, reservation.Available())
	if err != nil {
		return archive.SourceBundle{}, receipt, nil, err
	}
	if !reservation.Reserve(n) {
		return archive.SourceBundle{}, receipt, nil, agentapi.ErrReadBudget
	}
	defer reservation.Release(n)
	chosen.SupplementalEvidence = archive.MergeSupplementalEvidence(chosen.SupplementalEvidence, candidate.SupplementalEvidence)
	gaps := append([]archive.CaptureGap(nil), chosen.Capture.Gaps...)
	for _, gap := range candidate.Capture.Gaps {
		if !slices.Contains(gaps, gap) {
			gaps = append(gaps, gap)
		}
	}
	chosen.Capture.Gaps = gaps
	detached, closeEnvelope, err := agentapi.DetachRetainedEnvelope(ctx, chosen, budget)
	if err != nil {
		return archive.SourceBundle{}, receipt, nil, err
	}
	parent := header.ParentSessionID
	marker := header.NativeChild
	receipt = CoveredPrivacySource{ParentSessionID: &parent, NativeChild: &marker, Version: 1, MetadataSHA256: publicationSHA256(alternative.MetadataBody), SourceSetSHA256: setSHA, Reference: alternative.Input.Reference, Selection: alternative.Input.Selection, Policy: alternative.Policy, Direction: "alternative-covered-by-original"}
	return detached, receipt, closeEnvelope, nil
}

func validateCoveredPrivacyFacts(r PrivacySource) error {
	if len(r.Covered) > 2 {
		return ErrDurableStorageRecovery
	}
	for i, fact := range r.Covered {
		if fact.Version != 1 || fact.SHA256 != coveredPrivacySHA(fact) || fact.Direction != "alternative-covered-by-original" || fact.Next != r.Next || !validPublicationDigest(fact.MetadataSHA256) || !validPublicationDigest(fact.SourceSetSHA256) || !validSourceReference(fact.Reference) || fact.Reference.CompressedBytes <= 0 || fact.Reference.CompressedBytes > 128<<20 || fact.Selection.Role != r.Selection.Role || fact.Selection.RevisionID != r.Selection.RevisionID || fact.Selection.CapturedAt.IsZero() || fact.Policy.validate() != nil {
			return ErrDurableStorageRecovery
		}
		for j := range i {
			other := r.Covered[j]
			if fact.MetadataSHA256 == other.MetadataSHA256 && fact.Reference == other.Reference {
				return ErrDurableStorageRecovery
			}
		}
	}
	return nil
}

func privacyAlternativeOwnerChanged(metadata, origin archive.Metadata, alternative PublicationPrivacyAlternative, context PublicationContext) bool {
	return !reflect.DeepEqual(metadata, alternative.Metadata) || metadata.SessionID != origin.SessionID || metadata.NativeSessionID != origin.NativeSessionID || metadata.ProjectID != origin.ProjectID || metadata.MachineID != origin.MachineID || metadata.Harness != origin.Harness || metadata.Origin != origin.Origin || !sameOptionalTime(metadata.ImportedAt, origin.ImportedAt) || metadata.StartedAtSource != origin.StartedAtSource || metadata.PreviousGenerationID != origin.PreviousGenerationID || !metadata.StartedAt.Equal(origin.StartedAt) || publicationOwner(metadata, context.DestinationID, context.AdmissionContext) != publicationOwner(origin, context.DestinationID, context.AdmissionContext)
}

func decodePrivacyOriginal(ctx context.Context, registration archive.SessionRegistration, selected archive.Metadata, input PreparationInput, original []byte, budget *agentapi.NativeReadBudget, admittedRegistration archive.SessionRegistration) (archive.SourceBundle, func(), error) {
	if input.ParentSessionID != nil {
		selected.ParentSessionID = *input.ParentSessionID
	}
	decode := func() (archive.SourceBundle, func(), error) {
		if selected.History == nil {
			return agentapi.DecodeReferencedSourceLeased(ctx, selected, original, agentapi.SourceReadLimits{}, budget)
		}
		return agentapi.DecodeRevisionSourceLeased(ctx, selected, input.Selection.RevisionID, original, agentapi.SourceReadLimits{}, budget)
	}
	bundle, closeOriginal, err := decode()
	if err != nil && input.ParentSessionID == nil && selected.NativeChild && registration.NativeChild && selected.ParentSessionID != "" {
		selected.ParentSessionID = ""
		bundle, closeOriginal, err = decode()
		if err == nil && !bundle.NativeChild {
			closeOriginal()
			return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
		}
	}
	if err != nil {
		return bundle, nil, err
	}
	if (bundle.ParentSessionID != registration.ParentSessionID || bundle.NativeChild != registration.NativeChild) && !agentapi.RetainedNativeChildOwned(admittedRegistration, bundle) {
		closeOriginal()
		return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
	}
	// A frozen output descriptor cannot turn an unbound registration into a
	// legacy child license. Positive checksum-decoded headers remain sufficient.
	if !bundle.NativeChild && registration.NativeChild {
		current := admittedRegistration.CodexBinding
		frozen := registration.CodexBinding
		if !agentapi.RetainedNativeChildOwned(admittedRegistration, bundle) || current == nil || frozen == nil || !current.PreservesFacts(frozen) || admittedRegistration.NativeSourceHome != registration.NativeSourceHome || frozen.ParentID != "" && admittedRegistration.ParentNativeSessionID != registration.ParentNativeSessionID || frozen.RootID != "" && admittedRegistration.NativeRootSessionID != registration.NativeRootSessionID {
			closeOriginal()
			return archive.SourceBundle{}, nil, ErrDurableStorageRecovery
		}
	}
	return bundle, closeOriginal, nil
}
