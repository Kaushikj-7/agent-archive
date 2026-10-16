package search

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ReadCurrentMetadata reads one canonical identity with bounded fresh authority.
// Callers provide a finite deadline. Catalog absence and selected bodies must
// still belong to the fresh validated root; legacy bodies use the same bounded
// reader as reconciliation and are never decoded after a failed/partial read.
// Only confirmed identity absence also matches ErrDeleted; an unavailable
// immutable catalog body can match storage.ErrNotFound without proving deletion.
func ReadCurrentMetadata(ctx context.Context, objects storage.ObjectStore, key string) (archive.Metadata, error) {
	var metadata archive.Metadata
	if reader.CatalogAuthority(objects) {
		ctx = catalog.NewReadView(ctx)
		bounded, _, err := catalog.DiscoveryBudget(ctx, 512, 16<<20)
		if err != nil {
			return metadata, err
		}
		snapshot, err := catalog.OpenSnapshot(bounded, objects, nil)
		if err != nil {
			return metadata, err
		}
		entry, err := snapshot.Find(bounded, key)
		if err != nil {
			return metadata, err
		}
		if entry == nil {
			if err = snapshot.ValidateContinuation(bounded); err != nil {
				return metadata, err
			}
			return metadata, errors.Join(ErrDeleted, storage.ErrNotFound)
		}
		metadata, err = reader.ReadCatalogSelection(bounded, snapshot, catalog.Row{Key: key, Entry: *entry})
		if err != nil {
			return archive.Metadata{}, err
		}
		if err = snapshot.ValidateContinuation(bounded); err != nil {
			return archive.Metadata{}, err
		}
	} else {
		body, err := readBoundedMetadata(ctx, objects, key, 16<<20)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return metadata, errors.Join(ErrDeleted, err)
			}
			return metadata, err
		}
		metadata = body.metadata
	}
	canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || canonical != key {
		return archive.Metadata{}, errors.New("search metadata disagrees with canonical session identity")
	}
	return metadata, nil
}

// ForgetSession removes derived state for a confirmed absent qualified identity.
// The caller must hold the shared work/lifecycle lock and establish fresh
// authoritative absence before calling. Removal and cursor invalidation commit
// together through the same transaction as namespace reconciliation.
func (s *Store) ForgetSession(ctx context.Context, harness, session string) error {
	key, err := archive.MetadataObjectKey(harness, session)
	if err != nil {
		return err
	}
	var known bool
	if err = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM search_work WHERE id=? UNION ALL SELECT 1 FROM search_sessions WHERE id=? UNION ALL SELECT 1 FROM search_observed WHERE key=?)", harness+"/"+session, harness+"/"+session, key).Scan(&known); err != nil || !known {
		return err
	}
	return s.forget(ctx, key, harness+"/"+session)
}
