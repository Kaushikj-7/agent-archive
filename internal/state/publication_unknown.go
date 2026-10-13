package state

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/wirepolicy"
)

// Future publication fields cannot become a legacy ready transaction simply
// because a configuration is missing or damaged. Reject them in the existing
// decode, without another body read or a second selecting schema.
type unsupportedPublicationField struct{}

func (*unsupportedPublicationField) UnmarshalJSON([]byte) error { return ErrDurableStorageRecovery }

type unsupportedPendingFields struct {
	Sources        unsupportedPublicationField `json:"sources"`
	Phase          unsupportedPublicationField `json:"phase"`
	Preparation    unsupportedPublicationField `json:"preparation"`
	Progress       unsupportedPublicationField `json:"progress"`
	Cleanup        unsupportedPublicationField `json:"cleanup"`
	AdmissionStage unsupportedPublicationField `json:"admission_stage"`
}

type unsupportedPublishedFields struct {
	Cursor             unsupportedPublicationField `json:"cursor_commit4"`
	Staged             unsupportedPublicationField `json:"staged_commit3"`
	Selection          unsupportedPublicationField `json:"selection_commit"`
	Commit             unsupportedPublicationField `json:"commit"`
	Sources            unsupportedPublicationField `json:"sources"`
	PredecessorUnknown unsupportedPublicationField `json:"predecessor_unknown"`
}

// UnmarshalJSON refuses foreign publication authority before legacy fallback.
func (p *PendingPublication) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateSelectionKeys(data); err != nil {
		return err
	}
	type pendingJSON PendingPublication
	wire := struct {
		*pendingJSON
		unsupportedPendingFields
		Commit    json.RawMessage `json:"commit"`
		Version   json.RawMessage `json:"journal_version"`
		Selection json.RawMessage `json:"selection_commit"`
		Staged    json.RawMessage `json:"staged_commit3"`
		Cursor    json.RawMessage `json:"cursor_commit4"`
	}{pendingJSON: (*pendingJSON)(p)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	p.Catalog = nil
	p.Selection = nil
	p.StagedSelection = nil
	p.CursorSelection = nil
	p.JournalVersion = 0
	if len(wire.Commit) != 0 {
		var commit CatalogPublication
		if err := json.Unmarshal(wire.Commit, &commit); err != nil {
			return err
		}
		p.Catalog = &commit
	}
	if len(wire.Cursor) != 0 {
		if len(wire.Staged) != 0 || len(wire.Selection) != 0 || len(wire.Commit) != 0 {
			return ErrDurableStorageRecovery
		}
		if err := rejectStagedNullAliases(data, []string{"journal_version", "cursor_commit4", "staged_commit3", "selection_commit", "commit"}); err != nil {
			return err
		}
		closed := json.NewDecoder(bytes.NewReader(data))
		closed.DisallowUnknownFields()
		if err := closed.Decode(&wire); err != nil {
			return ErrDurableStorageRecovery
		}
		if err := decodePendingCursorProof(p, wire.Version, wire.Cursor); err != nil {
			return err
		}
		return p.validateStagedShape()
	}
	if len(wire.Staged) != 0 {
		if err := rejectStagedNullAliases(data, []string{"journal_version", "staged_commit3", "selection_commit", "commit"}); err != nil {
			return err
		}
		closed := json.NewDecoder(bytes.NewReader(data))
		closed.DisallowUnknownFields()
		if err := closed.Decode(&wire); err != nil {
			return ErrDurableStorageRecovery
		}
		if json.Unmarshal(wire.Version, &p.JournalVersion) != nil || p.JournalVersion != 3 || len(wire.Selection) != 0 || bytes.Equal(bytes.TrimSpace(wire.Staged), []byte("null")) {
			return ErrDurableStorageRecovery
		}
		if err := json.Unmarshal(wire.Staged, &p.StagedSelection); err != nil {
			return err
		}
		return p.validateStagedShape()
	}
	if len(wire.Version) != 0 || len(wire.Selection) != 0 {
		if json.Unmarshal(wire.Version, &p.JournalVersion) != nil || p.JournalVersion != 2 || len(wire.Selection) == 0 || bytes.Equal(bytes.TrimSpace(wire.Selection), []byte("null")) {
			return ErrDurableStorageRecovery
		}
		var proof ReadySelection
		if err := json.Unmarshal(wire.Selection, &proof); err != nil {
			return err
		}
		p.Selection = &proof
		return p.validateReadySelectionShape()
	}
	return nil
}

func (p *publishedState) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateAuthorityKeys(data, []string{"publication_version", "last_published"}); err != nil {
		return err
	}
	type publishedJSON publishedState
	wire := struct {
		*publishedJSON
		unsupportedPublishedFields
		Version json.RawMessage `json:"publication_version"`
	}{publishedJSON: (*publishedJSON)(p)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	p.PublicationVersion = 0
	if p.LastPublished != nil && p.LastPublished.StagedProof() != nil {
		closed := json.NewDecoder(bytes.NewReader(data))
		closed.DisallowUnknownFields()
		if err := closed.Decode(&wire); err != nil {
			return ErrDurableStorageRecovery
		}
		if err := rejectStagedNullAliases(data, []string{"publication_version", "last_published"}); err != nil {
			return err
		}
		if json.Unmarshal(wire.Version, &p.PublicationVersion) != nil || p.PublicationVersion != p.LastPublished.StagedProof().Version || p.LastPublished.Selection != nil {
			return ErrDurableStorageRecovery
		}
		return p.LastPublished.StagedProof().validateDigest()
	}
	hasProof := p.LastPublished != nil && p.LastPublished.Selection != nil
	if len(wire.Version) != 0 || hasProof {
		if json.Unmarshal(wire.Version, &p.PublicationVersion) != nil || p.PublicationVersion != 2 || !hasProof {
			return ErrDurableStorageRecovery
		}
		if err := p.LastPublished.Selection.validateProofDigest(); err != nil {
			return err
		}
	}
	return nil
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

// rejectDuplicateSelectionKeys scans only top-level authority keys. JSON validity
// and nesting retain encoding/json's existing limits; source strings are never
// materialized by this check or retained as another decoded body view.
func rejectDuplicateSelectionKeys(data []byte) error {
	return rejectDuplicateAuthorityKeys(data, []string{"commit", "selection_commit", "staged_commit3", "cursor_commit4", "journal_version"})
}

func rejectDuplicateAuthorityKeys(data []byte, names []string) error {
	return wirepolicy.RejectDuplicateAuthorityKeys(data, names)
}

func (p *publishedSnapshot) UnmarshalJSON(data []byte) error {
	if err := rejectStagedNullAliases(data, []string{"cursor_commit4"}); err != nil {
		return err
	}
	if err := rejectDuplicateAuthorityKeys(data, []string{"selection_commit", "staged_commit3", "cursor_commit4"}); err != nil {
		return err
	}
	type plain publishedSnapshot
	var next plain
	wire := struct {
		*plain
		Cursor *StagedSelection `json:"cursor_commit4"`
	}{plain: &next}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Cursor != nil {
		if next.StagedSelection != nil || wire.Cursor.Version != 4 {
			return ErrDurableStorageRecovery
		}
		next.CursorSelection = wire.Cursor
	} else if next.StagedSelection != nil && next.StagedSelection.Version != 3 {
		return ErrDurableStorageRecovery
	}
	if next.StagedSelection != nil || next.CursorSelection != nil {
		if err := rejectStagedNullAliases(data, []string{"staged_commit3", "cursor_commit4", "selection_commit", "bundle", "source", "published_at"}); err != nil {
			return err
		}
		closed := json.NewDecoder(bytes.NewReader(data))
		closed.DisallowUnknownFields()
		if err := closed.Decode(&wire); err != nil {
			return ErrDurableStorageRecovery
		}
	}
	*p = publishedSnapshot(next)
	return nil
}

func decodePendingCursorProof(p *PendingPublication, version, raw json.RawMessage) error {
	if json.Unmarshal(version, &p.JournalVersion) != nil || p.JournalVersion != 4 {
		return ErrDurableStorageRecovery
	}
	if err := json.Unmarshal(raw, &p.CursorSelection); err != nil {
		return err
	}
	if p.CursorSelection == nil || p.CursorSelection.Version != 4 {
		return ErrDurableStorageRecovery
	}
	return nil
}
