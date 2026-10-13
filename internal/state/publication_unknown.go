package state

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/local"
	"strings"
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
	}{pendingJSON: (*pendingJSON)(p)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	p.Catalog = nil
	p.Selection = nil
	p.StagedSelection = nil
	p.JournalVersion = 0
	if len(wire.Commit) != 0 {
		var commit CatalogPublication
		if err := json.Unmarshal(wire.Commit, &commit); err != nil {
			return err
		}
		p.Catalog = &commit
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
	if p.LastPublished != nil && p.LastPublished.StagedSelection != nil {
		closed := json.NewDecoder(bytes.NewReader(data))
		closed.DisallowUnknownFields()
		if err := closed.Decode(&wire); err != nil {
			return ErrDurableStorageRecovery
		}
		if err := rejectStagedNullAliases(data, []string{"publication_version", "last_published"}); err != nil {
			return err
		}
		if json.Unmarshal(wire.Version, &p.PublicationVersion) != nil || p.PublicationVersion != 3 || p.LastPublished.Selection != nil {
			return ErrDurableStorageRecovery
		}
		return p.LastPublished.StagedSelection.validateDigest()
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
	if next.Protocol == nil || (*next.Protocol != 10 && *next.Protocol != 11) || next.ID == nil || next.ExpectedRevision == nil || *next.ID == "" || len(*next.ID) > 128 || len(*next.ExpectedRevision) > 128 {
		return ErrDurableStorageRecovery
	}
	if next.Recovery != nil && (next.Recovery.Validate() != nil || next.Recovery.WriterProtocol() != *next.Protocol || next.Recovery.MutationID != *next.ID || next.Recovery.ExpectedRevision != *next.ExpectedRevision) {
		return ErrDurableStorageRecovery
	}
	*p = CatalogPublication{Protocol: *next.Protocol, ID: *next.ID, ExpectedRevision: *next.ExpectedRevision, Recovery: next.Recovery}
	return nil
}

// rejectDuplicateSelectionKeys scans only top-level authority keys. JSON validity
// and nesting retain encoding/json's existing limits; source strings are never
// materialized by this check or retained as another decoded body view.
func rejectDuplicateSelectionKeys(data []byte) error {
	return rejectDuplicateAuthorityKeys(data, []string{"commit", "selection_commit", "staged_commit3", "journal_version"})
}

func rejectDuplicateAuthorityKeys(data []byte, names []string) error {
	if !json.Valid(data) {
		return ErrDurableStorageRecovery
	}
	seen := map[string]bool{}
	maxKey := 0
	for _, name := range names {
		maxKey = max(maxKey, len(name))
	}
	depth, expectKey := 0, false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '{', '[':
			depth++
			if depth == 1 {
				expectKey = true
			}
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				expectKey = true
			}
		case ':':
			if depth == 1 {
				expectKey = false
			}
		case '"':
			start := i
			for i++; i < len(data); i++ {
				if data[i] == '\\' {
					i++
					continue
				}
				if data[i] == '"' {
					break
				}
			}
			if depth != 1 || !expectKey || i-start > 6*maxKey+2 {
				continue
			}
			var key string
			if json.Unmarshal(data[start:i+1], &key) != nil {
				return ErrDurableStorageRecovery
			}
			canonical := ""
			for _, name := range names {
				if strings.EqualFold(key, name) {
					canonical = name
					break
				}
			}
			if canonical == "" {
				continue
			}
			key = canonical
			if seen[key] {
				return ErrDurableStorageRecovery
			}
			seen[key] = true
		}
	}
	return nil
}

func (p *publishedSnapshot) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateAuthorityKeys(data, []string{"selection_commit", "staged_commit3"}); err != nil {
		return err
	}
	type plain publishedSnapshot
	var next plain
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	if next.StagedSelection != nil {
		if err := rejectStagedNullAliases(data, []string{"staged_commit3", "selection_commit", "bundle", "source", "published_at"}); err != nil {
			return err
		}
		closed := json.NewDecoder(bytes.NewReader(data))
		closed.DisallowUnknownFields()
		if err := closed.Decode(&next); err != nil {
			return ErrDurableStorageRecovery
		}
	}
	*p = publishedSnapshot(next)
	return nil
}
