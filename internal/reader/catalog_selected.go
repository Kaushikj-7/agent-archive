package reader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ReadCachedMetadata reads one full body against this handle's successful
// refresh and query authority. The caller must keep the handle open. Remote
// callers share the same WithReadView context used for RefreshRemote. Missing
// or corrupt body bytes return false for an authoritative selected-store read;
// stale generations, damaged row tuples and expired/different views error.
func (c *SQLiteSessionCatalog) ReadCachedMetadata(ctx context.Context, key string) (MetadataLookup, bool, error) {
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	if !c.viewReady {
		return MetadataLookup{}, false, ErrStaleCatalogCursor
	}
	if c.remoteSnapshot != nil {
		snapshot, err := catalog.OpenSnapshot(ctx, c.store, nil)
		if err != nil {
			return MetadataLookup{}, false, err
		}
		if snapshot != c.remoteSnapshot {
			return MetadataLookup{}, false, ErrStaleCatalogCursor
		}
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MetadataLookup{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var epoch string
	var generation int64
	var complete bool
	if err = tx.QueryRowContext(ctx, "SELECT epoch,generation,complete FROM catalog_state WHERE id=1").Scan(&epoch, &generation, &complete); err != nil {
		return MetadataLookup{}, false, err
	}
	if epoch != c.viewEpoch || generation != c.viewGeneration || !complete {
		return MetadataLookup{}, false, ErrStaleCatalogCursor
	}
	var record catalogRecord
	err = record.scan(tx.QueryRowContext(ctx, "SELECT key,etag,hash,capture,activity,summary,search,lowerid,unlabeled,summary_hash FROM sessions WHERE key=?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return MetadataLookup{}, false, nil
	}
	if err != nil {
		return MetadataLookup{}, false, err
	}
	if !record.valid() {
		return MetadataLookup{}, false, ErrInvalidMetadata
	}
	if err = tx.Commit(); err != nil {
		return MetadataLookup{}, false, err
	}
	raw, found := c.opts.Cache.get(record.key, record.etag)
	if !found || !storage.VerifySHA256(raw, record.hash) {
		return MetadataLookup{}, false, nil
	}
	metadata, err := decodeMetadata(record.key, raw)
	if err != nil {
		return MetadataLookup{}, false, err
	}
	identity, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || identity != record.key {
		return MetadataLookup{}, false, ErrInvalidMetadata
	}
	projection, err := json.Marshal(summarize(metadata))
	if err != nil || string(projection) != string(record.summary) {
		return MetadataLookup{}, false, ErrInvalidMetadata
	}
	if err = ctx.Err(); err != nil {
		return MetadataLookup{}, false, err
	}
	if c.remoteSnapshot != nil {
		if _, err = catalog.OpenSnapshot(ctx, c.store, nil); err != nil {
			return MetadataLookup{}, false, err
		}
	}
	return MetadataLookup{Key: record.key, Metadata: metadata}, true, nil
}
