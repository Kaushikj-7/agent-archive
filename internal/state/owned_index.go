package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Index views retain the captured provider until the checked callback ends.
// Default readers preserve their existing I/O and decoding behavior.
func (s *Store) readIndexControlJSON(path string, value any) error {
	if s.resourceReservation == nil {
		return local.Read(path, value)
	}
	closeScratch, err := s.ownIndexExtent(32 << 10)
	if err != nil {
		return err
	}
	defer closeScratch()
	return s.readBudgeted(path, value, true)
}

func readIndexControlValue[T any](s *Store, path string) (T, bool, error) {
	var value T
	err := s.readIndexControlJSON(path, &value)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	return value, err == nil, err
}

func (s *Store) ownIndexExtent(n int64) (func(), error) {
	if s.resourceReservation == nil {
		return func() {}, nil
	}
	if err := s.resourceContext.Err(); err != nil {
		return nil, err
	}
	owner := s.readReservation()
	if !owner.Reserve(n) {
		return nil, errStateBudget
	}
	return func() { owner.Release(n) }, nil
}

func (s *Store) retainIndexExtent(n int64) error {
	releaseExtent, err := s.ownIndexExtent(n)
	if err != nil {
		return err
	}
	if s.resourceReservation != nil {
		*s.resourceReleases = append(*s.resourceReleases, releaseExtent)
	}
	return nil
}

func (s *Store) indexRecoveryError(err error) error {
	if errors.Is(err, errUnsupportedNativeReservation) {
		return err
	}
	if s.resourceReservation != nil {
		return errors.Join(ErrSessionIndexRecoveryRequired, err)
	}
	return ErrSessionIndexRecoveryRequired
}

func (s *Store) readIndexRaw(path string) ([]byte, error) {
	if s.resourceReservation == nil {
		return os.ReadFile(path)
	}
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	rel, err := filepath.Rel(s.home, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, errors.Join(errStateFileUnstable, err)
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	n := info.Size()
	if !info.Mode().IsRegular() || n < 0 || n > ((1<<62)-1) {
		return nil, errStateFileUnstable
	}
	// Raw bytes and their string projection coexist in sentinel classification.
	if err = s.retainIndexExtent(2 * n); err != nil {
		return nil, err
	}
	raw := make([]byte, n)
	if _, err = io.ReadFull(file, raw); err != nil {
		return nil, err
	}
	var extra [1]byte
	if count, readErr := file.Read(extra[:]); count != 0 || !errors.Is(readErr, io.EOF) {
		return nil, errStateFileUnstable
	}
	if err = s.resourceContext.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *Store) checkOwnedPackedMiss(marker sessionIndexMarker) error {
	raw, err := s.readIndexRaw(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel))
	if err != nil {
		return s.indexRecoveryError(err)
	}
	anchor, valid := packedOverlayAnchor(string(raw), marker)
	if !valid {
		return ErrSessionIndexRecoveryRequired
	}
	if anchor == "" {
		return nil
	}
	var entry qualifiedSessionIndexEntry
	if err = s.readIndexControlJSON(filepath.Join(s.home, "sessions-v1", anchor+".json"), &entry); err != nil {
		return s.indexRecoveryError(err)
	}
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	if key.Validate() != nil || packedIndexHash(key) != anchor {
		return ErrSessionIndexRecoveryRequired
	}
	err = entry.validate(key)
	if err == nil || errors.Is(err, ErrSessionIdentityConflict) {
		return nil
	}
	return err
}
