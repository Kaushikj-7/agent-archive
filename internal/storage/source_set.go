package storage

import (
	"context"
	"errors"
	"fmt"
)

// PutVerifiedSource stores immutable bytes only when absent, then verifies them.
// It never changes a metadata pointer and refuses an existing different object.
// verify belongs to the caller so its bounded reads share the source-data lease.
func PutVerifiedSource(ctx context.Context, store ObjectStore, key, sha string, data []byte, retry RetryPolicy, verify func(context.Context, string, string, int) error) error {
	if key == "" || !VerifySHA256(data, sha) || verify == nil {
		return errors.New("invalid immutable source checksum or key")
	}
	return retry.run(ctx, func() error {
		err := verify(ctx, key, sha, len(data))
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("verify immutable source: %w", err)
		}
		if err := store.Put(ctx, key, data); err != nil {
			return err
		}
		return verify(ctx, key, sha, len(data))
	})
}
