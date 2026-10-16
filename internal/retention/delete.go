package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DeleteWholeSession removes a session from the bucket: its metadata, then
// every object under sessions/<harness>/<archive id>/. Discovery goes first:
// if source deletion is interrupted, unreferenced objects remain for the next
// attempt, but no live metadata points at missing data. Whole-session
// retention and backfill undo both delete through it.
func DeleteWholeSession(ctx context.Context, store storage.ObjectStore, harness, archiveSessionID string) error {
	metadataKey, err := archive.MetadataObjectKey(harness, archiveSessionID)
	if err != nil {
		return err
	}
	if harness == "codex" {
		raw, readErr := boundedHistoryMetadata(ctx, store, metadataKey)
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return readErr
		}
		if readErr == nil {
			var metadata archive.Metadata
			if err := json.Unmarshal(raw, &metadata); err != nil {
				return err
			}
			if _, err := metadata.SourceReferences(); err != nil {
				return err
			}
			if metadata.SessionID != archiveSessionID || metadata.Harness.Name != harness {
				return errors.New("deletion metadata belongs to another session")
			}
		}
	}
	if transactional, ok := store.(interface {
		DeleteSession(context.Context, string) error
		CatalogMetadataAuthority() bool
	}); ok && transactional.CatalogMetadataAuthority() {
		return transactional.DeleteSession(ctx, metadataKey)
	}
	if err := store.Delete(ctx, metadataKey); err != nil {
		return fmt.Errorf("delete metadata: %w", err)
	}
	indexErr := listingindex.DeleteSession(ctx, store, harness, archiveSessionID)
	prefix := fmt.Sprintf("sessions/%s/%s/", harness, archiveSessionID)
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return errors.Join(indexErr, fmt.Errorf("list %q: %w", prefix, err))
	}
	for _, obj := range objects {
		if err := store.Delete(ctx, obj.Key); err != nil {
			return errors.Join(indexErr, fmt.Errorf("delete %q: %w", obj.Key, err))
		}
	}
	if indexErr != nil {
		return fmt.Errorf("delete listing index: %w", indexErr)
	}
	return nil
}

// boundedHistoryMetadata refuses allocation-unbounded deletion authority reads.
func boundedHistoryMetadata(ctx context.Context, store storage.ObjectStore, key string) ([]byte, error) {
	bounded, ok := store.(storage.LimitedGetter)
	if !ok {
		return nil, errors.New("history cleanup requires bounded metadata reads")
	}
	const limit = 32 << 20
	raw, err := bounded.GetLimited(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, storage.ErrObjectTooLarge
	}
	return raw, nil
}
