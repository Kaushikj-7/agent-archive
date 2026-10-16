package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// PublicationSelection binds the payload to its exact metadata role and age.
type PublicationSelection struct {
	Role                PublicationRole `json:"role"`
	RevisionID          string          `json:"revision_id,omitempty"`
	CapturedAt          time.Time       `json:"captured_at"`
	SourceSchemaVersion int             `json:"source_schema_version"`
}

// ImmutableStageHandle has no caller-selected filesystem path.
type ImmutableStageHandle struct {
	Version          int                    `json:"version"`
	Kind             PublicationPayloadKind `json:"kind"`
	Namespace        string                 `json:"namespace"`
	SessionID        string                 `json:"session_id"`
	OwnerSHA256      string                 `json:"owner_sha256"`
	DestinationID    string                 `json:"destination_id"`
	AdmissionContext string                 `json:"admission_context"`
	SourceSHA256     string                 `json:"source_sha256"`
	CompressedBytes  int                    `json:"compressed_bytes"`
	Selection        PublicationSelection   `json:"selection"`
}

// PublicationPayload is the closed single source-byte authority.
type PublicationPayload struct {
	Kind   PublicationPayloadKind `json:"kind"`
	Inline []byte                 `json:"inline,omitempty"`
	Stage  *ImmutableStageHandle  `json:"stage,omitempty"`
}

func publicationOwner(m archive.Metadata, destination, admission string) string {
	b, _ := json.Marshal(struct {
		Session     string `json:"Session"`
		Native      string `json:"Native"`
		Project     string `json:"Project"`
		Machine     string `json:"Machine"`
		Harness     string `json:"Harness"`
		Destination string `json:"Destination"`
		Admission   string `json:"Admission"`
	}{m.SessionID, m.NativeSessionID, m.ProjectID, m.MachineID, m.Harness.Name, destination, admission})
	return publicationSHA256(append([]byte("publication-owner/v1\x00"), b...))
}

func selectedPublicationSources(p PendingPublication, destination, admission string) ([]PublicationSource, error) {
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return nil, err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return nil, err
	}
	prior := make(map[string]PublicationSource, len(p.Sources))
	for _, s := range p.Sources {
		if _, ok := prior[s.Reference.Key]; ok {
			return nil, errors.New("duplicate publication payload")
		}
		prior[s.Reference.Key] = s
	}
	out := make([]PublicationSource, 0, len(refs))
	for i, ref := range refs {
		revision := ""
		if m.History != nil {
			revision = m.History.CurrentRevision
		}
		selection := PublicationSelection{Role: PublicationCurrent, RevisionID: revision, CapturedAt: m.CapturedAt.UTC(), SourceSchemaVersion: p.Bundle.SchemaVersion}
		if i > 0 {
			r := m.History.Preserved[i-1]
			selection = PublicationSelection{Role: PublicationPreserved, RevisionID: r.RevisionID, CapturedAt: r.CapturedAt.UTC(), SourceSchemaVersion: r.SourceSchemaVersion}
		}
		// Legacy metadata may omit a preserved source's producer schema. The
		// owning preparation has already decoded each exact input; bind that
		// provenance before constructing its handle, never change only one copy.
		selection, err = selectedHistoryProvenance(p.History, ref, selection)
		if err != nil {
			return nil, err
		}
		payload := PublicationPayload{Kind: PublicationRemote}
		if previous, ok := prior[ref.Key]; ok {
			if previous.Reference != ref {
				return nil, errors.New("payload reference changed")
			}
			payload = previous.Payload
			if payload.Kind == "" && len(previous.Bytes) > 0 {
				payload = PublicationPayload{Kind: PublicationInline, Inline: previous.Bytes}
			}
			delete(prior, ref.Key)
		}
		// Existing frozen inline preparations retain their exact payload authority.
		// New preparations can reuse the exact source already staged by history.
		preferCurrentStage := p.Preparation == nil || payload.Kind == PublicationHistoryStage
		if i == 0 && len(p.SourceBytes) > 0 {
			payload = PublicationPayload{Kind: PublicationInline, Inline: p.SourceBytes}
		}
		if p.History != nil && (i != 0 || len(p.SourceBytes) == 0 || preferCurrentStage) {
			payload = selectedHistoryStagePayload(p.History, PublicationSource{Reference: ref, Selection: selection, Payload: payload}, m, destination, admission)
		}
		if payload.Kind == "" {
			payload.Kind = PublicationRemote
		}
		out = append(out, PublicationSource{Reference: ref, Selection: selection, Payload: payload})
	}
	for _, unselected := range prior {
		known, retired := false, false
		if p.Preparation != nil && p.History != nil {
			for _, input := range p.Preparation.Inputs {
				known = known || input.Reference == unselected.Reference
			}
			for _, old := range p.History.Retired {
				retired = retired || old.Reference == unselected.Reference
			}
		}
		if !known || !retired {
			return nil, errors.New("unselected payload authority")
		}
	}

	return out, nil
}

// selectedHistoryStagePayload keeps the first exact staged reference, retaining
// the caller's payload when no matching stage exists.
func selectedHistoryStagePayload(history *PendingHistory, source PublicationSource, m archive.Metadata, destination, admission string) PublicationPayload {
	for _, stage := range history.Sources {
		if stage.Reference == source.Reference {
			ref := source.Reference
			return PublicationPayload{Kind: PublicationHistoryStage, Stage: &ImmutableStageHandle{Version: 1, Kind: PublicationHistoryStage, Namespace: "session-pending-sources", SessionID: m.SessionID, OwnerSHA256: publicationOwner(m, destination, admission), DestinationID: destination, AdmissionContext: admission, SourceSHA256: ref.SHA256, CompressedBytes: ref.CompressedBytes, Selection: source.Selection}}
		}
	}
	return source.Payload
}

func validatePublicationPayload(source PublicationSource, m archive.Metadata, destination, admission string) error {
	return validatePublicationPayloadWithFacts(source, m, destination, admission, nil)
}

func validatePublicationPayloadWithFacts(source PublicationSource, m archive.Metadata, destination, admission string, facts *payloadDigestFacts) (err error) {
	defer func() { err = facts.result(err) }()
	p := source.Payload
	switch p.Kind {
	case PublicationInline:
		if p.Stage != nil || len(p.Inline) != source.Reference.CompressedBytes || payloadInlineSHA(source, facts) != source.Reference.SHA256 {
			return errors.New("inline publication checksum or union mismatch")
		}
	case PublicationRemote:
		if p.Stage != nil || len(p.Inline) != 0 {
			return errors.New("remote publication union mismatch")
		}
	case PublicationHistoryStage:
		h := p.Stage
		if len(p.Inline) != 0 || h == nil || h.Version != 1 || h.Kind != p.Kind || (h.Namespace != "session-pending-sources" && h.Namespace != "session-deletion-sources") || h.SessionID != m.SessionID || h.OwnerSHA256 != publicationOwner(m, destination, admission) || h.DestinationID != destination || h.AdmissionContext != admission || h.SourceSHA256 != source.Reference.SHA256 || h.CompressedBytes != source.Reference.CompressedBytes || h.Selection != source.Selection {
			return errors.New("history stage authority mismatch")
		}
	case PublicationAdmissionStage:
		return errors.New("admission stage publication authority is unavailable")
	default:
		return fmt.Errorf("unsupported publication payload kind %q", p.Kind)
	}
	return nil
}

type payloadBinding struct {
	Reference    archive.SourceReference `json:"Reference"`
	Selection    PublicationSelection    `json:"Selection"`
	Kind         PublicationPayloadKind  `json:"Kind"`
	InlineSHA256 string                  `json:"InlineSHA256"`
	InlineSize   int                     `json:"InlineSize"`
	Stage        *ImmutableStageHandle   `json:"Stage"`
}

func payloadSetSHA(sources []PublicationSource) string { return payloadSetSHAWithFacts(sources, nil) }

func payloadSetSHAWithFacts(sources []PublicationSource, facts *payloadDigestFacts) string {
	bindings := make([]payloadBinding, len(sources))
	for i, s := range sources {
		inlineSHA, inlineSize := "", 0
		if s.Payload.Kind == PublicationInline {
			inlineSHA = payloadInlineSHA(s, facts)
			inlineSize = len(s.Payload.Inline)
		}
		bindings[i] = payloadBinding{Reference: s.Reference, Selection: s.Selection, Kind: s.Payload.Kind, Stage: s.Payload.Stage, InlineSHA256: inlineSHA, InlineSize: inlineSize}
	}
	raw, _ := json.Marshal(bindings)
	return publicationSHA256(append([]byte("payload-map/v1\x00"), raw...))
}

func selectedHistoryProvenance(history *PendingHistory, ref archive.SourceReference, selection PublicationSelection) (PublicationSelection, error) {
	if history != nil {
		for _, input := range history.Inputs {
			if input.Reference != ref {
				continue
			}
			if input.RevisionID != selection.RevisionID || !input.CapturedAt.Equal(selection.CapturedAt) || selection.SourceSchemaVersion != 0 && input.SourceSchemaVersion != selection.SourceSchemaVersion {
				return selection, errors.New("frozen history input disagrees with selected provenance")
			}
			selection.SourceSchemaVersion = input.SourceSchemaVersion
		}
	}
	return selection, nil
}
