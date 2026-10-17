package state

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// PreparePublication seals exact bytes before remote writes. Ref-only entries
// need remote verification; absence of inline bytes never permits regeneration.
func PreparePublication(p PendingPublication, prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) (PendingPublication, error) {
	if p.Commit != nil {
		err := p.ValidatePublication()
		return p, err
	}
	if purpose != PublicationCapture && purpose != PublicationMetadata {
		return p, errors.New("unsupported publication purpose")
	}
	if err := p.validatePublicationPredecessor(prior, destination, admission, policy, purpose); err != nil {
		return p, err
	}
	digest, refs, err := archive.PublicationIdentity(p.MetadataBytes, destination, admission, policy, string(purpose))
	if err != nil {
		return p, err
	}
	provided := map[string]PublicationSource{}
	for _, source := range p.Sources {
		if _, exists := provided[source.Reference.Key]; exists {
			return p, errors.New("duplicate pending source payload")
		}
		provided[source.Reference.Key] = source
	}
	p.Sources = nil
	for _, ref := range refs {
		source := PublicationSource{Reference: ref}
		if payload, ok := provided[ref.Key]; ok {
			if payload.Reference != ref {
				return p, errors.New("pending payload reference differs from metadata")
			}
			source = payload
			delete(provided, ref.Key)
		}
		p.Sources = append(p.Sources, source)
	}
	if len(provided) != 0 {
		return p, errors.New("pending payload is not selected by metadata")
	}
	p.Commit = &PublicationCommit{Version: 1, MetadataSHA256: publicationSHA256(p.MetadataBytes), SourceSetSHA256: digest, Predecessor: prior.State, DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, Purpose: purpose}
	if prior.State == PredecessorPresent {
		p.Commit.PredecessorSHA256 = publicationSHA256(prior.Body)
	}
	err = p.ValidatePublication()
	return p, err
}
