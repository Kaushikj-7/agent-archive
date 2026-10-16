package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/local"
)

// publishedProtocol classifies existing physical state under the held root.
// Classification only accounts bytes; it supplies no source or deletion authority.
func (q *durableQuota) publishedProtocol(ctx context.Context, path string, expected os.FileInfo) (mode int, err error) {
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if err = q.guard.CheckHome(q.path); err != nil {
		return 0, err
	}
	if reused, ok, e := q.accountingClassification(path, expected); e != nil {
		return 0, e
	} else if ok {
		return reused, nil
	}
	mode, _, err = q.store.readPublishedClassification(ctx, q.home, path, expected)
	if e := q.guard.CheckHome(q.path); e != nil {
		return 0, errors.Join(err, e)
	}
	return mode, err
}

func (s *Store) readPublishedClassification(ctx context.Context, home *local.RootedHome, path string, expected os.FileInfo) (mode int, sum [32]byte, err error) {
	var p publishedState
	sum, release, err := s.readPublishedDecoded(ctx, home, path, expected, &p)
	defer release()
	if err != nil {
		return 0, sum, err
	}
	mode = publishedAccountingMode(p)
	if mode == 0 {
		return 0, sum, ErrDurableStorageRecovery
	}
	return mode, sum, nil
}

func publishedAccountingMode(p publishedState) int {
	// These mature protocols keep their existing uncharged published-cache
	// contract. The closed decoder has already validated their exact proof.
	if p.LastPublished != nil {
		if p.PublicationVersion == 2 && p.LastPublished.Selection != nil && p.LastPublished.Selection.validateProofDigest() == nil {
			return 1
		}
		if proof := p.LastPublished.StagedProof(); proof != nil && (p.PublicationVersion == 3 || p.PublicationVersion == 4) && proof.Version == p.PublicationVersion && proof.validateDigest() == nil {
			return 1
		}
	}
	if p.PublicationVersion == 5 {
		return 2
	}
	if legacyBlockedPublicationControl(p) {
		switch p.BlockedReason {
		case BlockedReasonTranscriptRewritten, BlockedReasonTranscriptTooLarge, BlockedReasonRecordTooLarge, BlockedReasonTranscriptMissing:
			return 1
		}
	}
	if p.PublicationVersion == 0 && p.Bundle.ArchiveSessionID != "" && p.Bundle.NativeSessionID != "" {
		return 1
	}
	return 0
}

func (s *Store) readPublishedDecoded(ctx context.Context, home *local.RootedHome, path string, expected os.FileInfo, p *publishedState) (sum [32]byte, release func(), err error) {
	reservation := s.readReservation()
	if err := ctx.Err(); err != nil {
		return sum, func() {}, err
	}
	if !expected.Mode().IsRegular() || expected.Size() < 0 {
		return sum, func() {}, ErrDurableStorageRecovery
	}
	if expected.Size() > durableStorageQuota {
		return sum, func() {}, ErrDurableStorageCapacity
	}
	n := expected.Size()
	if s.resourceBudget != nil {
		if !reservation.Reserve(2 * n) {
			return sum, func() {}, errStateBudget
		}
		defer reservation.Release(n)
		keep := false
		defer func() {
			if !keep {
				reservation.Release(n)
			}
		}()
		defer func() {
			if err == nil {
				keep = true
				release = func() { reservation.Release(n) }
			}
		}()
	}
	f, err := home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sum, func() {}, err
	}
	defer func() {
		if closeErr := s.closePublicationFile(f); closeErr != nil {
			sum = [32]byte{}
			err = errors.Join(err, closeErr)
		}
	}()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(expected, opened) {
		return sum, func() {}, errors.Join(ErrDurableStorageRecovery, err)
	}
	raw := make([]byte, n)
	if _, err = io.ReadFull(f, raw); err != nil {
		return sum, func() {}, errors.Join(ErrDurableStorageRecovery, err)
	}
	var extra [1]byte
	count, e := f.Read(extra[:])
	if count != 0 || !errors.Is(e, io.EOF) {
		return sum, func() {}, errors.Join(ErrDurableStorageRecovery, e)
	}
	sum = sha256.Sum256(raw)
	if err = decodePublishedState(raw, p, ctx, s.resourceBudget, reservation); err != nil {
		return sum, func() {}, errors.Join(ErrDurableStorageRecovery, err)
	}
	finalOpened, err := f.Stat()
	if err != nil || !sameDurableStamp(expected, finalOpened) {
		return sum, func() {}, errors.Join(ErrDurableStorageRecovery, err)
	}
	named, err := home.Root.Lstat(path)
	if err != nil || !sameDurableStamp(expected, named) {
		return sum, func() {}, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err = home.Check(); err != nil {
		return sum, func() {}, err
	}
	if err = ctx.Err(); err != nil {
		return sum, func() {}, err
	}
	return sum, func() {}, nil
}

func (q *durableQuota) scanPublished(ctx context.Context, remaining *int, u *durableUsage) (err error) {
	root, err := privateDirectory(q.home.Root, "published", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		entries, e := dir.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return ErrDurableStorageRecovery
			}
			info, e := root.Lstat(entry.Name())
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || info.Size() < 0 {
				return ErrDurableStorageRecovery
			}
			path := filepath.Join("published", entry.Name())
			supportedName := !strings.HasPrefix(entry.Name(), ".pending-") && strings.HasSuffix(entry.Name(), ".json") && safeFileComponent(strings.TrimSuffix(entry.Name(), ".json"))
			if supportedName {
				version, e := q.publishedProtocol(ctx, path, info)
				if e != nil {
					return e
				}
				if version == 1 {
					continue
				}
			} else {
				u.recovery = true
			}
			if info.Size() > (durableStorageQuota-u.charged)/2 {
				return ErrDurableStorageCapacity
			}
			u.physical += info.Size()
			u.charged += 2 * info.Size()
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}

func (q *durableQuota) publishedAdditional(ctx context.Context, path string, old os.FileInfo, n int64) (int64, error) {
	if n < 0 || n > durableStorageQuota/2 {
		return 0, ErrDurableStorageCapacity
	}
	o, c := int64(0), int64(0)
	if old != nil {
		o = old.Size()
		if o < 0 || o > durableStorageQuota {
			return 0, ErrDurableStorageCapacity
		}
		version, err := q.publishedProtocol(ctx, path, old)
		if err != nil {
			return 0, err
		}
		if version == 2 {
			c = 2 * o
		}
	}
	// n <= Q/2 and o <= Q: these expressions cannot overflow int64.
	return max(int64(0), o+n-c, 2*n-c), nil
}

func legacyBlockedPublicationControl(p publishedState) bool {
	return p.PublicationVersion == 0 && p.Status == CacheStatusBlocked && p.Bundle.ArchiveSessionID == "" && p.Bundle.NativeSessionID == "" && p.Commit == nil && len(p.Sources) == 0 && len(p.MetadataBytes) == 0 && p.LastPublished == nil && p.PublishedAt.IsZero()
}
