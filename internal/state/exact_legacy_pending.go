package state

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// RemoveExactLegacyPending acknowledges an already-owned ordinary transaction.
// The caller holds the collector lock, as for publication. Exact canonical wire
// equality authenticates every field without allocating another decoded bundle.
// Noncanonical older wire remains recovery work; generic RemovePending is unchanged.
func (s *Store) RemoveExactLegacyPending(id string, expected PendingPublication) error {
	if !safeFileComponent(id) || expected.Catalog != nil || expected.History != nil || expected.Bundle.History != nil || expected.Bundle.SchemaVersion != archive.SourceSchemaVersion || expected.Bundle.ArchiveSessionID != id {
		return ErrCatalogJournalFrozen
	}
	ctx := s.durableContext()
	if err := s.validateExactLegacyPending(ctx, id, expected); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	want, length, err := s.exactLegacyPendingWire(ctx, expected)
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return config.WithDurableStorage(s.home, func(guard config.DurableStorageGuard) error {
		return s.removeExactLegacyPendingGuard(ctx, guard, id, want, length)
	})
}

// Authenticate the already-owned ordinary envelope before comparing its wire.
func (s *Store) validateExactLegacyPending(ctx context.Context, id string, expected PendingPublication) error {
	if err := s.validateReadablePendingContext(ctx, expected); err != nil {
		return err
	}
	key, err := archive.SourceObjectKey(expected.Bundle, expected.SourceSHA256)
	if err != nil || key != expected.SourceKey {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	key, err = archive.MetadataObjectKey(expected.Bundle.Capture.Harness.Name, id)
	if err != nil || key != expected.MetadataKey {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	scratch, closeScratch := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeScratch()
	var metadata archive.Metadata
	if err := scratch.unmarshalOwned(expected.MetadataBytes, &metadata); err != nil {
		return err
	}
	if metadata.SchemaVersion != archive.MetadataSchemaVersion || metadata.SessionID != id || metadata.NativeSessionID != expected.Bundle.NativeSessionID || metadata.ProjectID != expected.Bundle.ProjectID || metadata.Harness.Name != expected.Bundle.Capture.Harness.Name || metadata.ParentSessionID != expected.Bundle.ParentSessionID || metadata.NativeChild != expected.Bundle.NativeChild || !metadata.CapturedAt.Equal(expected.Bundle.Capture.CapturedAt) || metadata.SourceBundle != expected.SourceReference() {
		return ErrDurableStorageRecovery
	}
	return metadata.ValidateSourceReference()
}

// This is the same Encoder default and newline as savePendingGuard. The shared
// ledger covers its one encoder view; caller-owned expected data stays charged.
func (s *Store) exactLegacyPendingWire(ctx context.Context, expected PendingPublication) ([sha256.Size]byte, int64, error) {
	var digest [sha256.Size]byte
	const scratch = int64(32 << 10)
	if err := ctx.Err(); err != nil {
		return digest, 0, err
	}
	limit := int64(durableStorageQuota/2 - 1)
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(scratch) {
			return digest, 0, errStateBudget
		}
		limit = min(limit, s.resourceBudget.Available()-1)
	}
	n, err := jsonwire.Bound(ctx, expected, limit)
	if s.resourceBudget != nil {
		s.resourceBudget.Release(scratch)
	}
	if err != nil {
		return digest, 0, errors.Join(errStateBudget, err)
	}
	n++
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(n) {
			return digest, 0, errStateBudget
		}
		defer s.resourceBudget.Release(n)
	}
	hash := sha256.New()
	wire := exactPendingDigestWriter{writer: hash, remaining: n}
	if err := json.NewEncoder(&wire).Encode(expected); err != nil {
		return digest, 0, err
	}
	if err := ctx.Err(); err != nil {
		return digest, 0, err
	}
	copy(digest[:], hash.Sum(nil))
	return digest, wire.written, nil
}

// Bound reserves an upper ceiling; authenticate the actual encoder byte count.
type exactPendingDigestWriter struct {
	writer    io.Writer
	remaining int64
	written   int64
}

func (w *exactPendingDigestWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errStateBudget
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	w.written += int64(n)
	return n, err
}

func (s *Store) removeExactLegacyPendingGuard(ctx context.Context, guard config.DurableStorageGuard, id string, want [sha256.Size]byte, length int64) (err error) {
	home, err := guard.RootedHome(s.home)
	if err != nil {
		return err
	}
	dir, err := privateDirectory(home.Root, "pending", false)
	if errors.Is(err, os.ErrNotExist) {
		if owed, probeErr := s.protectedStorage(id); owed || probeErr != nil {
			return errors.Join(ErrDurableStorageRecovery, probeErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	name := id + ".json"
	stamp, err := s.readExactLegacyPendingWire(ctx, dir, name, want, length)
	if errors.Is(err, os.ErrNotExist) {
		if owed, probeErr := s.protectedStorage(id); owed || probeErr != nil {
			return errors.Join(ErrDurableStorageRecovery, probeErr)
		}
		return nil
	}
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if err = guard.CheckHome(s.home); err != nil {
		return err
	}
	// Preserve the original staged-source/evidence refusal and cleanup before unlink.
	if err = s.removePendingSources(id, nil); err != nil {
		return err
	}
	named, err := dir.Lstat(name)
	if err != nil || !named.Mode().IsRegular() || named.Mode().Perm() != 0600 || !sameDurableStamp(stamp, named) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if err = errors.Join(ctx.Err(), guard.CheckHome(s.home)); err != nil {
		return err
	}
	if err = dir.Remove(name); err != nil {
		return err
	}
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close(), guard.CheckHome(s.home))
}

func (s *Store) readExactLegacyPendingWire(ctx context.Context, dir *os.Root, name string, want [sha256.Size]byte, length int64) (stamp os.FileInfo, err error) {
	stamp, err = dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !stamp.Mode().IsRegular() || stamp.Mode().Perm() != 0600 || stamp.Size() != length {
		return nil, ErrDurableStorageRecovery
	}
	f, err := dir.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 || !sameDurableStamp(stamp, opened) {
		return nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	const scratch = int64(32 << 10)
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(scratch) {
			return nil, errStateBudget
		}
		defer s.resourceBudget.Release(scratch)
	}
	hash := sha256.New()
	reader := exactPendingContextReader{ctx: ctx, reader: io.LimitReader(f, length+1)}
	count, err := io.CopyBuffer(hash, reader, make([]byte, scratch))
	var got [sha256.Size]byte
	copy(got[:], hash.Sum(nil))
	if err != nil || count != length || got != want {
		return nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	named, err := dir.Lstat(name)
	if err != nil || !named.Mode().IsRegular() || named.Mode().Perm() != 0600 || !sameDurableStamp(stamp, named) {
		return nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	return stamp, ctx.Err()
}

type exactPendingContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r exactPendingContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}
