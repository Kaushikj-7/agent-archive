package state

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// PublicationHookFacts binds a single current revision's owned observations.
// Settled authority keeps these facts and drops the sensitive original body.
type PublicationHookFacts struct {
	Version          int       `json:"version"`
	OwnerSHA256      string    `json:"owner_sha256"`
	RevisionID       string    `json:"revision_id"`
	CapturedAt       time.Time `json:"captured_at"`
	DestinationID    string    `json:"destination_id"`
	AdmissionContext string    `json:"admission_context"`
	PolicyContext    string    `json:"policy_context"`
	BodySHA256       string    `json:"body_sha256"`
	BodySize         int       `json:"body_size"`
}

// PublicationHookObservations owns one frozen current-revision hook body and its closed target facts.
type PublicationHookObservations struct {
	Facts PublicationHookFacts `json:"facts"`
	Body  []byte               `json:"body"`
}

// FreezePublicationHookObservations borrows an already charged immutable body
// from the owning current request/candidate. Only its actual transform can mint
// a PrivacySource receipt. It never consults a later request or native source.
func FreezePublicationHookObservations(ctx context.Context, p *PendingPublication, body []byte, destination, admission, policy string, budget *agentapi.NativeReadBudget) error {
	reservation := budget.Reservation()
	if p.Preparation != nil || p.Commit != nil || len(body) == 0 || len(body) > 32<<20 || len(p.MetadataBytes) > 32<<20 || budget == nil {
		return ErrDurableStorageRecovery
	}
	n := 8*int64(len(body)+len(p.MetadataBytes)) + 64<<10
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	defer reservation.Release(n)
	if err := ctx.Err(); err != nil {
		return err
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	var observations []archive.SupplementalEvidence
	if err := closedPublicationDecode(body, &observations); err != nil {
		return err
	}
	if len(observations) == 0 {
		return ErrDurableStorageRecovery
	}
	for _, item := range observations {
		if item.Kind != archive.EvidenceKindLinkedSession && item.Kind != archive.EvidenceKindExplicitFeedback {
			return ErrDurableStorageRecovery
		}
	}
	revision := metadata.NativeSessionID
	if metadata.History != nil {
		revision = metadata.History.CurrentRevision
	}
	p.hookObservations = &PublicationHookObservations{Facts: PublicationHookFacts{Version: 1, OwnerSHA256: publicationOwner(metadata, destination, admission), RevisionID: revision, CapturedAt: metadata.CapturedAt.UTC(), DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, BodySHA256: publicationSHA256(body), BodySize: len(body)}, Body: body}
	return nil
}

func validatePublicationHookFacts(h *PublicationHookFacts, input PreparationInput, owner, destination, admission, policy string) error {
	if h == nil {
		return nil
	}
	if h.Version != 1 || input.Selection.Role != PublicationCurrent || h.OwnerSHA256 != owner || h.RevisionID != input.Selection.RevisionID || !h.CapturedAt.Equal(input.Selection.CapturedAt) || h.DestinationID != destination || h.AdmissionContext != admission || h.PolicyContext != policy || h.BodySize <= 0 || h.BodySize > 32<<20 || !validPublicationDigest(h.BodySHA256) {
		return ErrDurableStorageRecovery
	}
	return nil
}

func publicationHookFacts(h *PublicationHookObservations) *PublicationHookFacts {
	if h == nil {
		return nil
	}
	facts := h.Facts
	return &facts
}
