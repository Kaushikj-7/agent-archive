package state

import (
	"crypto/sha256"
	"errors"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// RemoveExactSourceSetPending acknowledges an already-owned ready source set.
// The caller holds the collector lock. Complete typed validation and exact
// canonical file equality avoid decoding a second copy of a large journal.
// Catalog transactions retain their separate joined completion/removal path.
func (s *Store) RemoveExactSourceSetPending(id string, expected PendingPublication) error {
	if !safeFileComponent(id) || expected.JournalVersion != 5 || expected.Phase != PublicationReady || expected.Commit == nil || expected.Catalog != nil || expected.Selection != nil || expected.StagedProof() != nil || expected.Bundle.ArchiveSessionID != id {
		return ErrDurableStorageRecovery
	}
	ctx := s.durableContext()
	if err := expected.ValidatePublicationBudgeted(ctx, s.resourceBudget); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := expected.ValidateHistoryBudgeted(id, s.resourceBudget); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	reservation := s.readReservation()
	const scratch = 32 << 10
	if !reservation.Reserve(scratch) {
		return errStateBudget
	}
	wire := publicationWire(expected)
	limit, err := jsonwire.Bound(ctx, wire, durableStorageQuota/2-1)
	reservation.Release(scratch)
	if err != nil {
		return err
	}
	hash := sha256.New()
	output := exactPendingDigestWriter{writer: hash, remaining: limit + 1}
	if err = jsonwire.Encode(ctx, &output, wire, s.readReservation()); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	var want [sha256.Size]byte
	copy(want[:], hash.Sum(nil))
	return config.WithPublicationComposition(s.home, func(g config.PublicationCompositionGuard) error {
		storage, err := g.Storage(s.home)
		if err != nil {
			return err
		}
		return s.removeExactLegacyPendingGuard(ctx, storage, id, want, output.written)
	})
}
