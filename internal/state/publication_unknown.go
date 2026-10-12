package state

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Future publication fields cannot become a legacy ready transaction simply
// because a configuration is missing or damaged. Reject them in the existing
// decode, without another body read or a second selecting schema.
type unsupportedPublicationField struct{}

func (*unsupportedPublicationField) UnmarshalJSON([]byte) error { return ErrDurableStorageRecovery }

type unsupportedPendingFields struct {
	Sources        unsupportedPublicationField `json:"sources"`
	JournalVersion unsupportedPublicationField `json:"journal_version"`
	Phase          unsupportedPublicationField `json:"phase"`
	Preparation    unsupportedPublicationField `json:"preparation"`
	Progress       unsupportedPublicationField `json:"progress"`
	Cleanup        unsupportedPublicationField `json:"cleanup"`
	AdmissionStage unsupportedPublicationField `json:"admission_stage"`
}

type unsupportedPublishedFields struct {
	PublicationVersion unsupportedPublicationField `json:"publication_version"`
	Commit             unsupportedPublicationField `json:"commit"`
	Sources            unsupportedPublicationField `json:"sources"`
	PredecessorUnknown unsupportedPublicationField `json:"predecessor_unknown"`
}

// UnmarshalJSON refuses foreign publication authority before legacy fallback.
func (p *PendingPublication) UnmarshalJSON(data []byte) error {
	type pendingJSON PendingPublication
	wire := struct {
		*pendingJSON
		unsupportedPendingFields
		Commit json.RawMessage `json:"commit"`
	}{pendingJSON: (*pendingJSON)(p)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	p.Catalog = nil
	if len(wire.Commit) != 0 {
		var commit CatalogPublication
		if err := json.Unmarshal(wire.Commit, &commit); err != nil {
			return err
		}
		p.Catalog = &commit
	}
	return nil
}

func (p *publishedState) UnmarshalJSON(data []byte) error {
	type publishedJSON publishedState
	wire := struct {
		*publishedJSON
		unsupportedPublishedFields
	}{publishedJSON: (*publishedJSON)(p)}
	return json.Unmarshal(data, &wire)
}

// UnmarshalJSON refuses foreign or incomplete commit descriptors before a
// protected catalog transaction could fall back to ordinary publication.
func (p *CatalogPublication) UnmarshalJSON(data []byte) error {
	var next struct {
		Protocol         *uint64               `json:"protocol"`
		ID               *string               `json:"id"`
		ExpectedRevision *string               `json:"expected_revision"`
		Recovery         *local.CatalogJournal `json:"recovery,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return ErrDurableStorageRecovery
	}
	if next.Protocol == nil || *next.Protocol != 10 || next.ID == nil || next.ExpectedRevision == nil || *next.ID == "" || len(*next.ID) > 128 || len(*next.ExpectedRevision) > 128 {
		return ErrDurableStorageRecovery
	}
	if next.Recovery != nil && (next.Recovery.Validate() != nil || next.Recovery.MutationID != *next.ID || next.Recovery.ExpectedRevision != *next.ExpectedRevision) {
		return ErrDurableStorageRecovery
	}
	*p = CatalogPublication{Protocol: *next.Protocol, ID: *next.ID, ExpectedRevision: *next.ExpectedRevision, Recovery: next.Recovery}
	return nil
}
