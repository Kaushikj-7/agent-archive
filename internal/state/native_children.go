package state

import (
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// maxNativeChildLinks bounds the child sessions one Muse parent may link.
const maxNativeChildLinks = 1024

// errNativeChildLinks refuses child links that did not come from the parent's
// own verified filter pass.
var errNativeChildLinks = errors.New("muse child links require the parent's own verified filter pass; run sync to read the session again")

// nativeChildLinks reserves an archive session ID for each child session a
// Muse parent's own filter pass verified, and returns the parent's
// linked-session evidence, each link unavailable because no child transcript
// is captured. Each link is observed at the parent's start, so every pass
// over the same parent yields identical evidence. owner is the parent that the calling admission, import or
// validation already owns, bound to its native session, and filtered is that same pass's output: the only
// source of child IDs. A child that is a root session, or is reserved as one,
// is refused, so a link can never claim another session's identity.
// Reservations are idempotent, so a retried pass returns the same IDs.
func (s *Store) nativeChildLinks(owner archive.SessionRegistration, filtered archive.FilteredTranscript) ([]archive.SupplementalEvidence, error) {
	children := filtered.NativeChildIDs
	if len(children) == 0 {
		return nil, nil
	}
	if owner.Harness.Name != "muse" || owner.IsChild() || owner.NativeBinding == nil || owner.NativeBinding.NativeID != owner.NativeSessionID || len(filtered.SessionIDs) != 1 || filtered.SessionIDs[0] != owner.NativeSessionID || len(children) > maxNativeChildLinks || owner.SessionStartedAt.IsZero() {
		return nil, errNativeChildLinks
	}
	// Every ID qualifies before any is reserved.
	seen := make(map[string]bool, len(children))
	for _, child := range children {
		if child == owner.NativeSessionID || seen[child] || !nativeReservationNativeID(child) {
			return nil, errNativeChildLinks
		}
		seen[child] = true
	}
	out := make([]archive.SupplementalEvidence, 0, len(children))
	for _, child := range children {
		id, err := s.reserveNativeChild(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: child}, owner.NativeSessionID)
		if err != nil {
			return nil, err
		}
		link, err := archive.NewLinkedSessionEvidence(id, archive.LinkedSessionUnavailable, owner.SessionStartedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, nil
}

// saveNativeChildLinks records a live Muse parent's child links as request
// evidence, which the parent's next bundle carries. It does not ask for a
// flush: the links ride along with the parent's next publication.
func (s *Store) saveNativeChildLinks(owner archive.SessionRegistration, filtered archive.FilteredTranscript) error {
	links, err := s.nativeChildLinks(owner, filtered)
	if err != nil || len(links) == 0 {
		return err
	}
	return s.SaveEvidence(owner.ArchiveSessionID, "muse-child-links", owner.SessionStartedAt, links...)
}

// reserveNativeChild reserves a child key that no root session owns; a
// registered session under the key must be a child of parent. A plain
// reservation carries no native authority marker, so it can never stand in
// for a root admission or import, which refuse such an entry.
func (s *Store) reserveNativeChild(key agentmeta.SessionKey, parent string) (string, error) {
	if err := s.generationReservationAllowed(key); err != nil {
		return "", err
	}
	for attempt := 0; ; attempt++ {
		entry, found, err := s.readQualifiedIndex(key)
		if err != nil {
			return "", err
		}
		if found && !entry.Absent {
			if entry.NativeReservation != "" {
				return "", errNativeChildLinks
			}
			if !safeFileComponent(entry.ArchiveSessionID) {
				return "", ErrSessionIndexRecoveryRequired
			}
			reg, registered, err := s.LoadRegistration(entry.ArchiveSessionID)
			if err != nil {
				return "", err
			}
			if registered && (!reg.IsChild() || reg.ParentNativeSessionID != parent) {
				return "", errNativeChildLinks
			}
		}
		id, _, err := s.reserveArchiveSessionID(key)
		if errors.Is(err, errIndexMoved) && attempt == 0 {
			continue
		}
		return id, err
	}
}
