package storage

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// ErrPublicationConflict keeps an obsolete or unknown mutation pending without
// replacing a different authoritative body. Read/compare/Put requires the
// documented single-owner boundary; it is not a cross-machine atomic CAS.
var ErrPublicationConflict = errors.New("publication predecessor differs or is unknown; reconcile retained publication before retrying")

// SourcePublication is an exact checksum-addressed object with optional replay bytes.
type SourcePublication struct {
	Key    string
	SHA256 string
	Size   int
	Bytes  []byte
}

// MetadataPredecessor distinguishes unknown, known absent and exact known present.
type MetadataPredecessor struct {
	Known  bool
	Exists bool
	SHA256 string
}

// SourceResolver borrows exactly one bounded replay payload. Release ends memory
// ownership only; durable evidence remains until the local selecting commit.
type SourceResolver func(context.Context, int) ([]byte, func(), error)

// SourceVerifier validates exact remote bytes under the caller's shared ledger.
type SourceVerifier func(context.Context, string, string, int) error

// ValidatedPublicationReadback is phase C, produced only by an actual bounded
// exact readback. It is consumed once inside the transaction frame.
type ValidatedPublicationReadback struct {
	key      string
	sha      string
	body     []byte
	etag     string
	consumed *atomic.Bool
}

// Consume transfers the exact response once for its bound metadata key and body.
func (r *ValidatedPublicationReadback) Consume(key string, metadata []byte) ([]byte, string, error) {
	if r == nil || r.consumed == nil || r.key != key || r.sha != SHA256Hex(metadata) || len(r.body) != len(metadata) || SHA256Hex(r.body) != r.sha {
		return nil, "", ErrPublicationConflict
	}
	if !r.consumed.CompareAndSwap(false, true) {
		return nil, "", ErrPublicationConflict
	}
	return r.body, r.etag, nil
}

// PutResolvedSourceSetThenMetadataReadback verifies predecessor positioning and selected sources before returning the exact metadata response.
func PutResolvedSourceSetThenMetadataReadback(ctx context.Context, store ObjectStore, sources []SourcePublication, key string, metadata []byte, prior MetadataPredecessor, retry RetryPolicy, resolve SourceResolver, verify SourceVerifier) (*ValidatedPublicationReadback, error) {
	if len(sources) == 0 || len(sources) > 65 || key == "" || len(metadata) == 0 || len(metadata) > 32<<20 {
		return nil, errors.New("publication source set or metadata exceeds bounds")
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if source.Key == "" || source.Key == key || seen[source.Key] || source.Size <= 0 || source.Size > 128<<20 || len(source.SHA256) != 64 {
			return nil, errors.New("invalid publication source bounds")
		}
		if len(source.Bytes) > 0 && (len(source.Bytes) != source.Size || !VerifySHA256(source.Bytes, source.SHA256)) {
			return nil, ErrChecksumMismatch
		}
		seen[source.Key] = true
	}
	next := SHA256Hex(metadata)
	if _, err := publicationPosition(ctx, store, key, next, prior); err != nil {
		return nil, err
	}
	if err := verifyResolvedSourceSet(ctx, store, sources, retry, resolve, verify); err != nil {
		return nil, err
	}
	var receipt *ValidatedPublicationReadback
	err := retry.run(ctx, func() error {
		completed, err := publicationPosition(ctx, store, key, next, prior)
		if err != nil {
			return err
		}
		if !completed {
			if err := store.Put(ctx, key, metadata); err != nil {
				return err
			}
		}
		body, etag, err := ReadPublicationMetadataRevision(ctx, store, key, int64(len(metadata)))
		if err != nil {
			if errors.Is(err, ErrObjectTooLarge) {
				return errors.Join(ErrChecksumMismatch, err)
			}
			return err
		}
		if len(body) != len(metadata) || SHA256Hex(body) != next {
			return fmt.Errorf("verify committed metadata: %w", ErrChecksumMismatch)
		}
		receipt = &ValidatedPublicationReadback{key: key, sha: next, body: body, etag: etag, consumed: &atomic.Bool{}}
		return nil
	})
	return receipt, err
}

// ReadPublicationMetadata bounds authoritative publication readback to 32 MiB.
// Providers with LimitedGetter enforce the allocation bound while reading.
func ReadPublicationMetadata(ctx context.Context, store ObjectStore, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	getter, ok := store.(LimitedGetter)
	if !ok {
		return nil, ErrVersionedReadUnavailable
	}
	body, err := getter.GetLimited(ctx, key, 32<<20)
	if err == nil && len(body) > 32<<20 {
		return nil, ErrObjectTooLarge
	}
	return body, err
}

// PublicationPositionValidator applies an owning reader's typed history/owner
// gate to the ACTUAL position response before source/metadata side effects.
type PublicationPositionValidator interface {
	ValidatePublicationPosition(context.Context, string, []byte) error
}

func publicationPosition(ctx context.Context, store ObjectStore, key, next string, prior MetadataPredecessor) (bool, error) {
	body, err := ReadPublicationMetadata(ctx, store, key)
	if errors.Is(err, ErrNotFound) {
		if prior.Known && !prior.Exists {
			if checker, ok := store.(PublicationPositionValidator); ok {
				if err := checker.ValidatePublicationPosition(ctx, key, nil); err != nil {
					return false, err
				}
			}
			return false, nil
		}
		return false, ErrPublicationConflict
	}
	if err != nil {
		return false, err
	}
	digest := SHA256Hex(body)
	if digest == next {
		if checker, ok := store.(PublicationPositionValidator); ok {
			if err := checker.ValidatePublicationPosition(ctx, key, body); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if prior.Known && prior.Exists && prior.SHA256 == digest {
		if checker, ok := store.(PublicationPositionValidator); ok {
			if err := checker.ValidatePublicationPosition(ctx, key, body); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	return false, ErrPublicationConflict
}

func publishExactSource(ctx context.Context, store ObjectStore, source SourcePublication) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := verifyStoredObject(ctx, store, source.Key, source.SHA256, source.Size)
	if err == nil || !errors.Is(err, ErrNotFound) || len(source.Bytes) == 0 {
		return err
	}
	if err := store.Put(ctx, source.Key, source.Bytes); err != nil {
		return err
	}
	return verifyStoredObject(ctx, store, source.Key, source.SHA256, source.Size)
}

// VerifySourceSet checks a bounded complete set sequentially. Ref-only entries
// are read-only; entries with frozen bytes may fill absent immutable objects.
// A mismatching existing object is never overwritten. Each object gets one
// bounded retry allowance, independently of the later metadata allowance.
func VerifySourceSet(ctx context.Context, store ObjectStore, sources []SourcePublication, retry RetryPolicy) error {
	return verifyResolvedSourceSet(ctx, store, sources, retry, nil, nil)
}

func verifyResolvedSourceSet(ctx context.Context, store ObjectStore, sources []SourcePublication, retry RetryPolicy, resolve SourceResolver, verify SourceVerifier) error {
	if len(sources) == 0 || len(sources) > 65 {
		return errors.New("source-set reference count exceeds bounds")
	}
	seen := map[string]bool{}
	for i, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if source.Key == "" || seen[source.Key] || source.Size <= 0 || source.Size > 128<<20 || len(source.SHA256) != 64 {
			return errors.New("invalid source-set object bounds")
		}
		seen[source.Key] = true
		if len(source.Bytes) > 0 && (len(source.Bytes) != source.Size || !VerifySHA256(source.Bytes, source.SHA256)) {
			return ErrChecksumMismatch
		}
		var release func()
		if resolve != nil {
			data, releaseBytes, err := resolve(ctx, i)
			if err != nil {
				if releaseBytes != nil {
					releaseBytes()
				}
				return err
			}
			source.Bytes, release = data, releaseBytes
			if len(data) > 0 && (len(data) != source.Size || !VerifySHA256(data, source.SHA256)) {
				if release != nil {
					release()
				}
				return ErrChecksumMismatch
			}
		}
		operation := func() error {
			if verify == nil {
				return publishExactSource(ctx, store, source)
			}
			err := verify(ctx, source.Key, source.SHA256, source.Size)
			if err == nil || !errors.Is(err, ErrNotFound) || len(source.Bytes) == 0 {
				return err
			}
			if err := store.Put(ctx, source.Key, source.Bytes); err != nil {
				return err
			}
			return verify(ctx, source.Key, source.SHA256, source.Size)
		}
		err := retry.run(ctx, operation)
		if release != nil {
			release()
		}
		if err != nil {
			return fmt.Errorf("verify publication source %q: %w", source.Key, err)
		}
	}
	return nil
}

// ErrVersionedReadUnavailable leaves listing repair owed without an unbounded
// versioned fallback or a fabricated validator from a later HEAD.
var ErrVersionedReadUnavailable = errors.New("bounded response metadata validator unavailable")

// ReadPublicationMetadataRevision returns bounded bytes and the validator from that same response.
func ReadPublicationMetadataRevision(ctx context.Context, store ObjectStore, key string, limit int64) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 32<<20 {
		return nil, "", ErrObjectTooLarge
	}
	if getter, ok := store.(LimitedVersionedGetter); ok {
		body, etag, err := getter.GetLimitedVersioned(ctx, key, limit)
		if !errors.Is(err, ErrVersionedReadUnavailable) {
			if err == nil && int64(len(body)) > limit {
				return nil, "", ErrObjectTooLarge
			}
			if err == nil && ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			return body, etag, err
		}
	}
	getter, ok := store.(LimitedGetter)
	if !ok {
		return nil, "", ErrVersionedReadUnavailable
	}
	body, err := getter.GetLimited(ctx, key, limit)
	if err == nil && int64(len(body)) > limit {
		return nil, "", ErrObjectTooLarge
	}
	if err == nil && ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	return body, "", err
}
