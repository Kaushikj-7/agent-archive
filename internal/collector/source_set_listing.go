package collector

import (
	"bytes"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

var errListingMetadataAbsent = errors.New("listing repair metadata is absent")

// repairListingRevision borrows every metadata/source response from the same
// pass ledger. A source proof never authorizes a different metadata winner.
func (s *sessionScan) repairListingRevision(key string) error {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	body, _, err := s.historyGetRevision(s.ctx, key, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		return errListingMetadataAbsent
	}
	if err != nil {
		return err
	}
	_, refs, err := archive.PublicationIdentity(body, s.reg.DestinationID, "", "", "listing_repair")
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.verifyHistorySource(s.ctx, ref.Key, ref.SHA256, ref.CompressedBytes); err != nil {
			return err
		}
	}
	// Bind the actual bounded post-verification response and its validator.
	// A later concurrent replacement may leave an advisory stale hint; it
	// cannot make a different winner inherit these source proofs.
	current, etag, err := s.historyGetRevision(s.ctx, key, int64(len(body)))
	if errors.Is(err, storage.ErrNotFound) {
		return errListingMetadataAbsent
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, body) {
		return storage.ErrPublicationConflict
	}
	if etag == "" {
		return storage.ErrVersionedReadUnavailable
	}
	revision, err := listingindex.NewRevision(key, current, etag)
	if err != nil {
		return err
	}
	return listingindex.RepairRevision(s.ctx, s.sourceSetRemote(), revision)
}
