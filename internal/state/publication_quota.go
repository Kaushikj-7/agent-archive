package state

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/local"
)

// publishedProtocol classifies existing physical state under the held root,
// returning its mode and the bytes it counts against the quota (see
// publishedCharge). Classification only accounts bytes; it supplies no source
// or deletion authority.
func (q *durableQuota) publishedProtocol(ctx context.Context, path string, expected os.FileInfo) (mode int, charged int64, err error) {
	if err = ctx.Err(); err != nil {
		return 0, 0, err
	}
	if err = q.guard.CheckHome(q.path); err != nil {
		return 0, 0, err
	}
	if reused, reusedCharge, ok, e := q.accountingClassification(path, expected); e != nil {
		return 0, 0, e
	} else if ok {
		return reused, reusedCharge, nil
	}
	mode, charged, _, err = q.store.readPublishedClassification(ctx, q.home, path, expected)
	if e := q.guard.CheckHome(q.path); e != nil {
		return 0, 0, errors.Join(err, e)
	}
	return mode, charged, err
}

func (s *Store) readPublishedClassification(ctx context.Context, home *local.RootedHome, path string, expected os.FileInfo) (mode int, charged int64, sum [32]byte, err error) {
	var p publishedState
	sum, release, err := s.readPublishedDecoded(ctx, home, path, expected, &p)
	defer release()
	if err != nil {
		return 0, 0, sum, err
	}
	mode = publishedAccountingMode(p)
	if mode == 0 {
		return 0, 0, sum, ErrDurableStorageRecovery
	}
	return mode, publishedCharge(p, mode, expected.Size()), sum, nil
}

// publishedCharge is what a classified published file counts against the
// durable quota, before the double charge for atomic replacement. A mode-1
// file is uncharged published cache. A mode-2 (source-set) file is charged
// only for the source bytes it still holds inline: its cached bundle is the
// same published cache every other version keeps outside the quota, and
// settled states release their inline bytes (releaseSettledInline), so
// charging the whole file would let ordinary history fill the quota and
// refuse every later publication.
func publishedCharge(p publishedState, mode int, size int64) int64 {
	if mode != 2 {
		return 0
	}
	var n int64
	retained := func(payload PublicationPayload) {
		if !payload.Released && len(payload.Inline) > 0 {
			// Charged as encoded on disk.
			n += int64(base64.StdEncoding.EncodedLen(len(payload.Inline)))
		}
	}
	for _, source := range p.Payloads {
		retained(source.Payload)
	}
	if p.Preparation != nil {
		for _, input := range p.Preparation.Inputs {
			retained(input.Payload)
		}
	}
	return min(n, size)
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
	if s.publishedReadDigest != nil {
		*s.publishedReadDigest = sum
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
			charged := info.Size()
			if supportedName {
				version, c, e := q.publishedProtocol(ctx, path, info)
				if e != nil {
					c = info.Size()
					version, e = q.unreadablePublishedCharge(ctx, path, info, e)
					if e != nil {
						return e
					}
				}
				if version == 1 {
					continue
				}
				charged = c
			} else {
				u.recovery = true
			}
			if charged > (durableStorageQuota-u.charged)/2 {
				return ErrDurableStorageCapacity
			}
			u.physical += info.Size()
			u.charged += 2 * charged
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}

// chargedInFull is the at-rest charge of a new file that counts its whole size.
const chargedInFull int64 = -1

// publishedAdditional is the quota growth for replacing published path (old,
// or nil when absent) with n bytes that will count charge at rest. A file
// charged in full needs room for its own double charge and for the old file
// beside it until the rename. Settled published state (charge >= 0) is cache
// outside the quota apart from its charge, so it needs only the transient copy,
// covered first by the old file's atomic-copy allowance, and its new charge.
// A copy left by an interrupted write holds the census in recovery until it
// is swept, whatever was reserved for it.
func (q *durableQuota) publishedAdditional(ctx context.Context, path string, old os.FileInfo, n, charge int64) (int64, error) {
	if n < 0 || n > durableStorageQuota/2 {
		return 0, ErrDurableStorageCapacity
	}
	o, c := int64(0), int64(0)
	if old != nil {
		o = old.Size()
		if o < 0 || o > durableStorageQuota {
			return 0, ErrDurableStorageCapacity
		}
		// Credit exactly what scanPublished charged the old file.
		_, charged, err := q.publishedProtocol(ctx, path, old)
		if err != nil {
			return 0, err
		}
		c = 2 * charged
	}
	// n <= Q/2 and o <= Q: these expressions cannot overflow int64.
	if charge < 0 {
		return max(int64(0), o+n-c, 2*n-c), nil
	}
	return max(int64(0), n-c/2, 2*min(charge, n)-c), nil
}

func legacyBlockedPublicationControl(p publishedState) bool {
	return p.PublicationVersion == 0 && p.Status == CacheStatusBlocked && p.Bundle.ArchiveSessionID == "" && p.Bundle.NativeSessionID == "" && p.Commit == nil && len(p.Sources) == 0 && len(p.MetadataBytes) == 0 && p.LastPublished == nil && p.PublishedAt.IsZero()
}

// unreadablePublishedCharge accounts only a stable permission-denied peer at
// the full protected rate. It grants no validated protocol fact or replacement
// credit; writes to that file still require publishedProtocol to succeed.
//
//nolint:errorlint // Only bare open errors and errno qualify; unwrapping joined failures would discard independent refusal causes.
func (q *durableQuota) unreadablePublishedCharge(ctx context.Context, path string, expected os.FileInfo, readErr error) (int, error) {
	//lint:ignore errorlint Only raw open errors qualify; unwrapping joined failures discards independent refusals.
	openErr, ok := readErr.(*os.PathError)
	//lint:ignore errorlint Only bare errno qualifies; wrapped or joined failures remain refusals.
	if !ok || openErr.Op != "openat" || openErr.Path != path || (openErr.Err != syscall.EACCES && openErr.Err != syscall.EPERM) {
		return 0, readErr
	}
	if err := ctx.Err(); err != nil {
		return 0, errors.Join(readErr, err)
	}
	if err := q.guard.CheckHome(q.path); err != nil {
		return 0, errors.Join(readErr, err)
	}
	named, err := q.home.Root.Lstat(path)
	if err != nil || expected == nil || !named.Mode().IsRegular() || !sameDurableStamp(expected, named) || expected.Mode() != named.Mode() {
		return 0, errors.Join(readErr, ErrDurableStorageRecovery, err)
	}
	if err := errors.Join(ctx.Err(), q.home.Check(), q.guard.CheckHome(q.path)); err != nil {
		return 0, errors.Join(readErr, err)
	}
	return 2, nil
}
