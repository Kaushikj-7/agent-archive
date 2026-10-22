package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// PublicationPrivacyOrigin is the oldest uncommitted ready selection that the
// Store retained before its first privacy successor replaced it. Policy is the
// skill-evidence ceiling that selection was authorized to publish.
type PublicationPrivacyOrigin struct {
	Pending PendingPublication
	Policy  config.SkillEvidence
	Sources []PublicationSource
}

// PublicationPrivacyOrigin returns the retained privacy original only while the
// Store links it to the installed current journal and prior is the exact
// position that original was itself prepared against. Any commit since then,
// a missing or unlinked original, or a different shape reports false, so the
// caller keeps its immediate restriction. Native files are never consulted.
func (s *Store) PublicationPrivacyOrigin(id string, current PendingPublication, prior PublicationPredecessor) (origin PublicationPrivacyOrigin, found bool, err error) {
	if current.JournalVersion != 5 || current.Phase != PublicationReady || current.Commit == nil || current.Commit.Purpose != PublicationPrivacyRewrite || current.Preparation == nil || prior.State == PredecessorUnknown || !safeFileComponent(id) {
		return origin, false, nil
	}
	owed, err := s.hasPublicationEvidence(id)
	if err != nil || !owed {
		return origin, false, err
	}
	reservation := s.readReservation()
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		raw, release, e := s.rootedPublicationBytes(g, evidencePath(id))
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		defer release()
		var evidence PublicationOriginalEvidence
		if e = closedPublicationDecode(raw, &evidence); e != nil {
			return e
		}
		if e = s.validateOriginalEvidence(evidence, id); e != nil {
			return e
		}
		role := evidence.PrivacyOrigin
		if role == nil || evidence.DestinationID != current.Commit.DestinationID || evidence.AdmissionContext != current.Commit.AdmissionContext {
			return nil
		}
		body, closeBody, e := s.rootedPublicationBytes(g, filepath.Join("pending", id+".json"))
		if e != nil {
			return e
		}
		defer closeBody()
		var installed PendingPublication
		if e = json.Unmarshal(body, &installed); e != nil {
			return errors.Join(ErrDurableStorageRecovery, e)
		}
		target := evidenceTarget(current, nil)
		if evidenceTarget(installed, body) != target || target != evidence.Link.Target && target != evidence.Link.PreviousTarget {
			return nil
		}
		var oldest PendingPublication
		if e = json.Unmarshal(role.Raw, &oldest); e != nil {
			return errors.Join(ErrDurableStorageRecovery, e)
		}
		if !privacyOriginPrecedes(oldest, current, prior) {
			return nil
		}
		sources, e := selectedPublicationSources(oldest, oldest.Commit.DestinationID, oldest.Commit.AdmissionContext)
		if e != nil {
			return errors.Join(ErrDurableStorageRecovery, e)
		}
		if s.resourceBudget != nil {
			n := role.Size
			if !reservation.Reserve(n) {
				return errStateBudget
			}
			*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
		}
		policy := config.SkillEvidence(oldest.SkillEvidence)
		if policy == "" {
			policy = config.SkillEvidenceBody
		}
		origin, found = PublicationPrivacyOrigin{Pending: oldest, Policy: policy, Sources: sources}, true
		return nil
	})
	if err != nil {
		return PublicationPrivacyOrigin{}, false, err
	}
	return origin, found, nil
}

// privacyOriginPrecedes requires the retained original to share the current
// owner and shape, and to have been prepared against the position still
// observed now. Otherwise a later selection has committed in between.
func privacyOriginPrecedes(oldest, current PendingPublication, prior PublicationPredecessor) bool {
	c := oldest.Commit
	if c == nil || oldest.JournalVersion != 5 || oldest.Phase != PublicationReady || oldest.Bundle.ArchiveSessionID != current.Bundle.ArchiveSessionID || (oldest.History == nil) != (current.History == nil) {
		return false
	}
	if c.DestinationID != current.Commit.DestinationID || c.AdmissionContext != current.Commit.AdmissionContext || c.Predecessor != prior.State {
		return false
	}
	return prior.State != PredecessorPresent || c.PredecessorSHA256 == publicationSHA256(prior.Body)
}

// OrdinaryAlternative describes the original's single selected source as an
// exact retained alternative bounded by the original's own policy ceiling.
func (o PublicationPrivacyOrigin) OrdinaryAlternative() (PublicationPrivacyAlternative, error) {
	p := o.Pending
	if p.History != nil || p.Bundle.SchemaVersion != archive.SourceSchemaVersion || len(o.Sources) != 1 {
		return PublicationPrivacyAlternative{}, ErrDurableStorageRecovery
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return PublicationPrivacyAlternative{}, err
	}
	if metadata.History != nil {
		return PublicationPrivacyAlternative{}, ErrDurableStorageRecovery
	}
	source := o.Sources[0]
	parent, child := p.Bundle.ParentSessionID, p.Bundle.NativeChild
	input := PreparationInput{ParentSessionID: &parent, NativeChild: &child, Reference: source.Reference, Selection: source.Selection, FilterVersion: metadata.FilterVersion, AdapterVersion: metadata.Adapter.Version, SkillPolicy: string(o.Policy), Payload: source.Payload}
	policy := PublicationPolicy{FilterVersion: input.FilterVersion, AdapterVersion: input.AdapterVersion, SkillEvidence: o.Policy}
	return PublicationPrivacyAlternative{Metadata: metadata, MetadataBody: p.MetadataBytes, Input: input, Policy: policy}, nil
}

// HistoryAuthorizes reports whether ref is an input or selected source of the
// original history journal for the same revision, so its bytes were produced
// within the original policy ceiling.
func (o PublicationPrivacyOrigin) HistoryAuthorizes(revision string, ref archive.SourceReference) bool {
	if o.Pending.History == nil {
		return false
	}
	for _, input := range o.Pending.History.Inputs {
		if input.RevisionID == revision && input.Reference == ref {
			return true
		}
	}
	for _, source := range o.Sources {
		if source.Selection.RevisionID == revision && source.Reference == ref {
			return true
		}
	}
	return false
}
