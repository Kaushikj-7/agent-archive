package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// CatalogJournalDigest validates frozen source/history authority and hashes the
// whole immutable journal. Source bytes use their verified hash and size so
// recovery adds no second source payload or persisted proof copy.
func (s *Store) CatalogJournalDigest(id string, pending PendingPublication) (string, error) {
	return s.catalogJournalDigest(s.durableContext(), id, pending)
}

func (s *Store) catalogJournalDigest(ctx context.Context, id string, pending PendingPublication) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if pending.Catalog == nil || pending.Catalog.Protocol != 10 {
		return "", ErrDurableStorageRecovery
	}
	if err := s.validateReadablePendingContext(ctx, pending); err != nil {
		return "", err
	}
	if err := pending.ValidateHistoryBudgeted(id, s.resourceBudget); err != nil {
		return "", err
	}
	frozen := pending
	commit := *pending.Catalog
	commit.Recovery = nil
	frozen.Catalog = &commit
	frozen.Attempted = false
	if !pending.CarriesNoSource() {
		frozen.SourceSize = len(pending.SourceBytes)
	}
	frozen.SourceBytes = nil
	digest := sha256.New()
	if err := json.NewEncoder(digest).Encode(frozen); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// VerifyCatalogJournal verifies a descriptor against the exact persisted
// pending transaction while its originating collector lock remains held.
func (s *Store) VerifyCatalogJournal(id string, pending PendingPublication, guard *local.CollectorGuard, destination string) error {
	digest, err := s.verifyCatalogJournal(id, pending, guard, destination)
	if err != nil {
		return err
	}
	persisted, found, err := s.LoadPending(id)
	if err != nil || !found || persisted.Catalog == nil || persisted.Catalog.Recovery == nil || *persisted.Catalog.Recovery != *pending.Catalog.Recovery {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	actual, err := s.verifyCatalogJournal(id, persisted, guard, destination)
	if err != nil || actual != digest {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

// VerifyPersistedCatalogJournal verifies the actual originating journal before
// destination admission. It loads bounded source/history authority once and
// accepts neither a caller digest nor metadata different from the frozen bytes.
func (s *Store) VerifyPersistedCatalogJournal(j local.CatalogJournal, raw []byte, guard *local.CollectorGuard, destination string) error {
	pending, found, err := s.LoadPending(j.SessionID)
	if err != nil || !found || pending.Catalog == nil || pending.Catalog.Recovery == nil || *pending.Catalog.Recovery != j || !bytes.Equal(raw, pending.MetadataBytes) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	_, err = s.verifyCatalogJournal(j.SessionID, pending, guard, destination)
	return err
}

func (s *Store) verifyCatalogJournal(id string, pending PendingPublication, guard *local.CollectorGuard, destination string) (string, error) {
	if pending.Catalog == nil || pending.Catalog.Recovery == nil {
		return "", ErrDurableStorageRecovery
	}
	j := *pending.Catalog.Recovery
	home, homeErr := guard.Home()
	expectedHome, resolveErr := filepath.EvalSymlinks(s.home)
	if homeErr != nil || resolveErr != nil || home != expectedHome {
		return "", ErrDurableStorageRecovery
	}
	origin, err := guard.Origin()
	if err != nil || j.Validate() != nil || j.Origin != origin || j.Destination != destination || j.SessionID != id || j.MutationID != pending.Catalog.ID || j.ExpectedRevision != pending.Catalog.ExpectedRevision {
		return "", errors.Join(ErrDurableStorageRecovery, err)
	}
	digest, err := s.CatalogJournalDigest(id, pending)
	if err != nil || digest != j.SHA256 {
		return "", errors.Join(ErrDurableStorageRecovery, err)
	}
	return digest, nil
}
